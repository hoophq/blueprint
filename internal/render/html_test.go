package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// renderDemo renders the demo snapshot to a temp file and returns the HTML.
func renderDemo(t *testing.T) string {
	t.Helper()
	snap := demoSnapshot("test")
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return string(b)
}

func TestHTMLReportSelfContained(t *testing.T) {
	snap := demoSnapshot("test")
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("report file is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	html := string(b)

	if !strings.Contains(html, "orders-prod") {
		t.Error("report does not contain known demo resource name orders-prod")
	}

	// Offline promise: zero resource loads. No scripts, styles, images, or
	// fonts from the network, no <link> tags, no CSS imports. Anchor
	// navigation hrefs are allowed but checked against an exact allowlist in
	// TestHTMLNavigationLinksAllowlisted.
	for _, needle := range []string{
		`src="http`, `src='http`,
		`href='http`,
		`url(http`, "@import",
		"<link", "integrity=",
	} {
		if strings.Contains(html, needle) {
			t.Errorf("report contains external-load marker %q — must render fully offline", needle)
		}
	}

	if !strings.Contains(html, `<script type="application/json" id="blueprint-data">`) {
		t.Error("report does not contain the embedded JSON data block")
	}
	if strings.Contains(html, dataMarker) {
		t.Error("data marker was not replaced with snapshot JSON")
	}
}

// hostileSnapshot builds a census of n copies of the same deliberately hostile
// resource: a name and an attribute that both try to close the script block the
// census travels inside.
func hostileSnapshot(n int) *model.Snapshot {
	resources := make([]model.Resource, n)
	for i := range resources {
		r := model.Resource{
			ARN:       fmt.Sprintf("arn:aws:rds:us-east-1:111111111111:db:evil-%d", i),
			Service:   model.ServiceRDS,
			Type:      model.TypeRDSInstance,
			Name:      "evil</script><script>alert(1)</script>",
			Region:    "us-east-1",
			AccountID: "111111111111",
			Tags:      map[string]string{"owner": "</script>"},
		}
		r.SetAttr(model.AttrEngine, "postgres</script><script>alert(2)</script>")
		resources[i] = r
	}
	return &model.Snapshot{
		Version:   "test",
		Accounts:  []string{"111111111111"},
		Regions:   []string{"us-east-1"},
		Resources: resources,
	}
}

// TestHTMLEscapesScriptBreakout covers the readable path, where the census is
// embedded as plain JSON and every hostile '<' must appear as its JSON escape.
// The compressed path is a different encoding with the same promise and is
// covered separately, below.
func TestHTMLEscapesScriptBreakout(t *testing.T) {
	evil := model.Resource{
		ARN:       "arn:aws:rds:us-east-1:111111111111:db:evil",
		Service:   model.ServiceRDS,
		Type:      model.TypeRDSInstance,
		Name:      "evil</script><script>alert(1)</script>",
		Region:    "us-east-1",
		AccountID: "111111111111",
		Tags:      map[string]string{"owner": "</script>"},
	}
	// The attribute bag carries arbitrary AWS-reported strings, so it is just
	// as much an injection surface as the core fields.
	evil.SetAttr(model.AttrEngine, "postgres</script><script>alert(2)</script>")
	snap := &model.Snapshot{
		Version:   "test",
		Accounts:  []string{"111111111111"},
		Regions:   []string{"us-east-1"},
		Resources: []model.Resource{evil},
	}
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	html := string(b)

	if strings.Contains(html, "evil</script>") {
		t.Error("resource name broke out of the JSON script block")
	}
	if strings.Contains(html, "postgres</script>") {
		t.Error("attribute value broke out of the JSON script block")
	}
	// json.Marshal unicode-escapes angle brackets inside strings; the exact
	// escaped form of every hostile value must appear in the embedded block.
	for _, raw := range []string{
		snap.Resources[0].Name,
		snap.Resources[0].Attr(model.AttrEngine),
	} {
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("marshal %q: %v", raw, err)
		}
		escaped := strings.Trim(string(b), `"`)
		if strings.Contains(escaped, "</") {
			t.Fatal("sanity: expected json.Marshal to escape the angle brackets")
		}
		if !strings.Contains(html, escaped) {
			t.Errorf("expected %q to appear unicode-escaped in the JSON block", raw)
		}
	}
}

// TestHTMLEscapesScriptBreakoutWhenCompressed is the same promise on the other
// side of the compression threshold, and it is checked differently on purpose.
//
// Base64's alphabet contains no angle bracket at all, so the safety argument
// here is not "the hostile text was escaped" but "the block cannot contain
// markup of any kind" — which is a stronger claim and a cheaper one to verify.
// The regex is the whole check. What it cannot tell you is whether the census
// survived, so the test also decodes the block and requires the hostile string
// back byte for byte: an encoding that neutralised the payload by mangling it
// would pass the first half and fail the second.
func TestHTMLEscapesScriptBreakoutWhenCompressed(t *testing.T) {
	snap := hostileSnapshot(compressAbove + 1)
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	html := string(b)

	meta := metaBlock(t, html)
	if meta.Encoding != encodingGzip {
		t.Fatalf("encoding = %q for a %d-resource census, want %q; "+
			"this test is not exercising the compressed path",
			meta.Encoding, len(snap.Resources), encodingGzip)
	}
	// Only bracket sequences matter here. The hostile engine name also reaches
	// the plain-JSON meta block as a platform row, so the words inside it —
	// "script", "alert(1)" — do appear in the page with every '<' escaped away.
	// That is the escaping working, not a leak; asserting those words are absent
	// would be asserting that the summary dropped data.
	for _, needle := range []string{"</script><script>", "<!--"} {
		if strings.Contains(html, needle) {
			t.Errorf("hostile sequence %q appears literally in the page", needle)
		}
	}
	if block := dataBlock(t, html); !regexp.MustCompile(`^[A-Za-z0-9+/=]+$`).MatchString(block) {
		t.Errorf("census block is not pure base64; first offending stretch: %q",
			regexp.MustCompile(`[^A-Za-z0-9+/=]+`).FindString(block))
	}
	// The meta block is plain JSON and carries the same hostile string as a
	// platform name, so it has to survive escaping unchanged too.
	wantEngine := snap.Resources[0].Attr(model.AttrEngine)
	seen := false
	for _, p := range meta.Summary.Platforms {
		if p.Name == wantEngine {
			seen = true
		}
	}
	if !seen {
		t.Errorf("the hostile platform name did not survive the meta block; got %+v", meta.Summary.Platforms)
	}

	got := decodeDataBlock(t, html)
	if len(got) != len(snap.Resources) {
		t.Fatalf("decoded %d resources, want %d", len(got), len(snap.Resources))
	}
	want := snap.Resources[0]
	if got[0].Name != want.Name {
		t.Errorf("hostile name did not survive compression:\n want %q\n  got %q", want.Name, got[0].Name)
	}
	if got[0].Attr(model.AttrEngine) != want.Attr(model.AttrEngine) {
		t.Errorf("hostile attribute did not survive compression:\n want %q\n  got %q",
			want.Attr(model.AttrEngine), got[0].Attr(model.AttrEngine))
	}
	if got[0].Tags["owner"] != want.Tags["owner"] {
		t.Errorf("hostile tag did not survive compression:\n want %q\n  got %q",
			want.Tags["owner"], got[0].Tags["owner"])
	}
}

// The cost report is embedded in the same JSON script block as the
// resources, and every string in it — service names, record types, currency
// codes, amounts — is chosen by AWS, not by this tool. It is the same
// injection surface and has to be escaped the same way.
func TestHTMLEscapesCostBreakout(t *testing.T) {
	evil := "</script><script>alert(3)</script>"
	snap := &model.Snapshot{
		Version:  "test",
		Accounts: []string{"111111111111"},
		Regions:  []string{"us-east-1"},
		Cost: &model.CostReport{
			Window:   model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: evil},
			Metric:   evil,
			Accounts: []string{evil},
			Currencies: []model.CostByCurrency{{
				Currency:            evil,
				Total:               "1.00",
				Attributed:          "1.00",
				Unattributed:        "0.00",
				Services:            []model.NamedAmount{{Name: "Amazon RDS" + evil, Amount: "1.00"}},
				UnattributedRecords: []model.NamedAmount{{Name: "Tax" + evil, Amount: "0.00"}},
			}},
			Meter: model.CostMeter{Requests: 2, EstimatedChargeUSD: "0.02"},
		},
	}
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	html := string(b)

	if strings.Contains(html, evil) {
		t.Error("a cost string broke out of the JSON script block")
	}
	escaped, err := json.Marshal(evil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, strings.Trim(string(escaped), `"`)) {
		t.Error("the hostile cost string does not appear unicode-escaped in the JSON block")
	}
}

func TestHTMLTemplateHasDataMarker(t *testing.T) {
	if !strings.Contains(reportTemplate, dataMarker) {
		t.Fatalf("embedded report template does not contain data marker %q", dataMarker)
	}
}

// TestHTMLMissingMarkerErrors covers both injection points. Each is removed on
// its own, because a template that has lost one still has the other and would
// otherwise render a report missing half its data — a page with a headline and
// no census, or a census the page never reads.
func TestHTMLMissingMarkerErrors(t *testing.T) {
	for _, marker := range []string{metaMarker, dataMarker} {
		t.Run(marker, func(t *testing.T) {
			orig := reportTemplate
			defer func() { reportTemplate = orig }()
			reportTemplate = strings.ReplaceAll(orig, marker, "")

			path := filepath.Join(t.TempDir(), "report.html")
			err := HTML(&model.Snapshot{Version: "test"}, path)
			if err == nil {
				t.Fatalf("HTML() succeeded with a template missing %q, want error", marker)
			}
			if !strings.Contains(err.Error(), marker) {
				t.Errorf("error %q does not mention the missing marker %q", err, marker)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("report file should not be written when a marker is missing (stat err = %v)", statErr)
			}
		})
	}
}

func TestHTMLNeutralizesCommentAndScriptOpeners(t *testing.T) {
	hostile := "<!--<script>alert(1)</script>-->"
	snap := &model.Snapshot{
		Version:  "test",
		Accounts: []string{"111111111111"},
		Regions:  []string{"us-east-1"},
		Resources: []model.Resource{{
			ARN:       "arn:aws:rds:us-east-1:111111111111:db:hostile",
			Service:   model.ServiceRDS,
			Type:      model.TypeRDSInstance,
			Name:      hostile,
			Region:    "us-east-1",
			AccountID: "111111111111",
		}},
	}
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	html := string(b)

	// The hostile name must never appear with literal angle brackets, and no
	// HTML comment opener may exist anywhere in the page (the template ships
	// none), so the parser's script-data state can never be changed by data.
	if strings.Contains(html, hostile) {
		t.Error("hostile resource name appears unescaped in the report")
	}
	if strings.Contains(html, "<!--") {
		t.Error("report contains a literal HTML comment opener sourced from data")
	}
	// The escaped form (every '<' as a JSON unicode escape, exactly what
	// json.Marshal produces for strings) must be present instead.
	nameJSON, err := json.Marshal(hostile)
	if err != nil {
		t.Fatalf("marshal name: %v", err)
	}
	escaped := strings.Trim(string(nameJSON), `"`)
	if strings.Contains(escaped, "<") {
		t.Fatal("sanity: expected json.Marshal to escape the angle brackets")
	}
	if !strings.Contains(html, escaped) {
		t.Error("expected the hostile resource name to appear unicode-escaped in the JSON block")
	}
}

// The report is a single file handed to a human, mailed around, and opened by
// whatever is on their machine. A C0 control character in it is a spec-level
// parse error that every consumer resolves differently: browsers substitute
// U+FFFD, grep declares the file binary, and a pipeline that strips control
// bytes changes the content on the way past. None of that is a wrong number,
// which is exactly why it survives review — a raw NUL sat in the template for
// a release as the delimiter of an internal dedupe key, and rendered into
// every report.
//
// Tab, newline, and carriage return are the three the format is written in.
// Nothing else may reach the artifact, from the template or from the data.
func TestHTMLCarriesNoControlCharacters(t *testing.T) {
	cases := map[string]string{
		"demo estate":            renderDemo(t),
		"demo estate with costs": renderDemoCosts(t),
	}
	for name, html := range cases {
		for i, r := range html {
			if r >= 0x20 || r == '\t' || r == '\n' || r == '\r' {
				continue
			}
			// One line of context, so the report a reader gets is the report the
			// failure is about.
			start := strings.LastIndexByte(html[:i], '\n') + 1
			end := i + strings.IndexByte(html[i:], '\n')
			if end < i {
				end = len(html)
			}
			t.Errorf("%s: control character U+%04X at byte %d:\n  %s",
				name, r, i, strings.TrimSpace(html[start:end]))
			break
		}
	}
}

func TestHTMLEmptySnapshot(t *testing.T) {
	snap := &model.Snapshot{Version: "test"}
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(b), "No resources found") {
		t.Error("report is missing the empty-state hint")
	}
}

// TestHTMLNavigationLinksAllowlisted verifies the only external href in the
// report is the footer's navigation anchor to the GitHub repo. Anything else
// would break the offline promise.
func TestHTMLNavigationLinksAllowlisted(t *testing.T) {
	html := renderDemo(t)
	allowed := map[string]bool{
		"https://github.com/hoophq/blueprint": true,
	}
	found := map[string]int{}
	for _, m := range regexp.MustCompile(`href="(http[^"]*)"`).FindAllStringSubmatch(html, -1) {
		found[m[1]]++
	}
	for url := range found {
		if !allowed[url] {
			t.Errorf("report links to disallowed external URL %q", url)
		}
	}
	for url := range allowed {
		if found[url] == 0 {
			t.Errorf("report is missing the expected navigation link to %q", url)
		}
	}
}

// TestHTMLBrandAndAttribution checks the simplified shell: the hoop logo mark,
// the attribution-tier vocabulary, and that a fully attributed fixture
// database ships in the data block for the tier computation to classify.
func TestHTMLBrandAndAttribution(t *testing.T) {
	html := renderDemo(t)
	for _, needle := range []string{
		"M96.4167 71.4077", // first path of the hoop logo mark
		"blueprint",
		"Attribution score",
		"Untagged",
		"Partially attributed",
		"Fully attributed",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("report is missing %q", needle)
		}
	}
	// orders-prod carries both owner and environment tags, so the JS tier
	// computation has a fully attributed row to classify.
	orders := only(t, decodeDataBlock(t, html), "orders-prod")
	if orders.Owner != "payments" {
		t.Errorf("orders-prod owner = %q, want payments", orders.Owner)
	}
	if orders.Environment == "" {
		t.Error("orders-prod carries an environment tag but shipped without one")
	}
}

// only returns the one resource with the given name.
//
// Not an index, because names are not unique: a Name tag belongs to whoever
// set it, and the fixture's bastion instance shares its name with the public
// IP address attached to it, exactly as a real account would. Every caller here
// means one specific row, so ambiguity is a failure rather than a coin toss.
func only(tb testing.TB, resources []model.Resource, name string) model.Resource {
	tb.Helper()
	var found []model.Resource
	for _, r := range resources {
		if r.Name == name {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 1:
		return found[0]
	case 0:
		tb.Fatalf("fixture %q is missing from the census block", name)
	default:
		tb.Fatalf("%d fixtures are named %q; this lookup is ambiguous", len(found), name)
	}
	return model.Resource{}
}

// TestHTMLEOLMarkers checks the EOL feature ships end to end: the KPI label
// in the shell and derived eol fields in the embedded demo data (legacy-crm
// runs mysql 5.7.44, EOL upstream since 2023-10-31).
func TestHTMLEOLMarkers(t *testing.T) {
	html := renderDemo(t)
	if !strings.Contains(html, "End-of-life") {
		t.Error("report is missing the End-of-life KPI label")
	}
	crm := only(t, decodeDataBlock(t, html), "legacy-crm")
	if !crm.EOL {
		t.Error("legacy-crm runs mysql 5.7.44 but shipped without its EOL flag")
	}
	if crm.EOLDate != "2023-10-31" {
		t.Errorf("legacy-crm eol_date = %q, want 2023-10-31", crm.EOLDate)
	}
}

// TestHTMLExposureMarkers checks the exposure feature ships end to end: the
// KPI label plus the deliberately risky legacy-crm fixture (public,
// unencrypted, no backups) present in the embedded data with explicit risky
// values, and the tri-state contract (nil fields absent from JSON).
func TestHTMLExposureMarkers(t *testing.T) {
	html := renderDemo(t)
	if !strings.Contains(html, "Exposed") {
		t.Error("report is missing the Exposed KPI label")
	}
	crm := only(t, decodeDataBlock(t, html), "legacy-crm")
	if crm.PubliclyAccessible == nil || !*crm.PubliclyAccessible {
		t.Errorf("legacy-crm publicly_accessible = %v, want true", crm.PubliclyAccessible)
	}
	if crm.Encrypted == nil || *crm.Encrypted {
		t.Errorf("legacy-crm encrypted = %v, want false", crm.Encrypted)
	}
	// The zero here is the whole point: legacy-crm reported 0 days of backup
	// retention, which is a finding, and the encoding must not turn it into the
	// absence of a reading.
	if got, ok := crm.Measure(model.MeasureBackupRetentionDays); !ok || got != 0 {
		t.Errorf("legacy-crm backup_retention_days = (%d, %v), want (0, true)", got, ok)
	}
}

// TestHTMLDataBlockShape reads both embedded blocks back and checks the shape
// end to end. Substring assertions elsewhere in this file cannot tell an
// attribute key from a core field, so this one parses.
func TestHTMLDataBlockShape(t *testing.T) {
	page := renderDemo(t)

	meta := metaBlock(t, page)
	if meta.Schema != model.SchemaVersion {
		t.Errorf("schema = %d, want %d", meta.Schema, model.SchemaVersion)
	}
	// The wire version guards the census encoding rather than the snapshot
	// schema, and the page refuses a block it does not recognise, so a mismatch
	// here is a report no browser would open.
	if meta.Wire != payloadWire {
		t.Errorf("wire = %d, want %d", meta.Wire, payloadWire)
	}

	resources := decodeDataBlock(t, page)
	for _, r := range resources {
		if r.Type == "" {
			t.Errorf("resource %q has no CloudFormation type", r.Name)
		}
		if r.ARN == "" {
			t.Errorf("resource %q lost its ARN in the census block", r.Name)
		}
	}

	// legacy-crm is the deliberately risky fixture: engine details live in the
	// attribute bag, and its zero backup retention is a stored measure — the
	// difference between "no backups" and "not reported".
	crm := only(t, resources, "legacy-crm")
	if got := crm.Attr(model.AttrEngine); got != "mysql" {
		t.Errorf("legacy-crm engine = %q, want mysql", got)
	}
	if got := crm.Attr(model.AttrEngineVersion); got != "5.7.44" {
		t.Errorf("legacy-crm engine_version = %q, want 5.7.44", got)
	}
	if got, ok := crm.Measure(model.MeasureBackupRetentionDays); !ok || got != 0 {
		t.Errorf("legacy-crm backup_retention_days = (%d, %v), want (0, true)", got, ok)
	}
	if !crm.Exposed() {
		t.Error("legacy-crm should read as exposed")
	}

	// A service that has no instances must not carry an instance class, and a
	// service that reports no storage must not carry a size.
	sessions := only(t, resources, "sessions")
	if _, ok := sessions.Attributes[model.AttrInstanceClass]; ok {
		t.Error("a DynamoDB table must not report an instance class")
	}
	users := only(t, resources, "users-aurora")
	if v, ok := users.Measure(model.MeasureSizeBytes); ok {
		t.Errorf("Aurora reports no allocated storage; got size_bytes = %d", v)
	}
}

// TestHTMLMetaBlockCarriesTheHeadline pins the split between the two blocks.
// Everything the page paints before the census arrives has to be in the small
// block, because once the census is gzipped it is decoded asynchronously and
// anything read from it paints late — or, in a browser too old to decode it,
// never.
func TestHTMLMetaBlockCarriesTheHeadline(t *testing.T) {
	snap := demoSnapshot("test")
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	meta := metaBlock(t, string(b))

	if meta.Summary.Total != len(snap.Resources) {
		t.Errorf("summary total = %d, want %d", meta.Summary.Total, len(snap.Resources))
	}
	if len(meta.Failures) != len(snap.Failures) {
		t.Errorf("meta carries %d failures, want %d", len(meta.Failures), len(snap.Failures))
	}
	if len(meta.Accounts) != len(snap.Accounts) || len(meta.Regions) != len(snap.Regions) {
		t.Errorf("meta scope = %d accounts / %d regions, want %d / %d",
			len(meta.Accounts), len(meta.Regions), len(snap.Accounts), len(snap.Regions))
	}
	if meta.Summary.Types == 0 || meta.Summary.Services == 0 {
		t.Error("summary reports no types or no services for a fixture that has both")
	}
	if len(meta.Summary.Platforms) == 0 {
		t.Error("summary carries no platform rows; the engines table would paint empty")
	}
}

// TestHTMLEnvironmentAndStatusTags checks the inventory tag pills ship in the
// shell: the CSS variants and the JS classifiers that color environment
// buckets and status lifecycle states. Rendering is client-side, so the test
// asserts the components exist rather than the painted rows.
func TestHTMLEnvironmentAndStatusTags(t *testing.T) {
	html := renderDemo(t)
	for _, needle := range []string{
		".tag-green", ".tag-amber", ".tag-red", ".tag-blue", ".tag-purple", ".tag-cyan",
		"function envClass", "function statusClass", "tag-dot",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("report is missing tag component marker %q", needle)
		}
	}
	// The demo fixture must keep exercising the semantic buckets: healthy,
	// stopped, paused, and storage-full statuses all appear in the census.
	shipped := map[string]bool{}
	for _, r := range decodeDataBlock(t, html) {
		shipped[r.Status] = true
	}
	for _, status := range []string{"available", "active", "stopped", "paused", "storage-full"} {
		if !shipped[status] {
			t.Errorf("demo data is missing a fixture with status %q", status)
		}
	}
}

// renderDemoCosts renders the demo snapshot with every cost tier attached.
func renderDemoCosts(t *testing.T) string {
	t.Helper()
	snap := demoCostSnapshot("test")
	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return string(b)
}

// The overlay is a large block of markup, CSS, and JS that only ever runs in a
// browser, so nothing else in the suite would notice if a section were dropped
// from the template. These are the anchors the rest of the report wires itself
// to: the ids the JS writes into, and the vocabulary that carries the honesty
// rulings — the em dash for an absence, the dagger for a lower bound, the
// coverage sentence. Losing any of them silently is losing the feature.
func TestHTMLCostOverlayShipsItsVocabulary(t *testing.T) {
	html := renderDemoCosts(t)
	for _, needle := range []string{
		// Structure the JS renders into.
		`id="cost-section"`, `id="cost-banner"`, `id="cost-hero"`,
		`id="cost-note"`, `id="cost-recon"`, `id="cost-recon-body"`,
		`id="spend-section"`, `id="spend-title"`, `id="spend-sub"`, `id="spend-rows"`,
		// The advice, which is its own top-level section rather than a panel
		// inside the spend one. Nesting it there would hide every suggestion on
		// a run that got no per-resource spend, which is the run they are most
		// worth reading on.
		`id="tips-section"`, `id="tips-sub"`, `id="tips-rows"`, `id="tips-note"`,
		// The overlay's own behaviour.
		"function methodBasis", "function renderCostHero", "function renderSpend",
		"function costCell", "function nullRank", "function coverageIssues",
		"_costSort",
		"function initTips", "function renderTips", "function tipAction",
		"function effortLabel",
		// Vocabulary the rulings depend on.
		"Untagged ", "carry no ", " figure", "modelled monthly rate",
		"is not the same as costing nothing",
		// The two sentences the whole section exists to keep apart. A saving is
		// a modelled monthly figure for a month that has not happened; spend is
		// what AWS billed. Neither is the other's arithmetic.
		"Ways to cut this bill", "Ranked by modelled monthly saving",
		"not an amount anyone was charged",
		"nothing here is added to or subtracted from the spend above",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("cost overlay is missing %q", needle)
		}
	}
	// A count of resources the active source did not price is not a count of
	// resources AWS said nothing about: the other source may have priced them,
	// or the figure may be in another currency and printed on the row itself.
	// The report said the former for a while, so the phrasing is barred rather
	// than merely corrected.
	for _, claim := range []string{
		"have no cost reported by AWS", "no cost reported by AWS",
	} {
		if strings.Contains(html, claim) {
			t.Errorf("report states a method-scoped gap as a fact about AWS: %q", claim)
		}
	}
}

// The reconciliation panel is the audit trail for the cost passes: which
// services were probed and what came back, the windows each pass asked over,
// and "What asking cost" — the Cost Explorer request meter, which matters
// because AWS bills per request. #cost-section hides itself when the hero drew
// no tiles, and `[hidden] { display: none !important; }` takes every descendant
// with it. That is precisely the run the panel exists for: cost was asked for,
// AWS was paid for the asking, and nothing came back. Nested, the one run the
// reader was billed for was the one run that never said what they paid.
func TestHTMLCostReconciliationSurvivesAnEmptyCostSection(t *testing.T) {
	html := renderDemoCosts(t)

	const anchor = `id="cost-section"`
	i := strings.Index(html, anchor)
	if i < 0 {
		t.Fatalf("no %s in the report", anchor)
	}
	end := strings.Index(html[i:], "</section>")
	if end < 0 {
		t.Fatal("the cost section is never closed")
	}
	if body := html[i : i+end]; strings.Contains(body, "cost-recon") {
		t.Error("the reconciliation panel is inside the section that hides itself when there are no figures")
	}

	// It needs a container of its own to hide, or moving it out just makes it
	// permanent furniture.
	for _, needle := range []string{
		`id="cost-audit-section"`,
		`document.getElementById("cost-audit-section").hidden = !any;`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("the reconciliation panel has no emptiness test of its own: missing %q", needle)
		}
	}
	if strings.Contains(html, `document.getElementById("cost-recon").hidden`) {
		t.Error("two nested hidden flags govern one panel; the section owns it now")
	}
}

// The overlay added markup, styles, and a second render path. The offline
// promise is asserted for the plain fixture in TestHTMLReportSelfContained;
// this repeats it for the cost-bearing one, because a report is only offline if
// every report is.
func TestHTMLCostReportStaysOffline(t *testing.T) {
	html := renderDemoCosts(t)
	for _, needle := range []string{
		`src="http`, `src='http`,
		`href='http`,
		`url(http`, "@import",
		"<link", "integrity=",
	} {
		if strings.Contains(html, needle) {
			t.Errorf("cost report contains external-load marker %q — must render fully offline", needle)
		}
	}
	if !strings.Contains(html, `<script type="application/json" id="blueprint-data">`) {
		t.Error("cost report does not contain the embedded JSON data block")
	}
}

// The report ranks resources by what they cost and must never turn that into a
// savings claim. Amortized cost spreads a commitment across the resources it
// covered, so deleting a covered row returns nothing — the commitment is
// already bought. A census that cannot see rate plans or what else would move
// onto the same commitment cannot know what deleting anything saves, and this
// pins that it never says otherwise.
func TestHTMLNoSavingsIfDeletedClaim(t *testing.T) {
	html := renderDemoCosts(t)
	for _, claim := range []string{
		"savings if deleted", "save by deleting", "you could save", "potential savings",
		"estimated savings", "would save", "saves you",
	} {
		if strings.Contains(strings.ToLower(html), claim) {
			t.Errorf("report makes a savings claim it cannot support: %q", claim)
		}
	}
	// And says so, rather than merely omitting it.
	for _, disclosure := range []string{
		"does not estimate what deleting a resource returns",
		"deleting a covered row does not return the amount shown",
	} {
		if !strings.Contains(html, disclosure) {
			t.Errorf("report is missing the commitment disclosure %q", disclosure)
		}
	}
}

// shortWriter fails after letting the first budget bytes through, either by
// returning an error or — the nastier case — by returning a short count and
// claiming success.
type shortWriter struct {
	budget int
	err    error // nil means report the short count with no error
	got    []byte
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if s.budget <= 0 {
		return 0, s.err
	}
	n := min(len(p), s.budget)
	s.got = append(s.got, p[:n]...)
	s.budget -= n
	if n < len(p) {
		return n, s.err
	}
	return n, nil
}

// The count htmlSafeWriter returns on failure is the prefix of the input the
// caller can stop worrying about. Returning zero instead would tell an upstream
// encoder that nothing was written when most of a census may already be in the
// file, and the natural response to that — write it again — would corrupt the
// block rather than abandon it.
func TestHTMLSafeWriterReportsInputBytesConsumedOnFailure(t *testing.T) {
	boom := errors.New("disk full")
	// "ab<cd" is five input bytes: two before the '<', the '<' itself at index
	// 2 (six bytes once escaped), then two more.
	const in = "ab<cd"

	cases := []struct {
		name    string
		budget  int // bytes the downstream writer accepts before it fails
		err     error
		want    int
		wantErr error
	}{
		{"fails on the first segment", 1, boom, 1, boom},
		{"fails at the segment boundary", 0, boom, 0, boom},
		{"fails on the escape", 2, boom, 2, boom},
		{"fails part way through the escape", 4, boom, 2, boom},
		{"fails on the trailing segment", 8, boom, 3, boom},
		// A writer that reports a short count with no error has dropped output
		// silently; the escaper must name that rather than pass the lie up.
		{"short write with no error", 1, nil, 1, io.ErrShortWrite},
		{"everything lands", 64, boom, len(in), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw := &shortWriter{budget: tc.budget, err: tc.err}
			n, err := (&htmlSafeWriter{w: sw}).Write([]byte(in))

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if n != tc.want {
				t.Errorf("consumed %d bytes of %q, want %d", n, in, tc.want)
			}
			if n < 0 || n > len(in) {
				t.Errorf("consumed count %d is outside 0..%d, which io.Writer forbids", n, len(in))
			}
			// Whatever the count claims was consumed must actually have gone
			// downstream, escapes and all — a count ahead of the bytes is the
			// same lie in the other direction. The reverse does not have to
			// hold: half an escape can be downstream while the '<' it stands
			// for is still uncounted, which is the whole reason the escape
			// case stops the count short.
			if want := strings.ReplaceAll(in[:n], "<", jsonLessThan); !strings.HasPrefix(string(sw.got), want) {
				t.Errorf("count claims %q was written, but downstream only received %q", want, sw.got)
			}
		})
	}
}
