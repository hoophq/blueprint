package scanners

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
)

func TestToTagMap(t *testing.T) {
	if toTagMap(nil, rdsTagKV) != nil {
		t.Error("expected nil map for nil slice")
	}
	if toTagMap([]rdstypes.Tag{}, rdsTagKV) != nil {
		t.Error("expected nil map for empty slice")
	}
	m := toTagMap([]rdstypes.Tag{
		{Key: aws.String("env"), Value: aws.String("prod")},
		{Key: aws.String("nilval")}, // nil Value → "" (aws.ToString semantics)
	}, rdsTagKV)
	if m["env"] != "prod" {
		t.Errorf("unexpected tag map: %v", m)
	}
	if v, ok := m["nilval"]; !ok || v != "" {
		t.Errorf("nil value should map to empty string, got %v", m)
	}
}

func TestCeilGB(t *testing.T) {
	const gb = int64(1 << 30)
	cases := []struct {
		n, perGB int64
		want     int32
	}{
		{0, gb, 0},
		{-1, gb, 0},
		{1, gb, 1},
		{gb, gb, 1},
		{gb + 1, gb, 2},
		{1023, 1024, 1},
		{1025, 1024, 2},
	}
	for _, c := range cases {
		if got := ceilGB(c.n, c.perGB); got != c.want {
			t.Errorf("ceilGB(%d, %d) = %d, want %d", c.n, c.perGB, got, c.want)
		}
	}
}

func TestTagFailures(t *testing.T) {
	var f tagFailures
	if f.err() != nil {
		t.Error("expected nil error before any failures")
	}
	f.record(nil)
	if f.err() != nil {
		t.Error("expected nil error after a successful attempt")
	}
	first := errors.New("access denied")
	f.record(first)
	f.record(errors.New("throttled"))
	err := f.err()
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if !errors.Is(err, first) {
		t.Errorf("aggregated error should wrap the first failure: %v", err)
	}
	if want := "tags: 2 of 3 ListTagsForResource calls failed: access denied"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}
