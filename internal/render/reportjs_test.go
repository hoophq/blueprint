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

// jsVar lifts a "var NAME = ...;" declaration out of the template, whether or
// not it fits on one line: some of them are multi-line object literals, and
// stopping at the first newline hands node an unterminated brace and a syntax
// error pointing at whatever was concatenated next.
//
// The scan counts brackets and stops at the first semicolon outside them. Like
// jsFunc's, it does not understand strings or comments — which holds because
// these are small data declarations, and fails loudly rather than silently if
// one ever isn't.
func jsVar(t *testing.T, name string) string {
	t.Helper()
	prefix := "var " + name + " ="
	pos := 0
	for line := range strings.SplitSeq(reportTemplate, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			start := pos + strings.Index(line, prefix)
			depth := 0
			for i := start; i < len(reportTemplate); i++ {
				switch reportTemplate[i] {
				case '{', '[', '(':
					depth++
				case '}', ']', ')':
					depth--
				case ';':
					if depth == 0 {
						return reportTemplate[start : i+1]
					}
				}
			}
			t.Fatalf("var %s is never terminated", name)
		}
		pos += len(line) + 1
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

// The two fixture tests below read the census back out of a rendered report
// with decodeDataBlock (payload_test.go) and then hand those rows to the
// template's own JS. That is the point of running them against the artifact
// rather than against demo.Snapshot directly: the rows the JS sees here have
// been through the columnar encoding, so a field the encoder drops shows up as
// a column the report declines to paint.

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
	shipped := decodeDataBlock(t, renderDemo(t))

	// Which resources the report owes a Class, judged from the data alone.
	type want struct {
		name  string
		sized bool
	}
	wants := make([]want, len(shipped))
	sizedCount := 0
	for i, r := range shipped {
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

	resources, err := json.Marshal(shipped)
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
	shipped := decodeDataBlock(t, renderDemo(t))

	type want struct {
		name     string
		platform string
	}
	wants := make([]want, len(shipped))
	runtimes := 0
	for i, r := range shipped {
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

	resources, err := json.Marshal(shipped)
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

// The decoder in report.html.tmpl is the only one a reader ever runs, and
// until now nothing ran it. payload_test.go's Go decoder is a deliberate
// second implementation written from the wire format, which proves the encoder
// is decodable — not that the shipped JS decodes it. This runs the real thing:
// the census block out of a rendered report, through the template's own
// readTable under node, compared against the Go decode of the same bytes.
func TestReportJSDecoderMatchesGoDecoder(t *testing.T) {
	page := renderDemo(t)
	want := decodeDataBlock(t, page)
	if len(want) == 0 {
		t.Fatal("the fixture census is empty; this test proves nothing")
	}

	script := strings.Join([]string{
		jsVar(t, "COL_TAG"),
		jsVar(t, "COL_ATTR"),
		jsFunc(t, "targetOf"),
		jsFunc(t, "readTable"),
		"console.log(JSON.stringify(readTable(" + string(dataBlockJSON(t, page)) + ")));",
	}, "\n")

	var got []model.Resource
	evalJSON(t, script, &got)
	assertResourcesEqual(t, want, got)
}

// A tag key of "__proto__" is legal in AWS and hostile in JavaScript: assigning
// it to an ordinary object runs the prototype setter and stores nothing, so the
// tag would disappear between the encoder and the page. Nothing a service
// reported may go missing on the way to the reader, whatever it is named.
func TestReportJSDecoderKeepsHostileTagKeys(t *testing.T) {
	r := model.Resource{
		ARN: "arn:aws:rds:us-east-1:111122223333:db:hostile", Service: "rds",
		Name: "hostile", Type: "AWS::RDS::DBInstance", Region: "us-east-1",
		AccountID: "111122223333",
	}
	r.Tags = map[string]string{"__proto__": "prod-owner", "constructor": "team-a", "Name": "hostile"}
	r.SetAttr("__proto__", "engine-x")

	table, err := buildTable([]model.Resource{r})
	if err != nil {
		t.Fatalf("buildTable: %v", err)
	}
	wire, err := json.Marshal(table)
	if err != nil {
		t.Fatalf("marshal table: %v", err)
	}

	script := strings.Join([]string{
		jsVar(t, "COL_TAG"),
		jsVar(t, "COL_ATTR"),
		jsFunc(t, "targetOf"),
		jsFunc(t, "readTable"),
		// The bags go to JSON.stringify as they are. Copying them first with
		// Object.assign would not do: it assigns rather than defines, so it
		// re-runs the prototype setter and loses the key all over again — which
		// is worth knowing, because the same trap is one refactor away from any
		// code downstream that decides to clone a bag.
		"var rows = readTable(" + string(wire) + ");",
		"console.log(JSON.stringify(rows.map(function (row) {",
		"  return { tags: row.tags, attributes: row.attributes };",
		"})));",
	}, "\n")

	var got []struct {
		Tags       map[string]string `json:"tags"`
		Attributes map[string]string `json:"attributes"`
	}
	evalJSON(t, script, &got)
	if len(got) != 1 {
		t.Fatalf("decoded %d rows, want 1", len(got))
	}
	for key, want := range r.Tags {
		if got[0].Tags[key] != want {
			t.Errorf("tag %q decoded as %q, want %q", key, got[0].Tags[key], want)
		}
	}
	if got[0].Attributes["__proto__"] != "engine-x" {
		t.Errorf("attribute %q decoded as %q, want %q", "__proto__", got[0].Attributes["__proto__"], "engine-x")
	}
}

// The wire format is agreed between Go and a copy of its constants written out
// in JavaScript, and nothing but this test connects the two. Bumping
// payloadWire without bumping WIRE ships a report that refuses to open itself;
// renaming an encoding or a column prefix on one side alone ships one that
// decodes to an empty census. Both are silent — the Go tests keep passing,
// because they compare the encoder against a Go decoder that moved with it.
func TestReportJSWireConstantsMatchGo(t *testing.T) {
	script := strings.Join([]string{
		jsVar(t, "WIRE"),
		jsVar(t, "ENCODING_JSON"),
		jsVar(t, "ENCODING_GZIP"),
		jsVar(t, "COL_TAG"),
		jsVar(t, "COL_ATTR"),
		`console.log(JSON.stringify({wire: WIRE, json: ENCODING_JSON,` +
			` gzip: ENCODING_GZIP, tag: COL_TAG, attr: COL_ATTR}));`,
	}, "\n")

	var got struct {
		Wire int    `json:"wire"`
		JSON string `json:"json"`
		Gzip string `json:"gzip"`
		Tag  string `json:"tag"`
		Attr string `json:"attr"`
	}
	evalJSON(t, script, &got)

	for _, c := range []struct {
		name    string
		js, api any
	}{
		{"WIRE / payloadWire", got.Wire, payloadWire},
		{"ENCODING_JSON / encodingJSON", got.JSON, encodingJSON},
		{"ENCODING_GZIP / encodingGzip", got.Gzip, encodingGzip},
		{"COL_TAG / colTagPrefix", got.Tag, colTagPrefix},
		{"COL_ATTR / colAttrPrefix", got.Attr, colAttrPrefix},
	} {
		if c.js != c.api {
			t.Errorf("%s disagree: the page reads %v, Go writes %v", c.name, c.js, c.api)
		}
	}
}

// A group header prints the group's whole membership next to a total summed
// over only the members something priced. Where those differ the header has to
// say which fraction it is, because the count and the total read as one
// sentence and that sentence would otherwise be false: the fixture's
// 111111111111 account is 73 resources of which Cost Explorer priced 22, and
// "73 resources · 4425.52 USD" states the other 51 cost nothing.
//
// The Go side of this is TestBuildGroupsCountsEveryMemberNotOnlyThePricedOnes;
// this is the half that puts it on the page.
func TestReportGroupHeaderDisclosesPartialCostCoverage(t *testing.T) {
	// el and the button are the only DOM appendGroupCost touches. Recording
	// them as plain objects keeps the assertion on the text the reader sees.
	const shim = `
var document = { createElement: function (tag) {
  return { tag: tag, className: "", textContent: "", title: "",
           children: [], appendChild: function (c) { this.children.push(c); } };
} };
function text(n) {
  return (n.textContent || "") + n.children.map(text).join(" ");
}`

	cases := []struct {
		name     string
		spend    string
		want     string // substring the header must carry
		wantSeen bool
	}{
		{
			name:     "partial coverage is disclosed",
			spend:    `{costs: [{method: "ce", currency: "USD", amount: "4425.52"}], priced: 22, total: 73}`,
			want:     "over 22 of 73",
			wantSeen: true,
		},
		{
			name:     "full coverage says nothing extra",
			spend:    `{costs: [{method: "ce", currency: "USD", amount: "36.28"}], priced: 12, total: 12}`,
			want:     "over ",
			wantSeen: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.Join([]string{
				shim,
				jsVar(t, "COST_METHODS"),
				jsFunc(t, "el"),
				jsFunc(t, "appendGroupCost"),
				`var btn = document.createElement("button");`,
				`appendGroupCost(btn, ` + tc.spend + `);`,
				`console.log(JSON.stringify(text(btn)));`,
			}, "\n")

			var got string
			evalJSON(t, script, &got)

			if seen := strings.Contains(got, tc.want); seen != tc.wantSeen {
				t.Errorf("header text %q: contains %q = %v, want %v",
					got, tc.want, seen, tc.wantSeen)
			}
			// The total itself must still be there either way. A disclosure
			// that swallowed the figure would pass the check above.
			if !strings.Contains(got, "USD") {
				t.Errorf("header text %q lost its total", got)
			}
		})
	}
}

// compareGroupCost ranks group headers, and it reads the totals off the summary
// entry Go writes. It used to be handed a bare costs array; it is now handed the
// whole entry, because the entry is what carries the coverage counts that decide
// whether ranking is allowed at all. A stale caller would read undefined off
// every group and sort the table into census order without saying so.
func TestReportCompareGroupCostReadsTheSummaryEntry(t *testing.T) {
	entry := func(amount string) string {
		return `{costs: [{method: "ce", currency: "USD", amount: "` + amount +
			`"}], priced: 1, total: 1}`
	}
	script := strings.Join([]string{
		jsFunc(t, "compareGroupCost"),
		jsFunc(t, "compareDecimal"),
		jsFunc(t, "compareMagnitude"),
		`console.log(JSON.stringify([`,
		`  compareGroupCost(` + entry("100.00") + `, ` + entry("90.00") + `),`,
		`  compareGroupCost(` + entry("90.00") + `, ` + entry("100.00") + `),`,
		`  compareGroupCost(` + entry("10.50") + `, ` + entry("10.50") + `),`,
		// A group whose figures all failed to parse carries no total, and must
		// not be ordered as though it were the cheapest thing in the census.
		`  compareGroupCost({costs: [], priced: 0, total: 4}, ` + entry("1.00") + `)`,
		`]));`,
	}, "\n")

	var got []int
	evalJSON(t, script, &got)

	want := []int{1, -1, 0, -1}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("comparison %d = %d, want %d; the comparator is not reading "+
				"amounts off the summary entry", i, got[i], w)
		}
	}
}
