package scanners

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// rdsScanner covers the whole RDS control plane: classic RDS instances plus
// Aurora, DocumentDB, Neptune and Multi-AZ DB clusters (all returned by
// DescribeDBClusters, which the dedicated docdb/neptune APIs merely filter).
// Clusters are the census unit for clustered engines; member instances are
// skipped so nothing is counted twice.
type rdsScanner struct{}

func init() { scan.Register(rdsScanner{}) }

func (rdsScanner) Service() string { return "rds" }

func (rdsScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := rds.NewFromConfig(cfg)
	var out []model.Resource

	clusters := rds.NewDescribeDBClustersPaginator(client, &rds.DescribeDBClustersInput{})
	for clusters.HasMorePages() {
		page, err := clusters.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, c := range page.DBClusters {
			out = append(out, clusterResource(c, region, accountID))
		}
	}

	instances := rds.NewDescribeDBInstancesPaginator(client, &rds.DescribeDBInstancesInput{})
	for instances.HasMorePages() {
		page, err := instances.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, inst := range page.DBInstances {
			// Cluster members are already represented by their cluster.
			if aws.ToString(inst.DBClusterIdentifier) != "" {
				continue
			}
			out = append(out, instanceResource(inst, region, accountID))
		}
	}
	return out, nil
}

func clusterResource(c rdstypes.DBCluster, region, accountID string) model.Resource {
	service := classifyEngine(aws.ToString(c.Engine))
	r := model.Resource{
		ARN:       aws.ToString(c.DBClusterArn),
		Service:   service,
		Type:      clusterType(service),
		Name:      aws.ToString(c.DBClusterIdentifier),
		Status:    aws.ToString(c.Status),
		Region:    region,
		AccountID: accountID,
		CreatedAt: c.ClusterCreateTime,
		Tags:      toTagMap(c.TagList, rdsTagKV),
		// Passed through as-is: PubliclyAccessible is only set by the API for
		// Multi-AZ DB clusters and stays nil ("not reported") for Aurora.
		PubliclyAccessible: c.PubliclyAccessible,
		Encrypted:          c.StorageEncrypted,
	}
	r.SetAttr(model.AttrEngine, aws.ToString(c.Engine))
	r.SetAttr(model.AttrEngineVersion, aws.ToString(c.EngineVersion))
	r.SetAttr(model.AttrInstanceClass, aws.ToString(c.DBClusterInstanceClass))
	r.SetAttr(model.AttrEndpoint, aws.ToString(c.Endpoint))
	r.SetBoolAttr(model.AttrMultiAZ, c.MultiAZ)
	setAllocatedStorage(&r, c.AllocatedStorage)
	r.SetMeasureInt32(model.MeasureBackupRetentionDays, c.BackupRetentionPeriod)
	return r
}

func instanceResource(inst rdstypes.DBInstance, region, accountID string) model.Resource {
	endpoint := ""
	if inst.Endpoint != nil {
		endpoint = aws.ToString(inst.Endpoint.Address)
	}
	service := classifyEngine(aws.ToString(inst.Engine))
	r := model.Resource{
		ARN:       aws.ToString(inst.DBInstanceArn),
		Service:   service,
		Type:      instanceType(service),
		Name:      aws.ToString(inst.DBInstanceIdentifier),
		Status:    aws.ToString(inst.DBInstanceStatus),
		Region:    region,
		AccountID: accountID,
		CreatedAt: inst.InstanceCreateTime,
		Tags:      toTagMap(inst.TagList, rdsTagKV),

		PubliclyAccessible: inst.PubliclyAccessible,
		Encrypted:          inst.StorageEncrypted,
	}
	r.SetAttr(model.AttrEngine, aws.ToString(inst.Engine))
	r.SetAttr(model.AttrEngineVersion, aws.ToString(inst.EngineVersion))
	r.SetAttr(model.AttrInstanceClass, aws.ToString(inst.DBInstanceClass))
	r.SetAttr(model.AttrEndpoint, endpoint)
	r.SetBoolAttr(model.AttrMultiAZ, inst.MultiAZ)
	setAllocatedStorage(&r, inst.AllocatedStorage)
	r.SetMeasureInt32(model.MeasureBackupRetentionDays, inst.BackupRetentionPeriod)
	return r
}

// setAllocatedStorage converts the RDS AllocatedStorage gigabyte count to the
// bytes the census records. Zero (Aurora, which manages storage itself) leaves
// the key absent rather than claiming a 0-byte database.
func setAllocatedStorage(r *model.Resource, gb *int32) {
	if aws.ToInt32(gb) <= 0 {
		return
	}
	r.SetMeasure(model.MeasureSizeBytes, int64(aws.ToInt32(gb))<<30)
}

// classifyEngine maps an RDS control-plane engine name to the census service.
func classifyEngine(engine string) string {
	switch {
	case strings.HasPrefix(engine, "aurora"):
		return model.ServiceAurora
	case strings.HasPrefix(engine, "docdb"):
		return model.ServiceDocumentDB
	case strings.HasPrefix(engine, "neptune"):
		return model.ServiceNeptune
	default:
		return model.ServiceRDS
	}
}

// clusterType and instanceType map the census service back to the
// CloudFormation type name. Aurora clusters are AWS::RDS::DBCluster — the
// shared RDS control plane is what returns them — while DocumentDB and
// Neptune have their own CloudFormation namespaces.
func clusterType(service string) string {
	switch service {
	case model.ServiceDocumentDB:
		return model.TypeDocDBCluster
	case model.ServiceNeptune:
		return model.TypeNeptuneCluster
	default:
		return model.TypeRDSCluster
	}
}

func instanceType(service string) string {
	switch service {
	case model.ServiceDocumentDB:
		return model.TypeDocDBInstance
	case model.ServiceNeptune:
		return model.TypeNeptuneInstance
	default:
		return model.TypeRDSInstance
	}
}

func rdsTagKV(t rdstypes.Tag) (*string, *string) { return t.Key, t.Value }
