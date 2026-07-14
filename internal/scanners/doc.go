// Package scanners holds one file per AWS service scanner. Each scanner
// self-registers with the scan package from init(), implements only
// read-only Describe*/List* calls, and paginates fully.
//
// File ownership (one scanner = one file, no cross-file edits):
//   - rds.go: RDS instances + clusters (classifies Aurora/DocumentDB/Neptune)
//   - dynamodb.go: DynamoDB tables
//   - elasticache.go: ElastiCache clusters + serverless caches
//   - redshift.go: Redshift clusters + serverless
package scanners
