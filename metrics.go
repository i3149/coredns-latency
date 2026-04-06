package latency

import (
	"github.com/coredns/coredns/plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// requestCount counts DNS queries handled by the latency plugin.
	requestCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: plugin.Namespace,
		Subsystem: "latency",
		Name:      "requests_total",
		Help:      "Total DNS requests handled by the latency plugin.",
	}, []string{"server"})

	// redisLookupDuration tracks how long Redis lookups take.
	redisLookupDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: plugin.Namespace,
		Subsystem: "latency",
		Name:      "redis_lookup_duration_seconds",
		Help:      "Histogram of Redis lookup durations in seconds.",
		Buckets:   []float64{.0001, .0005, .001, .005, .01, .025, .05, .1, .25},
	}, []string{"server"})
)
