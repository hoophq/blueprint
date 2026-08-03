package render

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// The report's data block, at census scale.
//
// The obvious embedding — the snapshot's own JSON, one object per resource —
// costs 588 bytes per resource, which is some 29 MB at 50k resources and a
// browser that spends its first seconds parsing repetition. Every row restates
// the same field names, the same dozen region strings, the same handful of
// account IDs, and an ARN that spells out the service, region, account and
// name already sitting in the row.
//
// So the census travels transposed. Each field becomes one column; string
// columns are dictionary-encoded, so "us-east-1" is stored once and referenced
// by index; the ARN keeps only the two pieces that cannot be derived from the
// rest of the row. That is 397 bytes per resource before compression, and 58
// after — small enough that gzip and base64 fit the whole thing inside a
// single HTML file. Those three figures are read off one measurement, the
// table on budgetBytesPerResource in budget_test.go, which is where to change
// them.
//
// The encoding is lossless, and that is not a nice-to-have. The honesty
// guardrails turn on the difference between "the service reported zero" and
// "the service reported nothing", and a transpose is precisely the operation
// that loses that distinction: a column that pads a missing cell with a zero
// value has just invented a reading. Every column below therefore takes
// presence as an explicit argument rather than inferring it from the value,
// every column is checked to hold exactly one cell per row, and
// payload_test.go decodes the block back into a Snapshot and requires it to
// marshal byte-for-byte identically to the one that went in.

// payloadWire versions the data block's own encoding, independently of
// model.SchemaVersion. The two move for different reasons: the snapshot schema
// can be untouched while the way the report packs it changes, and vice versa.
// The template refuses a block whose wire version it does not recognise rather
// than decoding it wrongly, which is the same bargain --compare strikes with
// SchemaVersion.
const payloadWire = 1

// Column key namespaces. Tags and attributes share one map of string columns
// and both carry keys chosen by somebody else — AWS for attributes, the
// account owner for tags — so neither can be trusted not to collide with a
// core field name. Prefixing them fixes that in one direction; the fact that
// no core field name contains a colon fixes it in the other, so a reader
// splits on the first colon and treats "no prefix" as core.
const (
	colTagPrefix  = "tag:"
	colAttrPrefix = "attr:"
)

// Core column names. Measures need none: they are the only occupants of the
// numeric map, so they keep their AWS-given keys unprefixed.
const (
	colARN             = "arn"
	colARNHead         = "arn_head"
	colARNInfix        = "arn_infix"
	colService         = "service"
	colType            = "type"
	colName            = "name"
	colStatus          = "status"
	colRegion          = "region"
	colAccountID       = "account_id"
	colEnvironment     = "environment"
	colOwner           = "owner"
	colEOLDate         = "eol_date"
	colCreatedAt       = "created_at"
	colCostUnavailable = "cost_unavailable"

	colEOL       = "eol"
	colPublic    = "publicly_accessible"
	colEncrypted = "encrypted"
)

// reportMeta is the small block: everything the page needs to paint its
// headline before it has looked at a single resource.
//
// It is kept out of the census block, and uncompressed, for three reasons.
// Decoding the census is asynchronous once it is gzipped, and the KPI strip,
// the attribution bar and the failure ledger have no reason to wait for it.
// The ledger and the cost rollup are the parts of the artifact somebody may
// want to read straight out of the file with grep. And none of it is large
// enough for compression to be worth the indirection — it is a few kilobytes
// beside a census of megabytes.
type reportMeta struct {
	Wire        int       `json:"wire"`
	Schema      int       `json:"schema"`
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Accounts    []string  `json:"accounts"`
	Regions     []string  `json:"regions"`
	Services    []string  `json:"services,omitempty"`
	// Encoding names how the census block is written: encodingJSON or
	// encodingGzip. The page reads this rather than sniffing the block, so an
	// encoding it does not know about is refused instead of misparsed.
	Encoding     string                    `json:"encoding"`
	Failures     []model.Failure           `json:"failures,omitempty"`
	Cost         *model.CostReport         `json:"cost,omitempty"`
	ResourceCost *model.ResourceCostReport `json:"resource_cost,omitempty"`
	Summary      reportSummary             `json:"summary"`
}

// Census block encodings.
const (
	// encodingJSON is the block written verbatim: still transposed, but plain
	// text, so a small report stays readable and greppable in the file.
	encodingJSON = "json"
	// encodingGzip is the block gzipped and base64'd, decoded in the browser by
	// the native DecompressionStream — no library, no external load.
	encodingGzip = "gzip+base64"
)

// resourceTable is the census, transposed. The three maps are kept apart by
// value kind rather than merged behind a tagged union so the reader knows how
// to read a column from the map it came out of, with no per-column type byte.
type resourceTable struct {
	N    int                      `json:"n"`
	Str  map[string]*stringColumn `json:"str,omitempty"`
	Num  map[string]*numberColumn `json:"num,omitempty"`
	Bool map[string]*boolColumn   `json:"bool,omitempty"`
	// Costs stays a dense array of the model's own structs. Per-resource cost
	// entries are nested, few, and carry decimal strings that must survive
	// untouched; columnarising them would buy little and add a second place
	// money could be reshaped. Absent for a census nobody priced.
	Costs [][]model.ResourceCost `json:"costs,omitempty"`
}

// stringColumn is a dictionary-encoded column. Idx holds one entry per row: an
// index into Dict, or absentIdx when the row does not carry the field. That
// distinction is the reason for the sentinel rather than an empty-string
// dictionary entry — a tag whose value really is "" is a different fact from a
// tag that was never set, and the report renders them differently.
type stringColumn struct {
	Dict []string `json:"d"`
	Idx  []int    `json:"i"`

	// seen indexes Dict during encoding. Unexported, so it never reaches the
	// wire and never comes back from it.
	seen map[string]int
}

// absentIdx marks a row that does not carry the column's field.
const absentIdx = -1

func newStringColumn(rows int) *stringColumn {
	return &stringColumn{Idx: make([]int, 0, rows), seen: map[string]int{}}
}

// Append records one row's cell. present is the caller's answer to "did the
// service report this field?" and is the only way to write an absent cell:
// there is deliberately no single-argument Append that would let "" quietly
// stand in for "not reported".
func (c *stringColumn) Append(v string, present bool) {
	if !present {
		c.Idx = append(c.Idx, absentIdx)
		return
	}
	i, ok := c.seen[v]
	if !ok {
		i = len(c.Dict)
		c.Dict = append(c.Dict, v)
		c.seen[v] = i
	}
	c.Idx = append(c.Idx, i)
}

func (c *stringColumn) len() int { return len(c.Idx) }

// numberColumn is a dense column of measures. A nil cell is "not reported"; a
// stored zero is a reading and survives as 0.
type numberColumn []*int64

func newNumberColumn(rows int) *numberColumn {
	c := make(numberColumn, 0, rows)
	return &c
}

// Append records one row's measure. As with stringColumn, presence is passed
// in rather than inferred — inferring it from the value is exactly the "> 0"
// bug that reclassifies a reported zero as silence.
func (c *numberColumn) Append(v int64, present bool) {
	if !present {
		*c = append(*c, nil)
		return
	}
	*c = append(*c, &v)
}

func (c *numberColumn) len() int { return len(*c) }

// boolColumn carries the tri-state exposure flags: true, false, or a nil cell
// for a service that does not report the field at all.
type boolColumn []*bool

func newBoolColumn(rows int) *boolColumn {
	c := make(boolColumn, 0, rows)
	return &c
}

func (c *boolColumn) Append(v bool, present bool) {
	if !present {
		*c = append(*c, nil)
		return
	}
	*c = append(*c, &v)
}

func (c *boolColumn) len() int { return len(*c) }

// encodeARN takes an ARN apart into the pieces the reader cannot already see,
// and verifies the reassembly before discarding anything.
//
// Every ARN in the census repeats its partition, its service namespace, its
// region and its account — the last two verbatim from columns the row already
// has. Dropping that repetition is where much of the saving comes from, and it
// is also exactly where a codec can quietly emit an ARN that AWS never issued.
//
// So nothing here derives an ARN from a per-type pattern table. It splits the
// real ARN, keeps the head ("arn:aws:rds") and the resource-part prefix
// ("db:", "cluster:", "table/") as dictionary entries, rebuilds the ARN from
// those plus the row's own region, account and name, and only reports success
// when the rebuild is byte-identical to the original. The head absorbs both
// the partition and the fact that an ARN's service namespace is often not
// Resource.Service — aurora lives under rds, EBS volumes and NAT gateways
// under ec2, load balancers under elasticloadbalancing — with no table to
// maintain. Anything that does not round-trip (too few fields, a region that
// disagrees with the row, a resource part that does not end in the name) is
// reported as not reconstructible and stored verbatim instead.
func encodeARN(r *model.Resource) (head, infix string, ok bool) {
	// An ARN is arn:partition:service:region:account-id:resource, and the
	// resource part is the only one allowed to contain further colons.
	parts := strings.SplitN(r.ARN, ":", 6)
	if len(parts) != 6 {
		return "", "", false
	}
	if parts[3] != r.Region || parts[4] != r.AccountID {
		return "", "", false
	}
	resource := parts[5]
	if !strings.HasSuffix(resource, r.Name) {
		return "", "", false
	}
	head = parts[0] + ":" + parts[1] + ":" + parts[2]
	infix = resource[:len(resource)-len(r.Name)]
	// Redundant given the construction directly above, and deliberately so:
	// this is the check that keeps encodeARN and decodeARN in agreement if
	// either of them is ever edited alone.
	if decodeARN(head, infix, r.Region, r.AccountID, r.Name) != r.ARN {
		return "", "", false
	}
	return head, infix, true
}

// decodeARN is the reader's half of encodeARN, kept beside it so the two are
// read and changed together. The template carries the same three lines in JS.
func decodeARN(head, infix, region, accountID, name string) string {
	return head + ":" + region + ":" + accountID + ":" + infix + name
}

// newReportMeta folds the snapshot's headline into the small block.
func newReportMeta(snap *model.Snapshot, encoding string) reportMeta {
	return reportMeta{
		Wire:         payloadWire,
		Schema:       snap.Schema,
		Version:      snap.Version,
		GeneratedAt:  snap.GeneratedAt,
		Accounts:     snap.Accounts,
		Regions:      snap.Regions,
		Services:     snap.Services,
		Encoding:     encoding,
		Failures:     snap.Failures,
		Cost:         snap.Cost,
		ResourceCost: snap.ResourceCost,
		Summary:      buildSummary(snap),
	}
}

func buildTable(resources []model.Resource) (resourceTable, error) {
	n := len(resources)
	tagKeys := unionKeys(resources, func(r *model.Resource) map[string]string { return r.Tags })
	attrKeys := unionKeys(resources, func(r *model.Resource) map[string]string { return r.Attributes })
	measureKeys := unionKeys(resources, func(r *model.Resource) map[string]int64 { return r.Measures })

	str := map[string]*stringColumn{}
	num := map[string]*numberColumn{}
	bl := map[string]*boolColumn{}
	strCol := func(key string) *stringColumn {
		c := newStringColumn(n)
		str[key] = c
		return c
	}
	numCol := func(key string) *numberColumn {
		c := newNumberColumn(n)
		num[key] = c
		return c
	}
	boolCol := func(key string) *boolColumn {
		c := newBoolColumn(n)
		bl[key] = c
		return c
	}

	arn := strCol(colARN)
	arnHead := strCol(colARNHead)
	arnInfix := strCol(colARNInfix)
	service := strCol(colService)
	typ := strCol(colType)
	name := strCol(colName)
	status := strCol(colStatus)
	region := strCol(colRegion)
	account := strCol(colAccountID)
	environment := strCol(colEnvironment)
	owner := strCol(colOwner)
	eolDate := strCol(colEOLDate)
	createdAt := strCol(colCreatedAt)
	costUnavailable := strCol(colCostUnavailable)

	eol := boolCol(colEOL)
	public := boolCol(colPublic)
	encrypted := boolCol(colEncrypted)

	tagCols := make([]*stringColumn, len(tagKeys))
	for i, k := range tagKeys {
		tagCols[i] = strCol(colTagPrefix + k)
	}
	attrCols := make([]*stringColumn, len(attrKeys))
	for i, k := range attrKeys {
		attrCols[i] = strCol(colAttrPrefix + k)
	}
	measureCols := make([]*numberColumn, len(measureKeys))
	for i, k := range measureKeys {
		measureCols[i] = numCol(k)
	}

	var costs [][]model.ResourceCost
	priced := false

	for i := range resources {
		r := &resources[i]

		head, infix, reconstructible := encodeARN(r)
		// Exactly one of the two representations is stored per row, so a reader
		// that finds a verbatim ARN uses it and never has to decide which
		// source wins.
		arn.Append(r.ARN, !reconstructible)
		arnHead.Append(head, reconstructible)
		arnInfix.Append(infix, reconstructible)

		// The core's own string fields are omitempty in the snapshot, so ""
		// already means "not reported" there and the two encodings agree.
		service.Append(r.Service, r.Service != "")
		typ.Append(r.Type, r.Type != "")
		name.Append(r.Name, r.Name != "")
		status.Append(r.Status, r.Status != "")
		region.Append(r.Region, r.Region != "")
		account.Append(r.AccountID, r.AccountID != "")
		environment.Append(r.Environment, r.Environment != "")
		owner.Append(r.Owner, r.Owner != "")
		eolDate.Append(r.EOLDate, r.EOLDate != "")
		costUnavailable.Append(r.CostUnavailable, r.CostUnavailable != "")

		stamp, err := encodeTime(r.CreatedAt)
		if err != nil {
			return resourceTable{}, fmt.Errorf("resource %s: created_at: %w", r.ARN, err)
		}
		createdAt.Append(stamp, r.CreatedAt != nil)

		// EOL is a plain bool, always reported; the other two are tri-state
		// pointers where nil means the service does not expose the concept.
		eol.Append(r.EOL, true)
		public.Append(deref(r.PubliclyAccessible), r.PubliclyAccessible != nil)
		encrypted.Append(deref(r.Encrypted), r.Encrypted != nil)

		for j, k := range tagKeys {
			v, ok := r.Tags[k]
			tagCols[j].Append(v, ok)
		}
		for j, k := range attrKeys {
			v, ok := r.Attributes[k]
			attrCols[j].Append(v, ok)
		}
		for j, k := range measureKeys {
			v, ok := r.Measures[k]
			measureCols[j].Append(v, ok)
		}

		costs = append(costs, r.Costs)
		if len(r.Costs) > 0 {
			priced = true
		}
	}

	table := resourceTable{N: n, Str: str, Num: num, Bool: bl}
	if priced {
		table.Costs = costs
	}
	// A column short or long by one cell would not fail loudly — it would shift
	// every row after the gap and attach one resource's value to another, which
	// is a worse honesty failure than the absent-vs-zero case the Append
	// signatures guard. Checking the lengths is the cheapest way to make that
	// impossible to ship.
	if err := table.checkLengths(); err != nil {
		return resourceTable{}, err
	}
	return table, nil
}

func (t resourceTable) checkLengths() error {
	for key, c := range t.Str {
		if c.len() != t.N {
			return fmt.Errorf("payload: string column %q has %d cells for %d resources", key, c.len(), t.N)
		}
	}
	for key, c := range t.Num {
		if c.len() != t.N {
			return fmt.Errorf("payload: number column %q has %d cells for %d resources", key, c.len(), t.N)
		}
	}
	for key, c := range t.Bool {
		if c.len() != t.N {
			return fmt.Errorf("payload: bool column %q has %d cells for %d resources", key, c.len(), t.N)
		}
	}
	if t.Costs != nil && len(t.Costs) != t.N {
		return fmt.Errorf("payload: cost column has %d cells for %d resources", len(t.Costs), t.N)
	}
	return nil
}

// unionKeys collects every key any resource carries in one of the open bags,
// sorted so the column set — and therefore the artifact — is deterministic.
func unionKeys[V any](resources []model.Resource, bag func(*model.Resource) map[string]V) []string {
	seen := map[string]struct{}{}
	for i := range resources {
		for k := range bag(&resources[i]) {
			seen[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// encodeTime renders a timestamp the way encoding/json would, minus the
// quotes, so decoding it back through time.UnmarshalJSON reproduces the
// original bytes — offset and all — rather than a UTC-normalised near-miss.
func encodeTime(t *time.Time) (string, error) {
	if t == nil {
		return "", nil
	}
	b, err := t.MarshalJSON()
	if err != nil {
		return "", err
	}
	return string(b[1 : len(b)-1]), nil
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
