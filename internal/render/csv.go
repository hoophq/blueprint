package render

import (
	"encoding/csv"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// csvHeader is the fixed column order for the CSV renderer.
//
// The columns are closed on purpose: they are exactly the narrow core of
// model.Resource, so the header stays stable as new services land and
// downstream scripts keep working. Everything service-specific — engine,
// instance class, size, multi-AZ, and whatever future scanners report — is
// carried in the "attributes" cell as k=v pairs, using the same reversible
// encoding as "tags".
//
// The cost columns are part of that closed set rather than an exception to it.
// They are not service-specific: any resource of any type can carry a cost, so
// they are added once for every service instead of once per service, which is
// the growth the rule exists to prevent. They sit after "attributes" so every
// pre-existing column keeps the index it has always had.
var csvHeader = []string{
	"arn", "service", "type", "name", "status", "region", "account_id",
	"created_at", "environment", "owner", "tags", "eol", "eol_date",
	"publicly_accessible", "encrypted", "attributes",
	"cost_amount", "cost_currency", "cost_method", "cost_estimated",
	"cost_observed_from", "cost_observed_to", "cost_caveats",
	"cost_unavailable_reason",
}

// CSV writes one row per resource for spreadsheet/script consumption.
//
// The write is atomic with respect to the destination: rows go to
// path+".tmp" and the temp file is renamed onto path only after every row is
// flushed and the file is closed, so a mid-write failure never truncates or
// leaves a partial file at path.
func CSV(snap *model.Snapshot, path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// On error paths the deferred calls discard the partial temp file. On the
	// success path the file is already closed and renamed away, so both are
	// no-ops. Defer order (LIFO): close first, then remove.
	defer os.Remove(tmp)
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return err
	}
	for _, r := range snap.Resources {
		if err := w.Write(csvRow(r)); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	// Close before rename so buffered data is durably in the temp file and
	// close errors are surfaced instead of silently renaming a bad file.
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func csvRow(r model.Resource) []string {
	createdAt := ""
	if r.CreatedAt != nil {
		createdAt = r.CreatedAt.Format(time.RFC3339)
	}
	row := []string{
		guardFormula(r.ARN),
		guardFormula(r.Service),
		guardFormula(r.Type),
		guardFormula(r.Name),
		guardFormula(r.Status),
		guardFormula(r.Region),
		guardFormula(r.AccountID),
		createdAt,
		guardFormula(r.Environment),
		guardFormula(r.Owner),
		guardFormula(joinTags(r.Tags)),
		strconv.FormatBool(r.EOL),
		r.EOLDate, // fixed YYYY-MM-DD format, never a formula trigger
		boolPtrCell(r.PubliclyAccessible),
		boolPtrCell(r.Encrypted),
		guardFormula(joinAttributes(r)),
	}
	return append(row, costCells(r)...)
}

// costCells renders the eight cost columns for one resource.
//
// Every cell is EMPTY when no source priced the resource — not "0", not
// "0.00". A spreadsheet's SUM and AVERAGE skip blank cells and count zeros, so
// a zero written where nothing was reported quietly drags an average down and
// tells the reader a resource is free. The blank keeps the user's own
// arithmetic honest, and cost_unavailable_reason is what stops the blank from
// being a mystery: when a source looked and found nothing, the cell says so.
//
// A reported "0.00" is a different thing entirely and is written out as
// "0.00" — a source saying a resource costs nothing is a real finding and
// survives to the artifact.
func costCells(r model.Resource) []string {
	if r.Cost == nil {
		return []string{"", "", "", "", "", "", "", guardFormula(r.CostUnavailable)}
	}
	c := r.Cost
	return []string{
		// Not formula-guarded, deliberately: this column is numeric, and a
		// negative amount is legal money — a credit or a refund. Prefixing it
		// would turn every credit into text a spreadsheet will not sum.
		// Amounts are validated as plain decimals at ingest, which is what
		// makes leaving them unguarded safe here rather than hopeful.
		c.Amount,
		guardFormula(c.Currency),
		guardFormula(c.Method),
		strconv.FormatBool(c.Estimated),
		timeCell(c.ObservedFrom),
		timeCell(c.ObservedTo),
		guardFormula(joinCaveats(c.Caveats)),
		// A priced resource has nothing unavailable to explain.
		"",
	}
}

func timeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// joinCaveats packs the caveat list into one cell with the same reversible
// percent-encoding the tag and attribute cells use, so a caveat containing the
// separator cannot be misread as two caveats.
func joinCaveats(caveats []string) string {
	if len(caveats) == 0 {
		return ""
	}
	escaped := make([]string, 0, len(caveats))
	for _, c := range caveats {
		escaped = append(escaped, tagEscaper.Replace(c))
	}
	return strings.Join(escaped, ";")
}

// joinAttributes flattens the attribute bag into one cell, using the same
// reversible k=v;k=v encoding as joinTags. Attributes and measures share the
// cell because their key sets are disjoint by construction (each key is
// declared once in model, in one map or the other), so a reader can split the
// cell without needing to know which side a key came from. An absent key
// means the service did not report it — never a zero value.
func joinAttributes(r model.Resource) string {
	if len(r.Attributes) == 0 && len(r.Measures) == 0 {
		return ""
	}
	flat := make(map[string]string, len(r.Attributes)+len(r.Measures))
	maps.Copy(flat, r.Attributes)
	for k, v := range r.Measures {
		flat[k] = strconv.FormatInt(v, 10)
	}
	return joinTags(flat)
}

// boolPtrCell renders a tri-state boolean: empty when the service did not
// report the field, true/false otherwise.
func boolPtrCell(v *bool) string {
	if v == nil {
		return ""
	}
	return strconv.FormatBool(*v)
}

// guardFormula defends against spreadsheet formula injection (CSV injection):
// cells beginning with '=', '+', '-', '@', tab, or carriage return are
// interpreted as formulas by Excel/Sheets/LibreOffice, so hostile
// data-derived strings (names, tags, ...) could execute on open. Prefixing a
// single quote forces the cell to be read as text — the standard OWASP
// mitigation — and composes cleanly with encoding/csv's structural quoting.
// It is applied only to data-derived string columns; numeric/bool/timestamp
// columns are formatted locally and never start with a formula trigger
// (negative numbers, if they ever occur, must stay unprefixed).
func guardFormula(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// tagEscaper percent-encodes the characters that are structural in the
// joined tag string, plus the escape character itself.
var tagEscaper = strings.NewReplacer("%", "%25", "=", "%3D", ";", "%3B")

// joinTags renders tags as k=v pairs joined with ";", keys sorted for
// deterministic output. Because '=' and ';' are legal inside AWS tag keys
// and values, '%', '=', and ';' are percent-encoded within each key and
// value ('%'→"%25", '='→"%3D", ';'→"%3B") before joining. That makes the
// encoding unambiguous and reversible: every literal '=' in the result
// separates a key from its value, and every literal ';' separates pairs.
// encoding/csv handles any quoting needed.
func joinTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, tagEscaper.Replace(k)+"="+tagEscaper.Replace(tags[k]))
	}
	return strings.Join(pairs, ";")
}
