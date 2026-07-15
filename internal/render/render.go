// Package render turns a Snapshot into local artifacts: terminal summary,
// JSON, CSV, and a self-contained single-file HTML report. Renderers must
// never make network calls — outputs render fully offline.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/hoophq/blueprint/internal/model"
)

// JSON writes the full snapshot as pretty-printed JSON.
func JSON(snap *model.Snapshot, path string) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Terminal prints the sprawl summary — the "aha" numbers — plus the honesty
// ledger of anything the scan could not see.
func Terminal(w io.Writer, snap *model.Snapshot, written []string) {
	sum := snap.Summarize()
	fmt.Fprintf(w, "\n━━ blueprint ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Fprintf(w, "  %d databases  ·  %d engines  ·  %d regions  ·  %d account(s)\n",
		sum.Total, len(sum.Engines), countNonZero(sum.Regions), len(sum.Accounts))
	fmt.Fprintf(w, "  %d without owner tag  ·  %d without environment tag\n", sum.NoOwner, sum.NoEnv)
	if len(sum.Services) > 0 {
		fmt.Fprintf(w, "  by service: %s\n", formatCounts(sum.Services))
	}
	if sum.Failures > 0 {
		fmt.Fprintf(w, "\n  ⚠ incomplete coverage — %d scan unit(s) failed:\n", sum.Failures)
		for _, f := range snap.Failures {
			fmt.Fprintf(w, "    - %s/%s %s: %s\n", f.AccountID, f.Region, f.Service, f.Error)
		}
	}
	for _, p := range written {
		fmt.Fprintf(w, "  → %s\n", p)
	}
	fmt.Fprintf(w, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
}

func countNonZero(m map[string]int) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}
