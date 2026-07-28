package scanners

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/redshift"
	redshifttypes "github.com/aws/aws-sdk-go-v2/service/redshift/types"
	"github.com/aws/aws-sdk-go-v2/service/redshiftserverless"
	rsstypes "github.com/aws/aws-sdk-go-v2/service/redshiftserverless/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// redshiftScanner covers provisioned Redshift clusters and Redshift
// Serverless workgroups. The serverless API is separate and not available in
// every region; if it fails after the provisioned pass succeeded, the
// provisioned results are returned together with the error so the runner
// keeps them and ledgers the gap.
type redshiftScanner struct{}

func init() { scan.Register(redshiftScanner{}) }

func (redshiftScanner) Service() string { return "redshift" }

func (redshiftScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := redshift.NewFromConfig(cfg)
	var out []model.Resource

	clusters := redshift.NewDescribeClustersPaginator(client, &redshift.DescribeClustersInput{})
	for clusters.HasMorePages() {
		page, err := clusters.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, c := range page.Clusters {
			out = append(out, redshiftClusterResource(c, region, accountID))
		}
	}

	serverless := redshiftserverless.NewFromConfig(cfg)
	// Tag failures keep the workgroup (with nil tags) but are aggregated into
	// one returned error so the runner ledgers the gap once.
	agg := tagFailures{}
	workgroups := redshiftserverless.NewListWorkgroupsPaginator(serverless, &redshiftserverless.ListWorkgroupsInput{})
	for workgroups.HasMorePages() {
		page, err := workgroups.NextPage(ctx)
		if err != nil {
			return out, errors.Join(err, agg.err())
		}
		for _, w := range page.Workgroups {
			r := workgroupResource(w, region, accountID)
			tags, tagErr := redshiftServerlessTags(ctx, serverless, r.ARN)
			agg.record(tagErr)
			r.Tags = tags
			out = append(out, r)
		}
	}
	return out, agg.err()
}

func redshiftClusterResource(c redshifttypes.Cluster, region, accountID string) model.Resource {
	endpoint := ""
	if c.Endpoint != nil {
		endpoint = aws.ToString(c.Endpoint.Address)
	}
	// MultiAZ arrives as a string ("Enabled"/"Disabled"); nil means the API
	// did not report it and must stay nil, not become false.
	var multiAZ *bool
	if c.MultiAZ != nil {
		multiAZ = aws.Bool(strings.EqualFold(aws.ToString(c.MultiAZ), "enabled"))
	}
	// The partition is not hardcoded: GovCloud/China clusters live in
	// aws-us-gov/aws-cn. ClusterNamespaceArn (returned by DescribeClusters)
	// carries the right partition; fall back to "aws" when it is absent.
	r := model.Resource{
		ARN:       RedshiftClusterARN(arnPartition(aws.ToString(c.ClusterNamespaceArn)), region, accountID, aws.ToString(c.ClusterIdentifier)),
		Service:   model.ServiceRedshift,
		Type:      model.TypeRedshiftCluster,
		Name:      aws.ToString(c.ClusterIdentifier),
		Status:    aws.ToString(c.ClusterStatus),
		Region:    region,
		AccountID: accountID,
		CreatedAt: c.ClusterCreateTime,
		Tags:      toTagMap(c.Tags, func(t redshifttypes.Tag) (*string, *string) { return t.Key, t.Value }),

		PubliclyAccessible: c.PubliclyAccessible,
		Encrypted:          c.Encrypted,
	}
	r.SetAttr(model.AttrEngine, "redshift")
	r.SetAttr(model.AttrEngineVersion, aws.ToString(c.ClusterVersion))
	r.SetAttr(model.AttrInstanceClass, aws.ToString(c.NodeType))
	r.SetAttr(model.AttrEndpoint, endpoint)
	r.SetBoolAttr(model.AttrMultiAZ, multiAZ)
	// Reported in whole megabytes. Nil means the cluster description carried no
	// capacity at all; a reported zero is a real reading and is stored as one.
	if mb := c.TotalStorageCapacityInMegaBytes; mb != nil {
		r.SetMeasure(model.MeasureSizeBytes, *mb<<20)
	}
	// Retention 0 means automated snapshots are disabled — the same
	// "no backups" signal BackupRetentionPeriod carries on RDS.
	r.SetMeasureInt32(model.MeasureBackupRetentionDays, c.AutomatedSnapshotRetentionPeriod)
	return r
}

func workgroupResource(w rsstypes.Workgroup, region, accountID string) model.Resource {
	endpoint := ""
	if w.Endpoint != nil {
		endpoint = aws.ToString(w.Endpoint.Address)
	}
	r := model.Resource{
		ARN:       aws.ToString(w.WorkgroupArn),
		Service:   model.ServiceRedshift,
		Type:      model.TypeRedshiftServerlessWorkgroup,
		Name:      aws.ToString(w.WorkgroupName),
		Status:    string(w.Status),
		Region:    region,
		AccountID: accountID,
		CreatedAt: w.CreationDate,
		// Serverless namespaces are always encrypted and have no retention
		// knob, so only public accessibility is reported.
		PubliclyAccessible: w.PubliclyAccessible,
	}
	r.SetAttr(model.AttrEngine, "redshift-serverless")
	// Base capacity is a number of Redshift Processing Units, not an instance
	// class, so it is recorded as a measure under its own AWS name.
	r.SetMeasureInt32(model.MeasureBaseCapacityRPU, w.BaseCapacity)
	r.SetAttr(model.AttrEndpoint, endpoint)
	return r
}

// RedshiftClusterARN builds a provisioned cluster ARN: DescribeClusters does
// not return one (ClusterNamespaceArn identifies the namespace, not the
// cluster). Exported so the demo fixture builds ARNs with the same shape.
func RedshiftClusterARN(partition, region, accountID, clusterID string) string {
	return fmt.Sprintf("arn:%s:redshift:%s:%s:cluster:%s", partition, region, accountID, clusterID)
}

// arnPartition extracts the partition from an ARN ("arn:PARTITION:...");
// empty or malformed input falls back to the default "aws" partition.
func arnPartition(arn string) string {
	if parts := strings.SplitN(arn, ":", 3); len(parts) == 3 && parts[0] == "arn" && parts[1] != "" {
		return parts[1]
	}
	return "aws"
}

// redshiftServerlessTags fetches tags for one workgroup ARN. A failure yields
// nil tags plus the error; the caller keeps the workgroup and aggregates the
// failure.
func redshiftServerlessTags(ctx context.Context, client *redshiftserverless.Client, arn string) (map[string]string, error) {
	if arn == "" {
		return nil, nil
	}
	resp, err := client.ListTagsForResource(ctx, &redshiftserverless.ListTagsForResourceInput{ResourceArn: aws.String(arn)})
	if err != nil {
		return nil, err
	}
	return toTagMap(resp.Tags, func(t rsstypes.Tag) (*string, *string) { return t.Key, t.Value }), nil
}
