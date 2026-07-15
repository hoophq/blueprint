package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hoophq/dbcensus/internal/demo"
	"github.com/hoophq/dbcensus/internal/model"
)

// renderDemo renders the demo snapshot to a temp file and returns the HTML.
func renderDemo(t *testing.T) string {
	t.Helper()
	snap := demo.Snapshot("test")
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
	snap := demo.Snapshot("test")
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

	if !strings.Contains(html, `<script type="application/json" id="dbcensus-data">`) {
		t.Error("report does not contain the embedded JSON data block")
	}
	if strings.Contains(html, dataMarker) {
		t.Error("data marker was not replaced with snapshot JSON")
	}
}

func TestHTMLEscapesScriptBreakout(t *testing.T) {
	snap := &model.Snapshot{
		Version:  "test",
		Accounts: []string{"111111111111"},
		Regions:  []string{"us-east-1"},
		Resources: []model.Resource{{
			ARN:       "arn:aws:rds:us-east-1:111111111111:db:evil",
			Service:   model.ServiceRDS,
			Kind:      "instance",
			Name:      "evil</script><script>alert(1)</script>",
			Engine:    "postgres",
			Region:    "us-east-1",
			AccountID: "111111111111",
			Tags:      map[string]string{"owner": "</script>"},
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

	if strings.Contains(html, "evil</script>") {
		t.Error("resource name broke out of the JSON script block")
	}
	// json.Marshal unicode-escapes angle brackets inside strings; the exact
	// escaped form of the hostile name must appear in the embedded block.
	nameJSON, err := json.Marshal(snap.Resources[0].Name)
	if err != nil {
		t.Fatalf("marshal name: %v", err)
	}
	escaped := strings.Trim(string(nameJSON), `"`)
	if strings.Contains(escaped, "</") {
		t.Fatal("sanity: expected json.Marshal to escape the angle brackets")
	}
	if !strings.Contains(html, escaped) {
		t.Error("expected the hostile resource name to appear unicode-escaped in the JSON block")
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
			Kind:      "instance",
			Name:      hostile,
			Engine:    "postgres",
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
	if !strings.Contains(string(b), "No databases found") {
		t.Error("report is missing the empty-state hint")
	}
}

// TestHTMLNavigationLinksAllowlisted verifies the only external hrefs in the
// report are the two navigation anchors (hoop.dev and the GitHub repo).
// Anything else would break the offline promise.
func TestHTMLNavigationLinksAllowlisted(t *testing.T) {
	html := renderDemo(t)
	allowed := map[string]bool{
		"https://hoop.dev":                   true,
		"https://github.com/hoophq/dbcensus": true,
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

// TestHTMLBrandAndAttribution checks the redesigned shell: the hoop logo mark,
// the attribution-tier vocabulary, and that a fully attributed fixture
// database ships in the data block for the tier computation to classify.
func TestHTMLBrandAndAttribution(t *testing.T) {
	html := renderDemo(t)
	for _, needle := range []string{
		"M96.4167 71.4077", // first path of the hoop logo mark
		"hoop.dev",
		"Database Census Report",
		"Attribution Score",
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
