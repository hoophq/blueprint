package render

import (
	"encoding/json"
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

func TestHTMLMissingMarkerErrors(t *testing.T) {
	orig := reportTemplate
	defer func() { reportTemplate = orig }()
	reportTemplate = "<html><body>doctored template with no marker</body></html>"

	path := filepath.Join(t.TempDir(), "report.html")
	err := HTML(&model.Snapshot{Version: "test"}, path)
	if err == nil {
		t.Fatal("HTML() succeeded with a template missing the data marker, want error")
	}
	if !strings.Contains(err.Error(), dataMarker) {
		t.Errorf("error %q does not mention the missing marker %q", err, dataMarker)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("report file should not be written when the marker is missing (stat err = %v)", statErr)
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
	if !strings.Contains(html, `"name":"orders-prod"`) {
		t.Error("expected fixture database orders-prod in the JSON block")
	}
	if !strings.Contains(html, `"owner":"payments"`) {
		t.Error("expected orders-prod's derived owner in the JSON block")
	}
}

// TestHTMLEOLMarkers checks the EOL feature ships end to end: the KPI label
// in the shell and derived eol fields in the embedded demo data (legacy-crm
// runs mysql 5.7.44, EOL upstream since 2023-10-31).
func TestHTMLEOLMarkers(t *testing.T) {
	html := renderDemo(t)
	for _, needle := range []string{
		"End-of-life",
		`"eol":true`,
		`"eol_date":"2023-10-31"`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("report is missing EOL marker %q", needle)
		}
	}
}

// TestHTMLExposureMarkers checks the exposure feature ships end to end: the
// KPI label plus the deliberately risky legacy-crm fixture (public,
// unencrypted, no backups) present in the embedded data with explicit risky
// values, and the tri-state contract (nil fields absent from JSON).
func TestHTMLExposureMarkers(t *testing.T) {
	html := renderDemo(t)
	for _, needle := range []string{
		"Exposed",
		`"publicly_accessible":true`,
		`"encrypted":false`,
		`"backup_retention_days":0`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("report is missing exposure marker %q", needle)
		}
	}
}

// TestHTMLDataBlockShape reads the embedded JSON back as a snapshot and checks
// the schema v2 shape end to end. Substring assertions elsewhere in this file
// cannot tell an attribute key from a core field, so this one parses.
func TestHTMLDataBlockShape(t *testing.T) {
	data := embeddedJSON(t, renderDemo(t))
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("embedded data block is not valid JSON: %v", err)
	}

	if snap.Schema != model.SchemaVersion {
		t.Errorf("schema = %d, want %d", snap.Schema, model.SchemaVersion)
	}
	byName := map[string]model.Resource{}
	for _, r := range snap.Resources {
		byName[r.Name] = r
		if r.Type == "" {
			t.Errorf("resource %q has no CloudFormation type", r.Name)
		}
	}

	// legacy-crm is the deliberately risky fixture: engine details live in the
	// attribute bag, and its zero backup retention is a stored measure — the
	// difference between "no backups" and "not reported".
	crm, ok := byName["legacy-crm"]
	if !ok {
		t.Fatal("fixture legacy-crm missing from the data block")
	}
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
	sessions, ok := byName["sessions"]
	if !ok {
		t.Fatal("fixture sessions missing from the data block")
	}
	if _, ok := sessions.Attributes[model.AttrInstanceClass]; ok {
		t.Error("a DynamoDB table must not report an instance class")
	}
	users, ok := byName["users-aurora"]
	if !ok {
		t.Fatal("fixture users-aurora missing from the data block")
	}
	if v, ok := users.Measure(model.MeasureSizeBytes); ok {
		t.Errorf("Aurora reports no allocated storage; got size_bytes = %d", v)
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
	// stopped, paused, and storage-full statuses all appear in the data block.
	for _, status := range []string{
		`"status":"available"`, `"status":"active"`,
		`"status":"stopped"`, `"status":"paused"`, `"status":"storage-full"`,
	} {
		if !strings.Contains(html, status) {
			t.Errorf("demo data is missing a fixture with %s", status)
		}
	}
}
