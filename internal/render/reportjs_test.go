package render

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// The report's formatting layer is the last place a value can stop being
// honest: it sits between an int64 measure and what a human actually reads, so
// a rounding choice there can print a number AWS never reported. That logic is
// vanilla JS inside the single-file template, which Go cannot call — these
// tests lift the pure functions out of the template and evaluate them with
// node. They skip when node is absent, so the build itself stays
// dependency-free while CI (which ships node) still enforces them.

// jsFunc lifts a self-contained function declaration out of the report
// template by brace-matching from its body. The report is one file by design,
// so there is no module to import: this is the seam.
func jsFunc(t *testing.T, name string) string {
	t.Helper()
	start := strings.Index(reportTemplate, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found in the report template", name)
	}
	open := strings.Index(reportTemplate[start:], "{")
	if open < 0 {
		t.Fatalf("function %s has no body", name)
	}
	depth := 0
	for i := start + open; i < len(reportTemplate); i++ {
		switch reportTemplate[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return reportTemplate[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces in function %s", name)
	return ""
}

// jsVar lifts a single-line "var NAME = ...;" declaration out of the template.
func jsVar(t *testing.T, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(reportTemplate, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "var "+name+" =") {
			return s
		}
	}
	t.Fatalf("var %s not found in the report template", name)
	return ""
}

// evalReportJS runs the given script under node and returns its stdout.
func evalReportJS(t *testing.T, script string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		// Skipping is right on a contributor's machine and wrong in CI, where a
		// silent skip would report coverage this suite is not providing.
		if os.Getenv("CI") != "" {
			t.Fatal("node is required to evaluate the report's JS in CI")
		}
		t.Skip("node not installed; skipping report JS evaluation")
	}
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// evalJSON runs the script (which must print one JSON value) and unmarshals it.
func evalJSON(t *testing.T, script string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(evalReportJS(t, script)), into); err != nil {
		t.Fatalf("decoding node output: %v", err)
	}
}

// embeddedJSON returns the report's embedded data block.
func embeddedJSON(t *testing.T, html string) string {
	t.Helper()
	_, rest, ok := strings.Cut(html, `<script type="application/json" id="blueprint-data">`)
	if !ok {
		t.Fatal("no embedded data block")
	}
	data, _, ok := strings.Cut(rest, "</script>")
	if !ok {
		t.Fatal("embedded data block is not closed")
	}
	return data
}

// A byte count must render at its own scale. The predecessor of this code
// rounded storage up to whole gigabytes, so every non-empty DynamoDB table
// claimed at least 1 GB; the bag refactor stores exact bytes precisely to end
// that, and the formatter must not reintroduce it one unit down. null stands
// for an absent measure, which is the only input allowed to render as nothing.
func TestReportFormatSizeNeverInflatesReportedBytes(t *testing.T) {
	cases := []struct {
		in   string // JSON; null means "not reported"
		want string
	}{
		{"null", ""}, // absent — the caller paints an em dash
		{"-1", ""},   // not a size anything could report
		{"0", "0 B"}, // a stored zero is a real reading, not silence
		{"1", "1 B"}, //
		{"512", "512 B"},
		{"1024", "1.0 KB"},
		{"4096", "4.0 KB"},    // the regression: must never read "1 MB"
		{"20480", "20 KB"},    //
		{"1048576", "1.0 MB"}, //
		{"5242880", "5.0 MB"}, //
		{"1073741824", "1.0 GB"},
		{"536870912000", "500 GB"},
		{"1099511627776", "1.0 TB"},
		{"16492674416640", "15 TB"},
	}

	in := make([]string, len(cases))
	for i, c := range cases {
		in[i] = c.in
	}
	script := jsFunc(t, "formatSize") + `
var input = [` + strings.Join(in, ",") + `];
console.log(JSON.stringify(input.map(function (b) {
  return formatSize(b === null ? undefined : b);
})));`

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("formatSize(%s) = %q, want %q", c.in, got[i], c.want)
		}
	}
}

// Every resource in the fixture that reports a sizing dimension must render a
// Class. The hand-built cases below pin the rule; this one pins it against the
// data the report actually ships, because the Redshift Serverless gap was
// invisible until someone looked at a real workgroup row.
func TestReportRendersEverySizingDimensionInFixture(t *testing.T) {
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(embeddedJSON(t, renderDemo(t))), &snap); err != nil {
		t.Fatalf("embedded data block is not valid JSON: %v", err)
	}

	// Which resources the report owes a Class, judged from the data alone.
	type want struct {
		name  string
		sized bool
	}
	wants := make([]want, len(snap.Resources))
	sizedCount := 0
	for i, r := range snap.Resources {
		_, hasRPU := r.Measures[model.MeasureBaseCapacityRPU]
		wants[i] = want{
			name: r.Name,
			sized: r.Attr(model.AttrInstanceClass) != "" ||
				r.Attr(model.AttrBillingMode) != "" || hasRPU,
		}
		if wants[i].sized {
			sizedCount++
		}
	}
	if sizedCount == 0 {
		t.Fatal("fixture reports no sizing dimension at all; this test proves nothing")
	}

	resources, err := json.Marshal(snap.Resources)
	if err != nil {
		t.Fatalf("re-encoding resources: %v", err)
	}
	script := strings.Join([]string{
		jsFunc(t, "attr"),
		jsFunc(t, "measure"),
		jsVar(t, "CLASS_KEYS"),
		jsVar(t, "CLASS_MEASURES"),
		jsFunc(t, "classOf"),
		"console.log(JSON.stringify((" + string(resources) + ").map(classOf)));",
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(wants) {
		t.Fatalf("got %d classes, want %d", len(got), len(wants))
	}
	for i, w := range wants {
		switch {
		case w.sized && got[i] == "":
			t.Errorf("%s reports a sizing dimension but renders no Class", w.name)
		case !w.sized && got[i] != "":
			t.Errorf("%s reports no sizing dimension but renders Class %q", w.name, got[i])
		}
	}
}

// The Platform column shows the software a resource runs, which each service
// names differently: RDS reports an engine, Lambda reports a runtime. Both feed
// the same column, the same grouping, and the same end-of-life badge, so a
// column that read only the first key this report happened to support would
// blank every Lambda row — and drop the runtime from the search haystack with
// it, hiding exactly the deprecated runtime the scanner exists to surface.
func TestReportPlatformReadsEveryServicesNameForIt(t *testing.T) {
	cases := []struct {
		name     string
		resource string // JSON
		want     string
	}{
		{"engine", `{"attributes":{"engine":"postgres"}}`, "postgres"},
		{"runtime", `{"attributes":{"runtime":"python3.8"}}`, "python3.8"},
		// A runtime identifier carries its own version, so such rows have no
		// engine_version beside them — the sub-label is simply absent.
		{"runtime with no version attribute", `{"attributes":{"runtime":"go1.x"}}`, "go1.x"},
		{"engine wins over runtime", `{"attributes":{"engine":"aurora-postgresql","runtime":"x"}}`, "aurora-postgresql"},
		// A container-image function seals its runtime inside the image, and no
		// verdict follows from a key AWS never filled in.
		{"nothing reported", `{"attributes":{"instance_class":"m5.large"}}`, ""},
		{"no attributes at all", `{}`, ""},
	}

	in := make([]string, len(cases))
	for i, c := range cases {
		in[i] = c.resource
	}
	script := strings.Join([]string{
		jsFunc(t, "attr"),
		jsVar(t, "PLATFORM_KEYS"),
		jsFunc(t, "platformOf"),
		`console.log(JSON.stringify([` + strings.Join(in, ",") + `].map(platformOf)));`,
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%s: platformOf(%s) = %q, want %q", c.name, c.resource, got[i], c.want)
		}
	}
}

// The same rule against the data the report actually ships. The hand-built
// cases above pin the function; this one pins that no shipped row reports a
// platform the report then declines to show — the gap the Lambda rows fell into
// when the column read "engine" alone.
func TestReportRendersEveryPlatformInFixture(t *testing.T) {
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(embeddedJSON(t, renderDemo(t))), &snap); err != nil {
		t.Fatalf("embedded data block is not valid JSON: %v", err)
	}

	type want struct {
		name     string
		platform string
	}
	wants := make([]want, len(snap.Resources))
	runtimes := 0
	for i, r := range snap.Resources {
		p := r.Attr(model.AttrEngine)
		if p == "" {
			p = r.Attr(model.AttrRuntime)
			if p != "" {
				runtimes++
			}
		}
		wants[i] = want{name: r.Name, platform: p}
	}
	// Without a runtime row in the fixture this test would pass on engines
	// alone and prove nothing about the case it was written for.
	if runtimes == 0 {
		t.Fatal("fixture reports no runtime at all; this test proves nothing")
	}

	resources, err := json.Marshal(snap.Resources)
	if err != nil {
		t.Fatalf("re-encoding resources: %v", err)
	}
	script := strings.Join([]string{
		jsFunc(t, "attr"),
		jsVar(t, "PLATFORM_KEYS"),
		jsFunc(t, "platformOf"),
		"console.log(JSON.stringify((" + string(resources) + ").map(platformOf)));",
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(wants) {
		t.Fatalf("got %d platforms, want %d", len(got), len(wants))
	}
	for i, w := range wants {
		if got[i] != w.platform {
			t.Errorf("%s renders platform %q, want %q", w.name, got[i], w.platform)
		}
	}
}

// The Class column shows whichever dimension a service is sized by. Redshift
// Serverless is sized by a number of RPUs, which lives in the measure bag
// rather than the attribute bag — reading only attributes would blank the
// column for a capacity the service did report, and drop it from the search
// haystack with it.
func TestReportClassFallsBackToMeasures(t *testing.T) {
	cases := []struct {
		name     string
		resource string // JSON
		want     string
	}{
		{"instance class", `{"attributes":{"instance_class":"db.r6g.xlarge"}}`, "db.r6g.xlarge"},
		{"billing mode", `{"attributes":{"billing_mode":"PAY_PER_REQUEST"}}`, "PAY_PER_REQUEST"},
		{"serverless capacity", `{"measures":{"base_capacity_rpu":32}}`, "32 RPU"},
		{"zero capacity is a reading", `{"measures":{"base_capacity_rpu":0}}`, "0 RPU"},
		{"named class wins", `{"attributes":{"instance_class":"dc2.large"},"measures":{"base_capacity_rpu":8}}`, "dc2.large"},
		{"nothing reported", `{}`, ""},
	}

	in := make([]string, len(cases))
	for i, c := range cases {
		in[i] = c.resource
	}
	script := strings.Join([]string{
		jsFunc(t, "attr"),
		jsFunc(t, "measure"),
		jsVar(t, "CLASS_KEYS"),
		jsVar(t, "CLASS_MEASURES"),
		jsFunc(t, "classOf"),
		`console.log(JSON.stringify([` + strings.Join(in, ",") + `].map(classOf)));`,
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%s: classOf(%s) = %q, want %q", c.name, c.resource, got[i], c.want)
		}
	}
}

// Most sizes in the report are current as of the scan; a bucket's is not,
// because CloudWatch publishes it once a day. The label that says so has to be
// the observation's own UTC day — not the reader's, or the report would name a
// different day than the datapoint belongs to — and has to be absent for every
// measure read directly, or the column would imply staleness that isn't there.
func TestReportObservationLabelNamesTheUTCDay(t *testing.T) {
	cases := []struct {
		name string
		in   string // JSON string; null stands for no observation time
		want string
	}{
		{"no observation time", `null`, ""},
		{"empty", `""`, ""},
		{"utc timestamp", `"2026-07-29T12:58:50Z"`, "as of 2026-07-29"},
		{"late enough to move a day westward", `"2026-07-29T01:15:00Z"`, "as of 2026-07-29"},
		{"early enough to move a day eastward", `"2026-07-29T23:45:00Z"`, "as of 2026-07-29"},
		// An offset timestamp names its own day as written, which is the day
		// the stored string claims.
		{"offset timestamp", `"2026-07-29T22:00:00-03:00"`, "as of 2026-07-29"},
		{"not a timestamp", `"whenever"`, ""},
		{"date with no time", `"2026-07-29"`, ""},
		{"well formed but impossible", `"2026-13-45T00:00:00Z"`, ""},
	}

	in := make([]string, len(cases))
	for i, c := range cases {
		in[i] = c.in
	}
	script := strings.Join([]string{
		jsFunc(t, "formatObserved"),
		`console.log(JSON.stringify([` + strings.Join(in, ",") + `].map(formatObserved)));`,
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%s: formatObserved(%s) = %q, want %q", c.name, c.in, got[i], c.want)
		}
	}
}
