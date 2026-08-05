package render

import (
	"encoding/csv"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	snap := demoSnapshot("test")
	records := renderAndParse(t, snap)

	if got, want := len(records), len(snap.Resources)+1; got != want {
		t.Fatalf("row count = %d, want %d (resources + header)", got, want)
	}

	// The columns are the narrow core, then the flattened bag, then cost, then
	// the hub's advice. Everything service-specific rides in the attributes
	// cell, so this header must not grow when a new scanner lands — that
	// stability is the contract downstream scripts depend on. Cost is not
	// service-specific: it is one block of columns per attribution source for
	// every service rather than one per service, and it sits last so every
	// column above keeps the index it has always had. The header is spelled out
	// literally rather than rebuilt from model.CostMethods() — a generated
	// expectation would rename these columns in lockstep with the code and never
	// notice the contract had broken.
	//
	// The tip columns share no prefix with the cost block on purpose. A modelled
	// monthly saving is not spend, and two adjacent ranges a spreadsheet would
	// happily subtract is exactly the arithmetic the naming is there to
	// discourage.
	wantHeader := []string{
		"arn", "service", "type", "name", "status", "region", "account_id",
		"created_at", "environment", "owner", "tags", "eol", "eol_date",
		"publicly_accessible", "encrypted", "attributes",
		"cost_ce_amount", "cost_ce_currency", "cost_ce_estimated",
		"cost_ce_observed_from", "cost_ce_observed_to", "cost_ce_caveats",
		"cost_ce_match_key",
		"cost_unavailable_reason",
		"tip_action", "tip_savings", "tip_currency", "tip_effort",
		"tip_restart", "tip_count",
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

// Timestamp cells go through the same formula guard as every other string
// column. Nearly every timestamp opens with a four-digit year and the guard
// does nothing — but Go renders a negative year as "-0001-…", which opens with
// a character a spreadsheet reads as arithmetic, so the guard is a real code
// path and not a formality. This pins both halves: the odd year is quoted, and
// an ordinary one is passed through byte for byte.
func TestCSVTimeCellsAreGuarded(t *testing.T) {
	odd := time.Date(-1, 3, 4, 5, 6, 7, 0, time.UTC)
	ordinary := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	hostile := model.Resource{
		ARN:       "arn:aws:rds:us-east-1:111111111111:db:odd",
		Service:   model.ServiceRDS,
		Type:      model.TypeRDSInstance,
		Name:      "odd",
		Region:    "us-east-1",
		AccountID: "111111111111",
		CreatedAt: &odd,
	}
	hostile.AddCost(model.ResourceCost{
		Amount: "1.00", Currency: "USD", Method: model.CostMethodCE,
		ObservedFrom: &odd, ObservedTo: &ordinary,
	})

	records := renderAndParse(t, &model.Snapshot{Resources: []model.Resource{hostile}})
	c := col(t, records[0])
	row := records[1]

	for _, tc := range []struct{ column, want string }{
		{"created_at", "'-0001-03-04T05:06:07Z"},
		{"cost_ce_observed_from", "'-0001-03-04T05:06:07Z"},
		{"cost_ce_observed_to", "2026-03-04T05:06:07Z"},
	} {
		if got := row[c[tc.column]]; got != tc.want {
			t.Errorf("%s = %q, want %q", tc.column, got, tc.want)
		}
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

// The cost columns carry the same absence rules as the rest of the row, with
// one extra hazard: a spreadsheet's SUM and AVERAGE skip blank cells and count
// zeros, so writing "0" where nothing was reported drags a user's own average
// down and tells them a resource is free.
func TestCSVCostCells(t *testing.T) {
	base := func(name string) model.Resource {
		return model.Resource{
			ARN: "arn:aws:rds:us-east-1:210987654321:db:" + name, Name: name,
			Service: model.ServiceRDS, Type: model.TypeRDSInstance,
			Region: "us-east-1", AccountID: "210987654321",
		}
	}
	from := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 14)

	full := base("full")
	full.AddCost(model.ResourceCost{
		Amount: "1620.00", Currency: "USD", Method: model.CostMethodCE, Estimated: true,
		ObservedFrom: &from, ObservedTo: &to, MatchKey: "full",
		// A caveat holding both structural characters proves the cell stays
		// splittable: neither may forge a separator.
		Caveats: []string{"covers storage only; not compute", "billed over usage=recent"},
	})

	// A source that priced this resource at nothing. Zero is a finding.
	zero := base("zero")
	zero.AddCost(model.ResourceCost{Amount: "0.00", Currency: "USD", Method: model.CostMethodCE})

	// A credit: negative money, and the reason the amount column is exempt
	// from the formula guard.
	credit := base("credit")
	credit.AddCost(model.ResourceCost{Amount: "-450.00", Currency: "USD", Method: model.CostMethodCE})

	// Looked at, nothing found — blank figures with the reason beside them.
	named := base("named")
	named.CostUnavailable = "the cost pass looked and AWS reported nothing for this resource"

	// Nobody looked: blank everywhere, including the reason.
	untouched := base("untouched")

	records := renderAndParse(t, &model.Snapshot{
		Resources: []model.Resource{full, zero, credit, named, untouched},
	})
	c := col(t, records[0])
	rows := map[string][]string{}
	for _, r := range records[1:] {
		rows[r[c["name"]]] = r
	}

	got := rows["full"]
	for name, want := range map[string]string{
		"cost_ce_amount":        "1620.00",
		"cost_ce_currency":      "USD",
		"cost_ce_estimated":     "true",
		"cost_ce_observed_from": "2026-06-15T12:00:00Z",
		"cost_ce_observed_to":   "2026-06-29T12:00:00Z",
		"cost_ce_match_key":     "full",
		// ';' inside a caveat is encoded so the joined cell stays splittable
		// on the literal ';'. '=' is encoded by the same replacer.
		"cost_ce_caveats":         "covers storage only%3B not compute;billed over usage%3Drecent",
		"cost_unavailable_reason": "",
	} {
		if got[c[name]] != want {
			t.Errorf("full.%s = %q, want %q", name, got[c[name]], want)
		}
	}

	// A reported zero renders as the figure the source gave, never as a blank.
	if got := rows["zero"][c["cost_ce_amount"]]; got != "0.00" {
		t.Errorf("reported zero amount = %q, want %q", got, "0.00")
	}
	// ...and it is priced, so it has no absence to explain.
	if got := rows["zero"][c["cost_unavailable_reason"]]; got != "" {
		t.Errorf("priced-at-zero resource carries a reason: %q", got)
	}
	// Estimated is written out even when false: "this is a real bill" is a
	// claim worth making explicitly rather than by omission.
	if got := rows["zero"][c["cost_ce_estimated"]]; got != "false" {
		t.Errorf("cost_ce_estimated = %q, want %q", got, "false")
	}

	// The amount column is deliberately not formula-guarded — a leading quote
	// would turn every credit into text a spreadsheet will not sum.
	if got := rows["credit"][c["cost_ce_amount"]]; got != "-450.00" {
		t.Errorf("credit amount = %q, want an unguarded %q", got, "-450.00")
	}

	// Looked and found nothing: every figure cell of every method blank, and a
	// reason.
	namedRow := rows["named"]
	for _, m := range model.CostMethods() {
		for _, name := range costColumns {
			if got := namedRow[c["cost_"+m+"_"+name]]; got != "" {
				t.Errorf("unpriced cost_%s_%s = %q, want empty", m, name, got)
			}
		}
	}
	if got := namedRow[c["cost_unavailable_reason"]]; got == "" {
		t.Error("a source looked and found nothing, but no reason was written")
	}

	// Nobody looked: even the reason is blank, because "not asked" is not a
	// coverage gap to report.
	if got := rows["untouched"][c["cost_unavailable_reason"]]; got != "" {
		t.Errorf("un-queried resource carries a reason: %q", got)
	}

	// Nobody was advised either, so every tip cell is blank on every row here —
	// tip_count included. A "0" would be the CSV claiming the hub looked, which
	// on a run where the stage never executed is a coverage statement this file
	// is in no position to make.
	for name, row := range rows {
		for _, tip := range tipColumns {
			if got := row[c[tip]]; got != "" {
				t.Errorf("un-advised %s.%s = %q, want empty", name, tip, got)
			}
		}
	}
}

// The case the two blocks exist to keep apart: one resource that Cost Explorer
// billed over a closed window and that Cost Optimization Hub has advice about
// for the month ahead. Both reach the row, in their own columns, and neither
// column is the other's.
//
// The numbers are chosen so a reader who mixed them up would produce something
// that looks plausible — 12.34 billed against a 40.00 saving is a negative bill
// — because the failure this pins is not a crash. It is a spreadsheet with two
// adjacent ranges and an obvious-looking subtraction between them.
func TestCSVCarriesSpendAndAdviceOnOneRow(t *testing.T) {
	from := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 14)

	r := model.Resource{
		ARN: "arn:aws:dynamodb:us-east-1:210987654321:table/orders", Name: "orders",
		Service: model.ServiceDynamoDB, Type: model.TypeDynamoDBTable,
		Region: "us-east-1", AccountID: "210987654321",
	}
	r.AddCost(model.ResourceCost{
		Amount: "12.34", Currency: "USD", Method: model.CostMethodCE,
		ObservedFrom: &from, ObservedTo: &to,
		// Cost Explorer's RESOURCE_ID is service-dependent, so the value the
		// join matched on is recorded verbatim rather than assumed to be an ARN.
		MatchKey: "orders",
	})
	r.AddRecommendation(model.Recommendation{
		ID: "rec-1", ActionType: "Rightsize",
		EstimatedMonthlySavings: "40.00", Currency: "USD",
		ImplementationEffort: "Medium", RestartNeeded: ptrTo(true),
	})

	records := renderAndParse(t, &model.Snapshot{Resources: []model.Resource{r}})
	c := col(t, records[0])
	row := records[1]

	for name, want := range map[string]string{
		"cost_ce_amount":        "12.34",
		"cost_ce_estimated":     "false",
		"cost_ce_observed_from": "2026-07-17T00:00:00Z",
		"cost_ce_observed_to":   "2026-07-31T00:00:00Z",
		"cost_ce_match_key":     "orders",
		"tip_action":            "Rightsize",
		"tip_savings":           "40.00",
		"tip_currency":          "USD",
		"tip_effort":            "Medium",
		"tip_restart":           "true",
		"tip_count":             "1",
		// Advice is not a price, so it explains no absence and fills no cost
		// cell. Nothing here reconciles the 40.00 against the 12.34.
		"cost_unavailable_reason": "",
	} {
		if got := row[c[name]]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// A resource can carry several suggestions — the hub models a database's
// compute and its storage separately — and a CSV row is one resource. The row
// reports the largest saving and says how many there were, which is the only
// summary that does not silently discard advice: tip_count tells the reader the
// row is a summary and the JSON has the rest.
func TestCSVTipRowReportsTheLargestSaving(t *testing.T) {
	r := model.Resource{
		ARN: "arn:aws:rds:us-east-1:210987654321:db:orders", Name: "orders",
		Service: model.ServiceRDS, Type: model.TypeRDSInstance,
		Region: "us-east-1", AccountID: "210987654321",
	}
	// Deliberately out of amount order, and the largest is neither first nor
	// last: a row that reported r.Recommendations[0] would pass a two-element
	// fixture by luck.
	for _, rec := range []model.Recommendation{
		{ID: "a", ActionType: "Rightsize", EstimatedMonthlySavings: "40.00", Currency: "USD"},
		{ID: "b", ActionType: "Upgrade", EstimatedMonthlySavings: "310.50", Currency: "USD"},
		{ID: "c", ActionType: "Stop", EstimatedMonthlySavings: "9.99", Currency: "USD"},
	} {
		r.AddRecommendation(rec)
	}

	records := renderAndParse(t, &model.Snapshot{Resources: []model.Resource{r}})
	c := col(t, records[0])
	row := records[1]

	if got := row[c["tip_savings"]]; got != "310.50" {
		t.Errorf("tip_savings = %q, want the largest, %q", got, "310.50")
	}
	if got := row[c["tip_action"]]; got != "Upgrade" {
		t.Errorf("tip_action = %q, want the action of the largest saving", got)
	}
	if got := row[c["tip_count"]]; got != "3" {
		t.Errorf("tip_count = %q, want %q — the summary has to say what it summarizes", got, "3")
	}
}

// The picker itself, on the states the rendered row cannot distinguish between.
// Both are about what "largest" is allowed to mean: never a number that could
// not be read, and never a comparison across a currency boundary this tool has
// no rate for.
func TestHeadlineTipRefusesComparisonsItCannotMake(t *testing.T) {
	of := func(recs ...model.Recommendation) model.Resource {
		r := model.Resource{ARN: "arn:aws:rds:us-east-1:1:db:orders", Name: "orders"}
		for _, rec := range recs {
			r.AddRecommendation(rec)
		}
		return r
	}
	for _, tc := range []struct {
		name string
		res  model.Resource
		want string // the winning recommendation's id, or "" for none
	}{
		{
			name: "nothing to pick",
			res:  of(),
			want: "",
		},
		{
			// An amount that does not parse is not a large one. It loses to the
			// figure that can be read, and the row still counts it.
			name: "an unreadable amount never wins",
			res: of(
				model.Recommendation{ID: "junk", EstimatedMonthlySavings: "a lot", Currency: "USD"},
				model.Recommendation{ID: "real", EstimatedMonthlySavings: "1.00", Currency: "USD"},
			),
			want: "real",
		},
		{
			// Every amount unreadable: there is no ranking to be had, so the row
			// falls back to the first in the census's own order rather than
			// reporting nothing at all.
			name: "nothing readable falls back to the first",
			res: of(
				model.Recommendation{ID: "one", EstimatedMonthlySavings: "lots", Currency: "USD"},
				model.Recommendation{ID: "two", EstimatedMonthlySavings: "loads", Currency: "USD"},
			),
			want: "one",
		},
		{
			// A suggestion AWS never priced has no currency either, and it must
			// not be read as a second currency — that would void the ranking on
			// the most ordinary row the hub produces.
			name: "an unpriced suggestion is not a second currency",
			res: of(
				model.Recommendation{ID: "small", EstimatedMonthlySavings: "1.00", Currency: "USD"},
				model.Recommendation{ID: "unpriced"},
				model.Recommendation{ID: "big", EstimatedMonthlySavings: "9.00", Currency: "USD"},
			),
			want: "big",
		},
		{
			// The boundary itself: 900 JPY is not more than 100 USD, and saying
			// so would need a rate this tool does not have and will not invent.
			name: "two currencies void the ranking",
			res: of(
				model.Recommendation{ID: "usd", EstimatedMonthlySavings: "100.00", Currency: "USD"},
				model.Recommendation{ID: "jpy", EstimatedMonthlySavings: "900.00", Currency: "JPY"},
			),
			want: "usd",
		},
		{
			// A named currency and an unnamed one are also two currencies. AWS
			// not saying what unit a figure is in is not permission to assume it
			// matches the one beside it.
			name: "a missing currency is not the other one",
			res: of(
				model.Recommendation{ID: "named", EstimatedMonthlySavings: "10.00", Currency: "USD"},
				model.Recommendation{ID: "bare", EstimatedMonthlySavings: "99.00"},
			),
			want: "named",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := headlineTip(tc.res)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("headlineTip = %+v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("headlineTip = nil, want %q", tc.want)
			}
			if got.ID != tc.want {
				t.Errorf("headlineTip = %q, want %q", got.ID, tc.want)
			}
		})
	}
}

// The awkward tips, each of which has a wrong answer that looks right.
func TestCSVTipEdgeCases(t *testing.T) {
	base := func(name string, recs ...model.Recommendation) model.Resource {
		r := model.Resource{
			ARN: "arn:aws:ec2:us-east-1:210987654321:instance/" + name, Name: name,
			Service: model.ServiceEC2, Type: model.TypeEC2Instance,
			Region: "us-east-1", AccountID: "210987654321",
		}
		for _, rec := range recs {
			r.AddRecommendation(rec)
		}
		return r
	}

	// AWS modelled the change as saving nothing. That is an answer, not a
	// silence, and filtering it out is the recurring bug.
	zero := base("zero", model.Recommendation{
		ID: "z", ActionType: "Upgrade", EstimatedMonthlySavings: "0.00", Currency: "USD",
	})

	// Named without being priced. Still advice worth acting on, still counted,
	// and the amount cell stays blank rather than becoming a zero.
	unpriced := base("unpriced", model.Recommendation{
		ID: "u", ActionType: "Stop", ImplementationEffort: "VeryLow",
	})

	// A priced suggestion and an unpriced one on one row: the priced one wins
	// the headline, and the unpriced one is not what "largest" means.
	mixed := base("mixed",
		model.Recommendation{ID: "m1", ActionType: "Stop"},
		model.Recommendation{ID: "m2", ActionType: "Rightsize",
			EstimatedMonthlySavings: "5.00", Currency: "USD"},
	)

	// Two currencies on one resource. Comparing them needs an exchange rate this
	// tool does not have, so the row falls back to the census's own first tip
	// rather than crowning whichever number is numerically bigger.
	twoCurrencies := base("two-currencies",
		model.Recommendation{ID: "c1", ActionType: "Rightsize",
			EstimatedMonthlySavings: "100.00", Currency: "USD"},
		model.Recommendation{ID: "c2", ActionType: "Stop",
			EstimatedMonthlySavings: "900.00", Currency: "JPY"},
	)

	// AWS priced it and did not say in what. Its own bucket everywhere else;
	// here, a blank currency cell rather than an assumed one.
	noCurrency := base("no-currency", model.Recommendation{
		ID: "n", ActionType: "Rightsize", EstimatedMonthlySavings: "41.20",
	})

	// RestartNeeded absent is a third state. "AWS did not say" is not "no".
	silent := base("silent", model.Recommendation{
		ID: "s", ActionType: "MigrateToGraviton", EstimatedMonthlySavings: "3.00", Currency: "USD",
	})
	noRestart := base("no-restart", model.Recommendation{
		ID: "nr", ActionType: "Upgrade", EstimatedMonthlySavings: "3.00", Currency: "USD",
		RestartNeeded: ptrTo(false),
	})

	// An action type a spreadsheet would evaluate. AWS does not send these, but
	// the column is a string column and the guard is not conditional on trust.
	hostile := base("hostile", model.Recommendation{
		ID: "h", ActionType: `=HYPERLINK("http://evil.example/")`,
		EstimatedMonthlySavings: "1.00", Currency: "USD", ImplementationEffort: "@Medium",
	})

	records := renderAndParse(t, &model.Snapshot{Resources: []model.Resource{
		zero, unpriced, mixed, twoCurrencies, noCurrency, silent, noRestart, hostile,
	}})
	c := col(t, records[0])
	rows := map[string][]string{}
	for _, r := range records[1:] {
		rows[r[c["name"]]] = r
	}

	for _, tc := range []struct{ row, column, want string }{
		{"zero", "tip_savings", "0.00"},
		{"zero", "tip_count", "1"},
		{"unpriced", "tip_savings", ""},
		{"unpriced", "tip_action", "Stop"},
		{"unpriced", "tip_count", "1"},
		{"mixed", "tip_savings", "5.00"},
		{"mixed", "tip_action", "Rightsize"},
		{"mixed", "tip_count", "2"},
		{"two-currencies", "tip_count", "2"},
		{"no-currency", "tip_savings", "41.20"},
		{"no-currency", "tip_currency", ""},
		{"silent", "tip_restart", ""},
		{"no-restart", "tip_restart", "false"},
		{"hostile", "tip_action", `'=HYPERLINK("http://evil.example/")`},
		{"hostile", "tip_effort", "'@Medium"},
		// The saving is a number and stays one — a leading quote would make a
		// spreadsheet refuse to sum the column.
		{"hostile", "tip_savings", "1.00"},
	} {
		if got := rows[tc.row][c[tc.column]]; got != tc.want {
			t.Errorf("%s.%s = %q, want %q", tc.row, tc.column, got, tc.want)
		}
	}

	// The mixed-currency row reports one of its own suggestions rather than
	// inventing a comparison. Which one is the census's business; that it is not
	// a cross-currency winner is this test's.
	got := rows["two-currencies"]
	pairs := map[string]string{"100.00": "USD", "900.00": "JPY"}
	if want, ok := pairs[got[c["tip_savings"]]]; !ok || got[c["tip_currency"]] != want {
		t.Errorf("mixed-currency row = (%q, %q), want one of the resource's own "+
			"amount/currency pairs, not a figure picked across the boundary",
			got[c["tip_savings"]], got[c["tip_currency"]])
	}
}

func TestCSVAtomicWrite(t *testing.T) {
	snap := demoSnapshot("test")
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
