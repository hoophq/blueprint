package demo

import (
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scanners"
)

// SnapshotN returns the storyboard grown to exactly n resources.
//
// n is the total, not an addition: the storyboard renders first and the
// generator fills whatever is left, so any n at or below the storyboard's own
// size returns the storyboard unchanged and `scan --demo` stays instant. The
// storyboard is never edited or dropped — the tests name rows in it, and a
// scale run has to keep every rendering path they cover.
//
// The generated rows go through the same constructors the storyboard uses, so
// they inherit its shape fidelity: a row is thin here only where the service
// it stands for reports nothing, never because a generator could not be
// bothered. That is the whole point of scaling this way. A report measured
// against 20,000 rows that are each half the weight of a real one would give
// the size budget a number it can meet and the real output cannot.
func SnapshotN(version string, n int) *model.Snapshot {
	snap := Snapshot(version)
	want := n - len(snap.Resources)
	if want <= 0 {
		return snap
	}

	// The split is decided before anything is built, because the makers consult
	// it: a row that names a peer has to know whether the run generates one.
	counts := allocate(makerWeights(snap.Resources), want)
	g := newGen(counts, want, snap.Regions)

	rows := make([]model.Resource, 0, want)
	uid := 0
	for i, count := range counts {
		for ord := range count {
			rows = append(rows, makers[i].build(g, slot{ord: ord, uid: uid}))
			uid++
		}
	}

	// The synthesized accounts join the declared scope, which is what makes a
	// scaled run its own history bucket: ScopeKey hashes the account list, so
	// a 20,000-row run never diffs against a storyboard run and reports 19,902
	// resources as new. Finalize sorts these; it does not derive them.
	snap.Accounts = append(snap.Accounts, g.accounts...)
	snap.Failures = append(snap.Failures, g.ledger(rows, snap.GeneratedAt)...)
	snap.Resources = append(snap.Resources, applyExposure(rows)...)
	snap.Finalize()
	return snap
}

// demoScaleSeed fixes the generator's stream.
//
// The source is explicit for a reason the global one cannot satisfy: Go seeds
// math/rand's global source randomly at startup, so a generator drawing from
// it would produce a different estate on every run — the artifacts would stop
// being reproducible, the size budget would measure a different report each
// time it ran, and every scaled scan would diff against the last one as if
// the whole estate had been replaced overnight.
const demoScaleSeed = 187

// rowsPerGeneratedAccount sets how many accounts the generator invents, at
// roughly the storyboard's own density of resources per account. The estate
// has to stay plausible at the top of the range: 20,000 resources in two
// accounts is not an estate anyone has, and the per-account rollups in the
// report would render one bar and nothing to compare it against.
const rowsPerGeneratedAccount = 50

// slot is where a generated row sits, and it carries two indices because they
// do two different jobs.
//
// ord is the row's index within its own shape, and it is what makes the estate
// join up. Placement and identifiers are pure functions of it, so the instance,
// the volume and the snapshot drawn at ordinal 40 land in the same account and
// region and can name each other without any of them having run first. A draw
// from the random source could not do that: each maker runs its own stretch of
// the stream, so a volume can never see the coin its instance tossed.
//
// uid is the row's index across the whole run, and it is what keeps the names
// apart. Several shapes share an ARN namespace — an Aurora, a DocumentDB and a
// Neptune cluster are all arn:aws:rds:…:cluster: — and for those the name is
// the whole of the identity, so two shapes drawing the same two words at the
// same ordinal would collide on the key the diff matches by.
type slot struct{ ord, uid int }

// gen carries the generator's draw state. Placement comes from the ordinal and
// values come from the random source: an estate spread evenly over its accounts
// and regions is what a real one looks like, and leaving that to chance would
// leave some accounts empty at small n.
type gen struct {
	r        *rand.Rand
	accounts []string
	regions  []string
	// counts is how many rows of each resource type this run generates, so a
	// row that wants to name a peer can ask whether the peer exists.
	counts map[string]int
	// failed is the set of scan units that lose a call, and units is the same
	// set in the order the ledger reports them.
	failed map[string]bool
	units  []failedUnit
	ipNext int
}

func newGen(counts []int, rows int, regions []string) *gen {
	n := max(rows/rowsPerGeneratedAccount, 1)
	accounts := make([]string, n)
	for i := range accounts {
		// Twelve digits, in a range the fixture's own accounts do not occupy.
		accounts[i] = fmt.Sprintf("9%011d", i+1)
	}
	g := &gen{
		r:        rand.New(rand.NewSource(demoScaleSeed)),
		accounts: accounts,
		regions:  append([]string(nil), regions...),
		counts:   make(map[string]int, len(makers)),
		failed:   make(map[string]bool),
	}
	for i, c := range counts {
		g.counts[makers[i].typ] = c
	}
	g.pickFailedUnits()
	return g
}

// place spreads row ord over the accounts and regions.
//
// The region cycles fastest and the account advances one step per full turn of
// it. That order matters, because the shapes have very different row counts: if
// the account cycled fastest, a shape with two hundred rows would sit in a
// single region while a shape with three thousand filled all four. Cycling the
// region first puts every shape in every region and still reaches every
// account, since the shape with the most rows walks the account list furthest.
//
// Being a pure function of the ordinal is the other half of it. Two shapes
// drawn at the same ordinal are in the same account, region and zone by
// construction, which is what lets an instance name its volume.
func (g *gen) place(ord int) (account, region, az string) {
	region = g.regions[ord%len(g.regions)]
	account = g.accounts[(ord/len(g.regions))%len(g.accounts)]
	return account, region, region + string(rune('a'+ord%3))
}

// pick draws one element. It is a function rather than a method because Go
// does not allow type parameters on methods, and a second int-only copy of
// three lines would be worse than the slightly heavier call.
func pick[T any](g *gen, ss []T) T { return ss[g.r.Intn(len(ss))] }

// chance reports true pct percent of the time.
func (g *gen) chance(pct int) bool { return g.r.Intn(100) < pct }

// between returns a value in [lo, hi].
func (g *gen) between(lo, hi int) int { return lo + g.r.Intn(hi-lo+1) }

// created draws a creation date across the years the storyboard spans, so the
// report's age columns and the EOL flags have a spread to work with.
func (g *gen) created() time.Time {
	return d(g.between(2016, 2025), g.between(1, 12), g.between(1, 28))
}

// name builds a resource name. The run-wide index is part of it rather than a
// hash of it: names must not collide across shapes, since for most services the
// name is what makes the ARN unique, and a counter makes that a property of the
// construction instead of a hope about a hash.
func (g *gen) name(uid int) string {
	return pick(g, genApps) + "-" + pick(g, genRoles) + "-" + strconv.Itoa(uid)
}

// tags reproduces the tag hygiene the storyboard documents — roughly a fifth
// of resources with no environment and a third with no owner — because the
// report's coverage metrics exist to show exactly that, and a fully tagged
// estate would render them as a pair of full bars. A row with nothing at all
// gets a nil map, which is what an untagged resource returns.
func (g *gen) tags(nameTag string) map[string]string {
	if nameTag == "" && g.chance(8) {
		return nil
	}
	kv := make([]string, 0, 6)
	if nameTag != "" {
		kv = append(kv, "Name", nameTag)
	}
	if !g.chance(20) {
		kv = append(kv, "environment", pick(g, genEnvs))
	}
	if !g.chance(30) {
		kv = append(kv, "owner", pick(g, genOwners))
	}
	if len(kv) == 0 {
		return nil
	}
	return tags(kv...)
}

// id builds a unique AWS-shaped identifier: seventeen hex digits after the
// prefix, which is the width EC2 has used since 2016. The ordinal occupies the
// low digits, so uniqueness within a kind is guaranteed by construction rather
// than left to a hash; the high digits are hashed from the kind so two kinds do
// not read as siblings numbered off the same sequence.
func (g *gen) id(prefix, kind string, ord int) string {
	return fmt.Sprintf("%s%s%08x", prefix, synthID("", kind, 9), ord)
}

// peer names the row of another shape drawn at this ordinal, or the empty
// string when the run generates no such row. Same ordinal means same account,
// same region and same zone, so the reference resolves to something that exists
// and sits where it ought to — and where the shape distribution runs out, the
// caller gets nothing rather than a plausible-looking name with no row behind
// it. Which of the two happens is the same on both sides of the reference,
// because both sides compute it the same way.
func (g *gen) peer(prefix, kind, typ string, ord int) string {
	if ord >= g.counts[typ] {
		return ""
	}
	return g.id(prefix, kind, ord)
}

// ipv4 hands out the next public address, from 198.18.0.0/15 — the block RFC
// 2544 reserves for benchmarking and nobody routes.
//
// The addresses are handed out rather than drawn because a public IPv4 is the
// billable unit and is unique at any instant: two rows sharing one would be two
// rows the census cannot tell apart. The three documentation /24s the
// storyboard writes down hold 762 addresses between them, which a scaled estate
// would exhaust inside its first thousand NAT gateways.
//
// The stride is odd, so it is coprime with the size of the block and walks
// every address in it exactly once before repeating. That keeps the guarantee
// while scattering the addresses, which matters only because a column of
// consecutive ones would read as generated at a glance.
func (g *gen) ipv4() string {
	const block = 1 << 17 // 198.18.0.0/15
	n := (g.ipNext * 40503) % block
	g.ipNext++
	return fmt.Sprintf("198.%d.%d.%d", 18+(n>>16), (n>>8)&0xff, n&0xff)
}

// privateDNS builds the internal hostname EC2 hands an instance. us-east-1 is
// the exception AWS never tidied up: it answers ec2.internal there and
// <region>.compute.internal everywhere else.
func privateDNS(region string, ord int) string {
	suffix := region + ".compute.internal"
	if region == "us-east-1" {
		suffix = "ec2.internal"
	}
	return fmt.Sprintf("ip-10-%d-%d-%d.%s", (ord>>16)&0xff, (ord>>8)&0xff, ord&0xff, suffix)
}

// failedUnit is one account, region and service the generator loses a call in.
type failedUnit struct {
	service, account, region string
	// typ is the resource type whose rows withhold a value because of it. The
	// unit only earns a ledger entry when the run actually put rows of that
	// type in it.
	typ string
	// errorFor renders the ledger message. It takes the row count because a
	// per-object call names how many objects it was refused on.
	errorFor func(rows int) string
}

// genFailures are the calls the generator lets fail.
//
// A census that never loses a call is not the census anyone runs: the ledger,
// and the withheld values that pair with it, are load-bearing parts of the
// report, and at scale they need more than the storyboard's five entries to
// render against.
//
// Both of these are calls that fail *around* a listing that worked, which is
// what makes them expressible here. The rows are still returned; one value on
// them is not. A failure that costs the listing itself — DescribeVolumes, say —
// would have to remove rows from a unit, and the generator cannot: every
// ordinal it fills is filled in every shape at once. That is why the withheld
// orphan verdict stays the storyboard's to carry, and why no snapshot generated
// here declines the question.
var genFailures = []struct {
	service  string
	typ      string
	errorFor func(rows int) string
}{
	{model.ServiceELB, model.TypeLoadBalancerV2, func(int) string {
		return "ThrottlingException: Rate exceeded (elasticloadbalancing:DescribeTargetGroups)"
	}},
	{model.ServiceS3, model.TypeS3Bucket, func(rows int) string {
		return fmt.Sprintf("GetBucketEncryption: %d of %d buckets: AccessDenied: "+
			"User is not authorized to perform s3:GetEncryptionConfiguration", rows, rows)
	}},
}

// pickFailedUnits chooses where the calls in genFailures fail.
//
// The choice is per scan unit, and that is the whole point of making it here
// rather than inside a maker. One account, one region, one service is what a
// scan loses, and every row it covered is affected together; a withheld value
// scattered row by row through a region the ledger reports as whole is a shape
// no scan produces and the ledger cannot explain. Drawing it up front also
// means every maker reaches the same answer about the same unit, which it could
// not do from its own stretch of the stream.
func (g *gen) pickFailedUnits() {
	for _, f := range genFailures {
		for _, account := range g.accounts {
			for _, region := range g.regions {
				// Roughly one unit in twenty-five: enough entries at scale to
				// fill the ledger panel, few enough that the rest of the estate
				// reads as a census that mostly worked.
				if !g.chance(4) {
					continue
				}
				g.failed[f.service+"|"+account+"|"+region] = true
				g.units = append(g.units, failedUnit{
					service: f.service, account: account, region: region,
					typ: f.typ, errorFor: f.errorFor,
				})
			}
		}
	}
}

// scanFailed reports whether this service lost its call in this scan unit.
func (g *gen) scanFailed(service, account, region string) bool {
	return g.failed[service+"|"+account+"|"+region]
}

// ledger renders the generated failures as the entries a reader sees.
//
// A unit only earns one when the run put rows of the affected type in it:
// DescribeTargetGroups is made once per region where load balancers were found
// and GetBucketEncryption once per bucket, so with nothing there the call was
// never made and there is nothing to have failed.
func (g *gen) ledger(rows []model.Resource, at time.Time) []model.Failure {
	held := make(map[string]int, len(rows))
	for _, r := range rows {
		held[r.Type+"|"+r.AccountID+"|"+r.Region]++
	}
	out := make([]model.Failure, 0, len(g.units))
	for _, u := range g.units {
		n := held[u.typ+"|"+u.account+"|"+u.region]
		if n == 0 {
			continue
		}
		out = append(out, model.Failure{
			AccountID: u.account, Region: u.region, Service: u.service,
			Error: u.errorFor(n),
			// Offset past the storyboard's own entries, in the same spirit:
			// the ledger is written as units come back, not when the run began.
			Time: at.Add(time.Duration(13+len(out)) * time.Second),
		})
	}
	return out
}

// Word lists the names are drawn from. Real estates name things after the
// system they belong to, so the report's grouping and search have something
// meaningful to group and match on.
var (
	genApps = []string{
		"orders", "checkout", "inventory", "billing", "identity", "catalog",
		"shipping", "payments", "search", "analytics", "reporting", "fraud",
		"pricing", "support", "telemetry", "notifications",
	}
	genRoles = []string{
		"api", "worker", "store", "cache", "index", "stream",
		"archive", "ledger", "registry", "gateway",
	}
	genEnvs   = []string{"production", "staging", "development", "qa"}
	genOwners = []string{"platform", "payments", "data", "identity", "growth", "risk", "support"}
)

// maker builds one shape of row. The pair of service and type is also the
// predicate that counts the storyboard's rows of this shape: every resource
// carries exactly one of each, so the makers partition the storyboard by
// construction and the weights below are a census of it rather than a
// distribution written down separately and left to drift.
type maker struct {
	service string
	typ     string
	build   func(g *gen, s slot) model.Resource
}

// makerWeights counts how many storyboard rows each maker stands for. A row
// matching no maker is a shape the storyboard has and the generator cannot
// produce; TestEveryStoryboardRowHasAMaker is what catches that.
func makerWeights(rows []model.Resource) []int {
	index := make(map[string]int, len(makers))
	for i, m := range makers {
		index[m.service+"|"+m.typ] = i
	}
	w := make([]int, len(makers))
	for _, r := range rows {
		if i, ok := index[r.Service+"|"+r.Type]; ok {
			w[i]++
		}
	}
	return w
}

// allocate splits total across the weights in proportion, handing the rounding
// remainder to the largest fractional parts so the result sums to exactly
// total. Ties break on the lower index, which keeps the split deterministic
// without spending a draw on it — the estate a given n produces has to be the
// same one every time, down to how many rows of each shape it holds.
//
// A weight of zero never receives a row: a shape the storyboard does not have
// is a shape the generator has no business inventing, and floor plus remainder
// would otherwise hand it the odd one out.
func allocate(weights []int, total int) []int {
	sum := 0
	for _, w := range weights {
		sum += w
	}
	out := make([]int, len(weights))
	if sum == 0 || total <= 0 {
		return out
	}
	assigned := 0
	for i, w := range weights {
		out[i] = w * total / sum
		assigned += out[i]
	}
	// At most one extra row per weight is outstanding, so each pass takes the
	// largest unspent remainder and marks it spent. len(weights) is twenty, so
	// rescanning reads more plainly than sorting pairs of index and remainder.
	spent := make([]bool, len(weights))
	for ; assigned < total; assigned++ {
		best, bestRem := -1, 0
		for i, w := range weights {
			if rem := w * total % sum; !spent[i] && w > 0 && rem > bestRem {
				best, bestRem = i, rem
			}
		}
		if best < 0 {
			// Unreachable: the outstanding count is at most the number of
			// nonzero remainders, so one is always available while the loop
			// runs. Bailing beats spinning if that ever stops being true.
			break
		}
		out[best]++
		spent[best] = true
	}
	return out
}

// makers covers every service and type the storyboard has. Order is fixed:
// it decides which shapes absorb the rounding remainder, so shuffling it
// would change the output for a given n.
var makers = []maker{
	{model.ServiceRDS, model.TypeRDSInstance, buildRDSInstance},
	{model.ServiceAurora, model.TypeRDSCluster, buildAuroraCluster},
	{model.ServiceDocumentDB, model.TypeDocDBCluster, buildDocDBCluster},
	{model.ServiceNeptune, model.TypeNeptuneCluster, buildNeptuneCluster},
	{model.ServiceDynamoDB, model.TypeDynamoDBTable, buildDynamoTable},
	{model.ServiceElastiCache, model.TypeElastiCacheReplicationGroup, buildCacheGroup},
	{model.ServiceElastiCache, model.TypeElastiCacheCacheCluster, buildCacheCluster},
	{model.ServiceElastiCache, model.TypeElastiCacheServerlessCache, buildServerlessCache},
	{model.ServiceRedshift, model.TypeRedshiftCluster, buildRedshiftCluster},
	{model.ServiceRedshift, model.TypeRedshiftServerlessWorkgroup, buildRedshiftWorkgroup},
	{model.ServiceEC2, model.TypeEC2Instance, buildEC2Instance},
	{model.ServiceEBS, model.TypeEBSVolume, buildEBSVolume},
	{model.ServiceEBS, model.TypeEBSSnapshot, buildEBSSnapshot},
	{model.ServiceNATGateway, model.TypeNATGateway, buildNATGateway},
	{model.ServicePublicIP, model.TypeEIP, buildElasticIP},
	{model.ServicePublicIP, model.TypeNetworkInterface, buildAutoAssignedIP},
	{model.ServiceELB, model.TypeLoadBalancerV2, buildLoadBalancerV2},
	{model.ServiceELB, model.TypeLoadBalancer, buildClassicLoadBalancer},
	{model.ServiceLambda, model.TypeLambdaFunction, buildLambdaFunction},
	{model.ServiceS3, model.TypeS3Bucket, buildS3Bucket},
}

// Engine and version pairs, drawn together so no row claims a version its
// engine never had. Several are past end of life, which is what gives the
// lifecycle table something to report at scale.
var genRDSEngines = [][2]string{
	{"postgres", "16.3"}, {"postgres", "15.7"}, {"postgres", "13.15"}, {"postgres", "11.22"},
	{"mysql", "8.0.36"}, {"mysql", "5.7.44"},
	{"mariadb", "10.11.8"}, {"mariadb", "10.6.18"},
	{"oracle-se2", "19.0.0.0.ru-2024-04.rur-2024-04.r1"},
	{"sqlserver-se", "15.00.4365.2.v1"},
}

var genDBClasses = []string{
	"db.t3.medium", "db.t4g.large", "db.m5.large", "db.m6g.xlarge",
	"db.r5.large", "db.r6g.2xlarge", "db.r5.4xlarge",
}

// endpointHost builds the host AWS hands out for a managed endpoint. The
// shard token in the middle is what makes these long, and length is part of
// what the report has to carry — a fixture with tidy short endpoints would
// under-report the payload the real one produces.
func endpointHost(name, infix, region, suffix string) string {
	return name + infix + synthID("", name+region, 12) + "." + region + "." + suffix
}

func buildRDSInstance(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	ev := pick(g, genRDSEngines)
	// Storage-full and stopped instances both still bill, which is the finding
	// in each case; available is the ordinary majority.
	status := "available"
	switch {
	case g.chance(4):
		status = "stopped"
	case g.chance(3):
		status = "storage-full"
	}
	return res(account, region, model.ServiceRDS, "instance", name, ev[0], ev[1],
		pick(g, genDBClasses), storage(int32(g.between(20, 4096))), g.chance(35), status,
		endpointHost(name, ".", region, "rds.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildAuroraCluster(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	engine, version := "aurora-postgresql", "15.4"
	if g.chance(50) {
		engine, version = "aurora-mysql", "8.0.mysql_aurora.3.05.2"
	}
	// No class and no size: a cluster's writer is its own row in a real
	// account, and its storage is a shared volume DescribeDBClusters
	// attributes to nothing.
	return res(account, region, model.ServiceAurora, "cluster", name, engine, version,
		"", nil, g.chance(60), "available",
		endpointHost(name, ".cluster-", region, "rds.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildDocDBCluster(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	return res(account, region, model.ServiceDocumentDB, "cluster", name, "docdb",
		pick(g, []string{"5.0.0", "4.0.0", "3.6.0"}), pick(g, genDBClasses),
		storage(int32(g.between(50, 2048))), g.chance(40), "available",
		endpointHost(name, ".cluster-", region, "docdb.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildNeptuneCluster(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	return res(account, region, model.ServiceNeptune, "cluster", name, "neptune",
		pick(g, []string{"1.3.2.0", "1.2.1.0"}), pick(g, genDBClasses),
		storage(int32(g.between(20, 1024))), g.chance(30), "available",
		endpointHost(name, ".cluster-", region, "neptune.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildDynamoTable(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	mode := "PAY_PER_REQUEST"
	if g.chance(45) {
		mode = "PROVISIONED"
	}
	// A stored zero every so often, because DynamoDB reports one for a table
	// that is empty or newer than the roughly six-hourly refresh of the
	// figure. Dropping it would be the recurring bug this census names.
	size := int32(0)
	if !g.chance(6) {
		size = int32(g.between(1, 8192))
	}
	// No endpoint and no version: DynamoDB is regional and reports neither.
	return res(account, region, model.ServiceDynamoDB, "table", g.name(s.uid), "dynamodb", "",
		mode, storage(size), false, "active", "", g.created(), g.tags(""))
}

func buildCacheGroup(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	// No engine version: DescribeReplicationGroups does not report one, and
	// the version lives on member clusters this census skips.
	return res(account, region, model.ServiceElastiCache, "cluster", name,
		pick(g, []string{"redis", "valkey"}), "",
		pick(g, []string{"cache.t3.medium", "cache.r6g.large", "cache.m6g.xlarge"}),
		nil, g.chance(45), "available",
		endpointHost(name, ".", region, "cache.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildCacheCluster(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	return res(account, region, model.ServiceElastiCache, "node", name, "memcached",
		pick(g, []string{"1.6.17", "1.6.22"}),
		pick(g, []string{"cache.t3.small", "cache.m5.large"}),
		nil, false, "available",
		endpointHost(name, ".", region, "cache.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildServerlessCache(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	// A version but no node type: serverless is sized by usage, and
	// DescribeServerlessCaches reports the full engine version and neither an
	// encryption flag nor a retention limit. applyExposure excludes it by type.
	return res(account, region, model.ServiceElastiCache, "serverless", name,
		pick(g, []string{"redis", "valkey"}), pick(g, []string{"7.1.0", "8.0.0"}),
		"", nil, false, "available",
		endpointHost(name, "-", region, "cache.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildRedshiftCluster(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	return res(account, region, model.ServiceRedshift, "cluster", name, "redshift",
		pick(g, []string{"1.0.63269", "1.0.59596"}),
		pick(g, []string{"ra3.xlplus", "ra3.4xlarge", "dc2.large"}),
		storage(int32(g.between(100, 16384))), false, "available",
		endpointHost(name, ".", region, "redshift.amazonaws.com"), g.created(),
		g.tags(""))
}

func buildRedshiftWorkgroup(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	// Base capacity rather than a node type, and no size: a workgroup's data
	// lives in the namespace, which is a different API object.
	return withMeasure(
		res(account, region, model.ServiceRedshift, "serverless", name,
			"redshift-serverless", "", "", nil, false, "available",
			name+"."+account+"."+region+".redshift-serverless.amazonaws.com",
			g.created(), g.tags("")),
		model.MeasureBaseCapacityRPU, int64(8*g.between(1, 16)))
}

func buildEC2Instance(g *gen, s slot) model.Resource {
	account, region, az := g.place(s.ord)
	// Stopped instances keep their volumes and their addresses, which is the
	// whole reason they are worth a row.
	status := "running"
	if g.chance(12) {
		status = "stopped"
	}
	// The root volume is the volume drawn at this ordinal: same account, same
	// region, same zone, and its own row names this instance back. Where the
	// run generates fewer volumes than instances the list is empty, which is
	// what an instance-store-backed instance reports.
	var volumes []string
	if v := g.peer("vol-", "volume", model.TypeEBSVolume, s.ord); v != "" {
		volumes = []string{v}
	}
	return ec2Instance(account, region, az, g.id("i-", "instance", s.ord),
		pick(g, []string{"t3.medium", "m5.xlarge", "m6i.large", "r5.2xlarge", "c6g.xlarge"}),
		pick(g, []string{"Linux/UNIX", "Red Hat Enterprise Linux", "Windows"}),
		status, privateDNS(region, s.ord),
		g.chance(15), volumes, g.created(), g.tags(g.name(s.uid)))
}

func buildEBSVolume(g *gen, s slot) model.Resource {
	account, region, az := g.place(s.ord)
	volumeType := pick(g, []string{"gp3", "gp2", "io2", "st1", "sc1"})
	// st1 and sc1 report neither IOPS nor throughput, and gp3 is the only type
	// DescribeVolumes reports a throughput for at all. Filling those in would
	// put numbers on the page that no response contains.
	var iops, throughput *int32
	switch volumeType {
	case "gp3":
		iops, throughput = ptr(int32(3000)), ptr(int32(g.between(125, 1000)))
	case "io2":
		iops = ptr(int32(g.between(1000, 64000)))
	case "gp2":
		iops = ptr(int32(g.between(100, 16000)))
	}
	// The instance drawn at this ordinal, when the run generates one. The
	// volumes past the last instance are the unattached ones — allocated,
	// attached to nothing, billing the full per-GiB rate, which is the finding —
	// and they fall out of the shape distribution rather than out of a coin the
	// instance's own row could never have seen.
	var instances []string
	if i := g.peer("i-", "instance", model.TypeEC2Instance, s.ord); i != "" {
		instances = []string{i}
	}
	return ebsVolume(account, region, az, g.id("vol-", "volume", s.ord), volumeType,
		int32(g.between(8, 4096)), iops, throughput, g.chance(70), instances,
		g.created(), g.tags(g.name(s.uid)+"-root"))
}

func buildEBSSnapshot(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	sourceGiB := int32(g.between(8, 2048))
	// A snapshot stores only the blocks that were written, so it is smaller
	// than its source and never larger.
	full := int64(sourceGiB) << 30 * int64(g.between(20, 90)) / 100
	// Whether the source is still there follows from the id the snapshot names.
	// An orphan names a volume this run does not generate — deleted, as its own
	// row says — and the rest name the volume at their ordinal, which is right
	// there in the same account and region.
	orphan := g.chance(20)
	source := g.peer("vol-", "volume", model.TypeEBSVolume, s.ord)
	if source == "" {
		orphan = true
	}
	if orphan {
		source = g.id("vol-", "deleted-volume", s.ord)
	}
	// A snapshot that outlived its volume can still be backing an AMI, which is
	// both why it is still there and why deleting it is not free.
	var images []string
	if orphan && g.chance(25) {
		images = []string{g.id("ami-", "image", s.ord)}
	}
	return ebsSnapshot(account, region, g.id("snap-", "snapshot", s.ord),
		source, sourceGiB, full, g.chance(70), images,
		ptr(!orphan), g.created(), g.tags(g.name(s.uid)+"-daily"))
}

func buildNATGateway(g *gen, s slot) model.Resource {
	account, region, az := g.place(s.ord)
	// A private gateway holds no address, which is why the public IP is what
	// the connectivity type is derived from rather than a separate flag.
	publicIP := ""
	if !g.chance(15) {
		publicIP = g.ipv4()
	}
	return natGateway(account, region, az, g.id("nat-", "natgateway", s.ord), publicIP,
		g.created(), g.tags(g.name(s.uid)+"-egress"))
}

func buildElasticIP(g *gen, s slot) model.Resource {
	account, region, az := g.place(s.ord)
	// Allocated and attached to nothing, billing the same as an attached one:
	// since February 2024 every public IPv4 carries a charge.
	holder, subnet := "", ""
	if i := g.peer("i-", "instance", model.TypeEC2Instance, s.ord); i != "" && !g.chance(20) {
		holder, subnet = i, subnetID(account, az)
	}
	return elasticIP(account, region, g.id("eipalloc-", "eip", s.ord), g.ipv4(),
		holder, subnet, g.tags(g.name(s.uid)+"-vip"))
}

func buildAutoAssignedIP(g *gen, s slot) model.Resource {
	account, region, az := g.place(s.ord)
	return autoAssignedIP(account, region, az, g.id("eni-", "interface", s.ord),
		g.ipv4(), g.peer("i-", "instance", model.TypeEC2Instance, s.ord),
		g.tags(g.name(s.uid)))
}

func buildLoadBalancerV2(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid)
	lbType := "application"
	if g.chance(30) {
		lbType = "network"
	}
	scheme := "internet-facing"
	if g.chance(45) {
		scheme = "internal"
	}
	// Three answers, and the fixture carries all three. A positive count and a
	// zero — a load balancer billing by the hour with nowhere to send traffic —
	// both come from a listing that worked. The nil comes from one that did
	// not, and DescribeTargetGroups is a per-region call, so it fails for every
	// load balancer in the unit at once or for none of them. The ledger says
	// which units those were.
	var groups *int
	switch {
	case g.scanFailed(model.ServiceELB, account, region):
		groups = nil
	case g.chance(10):
		groups = ptr(0)
	default:
		groups = ptr(g.between(1, 4))
	}
	prefix := ""
	if scheme == "internal" {
		prefix = "internal-"
	}
	// The token is the one the ARN carries. They are one identifier in AWS, and
	// a row whose DNS name disagreed with its own ARN would be a row no console
	// shows. Where the region sits is a quirk AWS never tidied: a network load
	// balancer answers on elb.<region>.amazonaws.com and an application one on
	// <region>.elb.amazonaws.com.
	domain := region + ".elb.amazonaws.com"
	if lbType == "network" {
		domain = "elb." + region + ".amazonaws.com"
	}
	dns := prefix + name + "-" + lbSuffix(account, region, name) + "." + domain
	return loadBalancerV2(account, region, lbType, name, scheme, dns,
		g.between(2, 3), groups, g.created(), g.tags(""))
}

func buildClassicLoadBalancer(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	name := g.name(s.uid) + "-elb"
	scheme := "internet-facing"
	if g.chance(50) {
		scheme = "internal"
	}
	prefix := ""
	if scheme == "internal" {
		prefix = "internal-"
	}
	// Zero registered instances is a real reading here, not a gap: the v1 API
	// returns the instance list inline, so the row that reports none is a load
	// balancer that has been billing for nothing.
	instances := 0
	if !g.chance(20) {
		instances = g.between(1, 8)
	}
	// A classic load balancer has no ARN suffix to agree with, and its DNS
	// carries a decimal id rather than the v2 generation's hex.
	dns := fmt.Sprintf("%s%s-%d.%s.elb.amazonaws.com", prefix, name, 1_000_000_000+s.ord, region)
	return classicLoadBalancer(account, region, name, scheme, dns,
		g.between(1, 3), instances, g.created(), g.tags(""))
}

// genLambdaRuntimes spans supported and deprecated releases. The deprecated
// ones are the point: a function on a runtime AWS no longer patches is what
// the lifecycle table is for, and at scale there have to be enough of them
// to fill it.
var genLambdaRuntimes = []string{
	"python3.12", "python3.11", "python3.9", "python3.8",
	"nodejs20.x", "nodejs18.x", "nodejs16.x", "nodejs14.x",
	"java21", "java8.al2", "go1.x", "dotnet8", "ruby3.2",
}

func buildLambdaFunction(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	// An empty runtime is a container image function, which reports a package
	// type of Image and a code size of zero — its bytes are in ECR and none of
	// them count against the region's code storage quota.
	runtime, codeSize := pick(g, genLambdaRuntimes), int64(g.between(1024, 78_643_200))
	if g.chance(8) {
		runtime, codeSize = "", 0
	}
	return lambdaFunction(account, region, g.name(s.uid), runtime,
		pick(g, []string{"x86_64", "arm64"}),
		128*g.between(1, 24), pick(g, []int{3, 15, 30, 60, 300, 900}),
		codeSize, g.chance(35), g.created(), g.tags(""))
}

func buildS3Bucket(g *gen, s slot) model.Resource {
	account, region, _ := g.place(s.ord)
	// Bucket names are globally unique and carry no account or region in the
	// ARN, so the run-wide index has to be in the name itself.
	name := "acme-" + g.name(s.uid)
	// An absent algorithm is a bucket whose encryption configuration the scan
	// could not read, which is not the same as one that has none. It goes absent
	// only where the ledger says the call was refused.
	sse, bucketKey := "AES256", ptr(false)
	switch {
	case g.scanFailed(model.ServiceS3, account, region):
		sse, bucketKey = "", nil
	case g.chance(35):
		sse, bucketKey = "aws:kms", ptr(true)
	}
	versioning, mfa := "", ""
	if !g.chance(40) {
		versioning = "Enabled"
		mfa = "Disabled"
		if g.chance(10) {
			mfa = "Enabled"
		}
	}
	// A bucket with no Block Public Access configuration at all is a different
	// finding from one configured off, so nil has to reach the row.
	var access *scanners.S3PublicAccess
	policyPublic := ptr(false)
	switch {
	case g.chance(10):
		access = nil
	case g.chance(8):
		access, policyPublic = bpaPolicyLive, ptr(true)
	default:
		access = bpaLocked
	}
	return s3Bucket(account, region, name, sse, bucketKey, versioning, mfa,
		access, policyPublic, g.created(), g.tags(""))
}
