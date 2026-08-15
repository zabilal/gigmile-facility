package http

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zabilal/gigmile-facility/internal/store/postgres"
)

// Labelled by route pattern, never the raw path: a customer id in a label value
// means one time series per customer, which takes the metrics backend down.
var (
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "facility_http_request_duration_seconds",
			Help: "Request latency by route and status.",
			// Tight at the low end: the question is whether a payment commits
			// in single-digit milliseconds, not whether it takes 5 or 10s.
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

	// A sustained non-zero rate is a provider defect or someone probing whether
	// a replayed reference clears a debt. It should alert, not sit on a board.
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
