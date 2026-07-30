package scanners

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/hoophq/blueprint/internal/model"
)

const testLambdaARN = "arn:aws:lambda:us-east-1:" + testAccount + ":function:orders-api"

func TestLambdaFunctionResource(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:      aws.String(testLambdaARN),
		FunctionName:     aws.String("orders-api"),
		Runtime:          lambdatypes.RuntimePython38,
		PackageType:      lambdatypes.PackageTypeZip,
		Architectures:    []lambdatypes.Architecture{lambdatypes.ArchitectureArm64},
		State:            lambdatypes.StateActive,
		MemorySize:       aws.Int32(512),
		Timeout:          aws.Int32(30),
		CodeSize:         4_194_304,
		EphemeralStorage: &lambdatypes.EphemeralStorage{Size: aws.Int32(1024)},
		LastModified:     aws.String("2021-03-14T11:22:33.000+0000"),
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	if r.Service != model.ServiceLambda {
		t.Errorf("Service = %q, want %q", r.Service, model.ServiceLambda)
	}
	if r.Type != model.TypeLambdaFunction {
		t.Errorf("Type = %q, want %q", r.Type, model.TypeLambdaFunction)
	}
	if r.ARN != testLambdaARN {
		t.Errorf("ARN = %q, want the ARN Lambda reported", r.ARN)
	}
	if r.Name != "orders-api" {
		t.Errorf("Name = %q, want %q", r.Name, "orders-api")
	}
	if r.Status != string(lambdatypes.StateActive) {
		t.Errorf("Status = %q, want %q", r.Status, lambdatypes.StateActive)
	}
	if got := r.Attr(model.AttrRuntime); got != string(lambdatypes.RuntimePython38) {
		t.Errorf("runtime = %q, want %q", got, lambdatypes.RuntimePython38)
	}
	if got := r.Attr(model.AttrPackageType); got != string(lambdatypes.PackageTypeZip) {
		t.Errorf("package_type = %q, want %q", got, lambdatypes.PackageTypeZip)
	}
	if got := r.Attr(model.AttrArchitecture); got != "arm64" {
		t.Errorf("architecture = %q, want %q", got, "arm64")
	}
	if got := r.Attr(model.AttrLastModified); got != "2021-03-14T11:22:33.000+0000" {
		t.Errorf("last_modified = %q, want the value Lambda reported", got)
	}
	for _, c := range []struct {
		key  string
		want int64
	}{
		{model.MeasureMemoryMB, 512},
		{model.MeasureTimeoutSeconds, 30},
		{model.MeasureCodeSizeBytes, 4_194_304},
		{model.MeasureEphemeralStorageMB, 1024},
	} {
		got, ok := r.Measure(c.key)
		if !ok || got != c.want {
			t.Errorf("%s = (%d, %v), want (%d, true)", c.key, got, ok, c.want)
		}
	}
}

// Lambda reports when a function last changed and never when it was created.
// Reading the one into the other would turn "deployed last week" into "created
// last week" for a function that has been running since 2018, so CreatedAt
// stays nil and the date is recorded for what it is.
func TestLambdaFunctionHasNoCreationTime(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("orders-api"),
		LastModified: aws.String("2021-03-14T11:22:33.000+0000"),
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	if r.CreatedAt != nil {
		t.Errorf("CreatedAt = %v, want nil: Lambda reports no creation time", r.CreatedAt)
	}
	if r.Attr(model.AttrLastModified) == "" {
		t.Error("last_modified is absent; the date Lambda did report must survive")
	}
}

// A container-image function's runtime lives inside an image blueprint cannot
// open. The absent key is the honest answer, and it must produce no lifecycle
// verdict rather than a default one.
func TestLambdaContainerImageFunctionReportsNoRuntime(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("ml-inference"),
		PackageType:  lambdatypes.PackageTypeImage,
		// Runtime is the enum's zero value — AWS reports none for an image.
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	if _, ok := r.Attributes[model.AttrRuntime]; ok {
		t.Errorf("runtime key present (%q) for a container-image function; absent is the honest answer",
			r.Attr(model.AttrRuntime))
	}
	if got := r.Attr(model.AttrPackageType); got != string(lambdatypes.PackageTypeImage) {
		t.Errorf("package_type = %q, want %q — it is what explains the absent runtime", got, lambdatypes.PackageTypeImage)
	}
}

// The zero CodeSize an image function reports is a real value, not a gap: the
// code is in ECR and none of it counts against the region's code storage quota.
// A `> 0` guard here would erase that distinction, which is the recurring bug
// the honesty guardrail names.
func TestLambdaStoresACompleteZeroCodeSize(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("ml-inference"),
		PackageType:  lambdatypes.PackageTypeImage,
		CodeSize:     0,
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	got, ok := r.Measure(model.MeasureCodeSizeBytes)
	if !ok {
		t.Fatal("code_size_bytes is absent; a reported zero must be stored")
	}
	if got != 0 {
		t.Errorf("code_size_bytes = %d, want 0", got)
	}
}

// Configuration AWS did not report leaves the key absent. Zero would be worse
// than useless here: no function can run with 0 MB or time out in 0 seconds, so
// the number would read as a real, alarming value.
func TestLambdaOmitsUnreportedConfiguration(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("orders-api"),
		// MemorySize, Timeout and EphemeralStorage all nil.
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	for _, key := range []string{model.MeasureMemoryMB, model.MeasureTimeoutSeconds, model.MeasureEphemeralStorageMB} {
		if v, ok := r.Measure(key); ok {
			t.Errorf("%s = %d, want absent: AWS reported no value", key, v)
		}
	}
}

// A function outside a VPC runs in Lambda's own network. That is not a blank
// VPC, so the keys are absent — the same reading every other scanner gives.
func TestLambdaFunctionOutsideAVPCHasNoNetworkKeys(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("orders-api"),
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	for _, key := range []string{model.AttrVPCID, model.AttrSubnetID} {
		if v, ok := r.Attributes[key]; ok {
			t.Errorf("%s = %q, want absent for a function with no VPC config", key, v)
		}
	}
}

func TestLambdaFunctionInAVPCRecordsItsPlacement(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("orders-api"),
		VpcConfig: &lambdatypes.VpcConfigResponse{
			VpcId: aws.String("vpc-0abc"),
			// Deliberately unsorted: AWS promises no order and the artifact has
			// to be byte-stable across scans or every diff is noise.
			SubnetIds: []string{"subnet-0c", "subnet-0a", "subnet-0b"},
		},
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	if got := r.Attr(model.AttrVPCID); got != "vpc-0abc" {
		t.Errorf("vpc_id = %q, want %q", got, "vpc-0abc")
	}
	if got := r.Attr(model.AttrSubnetID); got != "subnet-0a,subnet-0b,subnet-0c" {
		t.Errorf("subnet_id = %q, want the subnets sorted", got)
	}
}

// The runtime identifier is the key AWS's deprecation table is matched on, so
// the scanner must hand it over untouched — not lowercased, not split, not
// normalized. This pins the end-to-end path: a deprecated runtime read off a
// describe response must come out of Finalize with the red pill on.
func TestLambdaDeprecatedRuntimeFlagsEOL(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("legacy-cron"),
		Runtime:      lambdatypes.RuntimeNodejs12x,
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)
	if got := r.Attr(model.AttrRuntime); got != "nodejs12.x" {
		t.Fatalf("runtime = %q, want the identifier verbatim", got)
	}

	snap := &model.Snapshot{Resources: []model.Resource{r}}
	snap.Finalize()

	if !snap.Resources[0].EOL {
		t.Error("nodejs12.x did not flag EOL; AWS deprecated it on 2023-03-31")
	}
	if got := snap.Resources[0].EOLDate; got != "2023-03-31" {
		t.Errorf("EOLDate = %q, want %q", got, "2023-03-31")
	}
}

// A supported runtime gets no verdict. The table carries only runtimes AWS has
// already deprecated, so silence here means "not deprecated", not "unknown".
func TestLambdaSupportedRuntimeDoesNotFlagEOL(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("orders-api"),
		Runtime:      lambdatypes.RuntimePython312,
	}

	snap := &model.Snapshot{Resources: []model.Resource{lambdaFunctionResource(fn, "us-east-1", testAccount)}}
	snap.Finalize()

	if snap.Resources[0].EOL {
		t.Errorf("python3.12 flagged EOL %q; AWS still supports it", snap.Resources[0].EOLDate)
	}
}

// Reachability is a function URL or a resource policy — separate calls this
// scanner does not make — and a function with neither is still reachable
// through an API Gateway in another service. Answering false from a call that
// was never made would be a security field asserting safety it did not check.
func TestLambdaFunctionMakesNoExposureClaim(t *testing.T) {
	fn := lambdatypes.FunctionConfiguration{
		FunctionArn:  aws.String(testLambdaARN),
		FunctionName: aws.String("orders-api"),
		KMSKeyArn:    aws.String("arn:aws:kms:us-east-1:" + testAccount + ":key/abc"),
	}

	r := lambdaFunctionResource(fn, "us-east-1", testAccount)

	if r.PubliclyAccessible != nil {
		t.Errorf("PubliclyAccessible = %v, want nil: no call was made that could answer it", *r.PubliclyAccessible)
	}
	if r.Encrypted != nil {
		t.Errorf("Encrypted = %v, want nil: a KMS key over env vars is not a statement about function data", *r.Encrypted)
	}
}

func TestLambdaArchitectures(t *testing.T) {
	cases := []struct {
		name  string
		in    []lambdatypes.Architecture
		want  string
		wantP bool // want the attribute present
	}{
		{"single", []lambdatypes.Architecture{lambdatypes.ArchitectureX8664}, "x86_64", true},
		{"sorted", []lambdatypes.Architecture{lambdatypes.ArchitectureX8664, lambdatypes.ArchitectureArm64}, "arm64,x86_64", true},
		{"none reported", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := lambdatypes.FunctionConfiguration{
				FunctionArn:   aws.String(testLambdaARN),
				FunctionName:  aws.String("orders-api"),
				Architectures: c.in,
			}
			r := lambdaFunctionResource(fn, "us-east-1", testAccount)
			got, ok := r.Attributes[model.AttrArchitecture]
			if ok != c.wantP {
				t.Fatalf("architecture present = %v, want %v", ok, c.wantP)
			}
			if got != c.want {
				t.Errorf("architecture = %q, want %q", got, c.want)
			}
		})
	}
}
