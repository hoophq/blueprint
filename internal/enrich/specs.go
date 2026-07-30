package enrich

import "github.com/hoophq/blueprint/internal/model"

// Spec declares one CloudWatch time series to attach to one resource type.
// Adding a metric to the census is adding an entry to metricSpecs — the
// enricher itself knows nothing about RDS, S3, or anything else.
type Spec struct {
	// Namespace and MetricName identify the series, exactly as CloudWatch
	// names them.
	Namespace  string
	MetricName string

	// Stat is the statistic to aggregate each period by. It is part of what
	// the number means, so it is chosen per metric rather than defaulted:
	// "how full did this volume get today" is a Minimum of free space, and
	// answering it with an Average would smooth away the moment that matters.
	Stat string

	// Period is the aggregation bucket in seconds. Zero means defaultPeriod.
	//
	// It cannot go below the metric's own publishing interval. Daily metrics
	// (S3 BucketSizeBytes, and storage metrics generally) return nothing at a
	// finer period — silently, as an empty result rather than an error, which
	// reads exactly like "this resource has no data".
	Period int32

	// Measure is the model key the newest datapoint is stored under, always
	// via Resource.SetObservedMeasure so the reading carries its own age.
	Measure string

	// Dimensions maps a resource to the dimension set that selects its series.
	// Returning an entry with an empty name or value drops the query: a
	// dimension we cannot fill is a series we cannot identify, and querying a
	// partial dimension set would silently match an account-wide aggregate.
	Dimensions func(*model.Resource) map[string]string

	// Enumerate names a dimension whose values cannot be known in advance.
	// Discovery supplies them, one query per value, and the readings are
	// summed into a single Measure.
	//
	// It exists for S3. BucketSizeBytes has no all-storage-classes dimension
	// value — CloudWatch publishes one series per class — so "how big is this
	// bucket" is only answerable as a sum. The set of classes is not
	// enumerable through any API and AWS keeps adding to it, so hardcoding it
	// would mean a bucket's size quietly dropping a tier the day one shipped.
	// Asking ListMetrics what exists costs nothing and cannot go stale.
	//
	// The cost of that is a hard dependency on discovery: an enumerated spec
	// cannot fail open the way an ordinary one does, because without the list
	// there is no query to widen to. Those queries are dropped and reported.
	Enumerate string
}

// metricSpecs is the registry, keyed by the CloudFormation type on
// model.Resource.
//
// Aurora, DocumentDB and Neptune are deliberately absent: the RDS scanner
// records clustered engines as clusters (TypeRDSCluster and friends) and skips
// their member instances, and a cluster has no FreeStorageSpace series —
// Aurora storage grows on demand, so the question the metric answers does not
// exist for it. Keying on the instance type is what excludes them, so no
// engine check is needed here.
var metricSpecs = map[string][]Spec{
	model.TypeRDSInstance: {{
		Namespace:  "AWS/RDS",
		MetricName: "FreeStorageSpace",
		Stat:       "Minimum",
		Measure:    model.MeasureFreeStorageBytes,
		Dimensions: dimension("DBInstanceIdentifier", func(r *model.Resource) string { return r.Name }),
	}},

	// S3 is the reason this stage exists. A bucket's control-plane record says
	// nothing about how much is in it, and the only free source for the
	// difference between an empty bucket and a petabyte is these two daily
	// metrics.
	//
	// Both are Average, which is not a choice: S3 publishes one datapoint per
	// day and AWS documents Average as the only valid statistic for either. It
	// reads oddly next to FreeStorageSpace's Minimum — there, picking the
	// statistic is picking what the number means — but with a single datapoint
	// in the bucket every statistic returns the same value.
	//
	// Neither is what AWS bills for, and both overstate what a bucket listing
	// would show: the size includes noncurrent versions and the parts of
	// incomplete multipart uploads, and the count includes those plus delete
	// markers. That is the point. Those are the bytes that cost money and do
	// not appear when anyone looks.
	model.TypeS3Bucket: {{
		Namespace:  "AWS/S3",
		MetricName: "BucketSizeBytes",
		Stat:       "Average",
		Measure:    model.MeasureSizeBytes,
		Enumerate:  "StorageType",
		Dimensions: dimension("BucketName", func(r *model.Resource) string { return r.Name }),
	}, {
		Namespace:  "AWS/S3",
		MetricName: "NumberOfObjects",
		Stat:       "Average",
		Measure:    model.MeasureObjectCount,
		// The one metric of the two that does have an all-classes rollup, so
		// it needs no enumeration.
		Dimensions: func(r *model.Resource) map[string]string {
			return map[string]string{"BucketName": r.Name, "StorageType": "AllStorageTypes"}
		},
	}},
}

// dimension builds a single-dimension mapper, the common case.
func dimension(name string, value func(*model.Resource) string) func(*model.Resource) map[string]string {
	return func(r *model.Resource) map[string]string {
		return map[string]string{name: value(r)}
	}
}
