package scanners

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/hoophq/dbcensus/internal/model"
	"github.com/hoophq/dbcensus/internal/scan"
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
	return model.Resource{
		ARN:           aws.ToString(c.DBClusterArn),
		Service:       classifyEngine(aws.ToString(c.Engine)),
		Kind:          "cluster",
		Name:          aws.ToString(c.DBClusterIdentifier),
		Engine:        aws.ToString(c.Engine),
		EngineVersion: aws.ToString(c.EngineVersion),
		InstanceClass: aws.ToString(c.DBClusterInstanceClass),
		StorageGB:     aws.ToInt32(c.AllocatedStorage),
		MultiAZ:       aws.ToBool(c.MultiAZ),
		Status:        aws.ToString(c.Status),
		Endpoint:      aws.ToString(c.Endpoint),
		Region:        region,
		AccountID:     accountID,
		CreatedAt:     c.ClusterCreateTime,
		Tags:          toTagMap(c.TagList, rdsTagKV),
	}
}

func instanceResource(inst rdstypes.DBInstance, region, accountID string) model.Resource {
	endpoint := ""
	if inst.Endpoint != nil {
		endpoint = aws.ToString(inst.Endpoint.Address)
	}
	return model.Resource{
		ARN:           aws.ToString(inst.DBInstanceArn),
		Service:       classifyEngine(aws.ToString(inst.Engine)),
		Kind:          "instance",
		Name:          aws.ToString(inst.DBInstanceIdentifier),
		Engine:        aws.ToString(inst.Engine),
		EngineVersion: aws.ToString(inst.EngineVersion),
		InstanceClass: aws.ToString(inst.DBInstanceClass),
		StorageGB:     aws.ToInt32(inst.AllocatedStorage),
		MultiAZ:       aws.ToBool(inst.MultiAZ),
		Status:        aws.ToString(inst.DBInstanceStatus),
		Endpoint:      endpoint,
		Region:        region,
		AccountID:     accountID,
		CreatedAt:     inst.InstanceCreateTime,
		Tags:          toTagMap(inst.TagList, rdsTagKV),
	}
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

func rdsTagKV(t rdstypes.Tag) (*string, *string) { return t.Key, t.Value }
