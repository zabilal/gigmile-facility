// Package http exposes the payment API over HTTP.
package http

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gigmile/facility/internal/platform/config"
	"github.com/gigmile/facility/internal/store/postgres"
)

// Server wires the store and configuration into HTTP handlers.
type Server struct {
	store  *postgres.Store
	logger *slog.Logger
	cfg    config.Config
}

// NewServer constructs the API.
func NewServer(store *postgres.Store, logger *slog.Logger, cfg config.Config) *Server {
	return &Server{store: store, logger: logger, cfg: cfg}
}

// Handler builds the routed, instrumented handler chain.
func (s *Server) Handler() http.Handler {
	registry := registerMetrics()
	mux := http.NewServeMux()

	// The signature check wraps only the webhook. It is what makes an endpoint
	// that reduces debt safe to expose; the read endpoints carry no such power
	// and would be gated by ordinary service auth in front of this process.
	mux.Handle("POST /v1/payments",
		s.instrument("POST /v1/payments",
			withSignature(s.cfg.HMACSecret, s.logger, http.HandlerFunc(s.handlePayment))))

	s.route(mux, "GET /v1/customers/{customerID}/position", s.handlePosition)
	s.route(mux, "GET /v1/customers/{customerID}/ledger", s.handleLedger)
	s.route(mux, "GET /healthz", s.handleLive)
	s.route(mux, "GET /readyz", s.handleReady)

	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Outermost first: a correlation id must exist before anything logs, and
	// recovery must sit inside the logger so a panic is still recorded with its
	// status and duration rather than vanishing.
	return withRequestID(withLogging(s.logger, withRecovery(s.logger, mux)))
}

func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, s.instrument(pattern, h))
}

// instrument records latency against the route's registered pattern.
//
// The pattern is supplied at registration rather than read from the request,
// because the label must be the template ("/v1/customers/{customerID}/position")
// and never the concrete path -- one time series per customer would take down
// the metrics backend long before the service noticed.
func (s *Server) instrument(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			w = rec
		}

		next.ServeHTTP(w, r)
		observeRequest(pattern, rec.status, time.Since(start))
	})
}

// NewHTTPServer wraps the handler with sane transport-level limits.
func NewHTTPServer(handler http.Handler, cfg config.Config) *http.Server {
	return &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
		// A client that opens a connection and sends nothing must not be able to
		// hold a goroutine and a file descriptor indefinitely.
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
}
