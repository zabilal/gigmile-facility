// Package http exposes the payment API over HTTP.
package http

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zabilal/gigmile-facility/internal/platform/config"
	"github.com/zabilal/gigmile-facility/internal/store/postgres"
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

	// Signature checks wrap only the webhook, which is the endpoint that can
	// reduce debt. Reads would sit behind ordinary service auth upstream.
	mux.Handle("POST /v1/payments",
		s.instrument("POST /v1/payments",
			withSignature(s.cfg.HMACSecret, s.logger, http.HandlerFunc(s.handlePayment))))

	s.route(mux, "GET /v1/customers/{customerID}/position", s.handlePosition)
	s.route(mux, "GET /v1/customers/{customerID}/ledger", s.handleLedger)
	s.route(mux, "GET /healthz", s.handleLive)
	s.route(mux, "GET /readyz", s.handleReady)

	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Outermost first: an id must exist before anything logs, and recovery sits
	// inside the logger so a panic is still recorded with status and duration.
	return withRequestID(withLogging(s.logger, withRecovery(s.logger, mux)))
}

func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, s.instrument(pattern, h))
}

// instrument records latency against the route's registered pattern, supplied
// at registration so the label is the template and never the concrete path.
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
		// A client that connects and sends nothing must not hold a goroutine
		// and a file descriptor indefinitely.
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
}
