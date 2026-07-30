package scanners

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// lambdaScanner censuses Lambda functions.
//
// Every other service in this census is here because it bills while idle.
// Lambda is the inverse: a function nobody invokes costs nothing, so it never
// appears in a cost report, never shows up in a rightsizing recommendation, and
// nothing about it ever prompts anyone to look. That is precisely how a
// function on a runtime AWS stopped patching in 2021 is still sitting in an
// account in 2026, reachable from an API gateway somebody also forgot.
//
// So this scanner exists for the lifecycle finding rather than the spend one:
// the runtime identifier, matched against AWS's published deprecation table.
// The configuration that does drive cost when the function *is* invoked —
// memory, timeout, ephemeral storage — comes along because it is in the same
// response, and because a function provisioned at 10 GB to do the work of 256
// MB is paying forty times over on every invocation.
//
// One paginated call, then one ListTags per function: ListFunctions returns no
// tags, and there is no batch form.
type lambdaScanner struct{}

func init() { scan.Register(lambdaScanner{}) }

func (lambdaScanner) Service() string { return model.ServiceLambda }

func (lambdaScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := lambda.NewFromConfig(cfg)
	var out []model.Resource

	// Same aggregation as the other per-resource tag calls: a failure never
	// drops the function, but swallowing it would quietly count the function as
	// untagged and worsen the tag-hygiene numbers with nothing in the ledger to
	// explain why.
	agg := tagFailures{}
	getTags := func(arn string) map[string]string {
		tags, err := lambdaTags(ctx, client, arn)
		agg.record(err)
		return tags
	}

	// ListFunctions returns only $LATEST for each function. Published versions
	// and aliases are deliberately not enumerated: they share the function's
	// runtime and configuration, so listing them would multiply one lifecycle
	// finding into a row per version and make an estate look worse in exact
	// proportion to how carefully it versions its deploys.
	pages := lambda.NewListFunctionsPaginator(client, &lambda.ListFunctionsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			// Partial results per the Scanner contract.
			return out, errors.Join(fmt.Errorf("list functions: %w", err), agg.err())
		}
		for _, fn := range page.Functions {
			r := lambdaFunctionResource(fn, region, accountID)
			r.Tags = getTags(r.ARN)
			out = append(out, r)
		}
	}
	return out, agg.err()
}

func lambdaFunctionResource(fn lambdatypes.FunctionConfiguration, region, accountID string) model.Resource {
	name := aws.ToString(fn.FunctionName)

	r := model.Resource{
		// Lambda reports the ARN, so it is used rather than rebuilt. It is the
		// diff's match key and the cost join key, and a constructed one would
		// have to guess the partition.
		ARN:       aws.ToString(fn.FunctionArn),
		Service:   model.ServiceLambda,
		Type:      model.TypeLambdaFunction,
		Name:      name,
		Status:    string(fn.State),
		Region:    region,
		AccountID: accountID,
		// CreatedAt stays nil. Lambda reports LastModified and nothing else —
		// there is no creation time in the API — and reading one into the other
		// would turn "deployed last week" into "created last week" for a
		// function that has existed since 2018. The date is recorded for what it
		// is, under AttrLastModified.
		//
		// PubliclyAccessible stays nil too: reachability is a function URL or a
		// resource policy, both separate calls this scanner does not make, and
		// a function with neither is still reachable through an API Gateway that
		// lives in another service entirely. Answering false from the absence of
		// a call blueprint never made would be the worst kind of wrong — a
		// security field asserting safety it did not check.
		//
		// Encrypted stays nil on the same grounds. KMSKeyArn describes the key
		// over environment variables and snapshots, not "is this function's data
		// encrypted", and every function's env vars are encrypted at rest
		// regardless. Reporting the presence of a customer managed key as
		// Encrypted=true would make the AWS-owned-key default read as
		// unencrypted, which is false.
	}

	// The runtime identifier whole, exactly as AWS names it. This is the key
	// the deprecation table is matched on (see model.lifecyclePlatform), so it
	// must not be normalized, lowercased or split here.
	//
	// Absent for a container-image function: Runtime is an enum whose zero
	// value is the empty string, SetAttr skips empties, and the honest reading
	// of a missing runtime is "the runtime is inside an image blueprint cannot
	// open", not "unknown". No lifecycle verdict follows from an absent key,
	// which is the correct outcome — the base image may well be deprecated, but
	// nothing in this response says so.
	r.SetAttr(model.AttrRuntime, string(fn.Runtime))
	r.SetAttr(model.AttrPackageType, string(fn.PackageType))
	r.SetAttr(model.AttrArchitecture, lambdaArchitectures(fn.Architectures))
	r.SetAttr(model.AttrLastModified, aws.ToString(fn.LastModified))

	if fn.VpcConfig != nil {
		// A VPC-attached function holds Hyperplane ENIs and can reach private
		// resources; one without VpcConfig runs in Lambda's own network. The
		// key is absent for the latter rather than blank, matching how every
		// other scanner reports a resource that is not in a VPC.
		r.SetAttr(model.AttrVPCID, aws.ToString(fn.VpcConfig.VpcId))
		r.SetAttr(model.AttrSubnetID, strings.Join(slices.Sorted(slices.Values(fn.VpcConfig.SubnetIds)), ","))
	}

	// Pointers, so a function whose configuration AWS did not report leaves the
	// key absent instead of claiming 0 MB of memory or a zero-second timeout —
	// both of which are impossible values that would read as real ones.
	r.SetMeasureInt32(model.MeasureMemoryMB, fn.MemorySize)
	r.SetMeasureInt32(model.MeasureTimeoutSeconds, fn.Timeout)
	if fn.EphemeralStorage != nil {
		r.SetMeasureInt32(model.MeasureEphemeralStorageMB, fn.EphemeralStorage.Size)
	}

	// CodeSize is a plain int64 in the API, not a pointer: Lambda always
	// reports it, and there is no absent case to distinguish. It is stored
	// unconditionally, including the zero a container-image function reports,
	// because that zero is the finding — the code is in ECR and this function's
	// bytes are not counted against the region's code storage quota.
	r.SetMeasure(model.MeasureCodeSizeBytes, fn.CodeSize)

	// No invocation count, duration or error rate. All three are CloudWatch
	// questions, and a function that has never been called reports no
	// datapoints at all — so a zero written here would be indistinguishable
	// from a function that CloudWatch simply had nothing to say about, and
	// "never invoked" is exactly the conclusion a reader would act on.
	return r
}

// lambdaArchitectures renders the instruction sets a function declares as one
// sorted, comma-separated value. Almost always a single entry; the slice form
// is AWS's, so it is preserved rather than collapsed to the first element.
func lambdaArchitectures(arches []lambdatypes.Architecture) string {
	return joinIDs(arches, func(a lambdatypes.Architecture) *string {
		s := string(a)
		return &s
	})
}

// lambdaTags fetches one function's tags. ListFunctions returns none inline and
// Lambda offers no batch tag call, so this is one request per function — the
// N+1 is the API's, not a choice, and the alternative is reporting every
// function in the account as untagged.
func lambdaTags(ctx context.Context, client *lambda.Client, arn string) (map[string]string, error) {
	out, err := client.ListTags(ctx, &lambda.ListTagsInput{Resource: aws.String(arn)})
	if err != nil {
		return nil, fmt.Errorf("list tags %s: %w", arn, err)
	}
	if len(out.Tags) == 0 {
		// nil rather than an empty map, matching toTagMap: an untagged resource
		// carries no map at all, so the JSON has no "tags" key to be misread as
		// an empty set someone configured.
		return nil, nil
	}
	// Cloned rather than aliased — the census outlives the SDK response, and
	// every other scanner hands Resource.Tags a map it owns.
	return maps.Clone(out.Tags), nil
}
