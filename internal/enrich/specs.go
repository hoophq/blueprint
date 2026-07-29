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
}

// metricSpecs is the registry, keyed by the CloudFormation type on
// model.Resource.
//
// Only RDS instances are here today. Aurora, DocumentDB and Neptune are
// deliberately absent: the RDS scanner records clustered engines as clusters
// (TypeRDSCluster and friends) and skips their member instances, and a cluster
// has no FreeStorageSpace series — Aurora storage grows on demand, so the
// question the metric answers does not exist for it. Keying on the instance
// type is what excludes them, so no engine check is needed here.
var metricSpecs = map[string][]Spec{
	model.TypeRDSInstance: {{
		Namespace:  "AWS/RDS",
		MetricName: "FreeStorageSpace",
		Stat:       "Minimum",
		Measure:    model.MeasureFreeStorageBytes,
		Dimensions: dimension("DBInstanceIdentifier", func(r *model.Resource) string { return r.Name }),
	}},
}

// dimension builds a single-dimension mapper, the common case.
func dimension(name string, value func(*model.Resource) string) func(*model.Resource) map[string]string {
	return func(r *model.Resource) map[string]string {
		return map[string]string{name: value(r)}
	}
}
