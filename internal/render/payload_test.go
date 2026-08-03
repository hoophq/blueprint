package render

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// The decoder below is deliberately a second implementation.
//
// The real decoder is the JavaScript in report.html.tmpl, and there is no way
// to call it from here. Writing a Go decoder in payload.go and testing against
// that would prove the encoder agrees with a decoder nobody runs. Writing one
// here, from the wire format rather than from the encoder's internals, means
// two independent readers — this one and the browser's — have to agree with the
// encoder, which is the property that actually matters. The two are short
// enough that keeping them in step is cheaper than the alternative.

// decodeTable rebuilds resources from the wire format.
func decodeTable(tb testing.TB, t resourceTable) []model.Resource {
	tb.Helper()
	out := make([]model.Resource, t.N)
	// The split ARN pieces are held aside until every column has been read: the
	// rebuild needs the row's region, account and name, and map iteration order
	// says nothing about when those arrive.
	head := make([]string, t.N)
	infix := make([]string, t.N)
	split := make([]bool, t.N)

	for key, col := range t.Str {
		if col.len() != t.N {
			tb.Fatalf("string column %q has %d cells for %d resources", key, col.len(), t.N)
		}
		for i, idx := range col.Idx {
			if idx == absentIdx {
				continue
			}
			if idx < 0 || idx >= len(col.Dict) {
				tb.Fatalf("string column %q row %d: dictionary index %d out of range (%d entries)",
					key, i, idx, len(col.Dict))
			}
			switch key {
			case colARNHead:
				head[i], split[i] = col.Dict[idx], true
			case colARNInfix:
				infix[i] = col.Dict[idx]
			default:
				setDecodedString(tb, &out[i], key, col.Dict[idx])
			}
		}
	}

	for key, col := range t.Num {
		for i, v := range *col {
			if v == nil {
				continue
			}
			if out[i].Measures == nil {
				out[i].Measures = map[string]int64{}
			}
			out[i].Measures[key] = *v
		}
	}

	for key, col := range t.Bool {
		for i, v := range *col {
			if v == nil {
				continue
			}
			switch key {
			case colEOL:
				out[i].EOL = *v
			case colPublic:
				out[i].PubliclyAccessible = v
			case colEncrypted:
				out[i].Encrypted = v
			default:
				tb.Fatalf("unknown bool column %q", key)
			}
		}
	}

	for i := range out {
		if i < len(t.Costs) {
			out[i].Costs = t.Costs[i]
		}
		if split[i] {
			out[i].ARN = decodeARN(head[i], infix[i], out[i].Region, out[i].AccountID, out[i].Name)
		}
	}
	return out
}

func setDecodedString(tb testing.TB, r *model.Resource, key, v string) {
	tb.Helper()
	switch {
	case strings.HasPrefix(key, colTagPrefix):
		if r.Tags == nil {
			r.Tags = map[string]string{}
		}
		r.Tags[strings.TrimPrefix(key, colTagPrefix)] = v
		return
	case strings.HasPrefix(key, colAttrPrefix):
		if r.Attributes == nil {
			r.Attributes = map[string]string{}
		}
		r.Attributes[strings.TrimPrefix(key, colAttrPrefix)] = v
		return
	}
	switch key {
	case colARN:
		r.ARN = v
	case colService:
		r.Service = v
	case colType:
		r.Type = v
	case colName:
		r.Name = v
	case colStatus:
		r.Status = v
	case colRegion:
		r.Region = v
	case colAccountID:
		r.AccountID = v
	case colEnvironment:
		r.Environment = v
	case colOwner:
		r.Owner = v
	case colEOLDate:
		r.EOLDate = v
	case colCostUnavailable:
		r.CostUnavailable = v
	case colCreatedAt:
		ts, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			tb.Fatalf("created_at %q: %v", v, err)
		}
		r.CreatedAt = &ts
	default:
		tb.Fatalf("unknown string column %q", key)
	}
}

// decodeDataBlock pulls the census block out of a rendered page and decodes it
// the way the browser would: base64, then gzip, then JSON, then the columns.
func decodeDataBlock(tb testing.TB, page string) []model.Resource {
	tb.Helper()
	var table resourceTable
	if err := json.Unmarshal(dataBlockJSON(tb, page), &table); err != nil {
		tb.Fatalf("census block JSON: %v", err)
	}
	return decodeTable(tb, table)
}

// dataBlockJSON unwraps the census block down to the table JSON, stopping short
// of decoding the columns. The JS decoder test hands these bytes to node, which
// is the only way to run the decoder that ships.
func dataBlockJSON(tb testing.TB, page string) []byte {
	tb.Helper()
	raw := unescapeLessThan(dataBlock(tb, page))
	body := []byte(raw)
	if metaBlock(tb, page).Encoding == encodingGzip {
		packed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			tb.Fatalf("census block is not valid base64: %v", err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(packed))
		if err != nil {
			tb.Fatalf("census block is not valid gzip: %v", err)
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			tb.Fatalf("census block gzip: %v", err)
		}
		if err := zr.Close(); err != nil {
			tb.Fatalf("census block gzip close: %v", err)
		}
	}
	return body
}

// metaBlock pulls the headline block back out of a rendered page.
func metaBlock(tb testing.TB, page string) reportMeta {
	tb.Helper()
	raw := blockBetween(tb, page, `<script type="application/json" id="blueprint-meta">`)
	var meta reportMeta
	if err := json.Unmarshal([]byte(unescapeLessThan(raw)), &meta); err != nil {
		tb.Fatalf("meta block: %v", err)
	}
	return meta
}

func dataBlock(tb testing.TB, page string) string {
	tb.Helper()
	return blockBetween(tb, page, `<script type="application/json" id="blueprint-data">`)
}

func blockBetween(tb testing.TB, page, open string) string {
	tb.Helper()
	_, rest, ok := strings.Cut(page, open)
	if !ok {
		tb.Fatalf("page has no block opened by %q", open)
	}
	body, _, ok := strings.Cut(rest, "</script>")
	if !ok {
		tb.Fatalf("block opened by %q is never closed", open)
	}
	return body
}

// unescapeLessThan undoes htmlSafeWriter for tests that want to parse a block
// with something other than a JSON parser. encoding/json decodes < itself, so
// this is only needed where the raw text is inspected.
func unescapeLessThan(s string) string {
	return strings.ReplaceAll(s, jsonLessThan, "<")
}

// assertResourcesEqual compares two censuses through their JSON, which is the
// representation the honesty rules are written against: an absent key and a
// stored zero look different there, and an empty bag and a nil bag look the
// same, which is exactly the distinction the encoding has to preserve and the
// one it is allowed to lose.
func assertResourcesEqual(tb testing.TB, want, got []model.Resource) {
	tb.Helper()
	if len(want) != len(got) {
		tb.Fatalf("decoded %d resources, want %d", len(got), len(want))
	}
	for i := range want {
		w, err := json.Marshal(want[i])
		if err != nil {
			tb.Fatalf("marshal want[%d]: %v", i, err)
		}
		g, err := json.Marshal(got[i])
		if err != nil {
			tb.Fatalf("marshal got[%d]: %v", i, err)
		}
		if !bytes.Equal(w, g) {
			tb.Fatalf("resource %d did not survive the round trip:\n want %s\n  got %s", i, w, g)
		}
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	snap := demoSnapshot("test")
	table, err := buildTable(snap.Resources)
	if err != nil {
		t.Fatalf("buildTable: %v", err)
	}
	if table.N != len(snap.Resources) {
		t.Fatalf("table.N = %d, want %d", table.N, len(snap.Resources))
	}
	assertResourcesEqual(t, snap.Resources, decodeTable(t, table))
}

// TestPayloadKeepsReportedZeros is the guardrail this encoding most needs. A
// zero-byte bucket and a bucket nobody could measure are different findings,
// and a column of numbers is the easiest place in the codebase to accidentally
// merge them.
func TestPayloadKeepsReportedZeros(t *testing.T) {
	zero := model.Resource{ARN: "arn:aws:s3:::empty", Service: "s3", Name: "empty", Type: "AWS::S3::Bucket"}
	zero.SetMeasure(model.MeasureSizeBytes, 0)
	silent := model.Resource{ARN: "arn:aws:s3:::unmeasured", Service: "s3", Name: "unmeasured", Type: "AWS::S3::Bucket"}

	table, err := buildTable([]model.Resource{zero, silent})
	if err != nil {
		t.Fatalf("buildTable: %v", err)
	}
	col := table.Num[model.MeasureSizeBytes]
	if col == nil {
		t.Fatal("size_bytes column is missing entirely; the reported zero was dropped")
	}
	cells := *col
	if cells[0] == nil {
		t.Error("a bucket reported as 0 bytes decoded as not reported")
	} else if *cells[0] != 0 {
		t.Errorf("a bucket reported as 0 bytes decoded as %d", *cells[0])
	}
	if cells[1] != nil {
		t.Errorf("a bucket nobody measured decoded as %d bytes", *cells[1])
	}

	got := decodeTable(t, table)
	if _, ok := got[0].Measures[model.MeasureSizeBytes]; !ok {
		t.Error("the reported zero did not survive decoding")
	}
	if _, ok := got[1].Measures[model.MeasureSizeBytes]; ok {
		t.Error("decoding invented a size for a bucket nobody measured")
	}
}

// TestPayloadKeepsTriStateFlags covers the other half of the same rule: a
// service that does not expose encryption at all must not decode as
// "unencrypted", which the report would draw as a finding.
func TestPayloadKeepsTriStateFlags(t *testing.T) {
	no := false
	yes := true
	rs := []model.Resource{
		{ARN: "arn:aws:x:::a", Name: "a", Encrypted: &no, PubliclyAccessible: &yes},
		{ARN: "arn:aws:x:::b", Name: "b"},
	}
	table, err := buildTable(rs)
	if err != nil {
		t.Fatalf("buildTable: %v", err)
	}
	got := decodeTable(t, table)
	if got[0].Encrypted == nil || *got[0].Encrypted {
		t.Errorf("encrypted=false decoded as %v", got[0].Encrypted)
	}
	if got[0].PubliclyAccessible == nil || !*got[0].PubliclyAccessible {
		t.Errorf("publicly_accessible=true decoded as %v", got[0].PubliclyAccessible)
	}
	if got[1].Encrypted != nil {
		t.Errorf("a resource that reports no encryption flag decoded as %v", *got[1].Encrypted)
	}
	if got[1].PubliclyAccessible != nil {
		t.Errorf("a resource that reports no exposure flag decoded as %v", *got[1].PubliclyAccessible)
	}
}

func TestEncodeARN(t *testing.T) {
	cases := []struct {
		name     string
		resource model.Resource
		wantOK   bool
		head     string
		infix    string
	}{
		{
			name: "rds instance",
			resource: model.Resource{
				ARN: "arn:aws:rds:us-east-1:111111111111:db:orders-prod",
				// Aurora and DocumentDB live under the rds namespace while
				// carrying their own Service, which is why the head is stored
				// rather than derived from Service.
				Service: "aurora", Region: "us-east-1", AccountID: "111111111111", Name: "orders-prod",
			},
			wantOK: true, head: "arn:aws:rds", infix: "db:",
		},
		{
			name: "dynamodb table uses a slash",
			resource: model.Resource{
				ARN:     "arn:aws:dynamodb:eu-west-1:222222222222:table/carts",
				Service: "dynamodb", Region: "eu-west-1", AccountID: "222222222222", Name: "carts",
			},
			wantOK: true, head: "arn:aws:dynamodb", infix: "table/",
		},
		{
			name: "s3 bucket has no region or account in its arn",
			resource: model.Resource{
				ARN:     "arn:aws:s3:::assets",
				Service: "s3", Name: "assets",
			},
			wantOK: true, head: "arn:aws:s3", infix: "",
		},
		{
			name: "region in the arn disagrees with the row",
			resource: model.Resource{
				ARN:     "arn:aws:s3:::assets",
				Service: "s3", Region: "us-east-1", Name: "assets",
			},
			wantOK: false,
		},
		{
			name: "resource part does not end in the name",
			resource: model.Resource{
				ARN:     "arn:aws:ec2:us-east-1:111111111111:volume/vol-abc",
				Service: "ec2", Region: "us-east-1", AccountID: "111111111111", Name: "data-volume",
			},
			wantOK: false,
		},
		{
			name:     "too few fields",
			resource: model.Resource{ARN: "arn:aws:ec2", Service: "ec2", Name: "ec2"},
			wantOK:   false,
		},
		{
			name:     "empty arn",
			resource: model.Resource{Service: "ec2", Name: "orphan"},
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, infix, ok := encodeARN(&tc.resource)
			if ok != tc.wantOK {
				t.Fatalf("encodeARN ok = %v, want %v (head %q infix %q)", ok, tc.wantOK, head, infix)
			}
			if !ok {
				return
			}
			if head != tc.head || infix != tc.infix {
				t.Errorf("encodeARN = (%q, %q), want (%q, %q)", head, infix, tc.head, tc.infix)
			}
			got := decodeARN(head, infix, tc.resource.Region, tc.resource.AccountID, tc.resource.Name)
			if got != tc.resource.ARN {
				t.Errorf("decodeARN = %q, want %q", got, tc.resource.ARN)
			}
		})
	}
}

// TestEncodeARNFallbackIsStoredVerbatim checks the whole-table behaviour rather
// than the codec: an ARN the codec cannot rebuild has to come back byte for
// byte, because a census that silently drops the identifiers it could not
// compress is worse than one that does not compress them.
func TestEncodeARNFallbackIsStoredVerbatim(t *testing.T) {
	odd := model.Resource{
		ARN:     "arn:aws:ec2:us-east-1:111111111111:volume/vol-0abc",
		Service: "ec2", Region: "us-east-1", AccountID: "111111111111", Name: "nightly-data",
	}
	if _, _, ok := encodeARN(&odd); ok {
		t.Fatal("this ARN was supposed to be unreconstructible")
	}
	table, err := buildTable([]model.Resource{odd})
	if err != nil {
		t.Fatalf("buildTable: %v", err)
	}
	got := decodeTable(t, table)
	if got[0].ARN != odd.ARN {
		t.Errorf("ARN = %q, want %q", got[0].ARN, odd.ARN)
	}
}

// TestPayloadColumnLengthsChecked makes sure the guard in buildTable is load
// bearing: a column short by one cell shifts every row after it and attaches
// one resource's value to another.
func TestPayloadColumnLengthsChecked(t *testing.T) {
	table := resourceTable{N: 2, Str: map[string]*stringColumn{colName: newStringColumn(2)}}
	table.Str[colName].Append("only-one", true)
	if err := table.checkLengths(); err == nil {
		t.Fatal("checkLengths accepted a column with 1 cell for 2 resources")
	}
}
