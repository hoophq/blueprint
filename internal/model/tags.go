package model

import "strings"

// Tag keys checked (case-insensitively, in order) to derive Environment and
// Owner. Derivation is lookup-only: no inference, per the PRD guardrail.
var (
	envTagKeys   = []string{"environment", "env", "stage", "tier"}
	ownerTagKeys = []string{"owner", "team", "squad", "maintainer", "contact"}
)

// DeriveEnvOwner fills Environment and Owner from tags when present.
func (r *Resource) DeriveEnvOwner() {
	r.Environment = firstTag(r.Tags, envTagKeys)
	r.Owner = firstTag(r.Tags, ownerTagKeys)
}

func firstTag(tags map[string]string, keys []string) string {
	if len(tags) == 0 {
		return ""
	}
	lower := make(map[string]string, len(tags))
	for k, v := range tags {
		lower[strings.ToLower(k)] = v
	}
	for _, k := range keys {
		if v, ok := lower[k]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
