package render

import (
	"encoding/csv"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoophq/blueprint/internal/demo"
	"github.com/hoophq/blueprint/internal/model"
)

func renderAndParse(t *testing.T, snap *model.Snapshot) [][]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.csv")
	if err := CSV(snap, path); err != nil {
		t.Fatalf("CSV() error: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open rendered csv: %v", err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse rendered csv: %v", err)
	}
	return records
}

// col maps header names to indexes so tests don't hardcode positions.
func col(t *testing.T, header []string) map[string]int {
	t.Helper()
	m := make(map[string]int, len(header))
	for i, h := range header {
		m[h] = i
	}
	return m
}

// splitPairs reverses the k=v;k=v encoding used by the tags and attributes
// cells. Decoding with the standard percent-decoder (rather than a bespoke
// inverse of tagEscaper) is the point: it proves the encoding a consumer would
// reach for round-trips, so a spreadsheet or script can recover the exact key
// and value AWS reported. PathUnescape, not QueryUnescape — the latter would
// turn a literal '+' in a tag value into a space.
func splitPairs(t *testing.T, cell string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if cell == "" {
		return out
	}
	for pair := range strings.SplitSeq(cell, ";") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("pair %q in cell %q has no separator", pair, cell)
		}
		dk, err := url.PathUnescape(k)
		if err != nil {
			t.Fatalf("undecodable key %q: %v", k, err)
		}
		dv, err := url.PathUnescape(v)
		if err != nil {
			t.Fatalf("undecodable value %q: %v", v, err)
		}
		out[dk] = dv
	}
	return out
}

func TestCSVDemoSnapshot(t *testing.T) {
	snap := demo.Snapshot("test")
	records := renderAndParse(t, snap)

	if got, want := len(records), len(snap.Resources)+1; got != want {
		t.Fatalf("row count = %d, want %d (resources + header)", got, want)
	}

	// The columns are the narrow core only. Everything service-specific rides
	// in the attributes cell, so this header must not grow when a new scanner
	// lands — that stability is the contract downstream scripts depend on.
	wantHeader := []string{
		"arn", "service", "type", "name", "status", "region", "account_id",
		"created_at", "environment", "owner", "tags", "eol", "eol_date",
		"publicly_accessible", "encrypted", "attributes",
	}
	if len(records[0]) != len(wantHeader) {
		t.Fatalf("header has %d columns, want %d", len(records[0]), len(wantHeader))
	}
	for i, h := range wantHeader {
		if records[0][i] != h {
			t.Errorf("header[%d] = %q, want %q", i, records[0][i], h)
		}
	}
	c := col(t, records[0])

	// Known cell values: the orders-prod RDS instance.
	const arn = "arn:aws:rds:us-east-1:111111111111:db:orders-prod"
	var row []string
	for _, r := range records[1:] {
		if r[c["arn"]] == arn {
			row = r
			break
		}
	}
	if row == nil {
		t.Fatalf("no row with arn %q", arn)
	}
	checks := map[string]string{
		"service":             "rds",
		"type":                model.TypeRDSInstance,
		"name":                "orders-prod",
		"status":              "available",
		"region":              "us-east-1",
		"account_id":          "111111111111",
		"created_at":          "2019-03-14T10:30:00Z",
		"environment":         "production",
		"owner":               "payments",
		"tags":                "app=orders;environment=production;owner=payments",
		"eol":                 "false", // postgres 15.4 is in support
		"eol_date":            "",
		"publicly_accessible": "false",
		"encrypted":           "true",
	}
	for name, want := range checks {
		if got := row[c[name]]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := time.Parse(time.RFC3339, row[c["created_at"]]); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", row[c["created_at"]], err)
	}

	// Attributes and measures share one cell and are keyed by AWS's own field
	// names. Storage is exact bytes, not a rounded GB figure, so sizes stay
	// comparable across services.
	attrs := splitPairs(t, row[c["attributes"]])
	wantAttrs := map[string]string{
		"engine":                "postgres",
		"engine_version":        "15.4",
		"instance_class":        "db.r6g.xlarge",
		"endpoint":              "orders-prod.c9k2hxu3qapb.us-east-1.rds.amazonaws.com",
		"multi_az":              "true",
		"size_bytes":            "536870912000", // 500 GiB
		"backup_retention_days": "7",
	}
	for k, want := range wantAttrs {
		if got, ok := attrs[k]; !ok || got != want {
			t.Errorf("attributes[%q] = (%q, %v), want (%q, true)", k, got, ok, want)
		}
	}

	// Every non-empty created_at cell must be RFC3339, and every attributes
	// cell must be decodable — a cell no reader can split is worse than an
	// empty one.
	for i, r := range records[1:] {
		if v := r[c["created_at"]]; v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				t.Errorf("row %d created_at %q is not RFC3339", i+1, v)
			}
		}
		splitPairs(t, r[c["attributes"]])
	}
}

func TestCSVEdgeCases(t *testing.T) {
	created := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	bareRes := model.Resource{
		ARN:       "arn:aws:rds:us-east-1:111111111111:db:bare",
		Service:   model.ServiceRDS,
		Type:      model.TypeRDSInstance,
		Name:      "bare",
		Region:    "us-east-1",
		AccountID: "111111111111",
		// nil CreatedAt, nil Tags, nothing in the bag
	}
	emptyTable := model.Resource{
		ARN:       "arn:aws:dynamodb:us-east-1:111111111111:table/quoted",
		Service:   model.ServiceDynamoDB,
		Type:      model.TypeDynamoDBTable,
		Name:      "quoted",
		Region:    "us-east-1",
		AccountID: "111111111111",
		CreatedAt: &created,
		Tags: map[string]string{
			"owner":       "data",
			"description": "has, comma and \"quotes\"",
		},
	}
	emptyTable.SetAttr(model.AttrEngine, "dynamodb")
	emptyTable.SetMeasure(model.MeasureSizeBytes, 0) // a genuinely empty table

	snap := &model.Snapshot{Resources: []model.Resource{bareRes, emptyTable}}
	records := renderAndParse(t, snap)
	if got, want := len(records), 3; got != want {
		t.Fatalf("row count = %d, want %d", got, want)
	}
	c := col(t, records[0])

	bare := records[1]
	if got := bare[c["created_at"]]; got != "" {
		t.Errorf("nil CreatedAt rendered as %q, want empty", got)
	}
	// Tri-state core fields: nil pointers render as empty cells, not as false,
	// so "not reported" stays distinguishable in spreadsheets too.
	for _, name := range []string{"publicly_accessible", "encrypted"} {
		if got := bare[c[name]]; got != "" {
			t.Errorf("nil %s rendered as %q, want empty", name, got)
		}
	}
	if got := bare[c["tags"]]; got != "" {
		t.Errorf("nil Tags rendered as %q, want empty", got)
	}
	// A resource that reported nothing service-specific gets an empty cell —
	// not a row of zeroes claiming a 0-byte, no-backup database.
	if got := bare[c["attributes"]]; got != "" {
		t.Errorf("empty bag rendered as %q, want empty", got)
	}

	quoted := records[2]
	// Keys sorted: description before owner; commas/quotes must survive the
	// encoding/csv round trip intact.
	wantTags := "description=has, comma and \"quotes\";owner=data"
	if got := quoted[c["tags"]]; got != wantTags {
		t.Errorf("tags = %q, want %q", got, wantTags)
	}
	if got := quoted[c["created_at"]]; got != "2024-06-01T00:00:00Z" {
		t.Errorf("created_at = %q, want %q", got, "2024-06-01T00:00:00Z")
	}
	// A stored zero is a real finding and must render, unlike an absent key.
	attrs := splitPairs(t, quoted[c["attributes"]])
	if got, ok := attrs["size_bytes"]; !ok || got != "0" {
		t.Errorf("size_bytes = (%q, %v), want (\"0\", true)", got, ok)
	}
	if _, ok := attrs["backup_retention_days"]; ok {
		t.Error("unreported backup_retention_days must stay out of the cell")
	}
}

func TestCSVFormulaGuard(t *testing.T) {
	hostile := model.Resource{
		ARN:         "arn:aws:rds:us-east-1:111111111111:db:hostile",
		Service:     model.ServiceRDS,
		Type:        model.TypeRDSInstance,
		Name:        `=HYPERLINK("http://evil.example/","click")`,
		Region:      "us-east-1",
		AccountID:   "111111111111",
		Environment: "@SUM(A1:A9)",
		Owner:       "-2+3+cmd|' /C calc'!A0",
		Tags: map[string]string{
			"-lead": "dash key",
			"note":  `=HYPERLINK("http://evil.example/")`,
		},
	}
	hostile.SetAttr(model.AttrEngine, "postgres")
	hostile.SetAttr(model.AttrEndpoint, `=cmd|' /C calc'!A0`)

	snap := &model.Snapshot{Resources: []model.Resource{hostile}}
	records := renderAndParse(t, snap)
	c := col(t, records[0])
	row := records[1]

	// Cells starting with a formula trigger come back with the OWASP
	// single-quote prefix after the encoding/csv round trip.
	if got, want := row[c["name"]], `'=HYPERLINK("http://evil.example/","click")`; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := row[c["environment"]], "'@SUM(A1:A9)"; got != want {
		t.Errorf("environment = %q, want %q", got, want)
	}
	if got, want := row[c["owner"]], "'-2+3+cmd|' /C calc'!A0"; got != want {
		t.Errorf("owner = %q, want %q", got, want)
	}
	// The hostile =HYPERLINK tag value is %3D-encoded by joinTags, and the
	// leading "-lead" key puts a formula trigger at the start of the tags
	// cell, so the whole cell carries the quote prefix.
	wantTags := `'-lead=dash key;note=%3DHYPERLINK("http://evil.example/")`
	if got := row[c["tags"]]; got != wantTags {
		t.Errorf("tags = %q, want %q", got, wantTags)
	}
	// A hostile attribute value gets the same treatment: its '=' is encoded,
	// so it can neither forge a key/value separator nor reach the spreadsheet
	// as a formula. The cell itself starts with a key we control, so it needs
	// no quote prefix.
	wantAttrs := `endpoint=%3Dcmd|' /C calc'!A0;engine=postgres`
	if got := row[c["attributes"]]; got != wantAttrs {
		t.Errorf("attributes = %q, want %q", got, wantAttrs)
	}
	if got := splitPairs(t, row[c["attributes"]])["endpoint"]; got != `=cmd|' /C calc'!A0` {
		t.Errorf("decoded endpoint = %q, want the original value back", got)
	}
	// Benign core columns are untouched.
	if got := row[c["service"]]; got != "rds" {
		t.Errorf("service = %q, want %q", got, "rds")
	}
}

func TestCSVTagEncoding(t *testing.T) {
	snap := &model.Snapshot{
		Resources: []model.Resource{{
			ARN:       "arn:aws:rds:us-east-1:111111111111:db:tagged",
			Service:   model.ServiceRDS,
			Type:      model.TypeRDSInstance,
			Name:      "tagged",
			Region:    "us-east-1",
			AccountID: "111111111111",
			Tags: map[string]string{
				"a=b":     "c;d",
				"percent": "100%",
				"plain":   "value",
			},
		}},
	}
	records := renderAndParse(t, snap)
	c := col(t, records[0])
	// '%', '=', and ';' are percent-encoded inside keys and values, so the
	// literal '=' and ';' separators in the joined string are unambiguous
	// and the encoding is reversible.
	want := "a%3Db=c%3Bd;percent=100%25;plain=value"
	if got := records[1][c["tags"]]; got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}
	if got := splitPairs(t, records[1][c["tags"]])["a=b"]; got != "c;d" {
		t.Errorf("decoded tag = %q, want %q", got, "c;d")
	}
}

func TestCSVAtomicWrite(t *testing.T) {
	snap := demo.Snapshot("test")
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	if err := CSV(snap, path); err != nil {
		t.Fatalf("CSV() error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("output file missing after successful render: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file %q left behind after successful render (stat err = %v)", path+".tmp", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.csv" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only [out.csv]", names)
	}

	// A failed rename (target path is a directory) must not leave the temp
	// file behind either.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := CSV(snap, blocked); err == nil {
		t.Error("CSV() succeeded writing onto a directory, want error")
	}
	if _, err := os.Stat(blocked + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file %q left behind after failed rename (stat err = %v)", blocked+".tmp", err)
	}
}
