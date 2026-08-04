// Package scanners holds one file per AWS service scanner. Each scanner
// self-registers with the scan package from init(), implements only
// read-only Describe*/List* calls, and paginates fully.
//
// File ownership (one scanner = one file, no cross-file edits):
//   - rds.go: RDS instances + clusters (classifies Aurora/DocumentDB/Neptune)
//   - dynamodb.go: DynamoDB tables
//   - elasticache.go: ElastiCache clusters + serverless caches
//   - redshift.go: Redshift clusters + serverless
//   - ec2.go: EC2 instances (terminated ones skipped)
//   - ebs.go: EBS volumes + self-owned snapshots (reads self-owned AMIs to
//     attribute snapshots to them, but does not emit AMIs as census rows)
//   - loadbalancer.go: ALB/NLB/GWLB and classic ELBs, with target groups
//   - natgateway.go: NAT gateways
//   - publicip.go: billable public IPv4 — Elastic IPs plus the addresses
//     associated with network interfaces, merged so each IP is one row
//   - lambda.go: Lambda functions
//   - s3.go: S3 buckets (listed per region, then described one by one)
//
// Shared helpers (no scanner of their own):
//   - tags.go: tag-map conversion, GB rounding, tag-failure aggregation
//   - arn.go: partition inference for ARNs built by hand
//   - ids.go: deterministic joining of ID lists into attribute values
package scanners
