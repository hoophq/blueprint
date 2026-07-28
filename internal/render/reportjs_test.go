package render

import (
	"encoding/json"
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
