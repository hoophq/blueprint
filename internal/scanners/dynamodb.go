package scanners

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// dynamoDBScanner lists every table in the region, then describes each one to
// pull ARN, size, status and billing mode. Regions with hundreds of tables are
// normal, so per-table describe/tag calls run through a small worker pool.
// A single table failing does not abort the scan: its error is collected and
// the remaining tables are still returned (the runner keeps partial results).
type dynamoDBScanner struct{}

func init() { scan.Register(dynamoDBScanner{}) }

func (dynamoDBScanner) Service() string { return "dynamodb" }

// dynamoDescribeWorkers bounds concurrent per-table DescribeTable calls.
const dynamoDescribeWorkers = 4

func (dynamoDBScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := dynamodb.NewFromConfig(cfg)

	var names []string
	var listErr error
	tables := dynamodb.NewListTablesPaginator(client, &dynamodb.ListTablesInput{})
	for tables.HasMorePages() {
		page, err := tables.NextPage(ctx)
		if err != nil {
			// Stop paginating but still describe the names gathered so far:
			// partial results per the Scanner contract. The pagination error
			// is joined into the returned error below.
			listErr = fmt.Errorf("list tables: %w", err)
			break
		}
		names = append(names, page.TableNames...)
	}

	var (
		mu   sync.Mutex
		out  []model.Resource
		errs []error
	)
	sem := make(chan struct{}, dynamoDescribeWorkers)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return // ctx.Err() is recorded once after the pool drains
			}
			defer func() { <-sem }()

			desc, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)})
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("table %s: %w", name, err))
				mu.Unlock()
				return
			}
			// Defensive: a nil Table would panic in this goroutine and crash
			// the whole process; ledger it and move on instead.
			if desc.Table == nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("table %s: DescribeTable returned no table description", name))
				mu.Unlock()
				return
			}
			tags, tagErr := tableTags(ctx, client, aws.ToString(desc.Table.TableArn))

			mu.Lock()
			// A tag failure still keeps the table itself in the census.
			if tagErr != nil {
				errs = append(errs, fmt.Errorf("table %s tags: %w", name, tagErr))
			}
			out = append(out, tableResource(*desc.Table, tags, region, accountID))
			mu.Unlock()
		}(name)
	}
	wg.Wait()

	if listErr != nil {
		errs = append(errs, listErr)
	}
	// On cancellation every in-flight call fails the same way; collapse the
	// per-table "context canceled" noise into the single root cause.
	if err := ctx.Err(); err != nil {
		errs = []error{err}
	}
	return out, errors.Join(errs...)
}

// tableTags fetches all tags for a table ARN. ListTagsOfResource has no SDK
// paginator, so the NextToken loop is manual.
func tableTags(ctx context.Context, client *dynamodb.Client, arn string) (map[string]string, error) {
	var tags []ddbtypes.Tag
	input := &dynamodb.ListTagsOfResourceInput{ResourceArn: aws.String(arn)}
	for {
		page, err := client.ListTagsOfResource(ctx, input)
		if err != nil {
			return nil, err
		}
		tags = append(tags, page.Tags...)
		if aws.ToString(page.NextToken) == "" {
			return toTagMap(tags, func(t ddbtypes.Tag) (*string, *string) { return t.Key, t.Value }), nil
		}
		input.NextToken = page.NextToken
	}
}

func tableResource(t ddbtypes.TableDescription, tags map[string]string, region, accountID string) model.Resource {
	return model.Resource{
		ARN:           aws.ToString(t.TableArn),
		Service:       model.ServiceDynamoDB,
		Kind:          "table",
		Name:          aws.ToString(t.TableName),
		Engine:        "dynamodb",
		InstanceClass: billingMode(t.BillingModeSummary),
		StorageGB:     bytesToGB(aws.ToInt64(t.TableSizeBytes)),
		Status:        string(t.TableStatus),
		Region:        region,
		AccountID:     accountID,
		CreatedAt:     t.CreationDateTime,
		Tags:          tags,
	}
}

// billingMode normalizes the billing mode summary. DynamoDB omits the summary
// for tables that have always been provisioned, so nil means PROVISIONED.
func billingMode(s *ddbtypes.BillingModeSummary) string {
	if s == nil || s.BillingMode == "" {
		return string(ddbtypes.BillingModeProvisioned)
	}
	return string(s.BillingMode)
}

// bytesToGB rounds a byte count up to whole gigabytes, so any non-empty table
// registers at least 1 GB.
func bytesToGB(bytes int64) int32 { return ceilGB(bytes, 1<<30) }
