// Package history keeps a local archive of past censuses so every scan can
// answer "what changed since last time" with zero user effort. Snapshots are
// bucketed by scan scope (accounts + regions) so runs against different
// estates never diff against each other. Everything lives under the local
// history dir — nothing leaves the machine.
package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hoophq/blueprint/internal/model"
)

// keepPerScope caps how many censuses are retained per scope bucket; older
// files are pruned on save.
const keepPerScope = 30

// Dir returns the history root: $BLUEPRINT_HISTORY_DIR when set, otherwise
// ~/.blueprint/history.
func Dir() (string, error) {
	if d := os.Getenv("BLUEPRINT_HISTORY_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".blueprint", "history"), nil
}

// ScopeKey derives the bucket for a snapshot from its accounts and regions.
// Same accounts + same regions → same bucket; anything else is a different
// census and must not be diffed against this one.
func ScopeKey(snap *model.Snapshot) string {
	accounts := append([]string(nil), snap.Accounts...)
	regions := append([]string(nil), snap.Regions...)
	sort.Strings(accounts)
	sort.Strings(regions)
	sum := sha256.Sum256([]byte(strings.Join(accounts, ",") + "|" + strings.Join(regions, ",")))
	return hex.EncodeToString(sum[:])[:12]
}

// Latest loads the most recent snapshot in the scope bucket, or nil when the
// bucket is empty (first scan for this scope).
func Latest(root string, scope string) (*model.Snapshot, error) {
	files, err := scopeFiles(root, scope)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	data, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		return nil, err
	}
	var snap model.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// Save writes the snapshot into its scope bucket (named by GeneratedAt, so
// filenames sort chronologically) and prunes the bucket to keepPerScope.
func Save(root string, snap *model.Snapshot) (string, error) {
	dir := filepath.Join(root, ScopeKey(snap))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, snap.GeneratedAt.UTC().Format("2006-01-02T150405Z")+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	prune(root, ScopeKey(snap))
	return path, nil
}

// prune drops the oldest files beyond keepPerScope; failures are ignored —
// pruning is best-effort housekeeping, never worth failing a scan over.
func prune(root, scope string) {
	files, err := scopeFiles(root, scope)
	if err != nil {
		return
	}
	for _, f := range files[:max(0, len(files)-keepPerScope)] {
		_ = os.Remove(f)
	}
}

// scopeFiles lists a bucket's census files sorted oldest → newest (the
// GeneratedAt-based names make lexical order chronological).
func scopeFiles(root, scope string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, scope))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(root, scope, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}
