package scanners

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// s3Scanner censuses S3 buckets.
//
// S3 is the one service here whose spend is invisible from the control plane.
// Every other resource states its own size — an EBS volume reports its
// gigabytes, an RDS instance its allocated storage — but a bucket holding a
// petabyte and a bucket holding nothing describe identically. What this
// scanner can see is the configuration: encryption, versioning, and whether
// the thing is reachable from the internet. The bytes arrive later, or not at
// all, from the CloudWatch stage (see internal/enrich).
//
// # Why this is a regional scanner
//
// ListBuckets is a global call, and for years the only way to place a bucket
// was GetBucketLocation, one request each. It now accepts a bucket-region
// filter and returns BucketRegion inline, so S3 fits the existing
// account × region fan-out with no special casing: each unit asks for the
// buckets in its own region and gets exactly those. No global-scanner concept,
// no duplicates to reconcile, and a region that fails is one ledger entry
// rather than a hole in a global list. It also settles the redirect problem
// for free — S3 requires the filter and the endpoint to agree, so the config
// the runner hands this unit is already the right client for every bucket it
// gets back, and the SDK never has to follow a 301 it does not follow.
//
// Pagination is mandatory, not defensive: passing a bucket-region filter
// without max-buckets makes S3 apply a default page size of 10,000 and hand
// back a continuation token, so an account past that count silently loses
// buckets without it.
//
// # The N+1 is the API's, not a choice
//
// Everything worth knowing about a bucket is its own request: encryption,
// public access block, policy status, versioning, tags. S3 offers no batch
// form of any of them, so the census costs five requests per bucket. They run
// on a small worker pool per region — sequential is unusable at four figures
// of buckets — and the pool is deliberately small, because scan units already
// run concurrently above this and the two multiply.
type s3Scanner struct{}

func init() { scan.Register(s3Scanner{}) }

func (s3Scanner) Service() string { return model.ServiceS3 }

// s3API is the slice of S3 this scanner uses. Named so tests can substitute a
// fake: the interesting behaviour here is which errors are data and which are
// coverage gaps, and that is only exercisable by making calls fail.
type s3API interface {
	ListBuckets(context.Context, *s3.ListBucketsInput, ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
	GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	GetBucketEncryption(context.Context, *s3.GetBucketEncryptionInput, ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error)
	GetPublicAccessBlock(context.Context, *s3.GetPublicAccessBlockInput, ...func(*s3.Options)) (*s3.GetPublicAccessBlockOutput, error)
	GetBucketPolicyStatus(context.Context, *s3.GetBucketPolicyStatusInput, ...func(*s3.Options)) (*s3.GetBucketPolicyStatusOutput, error)
	GetBucketVersioning(context.Context, *s3.GetBucketVersioningInput, ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
}

// s3DetailWorkers bounds the per-bucket fan-out within one region.
//
// Small on purpose. The runner already runs scan units concurrently, so this
// multiplies against the user's --concurrency rather than fitting inside it,
// and these are bucket-level control-plane calls whose rate limits are far
// below S3's headline per-prefix object throughput.
const s3DetailWorkers = 8

func (s3Scanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	return scanS3(ctx, s3.NewFromConfig(cfg), region, accountID)
}

func scanS3(ctx context.Context, api s3API, region, accountID string) ([]model.Resource, error) {
	buckets, listErr := listS3Buckets(ctx, api, region)

	// Partial results per the Scanner contract: a pagination failure keeps the
	// pages that did arrive and still describes them, because half a region's
	// buckets plus a ledger entry saying so beats none.
	out := make([]model.Resource, len(buckets))
	agg := &s3CallFailures{}

	var wg sync.WaitGroup
	sem := make(chan struct{}, s3DetailWorkers)
	for i, b := range buckets {
		wg.Add(1)
		go func(i int, b s3types.Bucket) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				// Leave the slot zero-valued; it is dropped below. Emitting a
				// half-described bucket would put a row in the census whose
				// blank fields mean "we were interrupted", which is
				// indistinguishable from "S3 reported nothing".
				return
			}
			defer func() { <-sem }()
			out[i] = describeS3Bucket(ctx, api, b, region, accountID, agg)
		}(i, b)
	}
	wg.Wait()

	// Slots never reached (cancellation, or a bucket S3 named with an empty
	// string) hold the zero Resource. ARN is the diff's match key and no real
	// bucket has an empty one, so it is the test.
	kept := out[:0]
	for _, r := range out {
		if r.ARN != "" {
			kept = append(kept, r)
		}
	}
	return kept, errors.Join(listErr, agg.err())
}

// listS3Buckets pages through the buckets in one region. The region filter is
// what keeps this scanner from reporting every bucket in the account once per
// scanned region.
func listS3Buckets(ctx context.Context, api s3API, region string) ([]s3types.Bucket, error) {
	var out []s3types.Bucket
	pages := s3.NewListBucketsPaginator(api, &s3.ListBucketsInput{
		BucketRegion: aws.String(region),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("list buckets: %w", err)
		}
		out = append(out, page.Buckets...)
	}
	return out, nil
}

// s3BucketResource builds the row from the listing alone, before any detail
// call. Everything here is reported by ListBuckets; the follow-up calls only
// add to it.
func s3BucketResource(b s3types.Bucket, region, accountID string) model.Resource {
	name := aws.ToString(b.Name)
	if name == "" {
		return model.Resource{}
	}

	// AWS's own answer for placement wins over the region asked for. They
	// agree today — the filter guarantees it — but if S3 ever answers with a
	// bucket from elsewhere, recording where S3 says it is beats recording
	// where we looked.
	if reported := aws.ToString(b.BucketRegion); reported != "" {
		region = reported
	}

	// BucketArn is populated for directory buckets only, so the general-purpose
	// case is built here. An S3 bucket ARN carries neither region nor account —
	// bucket names are globally unique, which is what makes that legal — so the
	// partition is the only part that can be got wrong, and getting it wrong
	// silently breaks both the diff's match key and the cost join.
	arn := aws.ToString(b.BucketArn)
	if arn == "" {
		arn = "arn:" + partitionFromARNs(region) + ":s3:::" + name
	}

	r := model.Resource{
		ARN:       arn,
		Service:   model.ServiceS3,
		Type:      model.TypeS3Bucket,
		Name:      name,
		Region:    region,
		AccountID: accountID,
		// Status stays empty: a bucket has no lifecycle state. It exists or it
		// does not, and there is no field in any response to read one from.
	}

	// CreationDate as reported. AWS documents that this can move when a bucket
	// is reconfigured, so it is a weaker claim than most CreatedAt values in
	// this census — but it is the date S3 gives, and substituting nothing for
	// it would drop the only age signal a bucket has.
	if b.CreationDate != nil {
		created := b.CreationDate.UTC()
		r.CreatedAt = &created
	}

	// No size and no object count here. Both are CloudWatch questions answered
	// daily, and the only control-plane route to them is listing the objects —
	// a data-plane operation, billed per request, hours long on a large bucket.
	// The enrich stage attaches them with an observation time; this scanner
	// leaves the keys absent rather than writing a zero that reads as "empty".
	return r
}

// describeS3Bucket runs the per-bucket detail calls and folds them into the
// row. Every call is independent: one failing costs its own field, not the
// bucket.
func describeS3Bucket(ctx context.Context, api s3API, b s3types.Bucket, region, accountID string, agg *s3CallFailures) model.Resource {
	r := s3BucketResource(b, region, accountID)
	if r.ARN == "" {
		return r
	}
	name := aws.String(r.Name)

	tagging, err := api.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: name})
	if agg.answered("GetBucketTagging", err, s3CodeNoSuchTagSet) && tagging != nil {
		r.Tags = toTagMap(tagging.TagSet, func(t s3types.Tag) (*string, *string) { return t.Key, t.Value })
	}

	enc, err := api.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: name})
	if agg.answered("GetBucketEncryption", err, s3CodeNoEncryptionConfiguration) {
		applyS3Encryption(&r, enc)
	}

	bpaOut, err := api.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: name})
	var bpa *S3PublicAccess
	if agg.answered("GetPublicAccessBlock", err, s3CodeNoSuchPublicAccessBlock) && bpaOut != nil {
		bpa = s3PublicAccessFrom(bpaOut.PublicAccessBlockConfiguration)
	}

	statusOut, err := api.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: name})
	var isPublic *bool
	if agg.answered("GetBucketPolicyStatus", err, s3CodeNoSuchBucketPolicy) && statusOut != nil && statusOut.PolicyStatus != nil {
		isPublic = statusOut.PolicyStatus.IsPublic
	}

	// Both public-access inputs land together: the verdict needs to see the
	// block configuration and the policy status at once, and a bucket where
	// one of the two calls was denied must not be judged on the other alone.
	ApplyS3PublicAccess(&r, bpa, isPublic)

	ver, err := api.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: name})
	if agg.answered("GetBucketVersioning", err, "") && ver != nil {
		// No absence code: a bucket that never enabled versioning gets a 200
		// with an empty body, which arrives as the enum's zero value. SetAttr
		// drops empties, so "never enabled" becomes an absent key on the same
		// footing as every other unreported field — rather than a "Disabled"
		// this census would have invented.
		r.SetAttr(model.AttrVersioning, string(ver.Status))
		r.SetAttr(model.AttrMFADelete, string(ver.MFADelete))
	}

	return r
}

// applyS3Encryption records a bucket's default encryption configuration.
//
// The core Encrypted flag is set true when S3 reports a configuration and left
// nil when it reports none — never false. Since January 2023 S3 encrypts every
// new object with SSE-S3 whether or not the bucket asks it to, so a bucket
// without an explicit configuration is not an unencrypted bucket; writing
// false would put it in the census's unencrypted count and in Exposed(),
// asserting a vulnerability that does not exist. What is genuinely unknown is
// the objects written before that default, and answering that needs the data
// plane this tool does not touch. So the flag declines, and the attribute says
// what was configured. (Same reasoning as the Lambda scanner's Encrypted.)
func applyS3Encryption(r *model.Resource, out *s3.GetBucketEncryptionOutput) {
	if out == nil || out.ServerSideEncryptionConfiguration == nil {
		return
	}
	// S3 accepts exactly one rule today, but the field is a list and the
	// census should not depend on that staying true. Every algorithm present
	// is recorded, de-duplicated and sorted, so a future multi-rule bucket
	// reports both rather than whichever happened to come first.
	algorithms := map[string]bool{}
	var bucketKey *bool
	for _, rule := range out.ServerSideEncryptionConfiguration.Rules {
		if d := rule.ApplyServerSideEncryptionByDefault; d != nil && string(d.SSEAlgorithm) != "" {
			algorithms[string(d.SSEAlgorithm)] = true
		}
		// Bucket Keys are per rule. true anywhere is the honest summary: the
		// KMS charge it avoids is avoided for the objects that rule covers.
		if rule.BucketKeyEnabled != nil && !aws.ToBool(bucketKey) {
			bucketKey = rule.BucketKeyEnabled
		}
	}
	r.SetBoolAttr(model.AttrBucketKeyEnabled, bucketKey)
	if len(algorithms) == 0 {
		// A configuration with no algorithm in it is not something S3 returns,
		// but if it ever did, "encrypted" would be a claim nothing supports.
		return
	}
	names := make([]string, 0, len(algorithms))
	for a := range algorithms {
		names = append(names, a)
	}
	sort.Strings(names)
	r.SetAttr(model.AttrSSEAlgorithm, strings.Join(names, ","))
	r.Encrypted = aws.Bool(true)
}

// applyS3PublicAccess decides whether a bucket is reachable from the internet.
//
// Neither input answers it alone. GetBucketPolicyStatus judges the bucket
// policy and knows nothing about ACLs or Block Public Access;
// GetPublicAccessBlock reports four switches that do two different jobs. Of
// the four, BlockPublicAcls and BlockPublicPolicy are write-time guards — they
// reject new public configurations and say nothing about the ones already in
// place. IgnorePublicAcls and RestrictPublicBuckets are the enforcers: they
// neutralize public ACLs and public policies that already exist.
//
// So the flag is written only in the two cases these calls actually decide:
//
//   - Both enforcers on: nothing can grant public access, whatever the policy
//     or the ACLs say. false, provably.
//   - The policy is public and RestrictPublicBuckets is off: the grant is
//     live. true.
//
// Everything else stays nil. The common leftover is a bucket with no public
// policy and public ACLs left unblocked — this scanner does not call
// GetBucketAcl, so it has no evidence either way, and a false there would be a
// security field asserting safety it never checked.
//
// It is exported, and takes plain pointers rather than the SDK's configuration
// type, so the demo fixture reaches the same verdict through the same code. A
// fixture that hand-wrote its exposure flags could claim a bucket is safe on
// evidence this function would refuse.
func ApplyS3PublicAccess(r *model.Resource, bpa *S3PublicAccess, isPublic *bool) {
	var ignoreACLs, restrict bool
	if bpa != nil {
		r.SetBoolAttr(model.AttrBlockPublicACLs, bpa.BlockACLs)
		r.SetBoolAttr(model.AttrIgnorePublicACLs, bpa.IgnoreACLs)
		r.SetBoolAttr(model.AttrBlockPublicPolicy, bpa.BlockPolicy)
		r.SetBoolAttr(model.AttrRestrictPublicBuckets, bpa.Restrict)
		ignoreACLs = aws.ToBool(bpa.IgnoreACLs)
		restrict = aws.ToBool(bpa.Restrict)
	}
	r.SetBoolAttr(model.AttrPolicyIsPublic, isPublic)

	switch {
	case ignoreACLs && restrict:
		r.PubliclyAccessible = aws.Bool(false)
	case aws.ToBool(isPublic) && !restrict:
		r.PubliclyAccessible = aws.Bool(true)
	}
}

// S3PublicAccess is a bucket's four Block Public Access settings. A nil field
// is a setting S3 did not report, which is not the same as one turned off.
type S3PublicAccess struct {
	BlockACLs   *bool
	IgnoreACLs  *bool
	BlockPolicy *bool
	Restrict    *bool
}

func s3PublicAccessFrom(c *s3types.PublicAccessBlockConfiguration) *S3PublicAccess {
	if c == nil {
		return nil
	}
	return &S3PublicAccess{
		BlockACLs:   c.BlockPublicAcls,
		IgnoreACLs:  c.IgnorePublicAcls,
		BlockPolicy: c.BlockPublicPolicy,
		Restrict:    c.RestrictPublicBuckets,
	}
}

// The codes S3 answers with when the thing being asked about was never
// configured. None is modelled as a type in aws-sdk-go-v2, so they are matched
// by code through smithy.APIError.
//
// Each is data, not a failure: "this bucket has no tags" and "this bucket has
// no encryption configuration" are answers. Ledgering them would report a
// correctly-scanned account as partially unreadable — nine hundred entries for
// nine hundred untagged buckets — and bury the one denial that matters.
const (
	s3CodeNoSuchTagSet              = "NoSuchTagSet"
	s3CodeNoEncryptionConfiguration = "ServerSideEncryptionConfigurationNotFoundError"
	s3CodeNoSuchPublicAccessBlock   = "NoSuchPublicAccessBlockConfiguration"
	s3CodeNoSuchBucketPolicy        = "NoSuchBucketPolicy"
)

// s3ErrorCode returns the API error code of err, or "" if err is not one.
func s3ErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// s3CallFailures aggregates per-bucket detail-call outcomes by operation, so a
// region where one call is denied for every bucket produces a single ledger
// entry naming that call and the count, rather than one entry per bucket.
type s3CallFailures struct {
	mu    sync.Mutex
	stats map[string]*s3CallStat
}

type s3CallStat struct {
	attempts int
	failed   int
	first    error
}

// answered reports whether a detail call produced a usable answer, and records
// the outcome.
//
// Three cases, and the difference between the last two is the point:
//
//   - Success: usable.
//   - The documented absence code (pass "" for a call that has none): not
//     usable, but not a failure either — the bucket simply has none of that
//     thing, and the caller writes nothing.
//   - Anything else: not usable, and a coverage gap. AccessDenied in
//     particular must reach the ledger, because a bucket whose encryption
//     blueprint was not allowed to read looks exactly like a bucket with none.
func (f *s3CallFailures) answered(op string, err error, absent string) bool {
	switch {
	case err == nil:
		f.record(op, nil)
		return true
	case absent != "" && s3ErrorCode(err) == absent:
		f.record(op, nil)
		return false
	default:
		f.record(op, err)
		return false
	}
}

func (f *s3CallFailures) record(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stats == nil {
		f.stats = map[string]*s3CallStat{}
	}
	st, ok := f.stats[op]
	if !ok {
		st = &s3CallStat{}
		f.stats[op] = st
	}
	st.attempts++
	if err != nil {
		st.failed++
		if st.first == nil {
			st.first = err
		}
	}
}

// err returns one joined error naming every operation that failed, ordered by
// operation name so the ledger text is deterministic across runs.
func (f *s3CallFailures) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make([]string, 0, len(f.stats))
	for op, st := range f.stats {
		if st.failed > 0 {
			ops = append(ops, op)
		}
	}
	sort.Strings(ops)
	errs := make([]error, 0, len(ops))
	for _, op := range ops {
		st := f.stats[op]
		errs = append(errs, fmt.Errorf("%s: %d of %d buckets: %w", op, st.failed, st.attempts, st.first))
	}
	return errors.Join(errs...)
}
