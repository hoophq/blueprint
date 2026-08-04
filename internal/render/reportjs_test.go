package render

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/cost"
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

// ---- Cost overlay ----------------------------------------------------------
//
// The overlay carries the report's money, and money in a browser is where this
// tool is most exposed: JavaScript has no decimal type, so every figure it
// prints is one parseFloat away from being a number AWS never reported. The
// template answers that with integer arithmetic over BigInt and a grammar
// copied from the collector. These tests are what keeps the copy honest —
// several of them run the JS against the very Go function it mirrors, so the
// two cannot drift apart quietly.

// costPrelude is the module scope the overlay's pure functions read from.
//
// COST is a multi-line object literal, which jsVar cannot lift, and lifting it
// would be wrong anyway: it is state the report computes at load time, so a
// test that wants a particular source active has to say so rather than inherit
// whatever the fixture produced.
// currencyBy is seeded from the same pair so a single-source harness reads the
// same currency however it asks for it. The report keeps the two separate
// because the table shows every source at once and each is judged against its
// own dominant currency; a test that only exercises one source has nothing to
// distinguish.
func costPrelude(method, currency string) string {
	return "var COST = { method: " + strconv.Quote(method) + ", currency: " + strconv.Quote(currency) +
		", currencyBy: { " + strconv.Quote(method) + ": " + strconv.Quote(currency) + " } };"
}

// The report re-implements the collector's idea of an amount, because the
// browser has to decide the same question the Go side already decided: is this
// string a figure, or is it something else that must not be added to anything?
// Two definitions would drift, and the drift would be invisible — a report that
// accepted "1e9" would total a number Cost Explorer never sent, and one that
// rejected "000123" would drop a figure it did. This runs the same corpus
// through both and requires the same verdict.
func TestReportDecimalGrammarMatchesTheCollector(t *testing.T) {
	corpus := []string{
		// Amounts AWS can actually send.
		"0", "0.00", "-0.00", "1", "-1", "1.5", "-1.5", "000123", "0.000000001",
		"9007199254740993.01", "12345678901234567890.123456789",
		// Shapes big.Rat would take and the collector will not.
		"1e9", "1E9", "1/3", "+1", "0x10", "Infinity", "NaN",
		// Truncated or malformed decimals.
		"1.", ".5", "-.5", "-", "", ".", "1.2.3", "--1", "1-", "1_000", "1,000",
		// Whitespace, which "$" in a regex would let through.
		" 1", "1 ", "1.00\n", "\n1.00", "1.00\t", "1\r\n",
		// Digits that are not ASCII digits.
		"١٢٣", "１２３",
	}

	in, err := json.Marshal(corpus)
	if err != nil {
		t.Fatalf("encoding corpus: %v", err)
	}
	script := strings.Join([]string{
		jsVar(t, "DEC_RE"),
		"console.log(JSON.stringify((" + string(in) + ").map(function (s) { return DEC_RE.test(s); })));",
	}, "\n")

	var got []bool
	evalJSON(t, script, &got)
	if len(got) != len(corpus) {
		t.Fatalf("got %d verdicts, want %d", len(got), len(corpus))
	}
	for i, s := range corpus {
		if want := cost.ValidDecimal(s); got[i] != want {
			t.Errorf("DEC_RE.test(%q) = %v, but cost.ValidDecimal says %v", s, got[i], want)
		}
	}
}

// The report's money arithmetic is exact or it is not worth doing: a total the
// reader can check against the console has to be the sum of the figures, not a
// float that lands near it. Scale is part of that — a sum of two-decimal
// figures reads as a two-decimal figure, and one that gained precision keeps
// it rather than being quietly rounded to look tidy.
func TestReportDecimalArithmeticIsExact(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"nothing at all", nil, ""}, // a sum of no figures is not zero spend
		{"a stored zero survives", []string{"0.00"}, "0.00"},
		{"same scale", []string{"1.10", "2.20"}, "3.30"},
		{"finer scale wins", []string{"1.10", "2.205"}, "3.305"},
		{"scale is kept, not trimmed", []string{"1.0", "2.00"}, "3.00"},
		{"credits subtract", []string{"10.00", "-2.50"}, "7.50"},
		{"a sum can be zero", []string{"-1.5", "1.5"}, "0.0"},
		{"a sum can be negative", []string{"10", "-10.001"}, "-0.001"},
		// float64 cannot hold this: it rounds to …94 and loses the cent.
		{"beyond float64", []string{"9007199254740993.01", "0.01"}, "9007199254740993.02"},
		{"long tail", []string{"0.000000001", "0.000000002"}, "0.000000003"},
		{"a figure that will not parse poisons the sum", []string{"1.00", "n/a"}, ""},
	}

	in := make([][]string, len(cases))
	for i, c := range cases {
		in[i] = c.in
	}
	lists, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := strings.Join([]string{
		jsVar(t, "DEC_RE"),
		jsFunc(t, "pow10"),
		jsFunc(t, "decParse"),
		jsFunc(t, "decAlign"),
		jsFunc(t, "decAdd"),
		jsFunc(t, "decSum"),
		jsFunc(t, "decFormat"),
		`console.log(JSON.stringify((` + string(lists) + `).map(function (list) {
  var parsed = (list || []).map(decParse);
  if (parsed.some(function (d) { return d === null; })) return "";
  return decFormat(decSum(parsed));
})));`,
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d sums, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%s: sum(%v) = %q, want %q", c.name, c.in, got[i], c.want)
		}
	}
}

// Two amounts that differ only in trailing zeros are the same money, and the
// report has to order and reconcile them as such. Comparing the printed strings
// would put "1.10" before "1.9" and declare "1.0" different from "1.00" — the
// reconciliation tick would then fail on arithmetic that actually held.
func TestReportDecimalComparisonIgnoresTrailingZeros(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.00", 0},
		{"0", "-0.00", 0},
		{"1.10", "1.9", -1}, // string order would say the opposite
		{"9.99", "10.0", -1},
		{"-1.00", "-1.000", 0},
		{"-2", "-1.5", -1},
		{"0.00", "0.000000001", -1}, // a stored zero is below the smallest figure, not equal to it
		{"100", "99.999", 1},
	}

	pairs := make([][]string, len(cases))
	for i, c := range cases {
		pairs[i] = []string{c.a, c.b}
	}
	in, err := json.Marshal(pairs)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := strings.Join([]string{
		jsVar(t, "DEC_RE"),
		jsFunc(t, "pow10"),
		jsFunc(t, "decParse"),
		jsFunc(t, "decAlign"),
		jsFunc(t, "decCmp"),
		`console.log(JSON.stringify((` + string(in) + `).map(function (p) {
  return decCmp(decParse(p[0]), decParse(p[1]));
})));`,
	}, "\n")

	var got []int
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d comparisons, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("decCmp(%q, %q) = %d, want %d", c.a, c.b, got[i], c.want)
		}
	}
}

// "41% of spend" is the sentence ATR-174 exists to print, so the percentage
// behind it is derived from the figures with integer arithmetic rather than
// from a float that approximates them. Rounding is half away from zero — the
// rule a reader checking with a calculator would apply — and a share of nothing
// is null rather than zero, because no share can be stated when the whole is
// unknown or nil.
func TestReportPercentIsExactAndRoundsHalfUp(t *testing.T) {
	cases := []struct {
		name        string
		part, whole string // "" stands for an absent figure
		want        string // JSON: a number, or null
	}{
		{"half", "1", "2", "50"},
		{"a third rounds down", "1", "3", "33"},
		{"two thirds rounds up", "2", "3", "67"},
		{"exactly a half rounds away from zero", "0.125", "1", "13"},
		{"the smallest share that rounds to one", "0.005", "1", "1"},
		{"just below it rounds to none", "0.0049", "1", "0"},
		{"a credit keeps its sign", "-0.125", "1", "-13"},
		{"a negative whole flips the sign", "1", "-2", "-50"},
		{"everything", "12.34", "12.340", "100"},
		{"more than everything", "20", "10", "200"},
		{"a stored zero has a share", "0.00", "10", "0"},
		{"a share of zero cannot be stated", "1", "0.00", "null"},
		{"no part", "", "10", "null"},
		{"no whole", "1", "", "null"},
	}

	args := make([][]any, len(cases))
	for i, c := range cases {
		var part, whole any
		if c.part != "" {
			part = c.part
		}
		if c.whole != "" {
			whole = c.whole
		}
		args[i] = []any{part, whole}
	}
	in, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := strings.Join([]string{
		jsVar(t, "DEC_RE"),
		jsFunc(t, "pow10"),
		jsFunc(t, "decParse"),
		jsFunc(t, "decAlign"),
		jsFunc(t, "decPercent"),
		`console.log(JSON.stringify((` + string(in) + `).map(function (p) {
  return decPercent(p[0] === null ? null : decParse(p[0]), p[1] === null ? null : decParse(p[1]));
})));`,
	}, "\n")

	var got []json.RawMessage
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d percentages, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if string(got[i]) != c.want {
			t.Errorf("%s: decPercent(%q, %q) = %s, want %s", c.name, c.part, c.whole, got[i], c.want)
		}
	}
}

// The terminal and the HTML report describe the same amount, and a currency the
// tool was never told the meaning of gets no invented symbol.
func TestReportMoneyFormatterMirrorsGo(t *testing.T) {
	cases := [][2]string{
		{"12.34", "USD"},
		{"12.34", ""}, // a source that reported no unit
		{"0.00", "USD"},
		{"-5.00", "EUR"},
		{"1234567.89", "JPY"},
		{"", "USD"},
	}

	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := jsFunc(t, "formatMoney") + "\n" +
		"console.log(JSON.stringify((" + string(in) + ").map(function (c) { return formatMoney(c[0], c[1]); })));"

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d results, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if want := FormatMoney(c[0], c[1]); got[i] != want {
			t.Errorf("formatMoney(%q, %q) = %q, but FormatMoney says %q", c[0], c[1], got[i], want)
		}
	}
}

// A window that stops short of its month has to say so, and must never be
// scaled up to what a full month "would have" cost. Both renderers state the
// same shortfall, so both count the same days — including February's, which is
// the case a hard-coded 30 would get wrong twice out of eight years.
func TestReportPartialSuffixMirrorsGo(t *testing.T) {
	cases := []model.CostWindow{
		{Start: "2026-07-01", End: "2026-07-15"}, // 14 of 31
		{Start: "2026-07-01", End: "2026-08-01"}, // whole month: no suffix
		{Start: "2026-02-01", End: "2026-02-15"}, // 14 of 28
		{Start: "2024-02-01", End: "2024-02-15"}, // 14 of 29, leap year
		{Start: "2026-02-01", End: "2026-03-01"}, // whole short month
		{Start: "2026-07-17", End: "2026-07-31"}, // mid-month span
		{Start: "2026-07-01", End: "2026-07-02"}, // a single day
		{Start: "2026-07-15", End: "2026-07-01"}, // reversed: says nothing
		{Start: "2026-07-01", End: "2026-07-01"}, // empty span
		{Start: "2026-06-01", End: "2026-09-01"}, // longer than its month
		{Start: "", End: ""},                     // no window at all
		{Start: "2026-02-30", End: "2026-03-05"}, // a date that does not exist
		{Start: "2026-7-1", End: "2026-07-15"},   // not the layout either side parses
	}

	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := strings.Join([]string{
		jsFunc(t, "parseDateOnly"),
		jsFunc(t, "daysInMonth"),
		jsFunc(t, "costPartialSuffix"),
		"console.log(JSON.stringify((" + string(in) + ").map(costPartialSuffix)));",
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d suffixes, want %d", len(got), len(cases))
	}
	for i, w := range cases {
		if want := partialSuffix(w); got[i] != want {
			t.Errorf("costPartialSuffix(%s→%s) = %q, but partialSuffix says %q", w.Start, w.End, got[i], want)
		}
	}
}

// The reconciliation line is the report inviting the reader to check its
// arithmetic, so the tick has to mean the addition was performed and held —
// never that it was skipped. Both renderers print the same verdict for the same
// three amounts, including when one of them is not a number at all.
func TestReportCostReconciliationMirrorsGo(t *testing.T) {
	cases := []model.CostByCurrency{
		{Total: "100.00", Attributed: "90.00", Unattributed: "10.00"},
		{Total: "100.0", Attributed: "90.00", Unattributed: "10.000"}, // same money, different scales
		{Total: "0.00", Attributed: "0.00", Unattributed: "0.00"},
		{Total: "100.00", Attributed: "110.00", Unattributed: "-10.00"}, // credits
		{Total: "100.00", Attributed: "90.00", Unattributed: "9.99"},    // does not reconcile
		{Total: "100.00", Attributed: "n/a", Unattributed: "10.00"},     // cannot be checked
		{Total: "unknown", Attributed: "90.00", Unattributed: "10.00"},
		{Total: "", Attributed: "", Unattributed: ""},
	}

	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := strings.Join([]string{
		jsVar(t, "DEC_RE"),
		jsFunc(t, "pow10"),
		jsFunc(t, "decParse"),
		jsFunc(t, "decAlign"),
		jsFunc(t, "decAdd"),
		jsFunc(t, "decCmp"),
		jsFunc(t, "reconcileLine"),
		"console.log(JSON.stringify((" + string(in) + ").map(reconcileLine)));",
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d lines, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if want := reconciliation(c); got[i] != want {
			t.Errorf("reconcileLine(%+v) =\n  %q\nbut reconciliation says\n  %q", c, got[i], want)
		}
	}
}

// A probe records what happened when this tool asked a service for its
// resource-level cost, and the difference between "returned no rows" and "was
// never asked" is the whole point of keeping the ledger. The HTML report and the
// terminal have to draw that distinction with the same words, or a reader
// comparing the two would think they were looking at different runs.
func TestReportProbeSentenceMirrorsGo(t *testing.T) {
	cases := []model.ServiceProbe{
		{Service: "Amazon RDS", Outcome: model.ProbeRows, Rows: 12, Matched: 9},
		{Service: "Amazon EC2", Outcome: model.ProbeRows, Rows: 500, Matched: 500, Truncated: true},
		{Service: "Amazon S3", Outcome: model.ProbeEmpty},
		{Service: "AWS Lambda", Outcome: model.ProbeUnsupported, Detail: "not supported for this service"},
		{Service: "Amazon DynamoDB", Outcome: model.ProbeDenied, Detail: "AccessDeniedException"},
		{Service: "Amazon ElastiCache", Outcome: model.ProbeSkipped},
		{Service: "Amazon MQ", Outcome: model.ProbeUncensused},
		{Service: "Amazon Redshift", Outcome: model.ProbeFailed, Detail: "throttled"},
		{Service: "Something new", Outcome: "an outcome this build has never heard of"},
		{Service: "Nothing at all", Outcome: ""},
	}

	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := jsFunc(t, "probeLine") + "\n" +
		"console.log(JSON.stringify((" + string(in) + ").map(probeLine)));"

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d lines, want %d", len(got), len(cases))
	}
	for i, p := range cases {
		if want := probeLine(p); got[i] != want {
			t.Errorf("%s: probeLine =\n  %q\nbut Go says\n  %q", p.Service, got[i], want)
		}
	}
}

// methodBasis is the one place that decides what a figure from the active
// source actually is, and this is the regression that made it necessary: the
// hero, the header, the attribution bar, and the section heading each read Cost
// Explorer's window and metric straight off resource_cost, so switching to Cost
// Optimization Hub stamped a real billed period onto a modelled monthly rate
// that never covered it. A wrong period on a right-looking number is worse than
// no period, because it survives being checked.
func TestReportMethodBasisNeverBorrowsAnotherSourcesWindow(t *testing.T) {
	const report = `{"metric":"AmortizedCost","window":{"start":"2026-07-17","end":"2026-07-31","label":"2026-07-17→2026-07-30"}}`

	type basis struct {
		Noun    string `json:"noun"`
		Metric  string `json:"metric"`
		Window  string `json:"window"`
		Monthly bool   `json:"monthly"`
	}
	cases := []struct {
		name   string
		method string
		report string // JSON, or "null"
		want   basis
	}{
		{
			// Both endpoints, exactly as the pass wrote them — and no month
			// fraction. That suffix belongs to the account rollup, whose window is
			// a calendar month; the resource-level window is a rolling fortnight
			// that need not sit inside one month at all.
			name: "Cost Explorer states its billed window and metric", method: "ce", report: report,
			want: basis{Noun: "spend", Metric: "AmortizedCost", Window: "2026-07-17→2026-07-30"},
		},
		{
			// The scan that exposed it: a window straddling a month boundary was
			// measured against the month its first day fell in, so a fortnight
			// covering five days of June and nine of July was reported as "14 of
			// 30 days" of June.
			name:   "a window straddling two months claims a fraction of neither",
			method: "ce",
			report: `{"metric":"AmortizedCost","window":{"start":"2026-06-26","end":"2026-07-10","label":"2026-06-26→2026-07-09"}}`,
			want:   basis{Noun: "spend", Metric: "AmortizedCost", Window: "2026-06-26→2026-07-09"},
		},
		{
			// The regression: not Cost Explorer's window, not its metric, and not
			// the word "spend" for something nothing has been billed for yet.
			name: "Cost Optimization Hub borrows neither", method: "coh", report: report,
			want: basis{Noun: "modelled cost", Metric: "modelled monthly rate", Monthly: true},
		},
		{
			name: "no resource-level pass ran", method: "ce", report: "null",
			want: basis{Noun: "spend"},
		},
		{
			name: "a pass with no window recorded", method: "ce", report: `{"metric":"UnblendedCost"}`,
			want: basis{Noun: "spend", Metric: "UnblendedCost"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := strings.Join([]string{
				costPrelude(c.method, "USD"),
				"var resourceCostReport = " + c.report + ";",
				// Nothing else is lifted on purpose: methodBasis reads two globals
				// and formats them. Reaching for a date helper again would fail
				// here as a ReferenceError before the expectation below could
				// disagree politely.
				jsFunc(t, "methodBasis"),
				"console.log(JSON.stringify(methodBasis()));",
			}, "\n")

			var got basis
			evalJSON(t, script, &got)
			if got != c.want {
				t.Errorf("methodBasis() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// A modelled rate carries the lookback the model read, per figure. One window
// can be stated as a fact about the whole set; a mixture cannot be collapsed
// into one without claiming coverage no single figure has, so the summary says
// nothing and the rows keep their own.
func TestReportModelledWindowIsOnlyStatedWhenEveryFigureAgrees(t *testing.T) {
	const a = `{"method":"coh","observed_from":"2026-06-01T00:00:00Z","observed_to":"2026-07-01T00:00:00Z"}`
	const b = `{"method":"coh","observed_from":"2026-05-01T00:00:00Z","observed_to":"2026-06-01T00:00:00Z"}`
	const ce = `{"method":"ce","observed_from":"2026-07-17T00:00:00Z","observed_to":"2026-07-31T00:00:00Z"}`
	const bare = `{"method":"coh"}`
	const toOnly = `{"method":"coh","observed_to":"2026-07-01T00:00:00Z"}`
	const fromOnly = `{"method":"coh","observed_from":"2026-06-01T00:00:00Z"}`

	cases := []struct {
		name      string
		resources string // JSON
		unread    bool   // COST.counted is false: nobody has read these rows
		want      string // JSON: the window, or null
	}{
		{"one window", `[{"costs":[` + a + `]},{"costs":[` + a + `]}]`, false,
			`{"from":"2026-06-01T00:00:00Z","to":"2026-07-01T00:00:00Z"}`},
		{"a mixture states none", `[{"costs":[` + a + `]},{"costs":[` + b + `]}]`, false, "null"},
		{"another source's window is not borrowed", `[{"costs":[` + ce + `]}]`, false, "null"},
		{"figures with no window at all", `[{"costs":[` + bare + `]}]`, false, "null"},
		{"one dated and one bare still disagree", `[{"costs":[` + a + `]},{"costs":[` + bare + `]}]`, false, "null"},
		// Half a window is not a window. Joining the two ends into one key hides
		// this: "|2026-07-01T00:00:00Z" is a perfectly good key that no other
		// figure matches, so a lone half-placed figure passed as agreement and
		// rendered "from  up to 2026-07-01 UTC" — a stated end with an origin
		// the source never gave. Go's sharedWindow bails on either end being nil.
		{"an end with no start", `[{"costs":[` + toOnly + `]}]`, false, "null"},
		{"a start with no end", `[{"costs":[` + fromOnly + `]}]`, false, "null"},
		{"a half-placed figure spoils an otherwise agreed window",
			`[{"costs":[` + a + `]},{"costs":[` + toOnly + `]}]`, false, "null"},
		{"nothing priced", `[{"costs":[]},{}]`, false, "null"},

		// The window is a fact about rows, and the panels that print it now run
		// at boot, before any row exists, and again from failCensus. There are
		// two decode failures and only one of them clears the rows: the
		// continuation assigns resources before it checks their number against
		// the summary's, so a census that contradicts its own summary arrives at
		// failCensus fully populated and perfectly foldable. Folding it would
		// stamp a census-derived window onto the panel directly under a banner
		// that says the inventory could not be read and the trail beneath it
		// "comes from the report's metadata, which read fine" — and it would do
		// it on one of the two failures, which is worse than on both.
		{"an unread census states no window, however well its rows agree",
			`[{"costs":[` + a + `]},{"costs":[` + a + `]}]`, true, "null"},
		{"nor at boot, before the rows are there at all", "null", true, "null"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := strings.Join([]string{
				"var resources = " + c.resources + ";",
				"var COST = { counted: " + strconv.FormatBool(!c.unread) + " };",
				jsFunc(t, "methodWindow"),
				`console.log(JSON.stringify(methodWindow("coh")));`,
			}, "\n")
			if got := evalReportJS(t, script); got != c.want {
				t.Errorf("methodWindow(\"coh\") = %s, want %s", got, c.want)
			}
		})
	}
}

// Ranking by cost puts a question to every row the sort touches: what does a
// resource AWS reported nothing for rank as? Not zero — an unattributed
// resource is not a free one — so absence is partitioned out before the
// direction is applied, and stays at the bottom whichever way the arrow points.
// A stored zero is on the other side of that partition, ranked as the finding
// it is. The size column answers the same question, and did so wrongly once:
// reading it as "value or 0" tied every unmeasured bucket with every empty one.
func TestReportSortKeepsAbsenceOutOfTheRanking(t *testing.T) {
	// rows() builds the shape currentRows sorts, from a compact literal.
	// _costBy is keyed by source because each cost column sorts on its own
	// source alone. The harness fills only "ce"; a second key would be a second
	// column, never a second value in this one.
	const rows = `function rows(spec) {
  return spec.map(function (s) {
    return {
      name: s[0],
      _costBy: { ce: { dec: s[1] === null ? null : decParse(s[1]) } },
      _sizeBytes: s[2]
    };
  });
}`
	cases := []struct {
		name string
		key  string
		dir  int
		spec string // [name, amount|null, sizeBytes|null]
		want string // space-separated names, in order
	}{
		{
			name: "cost, descending", key: "_costSort:ce", dir: -1,
			spec: `[["a","10.00",null],["b",null,null],["c","0.00",null],["d","2.50",null],["e",null,null],["f","-3.00",null]]`,
			want: "a d c f b e",
		},
		{
			// Reversing must not promote silence to the top.
			name: "cost, ascending", key: "_costSort:ce", dir: 1,
			spec: `[["a","10.00",null],["b",null,null],["c","0.00",null],["d","2.50",null],["e",null,null],["f","-3.00",null]]`,
			want: "f c d a b e",
		},
		{
			name: "scale is not string order", key: "_costSort:ce", dir: -1,
			spec: `[["nine","9.99",null],["ten","10.0",null],["hundred","100",null]]`,
			want: "hundred ten nine",
		},
		{
			name: "size, descending", key: "_sizeBytes", dir: -1,
			spec: `[["big",null,1024],["unmeasured",null,null],["empty",null,0],["small",null,1]]`,
			want: "big small empty unmeasured",
		},
		{
			// The regression: "value or 0" put the unmeasured bucket level with
			// the empty one, and ascending order then buried the real finding.
			name: "size, ascending", key: "_sizeBytes", dir: 1,
			spec: `[["big",null,1024],["unmeasured",null,null],["empty",null,0],["small",null,1]]`,
			want: "empty small big unmeasured",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := strings.Join([]string{
				jsVar(t, "DEC_RE"),
				jsFunc(t, "pow10"),
				jsFunc(t, "decParse"),
				jsFunc(t, "decAlign"),
				jsFunc(t, "decCmp"),
				jsFunc(t, "costSortMethod"),
				jsFunc(t, "costSortDec"),
				jsFunc(t, "sortValue"),
				jsFunc(t, "nullRank"),
				jsFunc(t, "compareRows"),
				jsFunc(t, "currentRows"),
				rows,
				// The scope currentRows filters against: no query, no facet.
				`var filterEl = { value: "" };`,
				`var activeTier = "";`,
				`var activeService = null;`,
				"var sortKey = " + strconv.Quote(c.key) + ";",
				"var sortDir = " + strconv.Itoa(c.dir) + ";",
				"var resources = rows(" + c.spec + ");",
				`console.log(currentRows().map(function (r) { return r.name; }).join(" "));`,
			}, "\n")
			if got := evalReportJS(t, script); got != c.want {
				t.Errorf("order = %q, want %q", got, c.want)
			}
		})
	}
}

// A cost cell has three states a reader must be able to tell apart: a figure
// AWS reported, an absence it explained, and an absence it did not. None of
// them may render as zero, and the one figure that is a zero must render as
// itself — "0.00" is a real reading, and printing it as an em dash would erase
// the only row that proves the pass reached that resource.
func TestReportCostTextNeverPrintsZeroForAbsence(t *testing.T) {
	type cell struct {
		Text  string `json:"text"`
		Dim   bool   `json:"dim"`
		Flag  string `json:"flag"`
		Title string `json:"title"`
	}
	cases := []struct {
		name     string
		method   string // the active source; Cost Explorer when blank
		resource string // JSON
		want     cell
	}{
		{
			name: "no figure and no reason", resource: `{}`,
			want: cell{Dim: true, Title: "No Cost Explorer figure for this resource."},
		},
		{
			// The only reason string any collector writes, and the one that
			// exposed the bug: Cost Explorer's resource-level pass deliberately
			// records none, so a reason sitting on a row under Cost Explorer was
			// written by the *other* source. Joining the two with an em dash made
			// a Hub sentence answer for Cost Explorer's silence. The reason is
			// quoted and attributed to "the cost pass" instead, which is what the
			// coverage banner has always called it.
			//
			// A fixture reason no collector can produce is how this passed for a
			// while: the corpus said "Cost Explorer returned no rows for this
			// resource", which reads fine after an em dash and never occurs.
			name:     "no figure, with the reason a pass recorded",
			resource: `{"cost_unavailable":"no Cost Optimization Hub recommendation for this resource"}`,
			want: cell{Dim: true,
				Title: "No Cost Explorer figure for this resource. The cost pass recorded: " +
					"no Cost Optimization Hub recommendation for this resource"},
		},
		{
			// The same row under the source that wrote the reason. Still quoted:
			// one wording covers both, and neither claims the reason came from
			// whichever source the reader happens to be looking through.
			name:     "the source that recorded the reason is not privileged",
			method:   "coh",
			resource: `{"cost_unavailable":"no Cost Optimization Hub recommendation for this resource"}`,
			want: cell{Dim: true,
				Title: "No Cost Optimization Hub figure for this resource. The cost pass recorded: " +
					"no Cost Optimization Hub recommendation for this resource"},
		},
		{
			name:     "a reported zero is a figure",
			resource: `{"_cost":{"amount":"0.00","currency":"USD","method":"ce","estimated":false}}`,
			want:     cell{Text: "0.00 USD", Title: "Billed figure from Cost Explorer."},
		},
		{
			name: "a billed figure names its period",
			resource: `{"_cost":{"amount":"12.34","currency":"USD","method":"ce","estimated":false,` +
				`"observed_from":"2026-07-17T00:00:00Z","observed_to":"2026-07-31T00:00:00Z"}}`,
			want: cell{Text: "12.34 USD",
				Title: "Billed figure from Cost Explorer. Figure covers usage from 2026-07-17 up to 2026-07-31, UTC."},
		},
		{
			// A modelled figure's window is the lookback the model read, not the
			// period the amount covers, and the wording keeps the two apart.
			name: "a modelled rate says what its window is",
			resource: `{"_cost":{"amount":"40.00","currency":"USD","method":"coh","estimated":true,` +
				`"observed_from":"2026-06-01T00:00:00Z","observed_to":"2026-07-01T00:00:00Z"}}`,
			want: cell{Text: "40.00 USD",
				Title: "Modelled monthly rate from Cost Optimization Hub rather than a billed amount. " +
					"Modelled from usage that covers usage from 2026-06-01 up to 2026-07-01, UTC."},
		},
		{
			name: "a lower bound is flagged, not silently ranked",
			resource: `{"_cost":{"amount":"5.00","currency":"USD","method":"ce","estimated":false,` +
				`"caveats":["Covers 2 of 5 volumes on this instance."]}}`,
			want: cell{Text: "5.00 USD", Flag: "†",
				Title: "Billed figure from Cost Explorer. Lower bound — Covers 2 of 5 volumes on this instance."},
		},
		{
			name:     "a matched figure says what it was matched on",
			resource: `{"_cost":{"amount":"7.00","currency":"USD","method":"ce","match_key":"my-bucket"}}`,
			want: cell{Text: "7.00 USD",
				Title: `Billed figure from Cost Explorer. Matched to this resource by "my-bucket" rather than by ARN.`},
		},
		{
			name:     "another currency is shown but excluded",
			resource: `{"_cost":{"amount":"9.00","currency":"EUR","method":"ce"}}`,
			want: cell{Text: "9.00 EUR", Dim: true,
				Title: "Billed figure from Cost Explorer. Reported in EUR, so it is excluded from every total " +
					"on this page and sorts last."},
		},
		{
			name:     "no currency at all is still not this page's currency",
			resource: `{"_cost":{"amount":"9.00","method":"ce"}}`,
			want: cell{Text: "9.00", Dim: true,
				Title: "Billed figure from Cost Explorer. Reported in no stated currency, so it is excluded " +
					"from every total on this page and sorts last."},
		},
		{
			// Quoted verbatim rather than guessed at, and kept out of the totals.
			name:     "an amount that is not a number",
			resource: `{"_cost":{"amount":"n/a","currency":"USD","method":"ce"}}`,
			want: cell{Text: "n/a USD", Dim: true,
				Title: "Billed figure from Cost Explorer. This amount is not a decimal number, so it is " +
					"excluded from every total on this page rather than guessed at."},
		},
	}

	in := make([]string, len(cases))
	methods := make([]string, len(cases))
	for i, c := range cases {
		in[i] = c.resource
		// Most rows are read through Cost Explorer, which is the source whose
		// silence carries no reason of its own.
		methods[i] = model.CostMethodCE
		if c.method != "" {
			methods[i] = c.method
		}
	}
	perCase, err := json.Marshal(methods)
	if err != nil {
		t.Fatalf("encoding methods: %v", err)
	}
	script := strings.Join([]string{
		costPrelude(model.CostMethodCE, "USD"),
		"var METHODS = " + string(perCase) + ";",
		jsVar(t, "DEC_RE"),
		jsVar(t, "METHOD_LABELS"),
		jsFunc(t, "pow10"),
		jsFunc(t, "decParse"),
		jsFunc(t, "pad2"),
		jsFunc(t, "formatMoney"),
		jsFunc(t, "methodLabel"),
		jsFunc(t, "costWindowStamp"),
		jsFunc(t, "costWindowSentence"),
		jsFunc(t, "costText"),
		// Each case names the source its row is read through. The fixtures carry
		// _cost, the one-figure shape the single Cost column used; the table now
		// asks per source, so the harness keys it the way stampSources does. The
		// currency stays USD for every source, because what this test separates
		// is figure from absence, not one source's currency from another's.
		`console.log(JSON.stringify([` + strings.Join(in, ",") +
			`].map(function (r, i) {
  var m = METHODS[i];
  COST.method = m;
  COST.currencyBy[m] = "USD";
  r._costBy = {};
  if (r._cost) r._costBy[m] = { rec: r._cost };
  return costText(r, m);
})));`,
	}, "\n")

	var got []cell
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d cells, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%s:\n  got  %+v\n  want %+v", c.name, got[i], c.want)
		}
	}
	// Whatever else changes, no cell may put a bare zero where AWS said nothing.
	for i, c := range cases {
		if strings.HasPrefix(c.resource, `{"_cost"`) {
			continue
		}
		if got[i].Text != "" {
			t.Errorf("%s: an absence rendered as %q; the caller paints an em dash", c.name, got[i].Text)
		}
	}
}

// The commitment disclosure has to describe the figure actually in front of the
// reader, because each one is wrong about a resource in a different direction:
// amortized spreads a commitment over what it covered, unblended leaves it on
// the account, and Cost Optimization Hub has not billed anything at all. The
// last one was reached by handing the note an empty metric, which selects the
// branch for a Cost Explorer figure whose metric went unrecorded and ends "what
// a resource was charged" — a sentence about a charge that never happened.
// Whichever source is active, the note may not turn a cost into a saving: this
// tool cannot see rate plans or what else would move onto the commitment, so it
// has no basis for the claim and never makes it.
func TestReportCommitmentNoteNeverPromisesASaving(t *testing.T) {
	cases := []struct {
		Metric string `json:"metric"`
		Method string `json:"method"`
	}{
		{Metric: "AmortizedCost"}, {Metric: "NetAmortizedCost"},
		{Metric: "UnblendedCost"}, {Metric: "NetUnblendedCost"},
		{Metric: "BlendedCost"}, {Metric: "UsageQuantity"},
		{Metric: ""}, {Metric: "a metric AWS has not shipped yet"},
		// Cost Optimization Hub carries no metric, and carries one anyway if a
		// Cost Explorer pass also ran: neither may pull the note onto a branch
		// that speaks about billed amounts.
		{Metric: "", Method: "coh"},
		{Metric: "AmortizedCost", Method: "coh"},
	}

	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	script := jsFunc(t, "commitmentNote") + "\n" +
		"console.log(JSON.stringify((" + string(in) + ").map(function (c) " +
		"{ return commitmentNote(c.metric, c.method); })));"

	var got []string
	evalJSON(t, script, &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d notes, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		note, name := got[i], c.Metric+"/"+c.Method
		if note == "" {
			t.Errorf("%s gets no commitment disclosure at all", name)
			continue
		}
		// The amortized note uses "saving" to deny one, so the bar is the claim
		// itself rather than the word: no phrasing may tell a reader that
		// deleting something returns money.
		for _, claim := range []string{
			"you could save", "would save", "saves you", "potential saving",
			"estimated saving", "savings if deleted", "save by deleting",
		} {
			if strings.Contains(strings.ToLower(note), claim) {
				t.Errorf("%s: disclosure makes a savings claim (%q):\n  %s", name, claim, note)
			}
		}
		// A modelled rate is a price this tool computed for a configuration, not
		// a line on an invoice, and no wording may report it as one.
		if c.Method == "coh" {
			for _, charged := range []string{"was charged", "AWS billed on the day", "this account was charged"} {
				if strings.Contains(note, charged) {
					t.Errorf("the modelled-rate disclosure describes a charge (%q):\n  %s", charged, note)
				}
			}
			if !strings.Contains(note, "modelled to cost") {
				t.Errorf("the modelled-rate disclosure does not say the figure is modelled:\n  %s", note)
			}
		}
	}
	// Amortized is the one that most invites the misreading, so it names it.
	if !strings.Contains(got[0], "deleting a covered row does not return the amount shown") {
		t.Errorf("the amortized disclosure does not say what deleting a covered resource returns:\n  %s", got[0])
	}
	// An unrecognized metric is quoted rather than described, so the report never
	// explains a metric it has never seen.
	if !strings.Contains(got[7], "a metric AWS has not shipped yet") {
		t.Errorf("an unrecognized metric is not named in its own disclosure:\n  %s", got[7])
	}
	// And a metric name left over from a Cost Explorer pass is not repeated as a
	// description of a Cost Optimization Hub figure.
	if strings.Contains(got[9], "AmortizedCost") {
		t.Errorf("a Cost Explorer metric is described over a modelled rate:\n  %s", got[9])
	}
	// The branch above only exists if the caller says which source it is holding.
	// Dropping the argument is silent — the parameter goes undefined, the note
	// falls through to the Cost Explorer wording, and every case here still
	// passes because they all call the function directly. So the one call site
	// is pinned too.
	if !strings.Contains(reportTemplate, "commitmentNote(metric, COST.method)") {
		t.Error("renderCostNote no longer tells the disclosure which source the figure came from")
	}
}

// Every source now gets its own hero tile, so the sub-lines under one no longer
// have to name it — the tile is headed with it. What they still have to do is
// keep the tile's own gap scoped to the tile's own source, and keep the two
// kinds of gap apart: a resource this source never priced, and one it priced in
// a currency nothing on the page can add. Folding the second into the first
// would call an amount printed two rows below "missing".
func TestReportHeroSplitsEachSourcesGapByKind(t *testing.T) {
	cases := []struct {
		name        string
		priced      int
		offCurrency int
		total       int
		want        string
	}{
		{
			name:   "the plain gap does not name a source, because the tile is the source",
			priced: 28, total: 98,
			want: "70 carry no figure from this source",
		},
		{
			// Both kinds at once, and neither absorbs the other: 98 = 28 priced
			// + 2 off-currency + 68 the source never reported.
			name:   "an off-currency figure is not counted as missing",
			priced: 28, offCurrency: 2, total: 98,
			want: "68 carry no figure from this source · 2 priced in another currency, added to nothing",
		},
		{
			// The source reached every resource; the only gap is this page's
			// inability to add two currencies together.
			name:   "off-currency alone",
			priced: 10, offCurrency: 2, total: 12,
			want: "2 priced in another currency, added to nothing",
		},
		{
			// Full coverage has no gap to describe, and "0 carry no figure" is a
			// sentence about nothing.
			name:   "everything priced states no gap",
			priced: 12, total: 12,
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := strings.Join([]string{
				jsFunc(t, "heroGap"),
				"console.log(JSON.stringify(heroGap({ priced: " + strconv.Itoa(c.priced) +
					", offCurrency: " + strconv.Itoa(c.offCurrency) + " }, " + strconv.Itoa(c.total) + ")));",
			}, "\n")

			var got string
			evalJSON(t, script, &got)
			if got != c.want {
				t.Errorf("heroGap() =\n  %q\nwant\n  %q", got, c.want)
			}
			// The phrasing this replaced, barred rather than merely corrected.
			if strings.Contains(got, "reported by AWS") {
				t.Errorf("a method-scoped gap is stated as a fact about AWS:\n  %s", got)
			}
		})
	}

	// The count line itself, which is now the whole of what heroCoverage says.
	for _, c := range []struct {
		priced, total int
		want          string
	}{
		{28, 98, "across 28 of 98 resources"},
		{1, 1, "across 1 of 1 resource"},
	} {
		script := strings.Join([]string{
			jsFunc(t, "plural"),
			jsFunc(t, "heroCoverage"),
			"console.log(JSON.stringify(heroCoverage(" + strconv.Itoa(c.priced) + ", " + strconv.Itoa(c.total) + ")));",
		}, "\n")
		var got string
		evalJSON(t, script, &got)
		if got != c.want {
			t.Errorf("heroCoverage(%d, %d) = %q, want %q", c.priced, c.total, got, c.want)
		}
	}

	// The source's name has to be somewhere, and it moved to the tile's head.
	if !strings.Contains(reportTemplate, "tile(methodLabel(m.method), decFormat(s.total), s.currency") {
		t.Error("hero tiles no longer name the source they total")
	}
}

// A share of a whole that is zero is not a small share; it is not a share, and
// decPercent says so by returning null — which concatenates into the literal
// word "null" if it is used unguarded. Zero is reached by an estate of stored
// zeros and, more often, by credits cancelling the charges out, which is when
// the tile above this line is still showing a real untagged amount. So the
// missing percentage gets a reason rather than a blank, and the guard is the
// one renderAttrBar uses: a whole that nets negative would produce a signed
// percentage here while the bar has already refused to weight by spend at all.
func TestReportUntaggedTileStatesNoShareOfAWholeItCannotDivide(t *testing.T) {
	cases := []struct {
		name  string
		sum   string // an amount as the collector writes it
		whole string // "" for no total at all
		want  string
	}{
		{
			name: "an ordinary share",
			sum:  "410.00", whole: "1000.00",
			want: "41% of priced spend · 7 of 22 priced resources",
		},
		{
			// Credits cancelling the charges: the amount above this line is real,
			// the denominator is not divisible, and the tile says which.
			name: "a zero whole states why there is no percentage",
			sum:  "12.50", whole: "0.00",
			want: "7 of 22 priced resources · priced spend totals 0.00 USD, so no share of it can be stated",
		},
		{
			// A net-negative estate is refused for the same reason the attribution
			// bar refuses it, not merely because decPercent returns null — here it
			// would return a signed number, and the two must not disagree.
			name: "a negative whole is refused, not signed",
			sum:  "12.50", whole: "-50.00",
			want: "7 of 22 priced resources · priced spend totals -50.00 USD, so no share of it can be stated",
		},
		{
			name: "no total at all",
			sum:  "12.50", whole: "",
			want: "7 of 22 priced resources",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			whole := "null"
			if c.whole != "" {
				whole = "decParse(" + strconv.Quote(c.whole) + ")"
			}
			script := strings.Join([]string{
				jsVar(t, "DEC_RE"),
				jsFunc(t, "pow10"),
				jsFunc(t, "decParse"),
				jsFunc(t, "decAlign"),
				jsFunc(t, "decFormat"),
				jsFunc(t, "decPercent"),
				jsFunc(t, "formatMoney"),
				jsFunc(t, "untaggedShare"),
				"console.log(JSON.stringify(untaggedShare(decParse(" + strconv.Quote(c.sum) + "), " +
					whole + `, 7, 22, "spend", "USD")));`,
			}, "\n")

			var got string
			evalJSON(t, script, &got)
			if got != c.want {
				t.Errorf("untaggedShare() =\n  %q\nwant\n  %q", got, c.want)
			}
			for _, nonValue := range []string{"null", "NaN", "undefined"} {
				if strings.Contains(got, nonValue) {
					t.Errorf("a JS non-value reached the reader (%q):\n  %s", nonValue, got)
				}
			}
		})
	}
}

// methodCounts is what the method line under the hero tiles is counting, and it
// counts resources rather than figures: "Cost Explorer priced 28 resources" has
// to mean 28 things a reader can point at, whatever number of cost records AWS
// attached to each. So one resource priced twice by one method is one, and one
// resource priced by that method in two currencies is one in each.
//
// The per-currency dedupe used to key a flat map on the method and the currency
// joined by a raw NUL — a control character in every rendered report, chosen
// because it seemed like one AWS would never send. There is no such character:
// currency is whatever AWS returned, held verbatim under the same rule as every
// other reported value. So the two dimensions are nested instead of joined, and
// the last case here is the one a delimiter cannot pass.
func TestReportMethodCountsTalliesResourcesNotFigures(t *testing.T) {
	type tally struct {
		Method     string         `json:"method"`
		Priced     int            `json:"priced"`
		Currencies map[string]int `json:"currencies"`
	}

	cases := []struct {
		name string
		rows string
		want []tally
	}{
		{
			name: "one resource, one figure",
			rows: `[{"costs":[{"method":"ce","currency":"USD"}]}]`,
			want: []tally{{Method: "ce", Priced: 1, Currencies: map[string]int{"USD": 1}}},
		},
		{
			// Cost Explorer returns a row per record type, so one resource commonly
			// arrives with several. It is still one resource.
			name: "a resource priced twice by one method counts once",
			rows: `[{"costs":[{"method":"ce","currency":"USD"},{"method":"ce","currency":"USD"}]}]`,
			want: []tally{{Method: "ce", Priced: 1, Currencies: map[string]int{"USD": 1}}},
		},
		{
			name: "one resource in two currencies counts once in each",
			rows: `[{"costs":[{"method":"ce","currency":"USD"},{"method":"ce","currency":"EUR"}]}]`,
			want: []tally{{Method: "ce", Priced: 1, Currencies: map[string]int{"USD": 1, "EUR": 1}}},
		},
		{
			name: "two resources, same method and currency",
			rows: `[{"costs":[{"method":"ce","currency":"USD"}]},{"costs":[{"method":"ce","currency":"USD"}]}]`,
			want: []tally{{Method: "ce", Priced: 2, Currencies: map[string]int{"USD": 2}}},
		},
		{
			// Commonest method first; the tie breaks alphabetically so the line does
			// not reorder itself between runs of the same estate.
			name: "methods ordered by reach, then by name",
			rows: `[{"costs":[{"method":"coh","currency":"USD"},{"method":"ce","currency":"USD"}]},` +
				`{"costs":[{"method":"coh","currency":"USD"}]}]`,
			want: []tally{
				{Method: "coh", Priced: 2, Currencies: map[string]int{"USD": 2}},
				{Method: "ce", Priced: 1, Currencies: map[string]int{"USD": 1}},
			},
		},
		{
			name: "a resource with no costs contributes nothing",
			rows: `[{},{"costs":[]},{"costs":[{"method":"ce","currency":"USD"}]}]`,
			want: []tally{{Method: "ce", Priced: 1, Currencies: map[string]int{"USD": 1}}},
		},
		{
			// Neither field is guaranteed, and an absent one is its own bucket
			// rather than an excuse to drop the record.
			name: "an unstated method and currency are their own buckets",
			rows: `[{"costs":[{"amount":"1.00"}]}]`,
			want: []tally{{Method: "", Priced: 1, Currencies: map[string]int{"": 1}}},
		},
		{
			// The delimiter case. Any single character used to join the two fields
			// is a character that makes these two records the same key, and the
			// second currency then goes uncounted under a method that is priced.
			name: "two records whose fields collide under any joining character",
			rows: `[{"costs":[{"method":"ce","currency":"|x"},{"method":"ce|","currency":"x"}]}]`,
			want: []tally{
				{Method: "ce", Priced: 1, Currencies: map[string]int{"|x": 1}},
				{Method: "ce|", Priced: 1, Currencies: map[string]int{"x": 1}},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := jsFunc(t, "methodCounts") + "\n" +
				"console.log(JSON.stringify(methodCounts(" + c.rows + ")));"

			var got []tally
			evalJSON(t, script, &got)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("methodCounts() =\n  %+v\nwant\n  %+v", got, c.want)
			}
			// A method is priced for a resource or it is not, so no currency can be
			// counted more often than the method that reported it.
			for _, m := range got {
				for cur, n := range m.Currencies {
					if n > m.Priced {
						t.Errorf("%s reports %s for %d resources but is priced for %d",
							m.Method, cur, n, m.Priced)
					}
				}
			}
		})
	}
}

// stampCosts is the producer for the panel below: it decides, per row, why the
// row is not in the totals, and groupReasons only sorts what it was told. The
// distinction it has to keep is between a resource AWS never priced and one AWS
// did price with a figure this page will not add up — because the second one has
// an amount printed in its own row, and filing it under "no cause recorded"
// makes the page contradict itself a line apart.
func TestReportStampCostsSeparatesAnExcludedFigureFromAMissingOne(t *testing.T) {
	type stamped struct {
		Name string `json:"name"`
		Gap  string `json:"gap"`
		Dec  bool   `json:"dec"`
	}
	type result struct {
		Rows     []stamped `json:"rows"`
		Priced   int       `json:"priced"`
		Caveated int       `json:"caveated"`
	}

	const rows = `[
	  {"name":"totalled",   "costs":[{"method":"ce","currency":"USD","amount":"12.50"}]},
	  {"name":"zero",       "costs":[{"method":"ce","currency":"USD","amount":"0"}]},
	  {"name":"caveated",   "costs":[{"method":"ce","currency":"USD","amount":"3.00","caveats":["partial"]}]},
	  {"name":"euros",      "costs":[{"method":"ce","currency":"EUR","amount":"9.99"}]},
	  {"name":"nocurrency", "costs":[{"method":"ce","amount":"9.99"}]},
	  {"name":"notanumber", "costs":[{"method":"ce","currency":"USD","amount":"n/a"}]},
	  {"name":"othermethod","costs":[{"method":"coh","currency":"USD","amount":"5.00"}]},
	  {"name":"nocosts"}
	]`

	cases := []struct {
		name string
		cost string
		want result
	}{
		{
			name: "Cost Explorer active, totalling dollars",
			cost: `{ method: "ce", currency: "USD" }`,
			want: result{
				Rows: []stamped{
					{Name: "totalled", Gap: "", Dec: true},
					// A zero AWS reported is a finding, not an absence: it is in the
					// currency being totalled and it parses, so it is priced.
					{Name: "zero", Gap: "", Dec: true},
					{Name: "caveated", Gap: "", Dec: true},
					{Name: "euros", Gap: "offCurrency", Dec: false},
					// No stated currency is not the currency being totalled, and
					// guessing that it is would add an unknown unit into the total.
					{Name: "nocurrency", Gap: "offCurrency", Dec: false},
					{Name: "notanumber", Gap: "unparsed", Dec: false},
					{Name: "othermethod", Gap: "absent", Dec: false},
					{Name: "nocosts", Gap: "absent", Dec: false},
				},
				Priced:   3,
				Caveated: 1,
			},
		},
		{
			// Nothing is active, so nothing was excluded: every row is an absence
			// and none of them may be reported as a figure this page turned down.
			name: "no method active",
			cost: `{ method: "", currency: "" }`,
			want: result{
				Rows: []stamped{
					{Name: "totalled", Gap: "absent"},
					{Name: "zero", Gap: "absent"},
					{Name: "caveated", Gap: "absent"},
					{Name: "euros", Gap: "absent"},
					{Name: "nocurrency", Gap: "absent"},
					{Name: "notanumber", Gap: "absent"},
					{Name: "othermethod", Gap: "absent"},
					{Name: "nocosts", Gap: "absent"},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := jsVar(t, "DEC_RE") + "\n" + jsFunc(t, "decParse") + "\n" +
				jsFunc(t, "costFor") + "\n" + jsFunc(t, "stampCosts") + "\n" +
				"var COST = " + c.cost + ";\n" +
				"var resources = " + rows + ";\n" +
				"stampCosts();\n" +
				"console.log(JSON.stringify({ rows: resources.map(function (r) {\n" +
				"  return { name: r.name, gap: r._costGap, dec: !!r._costDec };\n" +
				"}), priced: COST.priced, caveated: COST.caveated }));"

			var got result
			evalJSON(t, script, &got)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("stampCosts() =\n  %+v\nwant\n  %+v", got, c.want)
			}
			// The two are not independent: a row carrying a usable figure has no
			// gap to explain, and a row without one is owed an explanation.
			for _, r := range got.Rows {
				if r.Dec != (r.Gap == "") {
					t.Errorf("%s: gap %q and a %v decimal disagree about whether it is in the totals",
						r.Name, r.Gap, r.Dec)
				}
			}
		})
	}
}

// The panel headed "Why a figure is missing" is what accounts for the gap the
// hero tile states, so its counts have to add up to that gap. They did not:
// only Cost Optimization Hub writes a reason, the resource-level Cost Explorer
// pass deliberately writes none rather than guess at a cause, and every row
// without one was dropped on the floor. The demo estate showed 70 unpriced
// resources above a list totalling 66, with the difference unexplained.
//
// Accounting for all of them means telling four things apart, and they are not
// interchangeable. Two are absences: a cost pass recorded a cause, or nobody
// said anything. Two are exclusions: AWS did price the resource, and this page
// declines to add that figure into a total because it is in another currency or
// is not a decimal number. An exclusion reported as "no cause recorded"
// contradicts the amount printed in the very row it is about — so groupReasons
// keeps the four apart, and each caller words each one.
func TestReportMissingFigureReasonsAccountForEveryUnpricedResource(t *testing.T) {
	type group struct {
		Reason string `json:"reason"`
		Count  int    `json:"count"`
		Kind   string `json:"kind"`
	}
	const hub = "no Cost Optimization Hub recommendation for this resource"

	cases := []struct {
		name string
		rows string // as stampCosts leaves them: _costGap stamped, cost_unavailable from the census
		want []group
	}{
		{
			name: "rows nobody said anything about are still counted",
			rows: `[{"cost_unavailable":"` + hub + `"},{},{},{}]`,
			want: []group{{Count: 3, Kind: "silent"}, {Reason: hub, Count: 1, Kind: "recorded"}},
		},
		{
			name: "a recorded reason outranks an equal number of silences",
			rows: `[{"cost_unavailable":"` + hub + `"},{}]`,
			want: []group{{Reason: hub, Count: 1, Kind: "recorded"}, {Count: 1, Kind: "silent"}},
		},
		{
			name: "every row explained leaves no residual group",
			rows: `[{"cost_unavailable":"` + hub + `"},{"cost_unavailable":"` + hub + `"}]`,
			want: []group{{Reason: hub, Count: 2, Kind: "recorded"}},
		},
		{
			// "absent" is what stampCosts stamps when AWS priced nothing, which is
			// the ordinary case and not an exclusion.
			name: "nothing explained at all is one honest group",
			rows: `[{"_costGap":"absent"},{"_costGap":"absent"},{}]`,
			want: []group{{Count: 3, Kind: "silent"}},
		},
		{
			// An empty string is not a reason, and the census writes one for a
			// resource it has nothing to say about.
			name: "an empty reason string counts as silence, not as a reason",
			rows: `[{"cost_unavailable":""},{}]`,
			want: []group{{Count: 2, Kind: "silent"}},
		},
		{
			// Both of these rows print an amount in the table. Filing them under a
			// missing cause would have the page contradict itself one line apart.
			name: "an excluded figure is not an absence",
			rows: `[{"_costGap":"offCurrency"},{"_costGap":"unparsed"}]`,
			want: []group{{Count: 1, Kind: "offCurrency"}, {Count: 1, Kind: "unparsed"}},
		},
		{
			// Cost Optimization Hub recording that it has no recommendation says
			// nothing about why the active Cost Explorer figure is in euros. The
			// kind stampCosts stamped wins, because it is about the figure the
			// reader is looking at.
			name: "the active figure's exclusion outranks another pass's reason",
			rows: `[{"_costGap":"offCurrency","cost_unavailable":"` + hub + `"}]`,
			want: []group{{Count: 1, Kind: "offCurrency"}},
		},
		{
			name: "all four kinds at once, commonest-first then by kind",
			rows: `[{"cost_unavailable":"a"},{"_costGap":"offCurrency"},{"_costGap":"unparsed"},{}]`,
			want: []group{
				{Reason: "a", Count: 1, Kind: "recorded"},
				{Count: 1, Kind: "offCurrency"},
				{Count: 1, Kind: "unparsed"},
				{Count: 1, Kind: "silent"},
			},
		},
		{
			name: "commonest reason first, then alphabetically",
			rows: `[{"cost_unavailable":"b"},{"cost_unavailable":"b"},{"cost_unavailable":"a"},{"cost_unavailable":"c"}]`,
			want: []group{
				{Reason: "b", Count: 2, Kind: "recorded"},
				{Reason: "a", Count: 1, Kind: "recorded"},
				{Reason: "c", Count: 1, Kind: "recorded"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := jsVar(t, "REASON_KINDS") + "\n" + jsFunc(t, "groupReasons") + "\n" +
				"console.log(JSON.stringify(groupReasons(" + c.rows + ")));"

			var got []group
			evalJSON(t, script, &got)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("groupReasons() =\n  %+v\nwant\n  %+v", got, c.want)
			}
			// Whatever the grouping, it has to describe every row it was handed:
			// this list is the answer to a count printed elsewhere on the page.
			var in []map[string]string
			if err := json.Unmarshal([]byte(c.rows), &in); err != nil {
				t.Fatalf("decoding rows: %v", err)
			}
			sum := 0
			for _, g := range got {
				sum += g.Count
			}
			if sum != len(in) {
				t.Errorf("%d of %d unpriced resources are accounted for", sum, len(in))
			}
			// A reason string is a quotation from a cost pass. The other three
			// kinds have nothing to quote, and inventing prose here would put it
			// in both callers' mouths at once.
			for _, g := range got {
				if (g.Kind == "recorded") != (g.Reason != "") {
					t.Errorf("group %+v carries a reason string it cannot attribute to a cost pass", g)
				}
			}
		})
	}

	// And no caller may collapse the four back into two. Each kind that is not
	// silence has to be worded where it is rendered — once in the coverage
	// banner, once in the audit panel — or an exclusion goes out as an absence
	// again and the page contradicts its own table.
	for _, kind := range []string{"recorded", "offCurrency", "unparsed"} {
		snippet := `g.kind === "` + kind + `"`
		if n := strings.Count(reportTemplate, snippet); n != 2 {
			t.Errorf("the template words the %s group %d times, want 2 "+
				"(the coverage banner and the audit panel)", kind, n)
		}
	}
}

// "Caveat" is taken. A cost record carries its own caveats field — the
// qualifiers AWS attached to an amount — and the report counts those into
// COST.caveated and prints them as the † line under the table: "3 figures
// below are lower bounds". The coverage banner counts something else entirely:
// every entry coverageIssues returned, which mixes a missing rollup, an
// estimated one, a reconciliation that does not add up, unpriced resources,
// their reasons, off-currency figures, unreadable amounts, and each probe that
// came back without rows.
//
// Naming both "caveat" puts two different counts of one word on a single page,
// a banner apart, where the smaller reads as a correction of the larger. So the
// banner title may not use the word at all, and the † line may not stop using
// it: the noun has to point at exactly one of the two counts, and it is the one
// that comes from the data rather than from this page's own bookkeeping.
func TestReportCoverageTitleDoesNotBorrowTheWordForCostRecordCaveats(t *testing.T) {
	cases := []struct {
		name   string
		active bool
		n      int
	}{
		{name: "one issue", active: true, n: 1},
		{name: "several issues", active: true, n: 8},
		// The count is still passed when nothing priced, and the branch that
		// ignores it must not reintroduce the word by another route.
		{name: "nothing priced", active: false, n: 3},
	}

	for _, c := range cases {
		script := jsFunc(t, "plural") + "\n" + jsFunc(t, "coverageTitle") + "\n" +
			"var COST = { active: " + strconv.FormatBool(c.active) + " };\n" +
			"console.log(JSON.stringify(coverageTitle(" + strconv.Itoa(c.n) + ")));"
		var got string
		evalJSON(t, script, &got)
		if got == "" {
			t.Errorf("%s: the banner gets no title at all", c.name)
			continue
		}
		if strings.Contains(strings.ToLower(got), "caveat") {
			t.Errorf("%s: banner title says %q — the word belongs to the † count, "+
				"which is a different number on the same page", c.name, got)
		}
		// Not using the word is only half of it: the title still has to say how
		// many of them there are, or the banner stops carrying the count.
		if c.active && !strings.Contains(got, strconv.Itoa(c.n)) {
			t.Errorf("%s: banner title %q dropped the count", c.name, got)
		}
	}

	// The other half of the split. If the † line stops calling them caveats, the
	// word is free again and this whole test decays into a style preference —
	// so the pin is on both sides of it.
	if !strings.Contains(reportTemplate, `plural(COST.caveated, "figure")`) {
		t.Error("the † lower-bound line no longer counts a cost record's caveats; " +
			"the banner title gave up the word for it")
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

// groupHeaderShim is the DOM appendGroupCost touches — el and a button — as
// plain objects, so the assertion lands on the text a reader sees.
const groupHeaderShim = `
var document = { createElement: function (tag) {
  return { tag: tag, className: "", textContent: "", title: "",
           children: [], appendChild: function (c) { this.children.push(c); } };
} };
function text(n) {
  return (n.textContent || "") + n.children.map(text).join(" ");
}
function titles(n) {
  return [n.title || ""].concat(n.children.map(titles)).join(" ");
}`

// groupHeader runs appendGroupCost against one summary entry with the source
// selector set to (method, currency), and returns the header's text and the
// concatenation of its tooltips.
func groupHeader(t *testing.T, method, currency, spend string) (string, string) {
	t.Helper()
	script := strings.Join([]string{
		groupHeaderShim,
		jsVar(t, "COST"),
		jsVar(t, "METHOD_LABELS"),
		jsVar(t, "COST_METHODS"),
		jsFunc(t, "methodLabel"),
		jsFunc(t, "el"),
		jsFunc(t, "groupBucket"),
		jsFunc(t, "appendGroupCost"),
		`COST.method = ` + strconv.Quote(method) + `;`,
		`COST.currency = ` + strconv.Quote(currency) + `;`,
		`var btn = document.createElement("button");`,
		`appendGroupCost(btn, ` + spend + `);`,
		`console.log(JSON.stringify([text(btn), titles(btn)]));`,
	}, "\n")

	var got []string
	evalJSON(t, script, &got)
	if len(got) != 2 {
		t.Fatalf("harness returned %d values, want 2", len(got))
	}
	return got[0], got[1]
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
	cases := []struct {
		name     string
		spend    string
		want     string // substring the header must carry
		wantSeen bool
	}{
		{
			name:     "partial coverage is disclosed",
			spend:    `{costs: [{method: "ce", currency: "USD", amount: "4425.52", priced: 22}], priced: 22, total: 73}`,
			want:     "over 22 of 73",
			wantSeen: true,
		},
		{
			name:     "full coverage says nothing extra",
			spend:    `{costs: [{method: "ce", currency: "USD", amount: "36.28", priced: 12}], priced: 12, total: 12}`,
			want:     "over ",
			wantSeen: false,
		},
		{
			// The count that matters is the printed bucket's, not the group's.
			// Ten of these twelve have a Cost Optimization Hub estimate, so the
			// group's own priced count is 12 — but the total on screen is Cost
			// Explorer's, and Cost Explorer reached two of them. Reading the
			// group's count here would suppress the disclosure on a total
			// covering a sixth of what it is printed beside.
			name: "the fraction is the printed bucket's reach, not the group's",
			spend: `{costs: [{method: "ce", currency: "USD", amount: "40.00", priced: 2},` +
				`{method: "coh", currency: "USD", amount: "900.00", priced: 10}], priced: 12, total: 12}`,
			want:     "over 2 of 12",
			wantSeen: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := groupHeader(t, "ce", "USD", tc.spend)

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

// The header prints one total: the one from the source the reader selected.
//
// A group carries a total per (method, currency) pair its members were priced
// in, and the page has exactly one cost axis. Printing every pair at once sets
// a billed Cost Explorer window total beside a modelled Cost Optimization Hub
// monthly rate — same currency suffix, joined by a middot — which is the
// cross-method ranking the rest of the report refuses to do, performed where
// the eye lands first. Worse, it does not move when the source selector does,
// so a header can print money from Cost Explorer over a column of Cost
// Optimization Hub figures, or over a column of "—".
func TestReportGroupHeaderPrintsOnlyTheSelectedSource(t *testing.T) {
	// One group, priced by both sources: 14 days of billed spend and a
	// modelled monthly rate, which are not comparable and must never share a
	// line.
	const spend = `{costs: [` +
		`{method: "ce", currency: "USD", amount: "5140.80", priced: 6},` +
		`{method: "coh", currency: "USD", amount: "10360.00", priced: 4}` +
		`], priced: 6, total: 6}`

	ce, _ := groupHeader(t, "ce", "USD", spend)
	coh, _ := groupHeader(t, "coh", "USD", spend)

	if !strings.Contains(ce, "5140.80") {
		t.Errorf("Cost Explorer header %q does not print the Cost Explorer total", ce)
	}
	if strings.Contains(ce, "10360.00") {
		t.Errorf("Cost Explorer header %q also prints the Cost Optimization Hub "+
			"total: a billed window total and a modelled monthly rate on one line "+
			"is the comparison this report exists to refuse", ce)
	}
	if !strings.Contains(coh, "10360.00") {
		t.Errorf("Cost Optimization Hub header %q does not print its own total: "+
			"the header is stranded on whichever bucket the summary listed first", coh)
	}
	if strings.Contains(coh, "5140.80") {
		t.Errorf("Cost Optimization Hub header %q still prints the Cost Explorer total", coh)
	}

	// Each source discloses its own reach. Six of six under Cost Explorer is
	// full coverage and says nothing; four of six under the Hub is not.
	if strings.Contains(ce, "over ") {
		t.Errorf("Cost Explorer header %q discloses partial coverage over 6 of 6", ce)
	}
	if !strings.Contains(coh, "over 4 of 6") {
		t.Errorf("Cost Optimization Hub header %q does not disclose that its total "+
			"covers 4 of the group's 6 members", coh)
	}
}

// A group the selected source never priced has no total to print. Printing
// nothing is not an option: a blank header among headers carrying figures reads
// as free, and the group is not free — it is unpriced by the source on screen.
// Printing the other source's figure is worse, and printing zero is a number
// nobody reported.
func TestReportGroupHeaderSaysWhenTheSelectedSourcePricedNothing(t *testing.T) {
	// The demo census's natgateway group: Cost Optimization Hub modelled it,
	// Cost Explorer never reported it, and under Cost Explorer it sat above
	// four rows of "—" printing 131.40 USD.
	const spend = `{costs: [{method: "coh", currency: "USD", amount: "131.40", priced: 4}], priced: 4, total: 4}`

	got, tips := groupHeader(t, "ce", "USD", spend)

	if strings.Contains(got, "131.40") {
		t.Errorf("header %q prints a Cost Optimization Hub total while Cost "+
			"Explorer is the selected source", got)
	}
	if !strings.Contains(got, "no Cost Explorer figure") {
		t.Errorf("header %q says nothing: an empty header beside headers that "+
			"carry totals reads as free", got)
	}
	// And it must point at the way out, since the figure does exist.
	if !strings.Contains(tips, "switch source") {
		t.Errorf("tooltips %q do not mention that another source may have priced "+
			"this group", tips)
	}

	// A currency the reader did not select is just as absent: two totals in
	// two currencies may not be summed, so the unselected one is not this
	// group's total either.
	other, _ := groupHeader(t, "coh", "EUR", spend)
	if strings.Contains(other, "131.40") {
		t.Errorf("header %q prints a USD total under a EUR selection", other)
	}
}

// A group *nothing* priced is the other way to have no figure, and it used to
// be the silent one: appendGroupCost returned early on a missing summary entry
// and appended no slot at all.
//
// On its own that was defensible — it is what the report did before there was a
// source selector, and back then a blank header meant one thing. Adding the
// selector added a second no-figure state, which the header labels; leaving the
// first blank makes the two contradict each other on the same screen. In the
// 6000-resource fixture that is twelve service headers printing nothing beside
// two printing money, while dynamodb — in the identical epistemic position, no
// figure from the selected source — reads "no Cost Optimization Hub figure".
// Whatever the header says about an unpriced group, it has to say it the same
// way both times, or the blank ones read as free.
//
// The tooltip is where the two part company, because only the tooltip can
// afford to: telling a reader to switch source is help when the figure exists
// somewhere in the file and a wild goose chase when it does not.
func TestReportGroupHeaderSaysWhenNothingPricedTheGroupAtAll(t *testing.T) {
	// No summary entry: Go attributed no spend to this group under any source
	// or currency, so there is no bucket to select and none to fall back to.
	got, tips := groupHeader(t, "ce", "USD", "null")

	if !strings.Contains(got, "no Cost Explorer figure") {
		t.Errorf("header %q is blank: a header with no cost slot, sitting among "+
			"headers that carry one, reads as a group that costs nothing", got)
	}
	if strings.Contains(tips, "switch source") {
		t.Errorf("tooltips %q send the reader to another source for a figure no "+
			"source in this report produced", tips)
	}
	if !strings.Contains(tips, "no source in this report priced") {
		t.Errorf("tooltips %q do not say why the figure is missing", tips)
	}

	// And it must not invent one. Zero is a number a source reported; nobody
	// reported anything here.
	for _, no := range []string{"0.00", "0 ", "over "} {
		if strings.Contains(got, no) {
			t.Errorf("header %q contains %q: an unpriced group has no total and no "+
				"coverage fraction", got, no)
		}
	}
}

// Whether a group header carries a cost slot at all is a property of the
// grouping, not of the group: either this dimension is one the report can talk
// about money in, and every header speaks — some with a total, some to say they
// have none — or it is not, and none of them do.
//
// The distinction the flag exists to keep is between "no figure" and "no
// question". Go writes a summary entry only for a group something priced, so a
// missing entry inside a priced dimension means the reader is looking at a
// group its neighbours' figures leave out, which is the header's job to say.
// A dimension Go priced nowhere is the other case: no header has a figure, a
// blank one contrasts with nothing, and stamping "no Cost Explorer figure" onto
// all fourteen of them is the coverage banner restated fourteen times.
func TestReportGroupSlotIsAPropertyOfTheDimensionNotTheGroup(t *testing.T) {
	// Two services, one of which Go priced. Both must come back with a slot.
	const priced = `{groups: {service: [
    {value: "rds", costs: [{method: "ce", currency: "USD", amount: "12.00", priced: 1}], priced: 1, total: 1}
  ]}}`
	// The same census under a dimension nothing was attributed to. Go drops a
	// dimension entirely when no group in it carries a total, so this is what
	// grouping by region looks like on a report whose costs are per service.
	const unpriced = `{groups: {service: []}}`

	type group struct {
		Group string `json:"_group"`
		Slot  bool   `json:"_costSlot"`
		Spend *struct {
			Total int `json:"total"`
		} `json:"_spend"`
	}

	run := func(t *testing.T, summary, filter string) []group {
		t.Helper()
		script := strings.Join([]string{
			"var summary = " + summary + ";",
			"var costIndexCache = Object.create(null);",
			"var openByDim = Object.create(null);",
			"var activeTier = null, activeService = null;",
			"var filterEl = { value: " + strconv.Quote(filter) + " };",
			"function defaultOpen() { return false; }",
			// Deliberately false, and the slot must appear anyway. COST.active
			// is the nearby flag that means "the selected source priced nothing
			// in this census", which is a state the header exists to report, not
			// a reason to fall silent; gating the slot on it would blank every
			// header on exactly the report that most needs them labelled.
			`var COST = { active: false, method: "ce", currency: "USD" };`,
			jsVar(t, "GROUP_FIELD"),
			jsFunc(t, "costIndex"),
			jsFunc(t, "filterActive"),
			jsFunc(t, "isOpen"),
			jsFunc(t, "flatten"),
			`var rows = [{service: "rds"}, {service: "sqs"}];`,
			`console.log(JSON.stringify(flatten(rows, "service")));`,
		}, "\n")
		var got []group
		evalJSON(t, script, &got)
		return got
	}

	t.Run("a priced dimension gives every group a slot", func(t *testing.T) {
		got := run(t, priced, "")
		if len(got) != 2 {
			t.Fatalf("flatten returned %d groups, want 2: %+v", len(got), got)
		}
		for _, g := range got {
			if !g.Slot {
				t.Errorf("group %q carries no cost slot; it will render blank beside "+
					"a sibling that prints money, which reads as free rather than "+
					"as unpriced", g.Group)
			}
		}
		// And the slot still reports the truth about each one: rds has Go's
		// entry, sqs has none and must not borrow its neighbour's.
		for _, g := range got {
			if (g.Group == "rds") != (g.Spend != nil) {
				t.Errorf("group %q spend = %+v; want the entry only for rds", g.Group, g.Spend)
			}
		}
	})

	t.Run("an unpriced dimension gives none of them one", func(t *testing.T) {
		for _, g := range run(t, unpriced, "") {
			if g.Slot {
				t.Errorf("group %q claims a cost slot in a dimension no source "+
					"priced: every header would say the same nothing", g.Group)
			}
		}
	})

	// A filtered table's group totals cover rows that are not on screen, so the
	// slot goes away with the same reasoning — and it must go away for the
	// priced groups too, not just the ones without an entry.
	t.Run("a filtered table shows no group totals", func(t *testing.T) {
		for _, g := range run(t, priced, "rds") {
			if g.Slot || g.Spend != nil {
				t.Errorf("group %q kept its cost slot under a filter: the total is "+
					"the whole group's, and the group is not what is on screen", g.Group)
			}
		}
	})
}

// compareGroupCost ranks group headers, and it reads the totals off the summary
// entry Go writes. It used to be handed a bare costs array; it is now handed the
// whole entry, because the entry is what carries the coverage counts that decide
// whether ranking is allowed at all. A stale caller would read undefined off
// every group and sort the table into census order without saying so.
func TestReportCompareGroupCostReadsTheSummaryEntry(t *testing.T) {
	entry := func(amount string) string {
		return `{costs: [{method: "ce", currency: "USD", amount: "` + amount +
			`", priced: 1}], priced: 1, total: 1}`
	}
	script := strings.Join([]string{
		jsVar(t, "COST"),
		jsFunc(t, "groupBucket"),
		jsFunc(t, "compareGroupCost"),
		jsFunc(t, "compareDecimal"),
		jsFunc(t, "compareMagnitude"),
		`COST.method = "ce"; COST.currency = "USD";`,
		`console.log(JSON.stringify([`,
		`  compareGroupCost(` + entry("100.00") + `, ` + entry("90.00") + `),`,
		`  compareGroupCost(` + entry("90.00") + `, ` + entry("100.00") + `),`,
		`  compareGroupCost(` + entry("10.50") + `, ` + entry("10.50") + `),`,
		// A group whose figures all failed to parse carries no total, and must
		// not be ordered as though it were the cheapest thing in the census.
		`  compareGroupCost({costs: [], priced: 0, total: 4}, ` + entry("1.00") + `),`,
		// A group only the other source priced is unpriced here. Ranking it on
		// a Cost Optimization Hub estimate would put a modelled monthly rate in
		// an ordering of billed windows, and would order the headers by a
		// figure none of them is printing.
		`  compareGroupCost({costs: [{method: "coh", currency: "USD", amount: "9000.00", priced: 3}],` +
			` priced: 3, total: 3}, ` + entry("1.00") + `)`,
		`]));`,
	}, "\n")

	var got []int
	evalJSON(t, script, &got)

	want := []int{1, -1, 0, -1, -1}
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
