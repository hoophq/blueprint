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
//
// There is one block of cost columns per attribution method, not one shared
// block with a "cost_method" cell naming which source won. A resource can be
// priced by more than one source — Cost Explorer bills a closed window, Cost
// Optimization Hub models a monthly rate — and a single block would have to
// pick one figure and drop the other, or join both into one cell. A joined cell
// is worse than it looks: a spreadsheet SUM silently skips exactly the rows that
// carry two figures, so the estate's total would be wrong in a way that shows no
// error. Separate columns keep every figure summable and keep the two methods
// from ever landing in the same column, which is the one arithmetic nobody
// should be able to do by accident.
var csvHeader = buildCSVHeader()

// coreColumns are the narrow core of model.Resource, in fixed order.
var coreColumns = []string{
	"arn", "service", "type", "name", "status", "region", "account_id",
	"created_at", "environment", "owner", "tags", "eol", "eol_date",
	"publicly_accessible", "encrypted", "attributes",
}

// costColumns are the per-figure cost columns, emitted once per method.
//
// "method" is not among them: the column name already carries it, and a cell
// repeating it would be a second place for the two to disagree. Order here is
// the order costCells writes, and the two are checked against each other by the
// header/row width test rather than by eye.
var costColumns = []string{
	"amount", "currency", "estimated", "observed_from", "observed_to",
	"caveats", "match_key",
}

func buildCSVHeader() []string {
	h := make([]string, 0, len(coreColumns)+len(model.CostMethods())*len(costColumns)+1)
	h = append(h, coreColumns...)
	for _, m := range model.CostMethods() {
		for _, c := range costColumns {
			h = append(h, "cost_"+m+"_"+c)
		}
	}
	// One shared column, not one per method: the reason is written by whichever
	// source looked a resource up and could not price it, and it names its own
	// source in the sentence.
	return append(h, "cost_unavailable_reason")
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
	row := []string{
		guardFormula(r.ARN),
		guardFormula(r.Service),
		guardFormula(r.Type),
		guardFormula(r.Name),
		guardFormula(r.Status),
		guardFormula(r.Region),
		guardFormula(r.AccountID),
		timeCell(r.CreatedAt),
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

// costCells renders one block of cost columns per method, then the shared
// unavailable-reason column.
//
// Every cell in a method's block is EMPTY when that method did not price the
// resource — not "0", not "0.00". A spreadsheet's SUM and AVERAGE skip blank
// cells and count zeros, so a zero written where nothing was reported quietly
// drags an average down and tells the reader a resource is free. The blank keeps
// the user's own arithmetic honest, and cost_unavailable_reason is what stops
// the blank from being a mystery: when a source looked and found nothing, the
// cell says so.
//
// A reported "0.00" is a different thing entirely and is written out as
// "0.00" — a source saying a resource costs nothing is a real finding and
// survives to the artifact.
//
// The unavailable reason is written even for a resource some other method
// priced. With more than one source, "Cost Optimization Hub does not model this
// resource type" stays true whether or not Cost Explorer billed it, and blanking
// it on the strength of an unrelated figure would hide a real coverage gap
// behind an answer to a different question.
func costCells(r model.Resource) []string {
	methods := model.CostMethods()
	cells := make([]string, 0, len(methods)*len(costColumns)+1)
	for _, m := range methods {
		c := r.CostBy(m)
		if c == nil {
			cells = append(cells, make([]string, len(costColumns))...)
			continue
		}
		cells = append(cells,
			// Not formula-guarded, deliberately: this column is numeric, and a
			// negative amount is legal money — a credit or a refund. Prefixing
			// it would turn every credit into text a spreadsheet will not sum.
			// Amounts are validated as plain decimals at ingest, which is what
			// makes leaving them unguarded safe here rather than hopeful.
			c.Amount,
			guardFormula(c.Currency),
			strconv.FormatBool(c.Estimated),
			timeCell(c.ObservedFrom),
			timeCell(c.ObservedTo),
			guardFormula(joinCaveats(c.Caveats)),
			// The match key is an identifier AWS chose, not one this tool
			// validated, so it is guarded like every other free-form string.
			guardFormula(c.MatchKey),
		)
	}
	return append(cells, guardFormula(r.CostUnavailable))
}

// timeCell renders an optional timestamp as RFC3339, empty when unreported.
//
// The result is formula-guarded like every other string cell. For every
// timestamp this tool will realistically write the guard is a no-op, because
// RFC3339 opens with a four-digit year — but "realistically" is the whole
// problem with reasoning about it case by case, and Go does render a negative
// year as "-0001-01-01T00:00:00Z", which opens with a character a spreadsheet
// reads as arithmetic. Guarding unconditionally is free and means every string
// column in this file is guarded, with exactly one exemption that says why it
// is one.
func timeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return guardFormula(t.Format(time.RFC3339))
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
