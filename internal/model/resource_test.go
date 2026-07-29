package model

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// The attribute bag carries the honesty guardrail that used to live in
// pointer-typed struct fields: an absent key means "the service did not report
// this", which must never collapse into an empty string or a zero.
func TestAttributesOmitUnreportedValues(t *testing.T) {
	var r Resource
	r.SetAttr(AttrEngine, "postgres")
	r.SetAttr(AttrEngineVersion, "") // not reported — must not create the key
	r.SetAttr("", "orphan")          // no key to file it under
	r.SetBoolAttr(AttrMultiAZ, nil)  // enum the API left empty

	if got := r.Attr(AttrEngine); got != "postgres" {
		t.Errorf("Attr(engine) = %q, want %q", got, "postgres")
	}
	for _, key := range []string{AttrEngineVersion, "", AttrMultiAZ} {
		if _, ok := r.Attributes[key]; ok {
			t.Errorf("Attributes[%q] present; unreported values must leave the key absent", key)
		}
	}
	if got := r.Attr(AttrEngineVersion); got != "" {
		t.Errorf("Attr on an absent key = %q, want empty", got)
	}
}

// Measures are the opposite case: a real zero is a finding (no backups, empty
// table), so it must be stored, while a nil SDK pointer must not become one.
func TestMeasuresDistinguishZeroFromUnreported(t *testing.T) {
	var r Resource
	r.SetMeasure(MeasureBackupRetentionDays, 0)
	r.SetMeasureInt32(MeasureSizeBytes, nil)

	if v, ok := r.Measure(MeasureBackupRetentionDays); !ok || v != 0 {
		t.Errorf("Measure(backup_retention_days) = (%d, %v), want (0, true)", v, ok)
	}
	if v, ok := r.Measure(MeasureSizeBytes); ok {
		t.Errorf("Measure(size_bytes) = (%d, true), want not reported", v)
	}
}

func TestExposed(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		mut  func(*Resource)
		want bool
	}{
		{"nothing reported", func(r *Resource) {}, false},
		{"publicly accessible", func(r *Resource) { r.PubliclyAccessible = &yes }, true},
		{"explicitly not public", func(r *Resource) { r.PubliclyAccessible = &no }, false},
		{"unencrypted", func(r *Resource) { r.Encrypted = &no }, true},
		{"encrypted", func(r *Resource) { r.Encrypted = &yes }, false},
		{"no backups", func(r *Resource) { r.SetMeasure(MeasureBackupRetentionDays, 0) }, true},
		{"backups kept", func(r *Resource) { r.SetMeasure(MeasureBackupRetentionDays, 7) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r Resource
			c.mut(&r)
			if got := r.Exposed(); got != c.want {
				t.Errorf("Exposed() = %v, want %v", got, c.want)
			}
		})
	}
}

// The bag must not cost determinism: encoding/json sorts map keys, so a given
// snapshot still marshals byte-for-byte the same however the maps were built.
func TestAttributeBagMarshalsDeterministically(t *testing.T) {
	build := func(keys []string) []byte {
		var r Resource
		for _, k := range keys {
			r.SetAttr(k, "v")
			r.SetMeasure(k+"_n", 1)
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return data
	}
	forward := build([]string{"alpha", "beta", "gamma", "delta"})
	reverse := build([]string{"delta", "gamma", "beta", "alpha"})
	if string(forward) != string(reverse) {
		t.Errorf("insertion order changed the JSON:\n %s\n %s", forward, reverse)
	}
	if !strings.Contains(string(forward), `"alpha":"v"`) {
		t.Errorf("attributes missing from JSON: %s", forward)
	}
}

func TestSummarizeCountsTypesAndEngines(t *testing.T) {
	mk := func(typ, engine string) Resource {
		r := Resource{ARN: typ + engine, Type: typ, AccountID: "1"}
		r.SetAttr(AttrEngine, engine)
		return r
	}
	s := &Snapshot{Resources: []Resource{
		mk(TypeRDSInstance, "postgres"),
		mk(TypeRDSCluster, "aurora-postgresql"),
		mk(TypeDynamoDBTable, "dynamodb"),
		// A resource with no engine concept must not invent an engine bucket.
		mk("AWS::S3::Bucket", ""),
	}}
	sum := s.Summarize()
	if len(sum.Types) != 4 {
		t.Errorf("Types = %v, want 4 distinct", sum.Types)
	}
	if len(sum.Engines) != 3 {
		t.Errorf("Engines = %v, want 3 (the engine-less resource must not be counted)", sum.Engines)
	}
	if _, ok := sum.Engines[""]; ok {
		t.Errorf("Engines has an empty-string bucket: %v", sum.Engines)
	}
}

// Sort's key must be unique even when names collide: resource types whose
// Name comes from an optional tag (EBS volumes, NAT gateways, …) can share an
// empty name within one (account, region, service), the runner appends in
// goroutine-completion order, and sort.Slice is unstable — only the ARN
// tie-break keeps the JSON artifact byte-for-byte deterministic.
func TestSortTieBreaksOnARN(t *testing.T) {
	mk := func(arn string) Resource {
		return Resource{ARN: arn, AccountID: "1", Region: "us-east-1", Service: "ec2", Name: ""}
	}
	arns := []string{
		"arn:aws:ec2:us-east-1:1:volume/vol-ccc",
		"arn:aws:ec2:us-east-1:1:volume/vol-aaa",
		"arn:aws:ec2:us-east-1:1:volume/vol-bbb",
	}
	// Both insertion orders must converge on the same output order.
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}} {
		s := &Snapshot{}
		for _, i := range order {
			s.Resources = append(s.Resources, mk(arns[i]))
		}
		s.Sort()
		for i := 1; i < len(s.Resources); i++ {
			if s.Resources[i-1].ARN >= s.Resources[i].ARN {
				t.Fatalf("insertion order %v: resources not ARN-ordered: %q before %q",
					order, s.Resources[i-1].ARN, s.Resources[i].ARN)
			}
		}
	}
}

func TestFinalizeSortsScopeLists(t *testing.T) {
	s := &Snapshot{
		Accounts: []string{"222222222222", "111111111111"},
		Regions:  []string{"us-west-2", "sa-east-1"},
		Services: []string{"redshift", "dynamodb", "rds"},
	}
	s.Finalize()
	for name, list := range map[string][]string{
		"Accounts": s.Accounts, "Regions": s.Regions, "Services": s.Services,
	} {
		if !sort.StringsAreSorted(list) {
			t.Errorf("%s not sorted after Finalize: %v", name, list)
		}
	}
}

// A failure's time is stamped by this tool rather than reported by AWS, so it
// carries no honesty question — but it does carry a determinism one. The
// runner appends in goroutine-completion order and sort.Slice is unstable, so
// two entries alike in every other field would otherwise swap places between
// two runs over an identical snapshot.
func TestSortFailuresBreaksTiesOnTime(t *testing.T) {
	early := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	late := early.Add(3 * time.Second)
	twin := func(at time.Time) Failure {
		return Failure{
			AccountID: "111111111111", Region: "us-east-1", Service: ServiceRDS,
			Error: "ThrottlingException: Rate exceeded", Time: at,
		}
	}

	// Both append orders must converge on the same ledger.
	for _, appended := range [][]Failure{{twin(late), twin(early)}, {twin(early), twin(late)}} {
		s := &Snapshot{Failures: appended}
		s.SortFailures()
		if !s.Failures[0].Time.Equal(early) || !s.Failures[1].Time.Equal(late) {
			t.Errorf("SortFailures produced %v then %v; entries differing only by time must "+
				"order by it, whatever order the runner appended them in",
				s.Failures[0].Time, s.Failures[1].Time)
		}
	}
}

// An unstamped entry is one written before the field existed. That is absence,
// and absence must not surface as a value — least of all as year 1, which
// reads like a real timestamp that went wrong.
func TestFailureOmitsAnUnstampedTime(t *testing.T) {
	unstamped, err := json.Marshal(Failure{Service: ServiceRDS, Error: "boom"})
	if err != nil {
		t.Fatalf("marshalling an unstamped failure: %v", err)
	}
	if strings.Contains(string(unstamped), "time") {
		t.Errorf("unstamped failure marshalled as %s; the key must be absent, not zero-valued", unstamped)
	}

	stamped, err := json.Marshal(Failure{
		Service: ServiceRDS, Error: "boom",
		Time: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshalling a stamped failure: %v", err)
	}
	if want := `"time":"2026-06-01T10:00:00Z"`; !strings.Contains(string(stamped), want) {
		t.Errorf("stamped failure marshalled as %s, want it to contain %s", stamped, want)
	}
}
