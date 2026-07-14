package scanners

// Pin the SDK modules every scanner needs so go.mod/go.sum stay stable while
// scanner files are developed in parallel. Remove once all scanners import
// these directly.
import (
	_ "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	_ "github.com/aws/aws-sdk-go-v2/service/elasticache"
	_ "github.com/aws/aws-sdk-go-v2/service/organizations"
	_ "github.com/aws/aws-sdk-go-v2/service/rds"
	_ "github.com/aws/aws-sdk-go-v2/service/redshift"
	_ "github.com/aws/aws-sdk-go-v2/service/redshiftserverless"
)
