package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hoophq/dbcensus/internal/model"
)

// reportTemplate is the single-file report shell. It contains all CSS/JS
// inline and exactly one injection point (dataMarker) for the snapshot JSON,
// so the report is self-contained and renders fully offline.
//
//go:embed report.html.tmpl
var reportTemplate string

const dataMarker = "__DBCENSUS_JSON__"

// HTML writes the browsable map: a fully self-contained single-file report
// (data embedded, no external assets, renders offline).
func HTML(snap *model.Snapshot, path string) error {
	if !strings.Contains(reportTemplate, dataMarker) {
		return fmt.Errorf("report template is missing the data marker %q; the embedded template is corrupt", dataMarker)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	// json.Marshal already unicode-escapes '<' inside JSON strings, and JSON
	// structure itself (delimiters, numbers, literals) never contains a
	// literal '<', so blindly replacing every remaining '<' in the payload
	// with the equivalent JSON escape is safe and total. This guarantees the
	// embedded block can never contain "</script", "<!--", or "<script" —
	// the sequences that change the HTML parser's script-data state — even
	// if marshaling behavior changes.
	payload := strings.ReplaceAll(string(data), "<", `\u003c`)
	page := strings.Replace(reportTemplate, dataMarker, payload, 1)
	return os.WriteFile(path, []byte(page), 0o644)
}
