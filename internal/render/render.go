// Package render turns a Snapshot into local artifacts: terminal summary,
// JSON, CSV, and a self-contained single-file HTML report. Renderers must
// never make network calls — outputs render fully offline.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
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
	fmt.Fprintf(w, "  %d resources  ·  %d types  ·  %d regions  ·  %d account(s)\n",
		sum.Total, len(sum.Types), countNonZero(sum.Regions), len(sum.Accounts))
	fmt.Fprintf(w, "  %d without owner tag  ·  %d without environment tag\n", sum.NoOwner, sum.NoEnv)
	if sum.EOL > 0 {
		fmt.Fprintf(w, "  ⚠ %d on end-of-life engine versions (upstream support ended)\n", sum.EOL)
	}
	if sum.Exposed > 0 {
		fmt.Fprintf(w, "  ⚠ %d exposed — %d publicly accessible · %d unencrypted · %d without backups\n",
			sum.Exposed, sum.Public, sum.Unencrypted, sum.NoBackups)
	}
	if len(sum.Services) > 0 {
		fmt.Fprintf(w, "  by service: %s\n", formatCounts(sum.Services))
	}
	costSection(w, snap)
	if sum.Failures > 0 {
		fmt.Fprintf(w, "\n  ⚠ incomplete coverage — %d scan unit(s) failed:\n", sum.Failures)
		groups := groupFailures(snap.Failures)
		for i, g := range groups {
			if i == maxFailuresListed {
				fmt.Fprintf(w, "    … and %d more (full ledger in the JSON output)\n", len(groups)-maxFailuresListed)
				break
			}
			fmt.Fprintf(w, "    - %s: %s\n", g.scope, g.err)
		}
	}
	for _, p := range written {
		fmt.Fprintf(w, "  → %s\n", p)
	}
	fmt.Fprintf(w, "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
}

// maxFailuresListed caps terminal failure lines (after rollup) so one broken
// permission boundary cannot flood stdout; the JSON ledger keeps every entry.
const maxFailuresListed = 20

type failureGroup struct{ scope, err string }

// groupFailures rolls identical (account, service, error) failures across
// regions into one line each — at many services × regions, "not authorized"
// repeated per region would bury the distinct problems. Output is sorted for
// deterministic rendering, and empty account/region components (global scan
// units) are omitted rather than printed as dangling separators.
func groupFailures(failures []model.Failure) []failureGroup {
	type key struct{ account, service, err string }
	regions := map[key][]string{}
	for _, f := range failures {
		k := key{f.AccountID, f.Service, f.Error}
		if f.Region != "" {
			regions[k] = append(regions[k], f.Region)
		} else if _, ok := regions[k]; !ok {
			regions[k] = nil
		}
	}
	keys := make([]key, 0, len(regions))
	for k := range regions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.account != b.account {
			return a.account < b.account
		}
		if a.service != b.service {
			return a.service < b.service
		}
		return a.err < b.err
	})
	out := make([]failureGroup, 0, len(keys))
	for _, k := range keys {
		var parts []string
		if k.account != "" {
			parts = append(parts, k.account)
		}
		if k.service != "" {
			parts = append(parts, k.service)
		}
		scope := strings.Join(parts, "/")
		if rs := regions[k]; len(rs) > 0 {
			sort.Strings(rs)
			// Duplicate scan units (e.g. --regions us-east-1,us-east-1) must
			// not render as "in us-east-1, us-east-1".
			rs = slices.Compact(rs)
			scope += " in " + strings.Join(rs, ", ")
		}
		out = append(out, failureGroup{scope: scope, err: k.err})
	}
	return out
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
	// Name tie-break: equal counts would otherwise order by map iteration,
	// which visibly flickers between runs.
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}
