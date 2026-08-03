package render

import (
	"encoding/json"
	"fmt"
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
