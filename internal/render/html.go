package render

import (
	"errors"

	"github.com/hoophq/dbcensus/internal/model"
)

// HTML writes the browsable map: a fully self-contained single-file report
// (data embedded, no external assets, renders offline).
func HTML(snap *model.Snapshot, path string) error {
	_ = snap
	_ = path
	return errors.New("html renderer not implemented yet")
}
