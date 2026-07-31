package demo

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// storyboardSize is what SnapshotN returns when it is asked for no more than
// the fixture already has. It is read rather than written down so adding a row
// to the storyboard does not fail a test that was never about the count.
func storyboardSize() int { return len(Snapshot("test").Resources) }

// The makers have to partition the storyboard, not merely overlap it. A shape
// with no maker is one the generator cannot produce, so a scaled estate would
// quietly stop containing it and any budget measured on that estate would be
// measuring a report with a column missing.
func TestEveryStoryboardRowHasExactlyOneMaker(t *testing.T) {
	for _, r := range Snapshot("test").Resources {
		var matched []string
		for _, m := range makers {
			if m.service == r.Service && m.typ == r.Type {
				matched = append(matched, m.service+"|"+m.typ)
			}
		}
		if len(matched) != 1 {
			t.Errorf("%s (%s %s): %d makers %v, want exactly 1",
				r.Name, r.Service, r.Type, len(matched), matched)
		}
	}
	// And nothing the other way: a maker for a shape the storyboard does not
	// have would be a distribution invented here rather than counted from the
	// fixture, which is the drift this design exists to prevent.
	for i, w := range makerWeights(Snapshot("test").Resources) {
		if w == 0 {
			t.Errorf("maker %s|%s stands for no storyboard row", makers[i].service, makers[i].typ)
		}
	}
}

func TestSnapshotNReturnsExactlyTheRequestedTotal(t *testing.T) {
	base := storyboardSize()
	for _, n := range []int{base + 1, base + 7, 500, 1000, 4321, 20000} {
		if got := len(SnapshotN("test", n).Resources); got != n {
			t.Errorf("SnapshotN(%d): %d resources, want %d", n, got, n)
		}
	}
}

// N is a total, not an addition, so anything at or under the storyboard's size
// is the storyboard — that is what keeps `scan --demo` instant and its output
// unchanged by this feature existing.
func TestSnapshotNAtOrBelowTheStoryboardIsTheStoryboard(t *testing.T) {
	want := Snapshot("test")
	for _, n := range []int{-1, 0, 1, storyboardSize() - 1, storyboardSize()} {
		got := SnapshotN("test", n)
		if !reflect.DeepEqual(got.Resources, want.Resources) {
			t.Errorf("SnapshotN(%d): %d resources, want the %d-row storyboard unchanged",
				n, len(got.Resources), len(want.Resources))
		}
		if !slices.Equal(got.Accounts, want.Accounts) {
			t.Errorf("SnapshotN(%d): accounts %v, want %v", n, got.Accounts, want.Accounts)
		}
	}
}

// The generator draws from an explicit source precisely so this holds. With
// the global source Go seeds at startup, two runs would produce two different
// estates, every scaled scan would diff against the last as if the account had
// been rebuilt overnight, and the size budget would measure a different report
// each time it ran.
func TestSnapshotNIsReproducible(t *testing.T) {
	first, err := json.Marshal(SnapshotN("test", 1500).Resources)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(SnapshotN("test", 1500).Resources)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, second) {
		t.Error("two calls to SnapshotN produced different resources; the generator is drawing from a source it does not own")
	}
}

// Scaling adds rows; it never edits or drops one. The storyboard is what the
// render tests name, and a scaled run has to keep every path they cover.
func TestSnapshotNKeepsEveryStoryboardRow(t *testing.T) {
	scaled := make(map[string]model.Resource)
	for _, r := range SnapshotN("test", 2000).Resources {
		scaled[r.ARN] = r
	}
	for _, want := range Snapshot("test").Resources {
		got, ok := scaled[want.ARN]
		if !ok {
			t.Errorf("%s: storyboard row missing from the scaled snapshot", want.ARN)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: storyboard row changed under scaling", want.ARN)
		}
	}
}

// An ARN is the identity the diff matches on. Two rows sharing one would make
// a scaled census report drift it does not have, and would let the report's
// own row count disagree with the number of resources in it.
func TestSnapshotNResourcesAreUniquelyIdentified(t *testing.T) {
	snap := SnapshotN("test", 5000)
	seen := make(map[string]string, len(snap.Resources))
	for _, r := range snap.Resources {
		if prev, dup := seen[r.ARN]; dup {
			t.Fatalf("duplicate ARN %s, shared by %s and %s", r.ARN, prev, r.Name)
		}
		seen[r.ARN] = r.Name
	}
}

// The synthesized accounts are declared scope, which is what gives a scaled
// run its own history bucket instead of a phantom diff against the storyboard.
// Declaring one that holds nothing would be the opposite error: an account the
// report says was scanned and shows empty.
func TestSnapshotNDeclaresExactlyTheAccountsItFills(t *testing.T) {
	snap := SnapshotN("test", 20000)
	declared := make(map[string]bool, len(snap.Accounts))
	for _, a := range snap.Accounts {
		if declared[a] {
			t.Errorf("account %s declared twice", a)
		}
		declared[a] = true
	}
	if len(declared) <= 2 {
		t.Errorf("20,000 resources in %d accounts; a scaled estate has to spread over accounts "+
			"or the per-account rollups render one bar", len(declared))
	}
	held := make(map[string]bool, len(declared))
	for _, r := range snap.Resources {
		if !declared[r.AccountID] {
			t.Fatalf("%s: resource in account %s, which the snapshot does not declare", r.ARN, r.AccountID)
		}
		held[r.AccountID] = true
	}
	for a := range declared {
		if !held[a] {
			t.Errorf("account %s is declared but holds no resource", a)
		}
	}
}

// A generated row has to report exactly the fields a hand-written one of its
// shape reports — no more, and above all no fewer. Fewer is the failure that
// matters: a budget measured against twenty thousand rows that each carry half
// a real row's fields is a budget the real report cannot meet, and the number
// it hands ATR-186 to ratchet down from would be fiction.
func TestGeneratedRowsReportTheSameFieldsAsHandWrittenOnes(t *testing.T) {
	written := Snapshot("test")
	storyboard := make(map[string]bool, len(written.Resources))
	for _, r := range written.Resources {
		storyboard[r.ARN] = true
	}

	// The union over every row of a type, since optionality is real: some
	// volumes report throughput and some cannot, and either set alone would
	// be the wrong thing to compare.
	keys := func(rows []model.Resource, want func(model.Resource) bool) map[string]map[string]bool {
		byType := make(map[string]map[string]bool)
		for _, r := range rows {
			if !want(r) {
				continue
			}
			u := byType[r.Type]
			if u == nil {
				u = make(map[string]bool)
				byType[r.Type] = u
			}
			// The core's optional fields belong in the comparison too. Which
			// of them a shape reports is not the generator's choice — Lambda
			// reports a last-modified stamp and no creation time, an elastic
			// IP reports neither — and reading the answer off the storyboard
			// keeps it right without an exemption list to maintain.
			for k, present := range map[string]bool{
				"core:status":              r.Status != "",
				"core:created_at":          r.CreatedAt != nil,
				"core:encrypted":           r.Encrypted != nil,
				"core:publicly_accessible": r.PubliclyAccessible != nil,
			} {
				if present {
					u[k] = true
				}
			}
			for k := range r.Attributes {
				u["attr:"+k] = true
			}
			for k := range r.Measures {
				u["measure:"+k] = true
			}
		}
		return byType
	}

	scaled := SnapshotN("test", 5000).Resources
	hand := keys(scaled, func(r model.Resource) bool { return storyboard[r.ARN] })
	gen := keys(scaled, func(r model.Resource) bool { return !storyboard[r.ARN] })

	for typ, want := range hand {
		got := gen[typ]
		if got == nil {
			t.Errorf("%s: the generator produced no rows of a type the storyboard has", typ)
			continue
		}
		if missing := difference(want, got); len(missing) > 0 {
			t.Errorf("%s: generated rows never report %v, which hand-written ones do",
				typ, missing)
		}
		if extra := difference(got, want); len(extra) > 0 {
			t.Errorf("%s: generated rows report %v, which no hand-written row does",
				typ, extra)
		}
	}
}

// difference returns the keys in a that are absent from b, sorted so a failure
// reads the same way twice.
func difference(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// Matching key sets still leaves room for rows that are technically complete
// and materially thinner — one-character names, empty tag maps, endpoints that
// are not the long shard-token hosts AWS hands out. Weight is what the size
// budget measures, so weight is what has to be comparable.
func TestGeneratedRowsAreNoThinnerThanHandWrittenOnes(t *testing.T) {
	written := Snapshot("test")
	storyboard := make(map[string]bool, len(written.Resources))
	for _, r := range written.Resources {
		storyboard[r.ARN] = true
	}

	var handBytes, handRows, genBytes, genRows int
	for _, r := range SnapshotN("test", 5000).Resources {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if storyboard[r.ARN] {
			handBytes, handRows = handBytes+len(b), handRows+1
		} else {
			genBytes, genRows = genBytes+len(b), genRows+1
		}
	}
	hand := float64(handBytes) / float64(handRows)
	gen := float64(genBytes) / float64(genRows)
	// One-sided on purpose: heavier generated rows only make the budget
	// stricter, and would be a fixture worth keeping.
	if gen < 0.8*hand {
		t.Errorf("generated rows average %.0f JSON bytes against %.0f hand-written; "+
			"a budget measured on rows this thin would not hold for a real census", gen, hand)
	}
}

// The split has to be exact whatever the remainder does, or SnapshotN cannot
// promise the total it was asked for.
func TestAllocateSplitsExactly(t *testing.T) {
	cases := [][]int{
		{1},
		{1, 1, 1},
		{16, 12, 10, 8, 7, 6, 5, 5, 4, 4, 4, 3, 3, 2, 1, 1, 1, 1, 1, 1},
		{100, 1},
		{0, 5, 0, 5},
	}
	for _, weights := range cases {
		for _, total := range []int{0, 1, 2, 19, 20, 97, 1000, 19902} {
			before := slices.Clone(weights)
			got := allocate(weights, total)
			if !slices.Equal(weights, before) {
				t.Fatalf("allocate(%v, %d) modified its weights to %v", before, total, weights)
			}
			sum := 0
			for i, n := range got {
				if n < 0 {
					t.Errorf("allocate(%v, %d)[%d] = %d, want no negative share", before, total, i, n)
				}
				if weights[i] == 0 && n != 0 {
					t.Errorf("allocate(%v, %d) gave %d rows to a zero weight at %d", before, total, n, i)
				}
				sum += n
			}
			if sum != total {
				t.Errorf("allocate(%v, %d) sums to %d, want %d", before, total, sum, total)
			}
		}
	}
}

// The invariants the storyboard is held to are not relaxed by volume. These
// are the same properties demo_test.go asserts on the fixture, re-run over a
// scaled estate because every one of them is a rule about the generator too.
func TestScaledSnapshotHoldsTheSameInvariantsAsTheStoryboard(t *testing.T) {
	snap := SnapshotN("test", 3000)
	regions := make(map[string]bool, len(snap.Regions))
	for _, r := range snap.Regions {
		regions[r] = true
	}
	for _, r := range snap.Resources {
		switch {
		case r.ARN == "" || r.Name == "":
			t.Errorf("%+v: a resource with no ARN or no name", r)
		case !strings.HasPrefix(r.ARN, "arn:aws:"):
			t.Errorf("%s: ARN does not start with arn:aws:", r.ARN)
		case r.Region != "" && !regions[r.Region]:
			t.Errorf("%s: region %s is not in the snapshot's scope", r.ARN, r.Region)
		}
		for k, v := range r.Attributes {
			if v == "" {
				t.Errorf("%s: attribute %q stored as an empty string, which is the absence "+
					"SetAttr exists to keep out of the bag", r.ARN, k)
			}
		}
		for k, v := range r.Measures {
			if v < 0 {
				t.Errorf("%s: measure %q = %d, which no service reports", r.ARN, k, v)
			}
		}
	}
	// Sorted, because the JSON artifact has to be byte-stable and Finalize is
	// what makes it so.
	if !slices.IsSortedFunc(snap.Resources, compareResources) {
		t.Error("scaled resources are not in Finalize's order; the JSON artifact would not be stable")
	}
	if !slices.IsSorted(snap.Accounts) || !slices.IsSorted(snap.Regions) {
		t.Error("scaled scope is unsorted")
	}
}

// compareResources mirrors the order Finalize sorts into.
func compareResources(a, b model.Resource) int {
	for _, pair := range [][2]string{
		{a.AccountID, b.AccountID}, {a.Region, b.Region},
		{a.Service, b.Service}, {a.Name, b.Name}, {a.ARN, b.ARN},
	} {
		if c := strings.Compare(pair[0], pair[1]); c != 0 {
			return c
		}
	}
	return 0
}

// The tag hygiene gaps the report's coverage metrics exist to show have to
// survive scaling. An estate where every row is fully tagged renders those
// metrics as a pair of full bars and proves nothing.
func TestScaledEstateKeepsTheStoryboardsTagGaps(t *testing.T) {
	snap := SnapshotN("test", 3000)
	var noEnv, noOwner, untagged int
	for _, r := range snap.Resources {
		if r.Environment == "" {
			noEnv++
		}
		if r.Owner == "" {
			noOwner++
		}
		if len(r.Tags) == 0 {
			untagged++
		}
	}
	total := len(snap.Resources)
	for _, c := range []struct {
		what string
		n    int
	}{{"no environment", noEnv}, {"no owner", noOwner}, {"no tags at all", untagged}} {
		if c.n == 0 {
			t.Errorf("no resource has %s; the coverage metrics have nothing to report", c.what)
		}
		if c.n == total {
			t.Errorf("every resource has %s; the coverage metrics have nothing to compare", c.what)
		}
	}
}

// A stored zero and an absent measure are different findings, and both have to
// survive at volume — the `> 0` filter is the recurring bug in this codebase,
// and a generator is a new place for it to reappear.
func TestScaledEstateCarriesBothReportedZerosAndUnreportedSizes(t *testing.T) {
	var zero, absent int
	for _, r := range SnapshotN("test", 3000).Resources {
		switch v, ok := r.Measure(model.MeasureSizeBytes); {
		case !ok:
			absent++
		case v == 0:
			zero++
		}
	}
	if zero == 0 {
		t.Error("no generated resource reports a size of zero; the stored-zero path is unexercised at scale")
	}
	if absent == 0 {
		t.Error("every generated resource reports a size; the em-dash path is unexercised at scale")
	}
}

// generatedResources returns a scaled estate with the storyboard removed, so a
// test can assert on the generator alone. The storyboard is curated, several of
// its rows are deliberate edge cases, and it is pinned by its own tests in
// demo_test.go — holding it to the generator's rules would be testing the wrong
// fixture. Three thousand rows is enough for every shape to appear many times
// over and small enough to keep `go test ./...` quick.
func generatedResources(t *testing.T) []model.Resource {
	t.Helper()
	snap := SnapshotN("test", 3000)
	hand := make(map[string]bool, len(Snapshot("test").Resources))
	for _, r := range Snapshot("test").Resources {
		hand[r.ARN] = true
	}
	out := make([]model.Resource, 0, len(snap.Resources))
	for _, r := range snap.Resources {
		if !hand[r.ARN] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		t.Fatal("no generated rows to check")
	}
	return out
}

// arnID is the identifier AWS puts at the end of an EC2-family ARN. It is where
// the id lives once a Name tag has taken over the name column.
func arnID(r model.Resource) string { return r.ARN[strings.LastIndex(r.ARN, "/")+1:] }

// A reference to a resource that is not in the census is the one thing a census
// cannot contain. It is also the failure a generator falls into by default:
// every id here is synthesized, so a volume can name an instance that reads
// perfectly and does not exist, and every report built on the fixture would
// then be showing joins that resolve to nothing.
//
// The one honest exception is a snapshot that says outright its source volume
// is gone, which is a finding rather than a dangling pointer.
func TestGeneratedEstateHasNoDanglingReferences(t *testing.T) {
	rows := generatedResources(t)
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[arnID(r)] = true
	}

	keys := []string{
		model.AttrEBSVolumeIDs, model.AttrAttachedInstanceIDs,
		model.AttrSourceVolumeID, model.AttrAssociatedWith,
	}
	checked := map[string]int{}
	for _, r := range rows {
		for _, key := range keys {
			for _, id := range strings.Split(r.Attr(key), ",") {
				if id == "" {
					continue
				}
				if key == model.AttrSourceVolumeID &&
					r.Attr(model.AttrSourceVolumeExists) == "false" {
					continue // deleted, and the row says so
				}
				checked[key]++
				if !ids[id] {
					t.Errorf("%s: %s names %s, which is in no row of the census", r.ARN, key, id)
				}
			}
		}
	}
	// Per key, not in total. A join that breaks does not usually produce a
	// dangling id — it produces no reference at all, because the peer lookup
	// finds nothing and the row comes away empty. Counting the whole set would
	// let the surviving joins cover for the broken one and this test would pass
	// on an estate where instances name no volumes whatsoever.
	for _, key := range keys {
		if checked[key] == 0 {
			t.Errorf("no %s in the generated estate: either nothing joins on it or the "+
				"join silently produces nothing, and this test cannot tell the difference", key)
		}
	}
	t.Logf("references that resolve, by key: %v", checked)
}

// A reference also has to point somewhere reachable. Two resources in different
// accounts cannot be attached to each other, and an EBS volume cannot be
// attached to an instance in another availability zone — so a census claiming
// either is describing an estate AWS would refuse to build.
func TestGeneratedReferencesStayWithinTheirScope(t *testing.T) {
	rows := generatedResources(t)
	place := make(map[string]model.Resource, len(rows))
	for _, r := range rows {
		place[arnID(r)] = r
	}
	for _, r := range rows {
		for _, key := range []string{model.AttrEBSVolumeIDs, model.AttrAttachedInstanceIDs} {
			for _, id := range strings.Split(r.Attr(key), ",") {
				peer, ok := place[id]
				if !ok {
					continue // the dangling-reference test owns that failure
				}
				if peer.AccountID != r.AccountID || peer.Region != r.Region {
					t.Errorf("%s (%s/%s) is attached to %s (%s/%s), across a boundary "+
						"no attachment crosses", r.ARN, r.AccountID, r.Region,
						id, peer.AccountID, peer.Region)
				}
				if a, b := r.Attr(model.AttrAvailabilityZone),
					peer.Attr(model.AttrAvailabilityZone); a != "" && b != "" && a != b {
					t.Errorf("%s is in %s and is attached to %s in %s; EBS attachment is "+
						"within one zone", r.ARN, a, id, b)
				}
			}
		}
	}
}

// The same pairing the storyboard is held to, applied to the generated estate:
// a value withheld because a call failed needs a ledger entry, and a ledger
// entry needs rows that are actually missing something. At scale the generator
// is what decides both halves, and deciding them in different places — a
// per-row coin toss for the value, a per-unit entry for the ledger — is how
// they come apart.
func TestGeneratedWithheldValuesAreExplainedByTheLedger(t *testing.T) {
	snap := SnapshotN("test", 3000)
	hand := make(map[string]bool)
	for _, r := range Snapshot("test").Resources {
		hand[r.ARN] = true
	}
	failed := make(map[string]bool, len(snap.Failures))
	for _, f := range snap.Failures {
		failed[f.Service+"/"+f.AccountID+"/"+f.Region] = true
	}

	var withheldLB, withheldSSE int
	for _, r := range snap.Resources {
		if hand[r.ARN] {
			continue
		}
		unit := r.Service + "/" + r.AccountID + "/" + r.Region
		switch r.Type {
		case model.TypeLoadBalancerV2:
			// DescribeTargetGroups is one call per region: it fails for every
			// load balancer in the unit or for none of them.
			_, has := r.Measure(model.MeasureTargetGroupCount)
			switch {
			case !has && !failed[unit]:
				t.Errorf("%s: no target_group_count, but nothing failed in %s", r.ARN, unit)
			case !has:
				withheldLB++
			case failed[unit]:
				t.Errorf("%s: target_group_count reported in a unit whose "+
					"DescribeTargetGroups the ledger says failed", r.ARN)
			}
		case model.TypeS3Bucket:
			has := r.Attr(model.AttrSSEAlgorithm) != ""
			switch {
			case !has && !failed[unit]:
				t.Errorf("%s: no sse_algorithm, but nothing failed in %s — a bucket with "+
					"no encryption reports AES256, not nothing", r.ARN, unit)
			case !has:
				withheldSSE++
			case failed[unit]:
				t.Errorf("%s: sse_algorithm reported in a unit whose GetBucketEncryption "+
					"the ledger says failed", r.ARN)
			}
		case model.TypeEBSSnapshot:
			// A failed DescribeVolumes costs the unit its volume rows, and the
			// generator fills every ordinal in every shape, so it cannot
			// produce that. The withheld verdict stays the storyboard's to
			// carry; a generated snapshot always answers.
			if _, has := r.Attributes[model.AttrSourceVolumeExists]; !has {
				t.Errorf("%s: no source_volume_exists, and the generator has no failure "+
					"that could explain one", r.ARN)
			}
		}
	}
	if withheldLB == 0 || withheldSSE == 0 {
		t.Errorf("withheld rows: %d load balancers, %d buckets — the generated estate "+
			"should exercise both, or the ledger panel renders against nothing",
			withheldLB, withheldSSE)
	}

	// And the other direction: an entry for a unit holding no rows of the
	// service is a call that was never made and therefore never failed.
	holds := make(map[string]bool, len(snap.Resources))
	for _, r := range snap.Resources {
		holds[r.Service+"/"+r.AccountID+"/"+r.Region] = true
	}
	for _, f := range snap.Failures {
		unit := f.Service + "/" + f.AccountID + "/" + f.Region
		// A failed listing is the case where the rows are missing *because* of
		// the failure, so only the ledger entries the generator adds — those in
		// units it invented — are held to this.
		if !strings.HasPrefix(f.AccountID, "9") {
			continue
		}
		if !holds[unit] {
			t.Errorf("ledger reports %s failing in %s/%s, a unit with no rows of that "+
				"service: the call it names was never made", f.Service, f.AccountID, f.Region)
		}
	}
}

// Synthesized ids have to be the width AWS uses. The hash they come from is
// sixty-four bits, so a format that pads to a minimum width and never truncates
// silently produces twenty-four-digit ids — recognisably not AWS's, and wrong
// in the direction that inflates every artifact the budget measures.
func TestGeneratedIDsAreShapedLikeAWSs(t *testing.T) {
	shape := regexp.MustCompile(`^(i|vol|snap|nat|eni|eipalloc)-[0-9a-f]{17}$`)
	seen := 0
	for _, r := range generatedResources(t) {
		id := arnID(r)
		if !strings.Contains(id, "-") || strings.HasPrefix(id, "arn") {
			continue
		}
		if prefix, _, ok := strings.Cut(id, "-"); ok {
			switch prefix {
			case "i", "vol", "snap", "nat", "eni", "eipalloc":
				seen++
				if !shape.MatchString(id) {
					t.Errorf("%s: id %q is not AWS-shaped (seventeen hex digits)", r.ARN, id)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no synthesized EC2-family ids in the generated estate")
	}
}

// A public IPv4 is the billable unit and is unique at any instant, so two rows
// holding the same one are two rows the census cannot tell apart — and the
// per-address cost would be counted twice. The generator hands them out rather
// than drawing them for exactly this reason.
func TestGeneratedPublicIPsAreUnique(t *testing.T) {
	holder := map[string]string{}
	for _, r := range generatedResources(t) {
		ip := r.Attr(model.AttrPublicIP)
		if ip == "" {
			continue
		}
		if first, dup := holder[ip]; dup {
			t.Errorf("%s is on both %s and %s", ip, first, r.ARN)
			continue
		}
		holder[ip] = r.ARN
	}
	if len(holder) == 0 {
		t.Fatal("no public addresses in the generated estate")
	}
	t.Logf("%d distinct public addresses", len(holder))
}

// The DNS name and the ARN carry the same identifier, because in AWS they are
// one identifier. Where the region sits in the name is the other half: AWS
// never reconciled the two generations, so a network load balancer answers on
// elb.<region>.amazonaws.com and an application one on <region>.elb.amazonaws.com.
func TestGeneratedLoadBalancerDNSAgreesWithItsARN(t *testing.T) {
	seen := 0
	for _, r := range generatedResources(t) {
		if r.Type != model.TypeLoadBalancerV2 {
			continue
		}
		seen++
		dns, suffix := r.Attr(model.AttrEndpoint), arnID(r)
		if !strings.Contains(dns, suffix) {
			t.Errorf("%s: DNS name %q does not carry the ARN's %q", r.ARN, dns, suffix)
		}
		want := r.Region + ".elb.amazonaws.com"
		if r.Attr(model.AttrLoadBalancerType) == "network" {
			want = "elb." + r.Region + ".amazonaws.com"
		}
		if !strings.HasSuffix(dns, want) {
			t.Errorf("%s: DNS name %q does not end in %q", r.ARN, dns, want)
		}
	}
	if seen == 0 {
		t.Fatal("no generated v2 load balancers")
	}
}

// Ensure the word lists the names are drawn from stay distinct, since a
// duplicate would silently bias the draw.
func TestGeneratorWordListsHaveNoDuplicates(t *testing.T) {
	for name, list := range map[string][]string{
		"genApps": genApps, "genRoles": genRoles,
		"genEnvs": genEnvs, "genOwners": genOwners,
		"genLambdaRuntimes": genLambdaRuntimes,
	} {
		seen := make(map[string]bool, len(list))
		for _, w := range list {
			if seen[w] {
				t.Errorf("%s: %q appears twice", name, w)
			}
			seen[w] = true
		}
	}
}
