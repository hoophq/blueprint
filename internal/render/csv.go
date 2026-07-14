package render

import (
	"errors"

	"github.com/hoophq/dbcensus/internal/model"
)

// CSV writes one row per resource for spreadsheet/script consumption.
func CSV(snap *model.Snapshot, path string) error {
	_ = snap
	_ = path
	return errors.New("csv renderer not implemented yet")
}
