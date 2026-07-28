package scanners

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// toTagMap converts a slice of SDK tag structs into a plain map, using kv to
// extract the key/value pointer pair from each element (aws.ToString
// semantics: nil pointers become ""). Returns nil for an empty slice so
// untagged resources carry no map at all.
func toTagMap[T any](tags []T, kv func(T) (*string, *string)) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v := kv(t)
		m[aws.ToString(k)] = aws.ToString(v)
	}
	return m
}

// tagFailures aggregates per-resource ListTagsForResource failures so a scan
// surfaces them as one failure-ledger entry instead of silently leaving tags
// nil (which would inflate the missing-owner/env metrics unnoticed).
type tagFailures struct {
	attempts int
	failed   int
	first    error
}

// record counts one tag fetch attempt; a non-nil err marks it failed.
func (f *tagFailures) record(err error) {
	f.attempts++
	if err != nil {
		f.failed++
		if f.first == nil {
			f.first = err
		}
	}
}

// err summarizes the recorded failures, or returns nil if none occurred.
func (f *tagFailures) err() error {
	if f.failed == 0 {
		return nil
	}
	return fmt.Errorf("tags: %d of %d ListTagsForResource calls failed: %w", f.failed, f.attempts, f.first)
}
