package scanners

import (
	"slices"
	"strings"
)

// joinIDs renders the identifiers a describe response listed alongside a
// resource — the instances a volume is attached to, the addresses a NAT gateway
// holds, the target groups behind a load balancer — as one comma-separated
// attribute value, using pick to reach the identifier on each element.
//
// Sorted and deduplicated, because AWS does not promise an order and a census
// artifact has to be byte-stable across scans or every diff is noise.
//
// Presence is the pointer, and usability is a separate question asked after it:
// an element AWS did not name is skipped, and so is one it named with an empty
// string, because appending that would render "i-a,,i-b" — a list with a hole
// in it. That is not the stored-zero case the honesty guardrail protects. A
// zero-byte volume is a fact about storage; an unnamed attachment is not a fact
// about anything.
//
// An empty result is what SetAttr turns into an absent key, which is the
// correct reading: nothing was listed.
func joinIDs[T any](items []T, pick func(T) *string) string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		id := pick(item)
		if id == nil || *id == "" {
			continue
		}
		ids = append(ids, *id)
	}
	slices.Sort(ids)
	return strings.Join(slices.Compact(ids), ",")
}
