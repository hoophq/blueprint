package render

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoophq/dbcensus/internal/demo"
	"github.com/hoophq/dbcensus/internal/model"
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

func TestCSVDemoSnapshot(t *testing.T) {
	snap := demo.Snapshot("test")
	records := renderAndParse(t, snap)

	if got, want := len(records), len(snap.Resources)+1; got != want {
		t.Fatalf("row count = %d, want %d (resources + header)", got, want)
	}

	wantHeader := []string{
		"arn", "service", "kind", "name", "engine", "engine_version",
		"instance_class", "storage_gb", "multi_az", "status", "endpoint",
		"region", "account_id", "created_at", "environment", "owner", "tags",
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
		"service":        "rds",
		"kind":           "instance",
		"name":           "orders-prod",
		"engine":         "postgres",
		"engine_version": "15.4",
		"instance_class": "db.r6g.xlarge",
		"storage_gb":     "500",
		"multi_az":       "true",
		"status":         "available",
		"endpoint":       "orders-prod.c9k2hxu3qapb.us-east-1.rds.amazonaws.com",
		"region":         "us-east-1",
		"account_id":     "111111111111",
		"created_at":     "2019-03-14T10:30:00Z",
		"environment":    "production",
		"owner":          "payments",
		"tags":           "app=orders;environment=production;owner=payments",
	}
	for name, want := range checks {
		if got := row[c[name]]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := time.Parse(time.RFC3339, row[c["created_at"]]); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", row[c["created_at"]], err)
	}

	// Every non-empty created_at cell must be RFC3339; tags cells with
	// content must be sorted k=v;k=v pairs (spot-checked above).
	for i, r := range records[1:] {
		if v := r[c["created_at"]]; v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				t.Errorf("row %d created_at %q is not RFC3339", i+1, v)
			}
		}
	}
}

func TestCSVEdgeCases(t *testing.T) {
	created := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	snap := &model.Snapshot{
		Resources: []model.Resource{
			{
				ARN:       "arn:aws:rds:us-east-1:111111111111:db:bare",
				Service:   model.ServiceRDS,
				Kind:      "instance",
				Name:      "bare",
				Engine:    "postgres",
				Region:    "us-east-1",
				AccountID: "111111111111",
				// nil CreatedAt, nil Tags
			},
			{
				ARN:       "arn:aws:dynamodb:us-east-1:111111111111:table/quoted",
				Service:   model.ServiceDynamoDB,
				Kind:      "table",
				Name:      "quoted",
				Engine:    "dynamodb",
				Region:    "us-east-1",
				AccountID: "111111111111",
				CreatedAt: &created,
				Tags: map[string]string{
					"owner":       "data",
					"description": "has, comma and \"quotes\"",
				},
			},
		},
	}
	records := renderAndParse(t, snap)
	if got, want := len(records), 3; got != want {
		t.Fatalf("row count = %d, want %d", got, want)
	}
	c := col(t, records[0])

	bare := records[1]
	if got := bare[c["created_at"]]; got != "" {
		t.Errorf("nil CreatedAt rendered as %q, want empty", got)
	}
	if got := bare[c["tags"]]; got != "" {
		t.Errorf("nil Tags rendered as %q, want empty", got)
	}
	if got := bare[c["multi_az"]]; got != "false" {
		t.Errorf("multi_az = %q, want %q", got, "false")
	}
	if got := bare[c["storage_gb"]]; got != "0" {
		t.Errorf("storage_gb = %q, want %q", got, "0")
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
}

func TestCSVFormulaGuard(t *testing.T) {
	snap := &model.Snapshot{
		Resources: []model.Resource{{
			ARN:         "arn:aws:rds:us-east-1:111111111111:db:hostile",
			Service:     model.ServiceRDS,
			Kind:        "instance",
			Name:        `=HYPERLINK("http://evil.example/","click")`,
			Engine:      "postgres",
			StorageGB:   -1, // pre-formatted numeric column must NOT be guarded
			Region:      "us-east-1",
			AccountID:   "111111111111",
			Environment: "@SUM(A1:A9)",
			Owner:       "-2+3+cmd|' /C calc'!A0",
			Tags: map[string]string{
				"-lead": "dash key",
				"note":  `=HYPERLINK("http://evil.example/")`,
			},
		}},
	}
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
	// Numeric columns are formatted locally from typed fields; a legitimate
	// negative number must survive without the quote prefix.
	if got := row[c["storage_gb"]]; got != "-1" {
		t.Errorf("storage_gb = %q, want %q (no formula guard on numeric columns)", got, "-1")
	}
	// Benign strings are untouched.
	if got := row[c["engine"]]; got != "postgres" {
		t.Errorf("engine = %q, want %q", got, "postgres")
	}
}

func TestCSVTagEncoding(t *testing.T) {
	snap := &model.Snapshot{
		Resources: []model.Resource{{
			ARN:       "arn:aws:rds:us-east-1:111111111111:db:tagged",
			Service:   model.ServiceRDS,
			Kind:      "instance",
			Name:      "tagged",
			Engine:    "postgres",
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
