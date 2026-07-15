package render

import (
	"encoding/csv"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// csvHeader is the fixed column order for the CSV renderer.
var csvHeader = []string{
	"arn", "service", "kind", "name", "engine", "engine_version",
	"instance_class", "storage_gb", "multi_az", "status", "endpoint",
	"region", "account_id", "created_at", "environment", "owner", "tags",
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
	return []string{
		guardFormula(r.ARN),
		guardFormula(r.Service),
		guardFormula(r.Kind),
		guardFormula(r.Name),
		guardFormula(r.Engine),
		guardFormula(r.EngineVersion),
		guardFormula(r.InstanceClass),
		strconv.FormatInt(int64(r.StorageGB), 10),
		strconv.FormatBool(r.MultiAZ),
		guardFormula(r.Status),
		guardFormula(r.Endpoint),
		guardFormula(r.Region),
		guardFormula(r.AccountID),
		createdAt,
		guardFormula(r.Environment),
		guardFormula(r.Owner),
		guardFormula(joinTags(r.Tags)),
	}
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
