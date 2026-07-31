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

func TestObservedMeasuresCarryTheirObservationTime(t *testing.T) {
	at := time.Date(2026, 7, 27, 6, 30, 0, 0, time.UTC)
	var r Resource
	// Zero is a real reading here — a full volume — and must survive with its
	// timestamp, exactly like a non-zero one.
	r.SetObservedMeasure(MeasureFreeStorageBytes, 0, at)

	if v, ok := r.Measure(MeasureFreeStorageBytes); !ok || v != 0 {
		t.Errorf("Measure(free_storage_bytes) = (%d, %v), want (0, true)", v, ok)
	}
	got, ok := r.MeasureAsOf(MeasureFreeStorageBytes)
	if !ok || !got.Equal(at) {
		t.Errorf("MeasureAsOf = (%v, %v), want (%v, true)", got, ok, at)
	}
	if raw := r.Attr(MeasureFreeStorageBytes + AsOfSuffix); raw != "2026-07-27T06:30:00Z" {
		t.Errorf("stored timestamp = %q, want RFC 3339 UTC", raw)
	}
}

// A non-UTC observation time must be normalized, not stored verbatim: the rest
// of the artifact is UTC, and a reader comparing an offset timestamp against
// GeneratedAt by string would misjudge the staleness the field exists to show.
func TestObservedMeasureNormalizesToUTC(t *testing.T) {
	zone := time.FixedZone("UTC-7", -7*3600)
	var r Resource
	r.SetObservedMeasure(MeasureFreeStorageBytes, 42, time.Date(2026, 7, 27, 23, 0, 0, 0, zone))

	at, ok := r.MeasureAsOf(MeasureFreeStorageBytes)
	if !ok {
		t.Fatal("observation time was not recorded")
	}
	if name, _ := at.Zone(); name != "UTC" {
		t.Errorf("observation time zone = %q, want UTC", name)
	}
	if want := time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC); !at.Equal(want) {
		t.Errorf("observation time = %v, want %v", at, want)
	}
}

// An untimed datapoint is one whose staleness cannot be judged, so it is
// dropped whole rather than stored as a value that looks current.
func TestObservedMeasureRejectsAnUntimedDatapoint(t *testing.T) {
	var r Resource
	r.SetObservedMeasure(MeasureFreeStorageBytes, 512, time.Time{})

	if v, ok := r.Measure(MeasureFreeStorageBytes); ok {
		t.Errorf("Measure(free_storage_bytes) = (%d, true), want not reported", v)
	}
	if _, ok := r.MeasureAsOf(MeasureFreeStorageBytes); ok {
		t.Error("an untimed measure recorded an observation time")
	}
}

// Describe-sourced measures are current as of the scan and carry no timestamp,
// so MeasureAsOf must say "no" rather than invent an epoch.
func TestMeasureAsOfIsAbsentForDescribeSourcedMeasures(t *testing.T) {
	var r Resource
	r.SetMeasure(MeasureBackupRetentionDays, 7)

	if at, ok := r.MeasureAsOf(MeasureBackupRetentionDays); ok {
		t.Errorf("MeasureAsOf(backup_retention_days) = (%v, true), want absent", at)
	}
	// A malformed timestamp is absence too — never a zero-year time.Time that
	// downstream code would render as 0001-01-01.
	r.SetAttr(MeasureBackupRetentionDays+AsOfSuffix, "not a timestamp")
	if at, ok := r.MeasureAsOf(MeasureBackupRetentionDays); ok {
		t.Errorf("MeasureAsOf on unparseable input = (%v, true), want absent", at)
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

// The end-of-life verdict is the one derivation that depends on when it runs,
// so the clock has to be something a caller can supply. Without the seam a
// fixture's census is a function of the day it is built on: rows gain eol and
// eol_date as dates roll past, and a test that never mentioned time changes
// what it sees. This pins the seam itself — that FinalizeAt judges against the
// instant it is handed, and re-judges rather than accumulating, so a snapshot
// finalized once can be finalized again against a different clock.
//
// This is the test that holds the seam open. Both instants below are in the
// past, so a FinalizeAt that ignored its argument and read the wall clock
// would flag the resource in the "before" cases and fail here — which is the
// regression that matters, because it would turn every pinned fixture in the
// tree back into a wall-clock one while leaving those tests green.
func TestFinalizeAtJudgesEOLAgainstTheClockItIsGiven(t *testing.T) {
	// postgres 13 is dated 2025-11-13 in the table.
	pg13 := func() *Snapshot {
		r := Resource{Service: ServiceRDS, ARN: "arn:aws:rds:us-east-1:1:db:reporting"}
		r.SetAttr(AttrEngine, "postgres")
		r.SetAttr(AttrEngineVersion, "13.13")
		return &Snapshot{Resources: []Resource{r}}
	}
	before := time.Date(2025, 11, 12, 0, 0, 0, 0, time.UTC)
	after := time.Date(2025, 11, 14, 0, 0, 0, 0, time.UTC)

	s := pg13()
	s.FinalizeAt(before)
	if s.Resources[0].EOL || s.Resources[0].EOLDate != "" {
		t.Errorf("finalized a day before the date: EOL = (%v, %q), want (false, \"\")",
			s.Resources[0].EOL, s.Resources[0].EOLDate)
	}

	// Same snapshot, later clock: the verdict has to follow the clock rather
	// than stick from the first call.
	s.FinalizeAt(after)
	if !s.Resources[0].EOL || s.Resources[0].EOLDate != "2025-11-13" {
		t.Errorf("re-finalized a day after the date: EOL = (%v, %q), want (true, 2025-11-13)",
			s.Resources[0].EOL, s.Resources[0].EOLDate)
	}

	// And back, because a fixture pinned to a past instant is exactly this
	// move: the demo package finalizes against the wall clock and the test
	// re-finalizes against its own. A derivation that only ever added fields
	// would leave the wall clock's verdict in place and the pin would be a
	// no-op that looked like it worked.
	s.FinalizeAt(before)
	if s.Resources[0].EOL || s.Resources[0].EOLDate != "" {
		t.Errorf("re-finalized back before the date: EOL = (%v, %q), want (false, \"\") — "+
			"the verdict must be re-derived, not accumulated",
			s.Resources[0].EOL, s.Resources[0].EOLDate)
	}
}

// Finalize is FinalizeAt against the current instant. This checks only that
// the two agree — that Finalize delegates rather than deriving differently —
// and it deliberately claims no more than that: both calls read the same wall
// clock, so a FinalizeAt that ignored its argument would pass here. The test
// above is what catches that.
func TestFinalizeMatchesFinalizeAtNow(t *testing.T) {
	mk := func() *Snapshot {
		r := Resource{Service: ServiceRDS, ARN: "arn:aws:rds:us-east-1:1:db:legacy"}
		r.SetAttr(AttrEngine, "mysql")
		r.SetAttr(AttrEngineVersion, "5.7.44")
		return &Snapshot{Resources: []Resource{r}}
	}
	wall, pinned := mk(), mk()
	wall.Finalize()
	pinned.FinalizeAt(time.Now().UTC())
	if wall.Resources[0].EOL != pinned.Resources[0].EOL ||
		wall.Resources[0].EOLDate != pinned.Resources[0].EOLDate {
		t.Errorf("Finalize = (%v, %q), FinalizeAt(now) = (%v, %q); they must agree",
			wall.Resources[0].EOL, wall.Resources[0].EOLDate,
			pinned.Resources[0].EOL, pinned.Resources[0].EOLDate)
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
