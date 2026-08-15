package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/google/uuid"
)

type contextKey int

const (
	ctxRequestID contextKey = iota
	ctxRawBody
)

const (
	// maxBodyBytes caps an inbound payload. The real one is a few hundred bytes;
	// anything larger is a mistake or an attempt to exhaust memory.
	maxBodyBytes = 16 << 10
	// signatureWindow bounds how long a signed request stays valid: a signature
	// proves authenticity, not freshness.
	signatureWindow = 5 * time.Minute

	headerSignature = "X-Signature"
	headerTimestamp = "X-Timestamp"
	headerRequestID = "X-Request-ID"
)

// requestIDFrom returns the correlation id assigned to this request.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// rawBodyFrom returns the exact bytes received, which the store persists
// verbatim so an incident stays replayable after the parser has changed.
func rawBodyFrom(ctx context.Context) []byte {
	body, _ := ctx.Value(ctxRawBody).([]byte)
	return body
}

// withRequestID assigns or adopts a correlation id.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// statusRecorder captures the status code for logging and metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		// At 100k/minute, logging every success costs more than the work and
		// buries what matters. Failures are logged; throughput is a metric.
		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}

		logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", elapsed.Milliseconds(),
			"request_id", requestIDFrom(r.Context()),
		)
	})
}

func withRecovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic recovered",
					"panic", v,
					"path", r.URL.Path,
					"request_id", requestIDFrom(r.Context()),
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, codeInternal, "internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withSignature authenticates the webhook: hex(HMAC-SHA256(secret, ts+"."+body)).
// The timestamp is signed too, so a captured request cannot be replayed.
func withSignature(secret string, logger *slog.Logger, next http.Handler) http.Handler {
	key := []byte(secret)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
					fmt.Sprintf("payload exceeds %d bytes", maxBodyBytes), "")
				return
			}
			writeError(w, http.StatusBadRequest, codeInvalidPayload, "could not read request body", "")
			return
		}

		reject := func(reason string) {
			// Vague to the caller, specific in the log: telling a prober which
			// half of the check failed helps only the prober.
			logger.Warn("signature rejected", "reason", reason, "request_id", requestIDFrom(r.Context()))
			writeError(w, http.StatusUnauthorized, codeUnauthorised, "invalid or missing signature", "")
		}

		tsHeader := r.Header.Get(headerTimestamp)
		seconds, err := strconv.ParseInt(tsHeader, 10, 64)
		if err != nil {
			reject("timestamp missing or unparseable")
			return
		}
		if drift := time.Since(time.Unix(seconds, 0)); drift > signatureWindow || drift < -signatureWindow {
			reject("timestamp outside the replay window")
			return
		}

		provided, err := hex.DecodeString(r.Header.Get(headerSignature))
		if err != nil {
			reject("signature not hex")
			return
		}

		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(tsHeader))
		mac.Write([]byte("."))
		mac.Write(body)

		// hmac.Equal, not bytes.Equal: a short-circuiting compare leaks how much
		// of a guessed signature was right, one byte at a time.
		if !hmac.Equal(provided, mac.Sum(nil)) {
			reject("signature mismatch")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRawBody, body)))
	})
}
