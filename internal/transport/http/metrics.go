package http

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gigmile/facility/internal/store/postgres"
)

// Metrics are labelled by the matched route pattern rather than the raw path.
// Using the path would put every customer id into a label value and produce one
// time series per customer, which is how a metrics backend gets taken down by
// the service it monitors.
var (
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "facility_http_request_duration_seconds",
			Help: "Request latency by route and status.",
			// Buckets are tight at the low end because the interesting question
			// is whether a payment commits in single-digit milliseconds, not
			// whether it takes 5 or 10 seconds.
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		[]string{"route", "status"},
	)

	paymentOutcomes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "facility_payment_outcomes_total",
			Help: "Payment notifications by outcome.",
		},
		[]string{"outcome"},
	)

	// A sustained non-zero rate here means a provider defect or someone probing
	// whether replaying a reference with a larger amount clears a debt. It should
	// alert, not sit in a dashboard.
	referenceAnomalies = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "facility_reference_anomalies_total",
			Help: "References replayed with a different amount.",
		},
	)
)

// registerMetrics builds a registry containing the service's collectors.
func registerMetrics() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(requestDuration, paymentOutcomes, referenceAnomalies)
	return registry
}

func observeRequest(route string, status int, elapsed time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	requestDuration.WithLabelValues(route, strconv.Itoa(status)).Observe(elapsed.Seconds())
}

func observeOutcome(outcome postgres.Outcome) {
	paymentOutcomes.WithLabelValues(string(outcome)).Inc()
}
