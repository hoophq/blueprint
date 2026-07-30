package scanners

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/hoophq/blueprint/internal/model"
)

func s3TestBucket(name string) s3types.Bucket {
	created := time.Date(2019, 4, 30, 10, 30, 0, 0, time.UTC)
	return s3types.Bucket{
		Name:         aws.String(name),
		BucketRegion: aws.String("us-east-1"),
		CreationDate: aws.Time(created),
	}
}

func TestS3BucketResource(t *testing.T) {
	r := s3BucketResource(s3TestBucket("acme-prod-assets"), "us-east-1", testAccount)

	if r.Service != model.ServiceS3 {
		t.Errorf("Service = %q, want %q", r.Service, model.ServiceS3)
	}
	if r.Type != model.TypeS3Bucket {
		t.Errorf("Type = %q, want %q", r.Type, model.TypeS3Bucket)
	}
	if r.Name != "acme-prod-assets" {
		t.Errorf("Name = %q, want %q", r.Name, "acme-prod-assets")
	}
	// Neither account nor region in the ARN: S3 bucket names are global, and an
	// ARN carrying either would not match what any other tool writes.
	if want := "arn:aws:s3:::acme-prod-assets"; r.ARN != want {
		t.Errorf("ARN = %q, want %q", r.ARN, want)
	}
	if r.Region != "us-east-1" || r.AccountID != testAccount {
		t.Errorf("placement = %q/%q, want us-east-1/%s", r.Region, r.AccountID, testAccount)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(time.Date(2019, 4, 30, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, want the CreationDate S3 reported", r.CreatedAt)
	}
	// A bucket has no lifecycle state. Inventing "available" would put a value
	// in a column no S3 response supports.
	if r.Status != "" {
		t.Errorf("Status = %q, want empty: S3 reports no bucket state", r.Status)
	}
}

// The whole reason S3 needs the CloudWatch stage: nothing in the control plane
// says how big a bucket is, so the scanner must leave both keys absent rather
// than write a zero that reads as "empty".
func TestS3BucketResourceReportsNoSize(t *testing.T) {
	r := s3BucketResource(s3TestBucket("acme-prod-assets"), "us-east-1", testAccount)

	for _, key := range []string{model.MeasureSizeBytes, model.MeasureObjectCount} {
		if v, ok := r.Measure(key); ok {
			t.Errorf("%s = %d, want absent: no S3 call reports it", key, v)
		}
	}
}

// The region filter makes these agree today. If S3 ever answers with a bucket
// from elsewhere, where S3 says it is beats where we looked.
func TestS3BucketResourcePrefersTheReportedRegion(t *testing.T) {
	b := s3TestBucket("acme-prod-assets")
	b.BucketRegion = aws.String("eu-west-1")

	r := s3BucketResource(b, "us-east-1", testAccount)

	if r.Region != "eu-west-1" {
		t.Errorf("Region = %q, want the region S3 reported", r.Region)
	}
}

func TestS3BucketResourcePrefersTheReportedARN(t *testing.T) {
	b := s3TestBucket("acme-directory")
	// Directory buckets are the case that populates it.
	b.BucketArn = aws.String("arn:aws:s3express:us-east-1:" + testAccount + ":bucket/acme-directory")

	r := s3BucketResource(b, "us-east-1", testAccount)

	if r.ARN != *b.BucketArn {
		t.Errorf("ARN = %q, want the ARN S3 reported", r.ARN)
	}
}

// Partition is the only part of an S3 ARN this tool can get wrong, and getting
// it wrong breaks the diff's match key and the cost join at once — silently, in
// both cases.
func TestS3BucketResourcePartitionFollowsTheRegion(t *testing.T) {
	for region, want := range map[string]string{
		"us-east-1":     "arn:aws:s3:::acme",
		"us-gov-west-1": "arn:aws-us-gov:s3:::acme",
		"cn-north-1":    "arn:aws-cn:s3:::acme",
	} {
		b := s3types.Bucket{Name: aws.String("acme"), BucketRegion: aws.String(region)}
		if got := s3BucketResource(b, region, testAccount).ARN; got != want {
			t.Errorf("%s: ARN = %q, want %q", region, got, want)
		}
	}
}

// The compaction key in scanS3. A bucket with no name cannot be addressed, and
// a row whose ARN is "arn:aws:s3:::" would collide with every other one.
func TestS3BucketResourceDropsAnUnnamedBucket(t *testing.T) {
	r := s3BucketResource(s3types.Bucket{BucketRegion: aws.String("us-east-1")}, "us-east-1", testAccount)

	if r.ARN != "" {
		t.Errorf("ARN = %q, want empty so the row is dropped", r.ARN)
	}
}

func TestApplyS3Encryption(t *testing.T) {
	var r model.Resource
	applyS3Encryption(&r, &s3.GetBucketEncryptionOutput{
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAwsKms,
				},
				BucketKeyEnabled: aws.Bool(true),
			}},
		},
	})

	if got := r.Attr(model.AttrSSEAlgorithm); got != "aws:kms" {
		t.Errorf("sse_algorithm = %q, want %q", got, "aws:kms")
	}
	if got := r.Attr(model.AttrBucketKeyEnabled); got != "true" {
		t.Errorf("bucket_key_enabled = %q, want %q", got, "true")
	}
	if r.Encrypted == nil || !*r.Encrypted {
		t.Errorf("Encrypted = %v, want true", r.Encrypted)
	}
}

// S3 accepts one rule today. The census should not break, or pick a winner, if
// that changes.
func TestApplyS3EncryptionRecordsEveryAlgorithmSorted(t *testing.T) {
	var r model.Resource
	applyS3Encryption(&r, &s3.GetBucketEncryptionOutput{
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{
				{ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAwsKms}},
				{ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAes256}},
				{ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAwsKms}},
			},
		},
	})

	if got := r.Attr(model.AttrSSEAlgorithm); got != "AES256,aws:kms" {
		t.Errorf("sse_algorithm = %q, want the algorithms de-duplicated and sorted", got)
	}
}

// Bucket Keys are per rule, so "on anywhere" is the honest summary: the KMS
// request charge is avoided for the objects that rule covers.
func TestApplyS3EncryptionBucketKeyIsTrueIfAnyRuleEnablesIt(t *testing.T) {
	var r model.Resource
	applyS3Encryption(&r, &s3.GetBucketEncryptionOutput{
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{
				{ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAes256}, BucketKeyEnabled: aws.Bool(false)},
				{ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAwsKms}, BucketKeyEnabled: aws.Bool(true)},
			},
		},
	})

	if got := r.Attr(model.AttrBucketKeyEnabled); got != "true" {
		t.Errorf("bucket_key_enabled = %q, want true: one rule enables it", got)
	}
}

// The honesty rule. Since January 2023 S3 encrypts every new object with
// SSE-S3 whether the bucket asks or not, so "no configuration" is not
// "unencrypted" — writing false would add the bucket to the census's
// unencrypted count and to Exposed(), asserting a vulnerability that is not
// there.
func TestApplyS3EncryptionNeverWritesFalse(t *testing.T) {
	for name, out := range map[string]*s3.GetBucketEncryptionOutput{
		"no response":      nil,
		"no configuration": {},
		"no algorithm": {ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{}},
		}},
	} {
		var r model.Resource
		applyS3Encryption(&r, out)
		if r.Encrypted != nil {
			t.Errorf("%s: Encrypted = %v, want nil", name, *r.Encrypted)
		}
		if v, ok := r.Attributes[model.AttrSSEAlgorithm]; ok {
			t.Errorf("%s: sse_algorithm = %q, want absent", name, v)
		}
	}
}

func TestApplyS3PublicAccess(t *testing.T) {
	yes, no := aws.Bool(true), aws.Bool(false)
	all := func(v bool) *S3PublicAccess {
		return &S3PublicAccess{BlockACLs: &v, IgnoreACLs: &v, BlockPolicy: &v, Restrict: &v}
	}

	cases := []struct {
		name     string
		bpa      *S3PublicAccess
		isPublic *bool
		want     *bool
		why      string
	}{
		{
			name: "both enforcers on", bpa: all(true), isPublic: no, want: no,
			why: "IgnorePublicAcls and RestrictPublicBuckets neutralize everything already granted",
		},
		{
			// The finding this scanner exists to surface: three of four switches
			// on reads as locked down and is not.
			name:     "public policy, restrict off",
			bpa:      &S3PublicAccess{BlockACLs: yes, IgnoreACLs: yes, BlockPolicy: yes, Restrict: no},
			isPublic: yes, want: yes,
			why: "BlockPublicPolicy only refuses new policies; the live one is still serving",
		},
		{
			// The enforcers settle it whatever the policy says.
			name: "public policy but both enforcers on", bpa: all(true), isPublic: yes, want: no,
			why: "RestrictPublicBuckets neutralizes the public policy",
		},
		{
			// The common leftover, and the one that must stay silent: no public
			// policy, public ACLs unblocked, and this scanner never calls
			// GetBucketAcl. A false here would assert safety it did not check.
			name:     "write-time guards only",
			bpa:      &S3PublicAccess{BlockACLs: yes, IgnoreACLs: no, BlockPolicy: yes, Restrict: no},
			isPublic: no, want: nil,
			why: "public ACLs would still be live and no call was made that could see them",
		},
		{
			name: "no block public access configuration at all", bpa: nil, isPublic: no, want: nil,
			why: "nothing neutralizes an ACL, and no ACL was read",
		},
		{
			name: "policy status unavailable", bpa: all(false), isPublic: nil, want: nil,
			why: "every switch off decides nothing without knowing the policy",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r model.Resource
			ApplyS3PublicAccess(&r, c.bpa, c.isPublic)

			switch {
			case c.want == nil && r.PubliclyAccessible != nil:
				t.Errorf("PubliclyAccessible = %v, want nil: %s", *r.PubliclyAccessible, c.why)
			case c.want != nil && r.PubliclyAccessible == nil:
				t.Errorf("PubliclyAccessible = nil, want %v: %s", *c.want, c.why)
			case c.want != nil && *r.PubliclyAccessible != *c.want:
				t.Errorf("PubliclyAccessible = %v, want %v: %s", *r.PubliclyAccessible, *c.want, c.why)
			}
		})
	}
}

// All four switches are recorded separately, because the verdict deliberately
// collapses cases it cannot decide and a reader still needs to see why.
func TestApplyS3PublicAccessRecordsEverySwitch(t *testing.T) {
	var r model.Resource
	ApplyS3PublicAccess(&r, &S3PublicAccess{
		BlockACLs: aws.Bool(true), IgnoreACLs: aws.Bool(false),
		BlockPolicy: aws.Bool(true), Restrict: aws.Bool(false),
	}, aws.Bool(true))

	for key, want := range map[string]string{
		model.AttrBlockPublicACLs:       "true",
		model.AttrIgnorePublicACLs:      "false",
		model.AttrBlockPublicPolicy:     "true",
		model.AttrRestrictPublicBuckets: "false",
		model.AttrPolicyIsPublic:        "true",
	} {
		if got := r.Attr(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// A switch S3 did not report is absent, not off. The two render identically if
// the scanner defaults, and "off" is the dangerous one to invent.
func TestApplyS3PublicAccessLeavesUnreportedSwitchesAbsent(t *testing.T) {
	var r model.Resource
	ApplyS3PublicAccess(&r, &S3PublicAccess{Restrict: aws.Bool(true)}, nil)

	for _, key := range []string{
		model.AttrBlockPublicACLs, model.AttrIgnorePublicACLs,
		model.AttrBlockPublicPolicy, model.AttrPolicyIsPublic,
	} {
		if v, ok := r.Attributes[key]; ok {
			t.Errorf("%s = %q, want absent: S3 reported no value", key, v)
		}
	}
}

func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code}
}

// The three-way split answered() exists for. The absence codes are answers —
// ledgering them would file nine hundred entries for nine hundred untagged
// buckets and bury the one denial that matters.
func TestS3CallFailuresAbsenceIsNotFailure(t *testing.T) {
	cases := []struct {
		op, absent string
		err        error
		wantUsable bool
		wantLedger bool
	}{
		{"GetBucketTagging", s3CodeNoSuchTagSet, nil, true, false},
		{"GetBucketTagging", s3CodeNoSuchTagSet, apiErr(s3CodeNoSuchTagSet), false, false},
		{"GetBucketEncryption", s3CodeNoEncryptionConfiguration, apiErr(s3CodeNoEncryptionConfiguration), false, false},
		{"GetPublicAccessBlock", s3CodeNoSuchPublicAccessBlock, apiErr(s3CodeNoSuchPublicAccessBlock), false, false},
		{"GetBucketPolicyStatus", s3CodeNoSuchBucketPolicy, apiErr(s3CodeNoSuchBucketPolicy), false, false},
		// The one that must not be swallowed: a bucket whose encryption
		// blueprint was not allowed to read looks exactly like one with none.
		{"GetBucketEncryption", s3CodeNoEncryptionConfiguration, apiErr("AccessDenied"), false, true},
		// A call with no absence code at all: every error is a gap.
		{"GetBucketVersioning", "", apiErr("NoSuchTagSet"), false, true},
	}

	for _, c := range cases {
		agg := &s3CallFailures{}
		if got := agg.answered(c.op, c.err, c.absent); got != c.wantUsable {
			t.Errorf("%s/%v: answered = %v, want %v", c.op, c.err, got, c.wantUsable)
		}
		if got := agg.err() != nil; got != c.wantLedger {
			t.Errorf("%s/%v: ledgered = %v, want %v", c.op, c.err, got, c.wantLedger)
		}
	}
}

// One entry per operation, not per bucket, and it names the count — which is
// what tells a reader whether a region is half-scanned or fully denied.
func TestS3CallFailuresAggregatesByOperation(t *testing.T) {
	agg := &s3CallFailures{}
	for i := 0; i < 3; i++ {
		agg.answered("GetBucketEncryption", apiErr("AccessDenied"), s3CodeNoEncryptionConfiguration)
		agg.answered("GetBucketTagging", nil, s3CodeNoSuchTagSet)
	}
	agg.answered("GetBucketTagging", apiErr("AccessDenied"), s3CodeNoSuchTagSet)

	got := agg.err()
	if got == nil {
		t.Fatal("err = nil, want the denied calls ledgered")
	}
	// Sorted by operation name: the ledger text goes into a JSON artifact that
	// has to be byte-stable across runs, and map order is not.
	denied := apiErr("AccessDenied").Error()
	want := "GetBucketEncryption: 3 of 3 buckets: " + denied + "\n" +
		"GetBucketTagging: 1 of 4 buckets: " + denied
	if got.Error() != want {
		t.Errorf("err =\n%s\nwant\n%s", got, want)
	}
}

// fakeS3 answers whatever each test needs and records nothing it does not.
// Zero-valued function fields answer with an empty successful response, which
// is what most of these calls look like for a plain bucket.
type fakeS3 struct {
	pages      []s3.ListBucketsOutput
	listErr    error
	calls      int
	tagging    func(string) (*s3.GetBucketTaggingOutput, error)
	encryption func(string) (*s3.GetBucketEncryptionOutput, error)
	bpa        func(string) (*s3.GetPublicAccessBlockOutput, error)
	policy     func(string) (*s3.GetBucketPolicyStatusOutput, error)
	versioning func(string) (*s3.GetBucketVersioningOutput, error)
}

func (f *fakeS3) ListBuckets(_ context.Context, in *s3.ListBucketsInput, _ ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	if aws.ToString(in.BucketRegion) == "" {
		return nil, errTestNoRegionFilter
	}
	i := f.calls
	f.calls++
	if i < len(f.pages) {
		page := f.pages[i]
		return &page, nil
	}
	// Past the scripted pages: listErr is how a test says the listing was cut
	// short, and the last scripted page has to carry a continuation token for
	// the paginator to ask again at all.
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &s3.ListBucketsOutput{}, nil
}

func (f *fakeS3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	if f.tagging == nil {
		return &s3.GetBucketTaggingOutput{}, nil
	}
	return f.tagging(aws.ToString(in.Bucket))
}

func (f *fakeS3) GetBucketEncryption(_ context.Context, in *s3.GetBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	if f.encryption == nil {
		return &s3.GetBucketEncryptionOutput{}, nil
	}
	return f.encryption(aws.ToString(in.Bucket))
}

func (f *fakeS3) GetPublicAccessBlock(_ context.Context, in *s3.GetPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.GetPublicAccessBlockOutput, error) {
	if f.bpa == nil {
		return &s3.GetPublicAccessBlockOutput{}, nil
	}
	return f.bpa(aws.ToString(in.Bucket))
}

func (f *fakeS3) GetBucketPolicyStatus(_ context.Context, in *s3.GetBucketPolicyStatusInput, _ ...func(*s3.Options)) (*s3.GetBucketPolicyStatusOutput, error) {
	if f.policy == nil {
		return &s3.GetBucketPolicyStatusOutput{}, nil
	}
	return f.policy(aws.ToString(in.Bucket))
}

func (f *fakeS3) GetBucketVersioning(_ context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	if f.versioning == nil {
		return &s3.GetBucketVersioningOutput{}, nil
	}
	return f.versioning(aws.ToString(in.Bucket))
}

var errTestNoRegionFilter = &smithy.GenericAPIError{
	Code:    "TestNoRegionFilter",
	Message: "ListBuckets was called without a bucket-region filter",
}

func s3Names(rs []model.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// Pagination is mandatory, not defensive: with a region filter and no
// max-buckets, S3 pages at 10,000 and an account past that silently loses
// buckets. The region filter is equally mandatory — without it every scanned
// region reports every bucket in the account.
func TestScanS3PagesTheListing(t *testing.T) {
	api := &fakeS3{pages: []s3.ListBucketsOutput{
		{Buckets: []s3types.Bucket{s3TestBucket("a"), s3TestBucket("b")}, ContinuationToken: aws.String("next")},
		{Buckets: []s3types.Bucket{s3TestBucket("c")}},
	}}

	got, err := scanS3(context.Background(), api, "us-east-1", testAccount)
	if err != nil {
		t.Fatalf("scanS3: %v", err)
	}
	if want := []string{"a", "b", "c"}; strings.Join(s3Names(got), ",") != strings.Join(want, ",") {
		t.Errorf("buckets = %v, want %v", s3Names(got), want)
	}
}

// Partial results per the Scanner contract: half a region plus a ledger entry
// saying so beats nothing.
func TestScanS3KeepsThePagesThatArrived(t *testing.T) {
	api := &fakeS3{
		pages:   []s3.ListBucketsOutput{{Buckets: []s3types.Bucket{s3TestBucket("a")}, ContinuationToken: aws.String("next")}},
		listErr: apiErr("RequestLimitExceeded"),
	}

	got, err := scanS3(context.Background(), api, "us-east-1", testAccount)
	if err == nil {
		t.Fatal("err = nil, want the truncated listing ledgered")
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("buckets = %v, want the page that arrived", s3Names(got))
	}
}

// One denied call costs its field on every bucket and nothing else: the rows
// are still real, and the ledger says which field is missing and how widely.
func TestScanS3ADeniedDetailCallCostsOnlyItsField(t *testing.T) {
	api := &fakeS3{
		pages: []s3.ListBucketsOutput{{Buckets: []s3types.Bucket{s3TestBucket("a"), s3TestBucket("b")}}},
		encryption: func(string) (*s3.GetBucketEncryptionOutput, error) {
			return nil, apiErr("AccessDenied")
		},
		versioning: func(string) (*s3.GetBucketVersioningOutput, error) {
			return &s3.GetBucketVersioningOutput{Status: s3types.BucketVersioningStatusEnabled}, nil
		},
	}

	got, err := scanS3(context.Background(), api, "us-east-1", testAccount)
	if len(got) != 2 {
		t.Fatalf("buckets = %v, want both rows", s3Names(got))
	}
	if err == nil || !strings.Contains(err.Error(), "GetBucketEncryption: 2 of 2 buckets") {
		t.Errorf("err = %v, want the denial ledgered with its count", err)
	}
	for _, r := range got {
		if r.Encrypted != nil {
			t.Errorf("%s: Encrypted = %v, want nil: the call was denied, not answered", r.Name, *r.Encrypted)
		}
		if got := r.Attr(model.AttrVersioning); got != "Enabled" {
			t.Errorf("%s: versioning = %q, want Enabled: its own call succeeded", r.Name, got)
		}
	}
}

// A bucket that never enabled versioning gets a 200 with an empty body. That
// arrives as the enum's zero value and must land as an absent key, not as a
// "Disabled" this census invented.
func TestScanS3UnversionedBucketHasNoVersioningKey(t *testing.T) {
	api := &fakeS3{pages: []s3.ListBucketsOutput{{Buckets: []s3types.Bucket{s3TestBucket("a")}}}}

	got, err := scanS3(context.Background(), api, "us-east-1", testAccount)
	if err != nil {
		t.Fatalf("scanS3: %v", err)
	}
	for _, key := range []string{model.AttrVersioning, model.AttrMFADelete} {
		if v, ok := got[0].Attributes[key]; ok {
			t.Errorf("%s = %q, want absent: S3 returned an empty status", key, v)
		}
	}
}

// Cancellation leaves a slot zero-valued and the compaction drops it. A
// half-described bucket would put a row in the census whose blank fields mean
// "we were interrupted", which is indistinguishable from "S3 reported nothing".
func TestScanS3CancelledMidRunEmitsNoHalfRows(t *testing.T) {
	api := &fakeS3{pages: []s3.ListBucketsOutput{{Buckets: []s3types.Bucket{
		s3TestBucket("a"), s3TestBucket("b"), s3TestBucket("c"),
	}}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, _ := scanS3(ctx, api, "us-east-1", testAccount)
	for _, r := range got {
		if r.ARN == "" || r.Name == "" {
			t.Errorf("emitted a half-described row: %+v", r)
		}
	}
}

func TestScanS3ReadsTags(t *testing.T) {
	api := &fakeS3{
		pages: []s3.ListBucketsOutput{{Buckets: []s3types.Bucket{s3TestBucket("a")}}},
		tagging: func(string) (*s3.GetBucketTaggingOutput, error) {
			return &s3.GetBucketTaggingOutput{TagSet: []s3types.Tag{
				{Key: aws.String("environment"), Value: aws.String("production")},
				{Key: aws.String("owner"), Value: aws.String("platform")},
			}}, nil
		},
	}

	got, err := scanS3(context.Background(), api, "us-east-1", testAccount)
	if err != nil {
		t.Fatalf("scanS3: %v", err)
	}
	if got[0].Tags["environment"] != "production" || got[0].Tags["owner"] != "platform" {
		t.Errorf("Tags = %v, want the tag set S3 returned", got[0].Tags)
	}
}

// An untagged bucket is the overwhelmingly common case and S3 answers it with
// an error. Reaching the ledger would report a correctly-scanned account as
// partially unreadable.
func TestScanS3UntaggedBucketIsNotAFailure(t *testing.T) {
	api := &fakeS3{
		pages:   []s3.ListBucketsOutput{{Buckets: []s3types.Bucket{s3TestBucket("a")}}},
		tagging: func(string) (*s3.GetBucketTaggingOutput, error) { return nil, apiErr(s3CodeNoSuchTagSet) },
	}

	got, err := scanS3(context.Background(), api, "us-east-1", testAccount)
	if err != nil {
		t.Errorf("err = %v, want nil: an untagged bucket is an answer", err)
	}
	if got[0].Tags != nil {
		t.Errorf("Tags = %v, want nil", got[0].Tags)
	}
}
