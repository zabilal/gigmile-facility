package http

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gigmile/facility/internal/domain"
)

// money renders an amount in both representations on purpose.
//
// Kobo is authoritative and is what a client should compute with; naira is for
// humans reading a response. Emitting only a formatted string invites clients to
// parse it back into a float, which is how money becomes wrong.
type money struct {
	Kobo  int64  `json:"kobo"`
	Naira string `json:"naira"`
}

func amount(k domain.Kobo) money {
	return money{Kobo: int64(k), Naira: k.String()}
}

// errorCode is a stable, machine-readable classification. Clients switch on
// this; the message is for humans and may be reworded without notice.
type errorCode string

const (
	codeInvalidPayload  errorCode = "invalid_payload"
	codeUnauthorised    errorCode = "unauthorised"
	codeAmountMismatch  errorCode = "amount_mismatch"
	codeNotFound        errorCode = "not_found"
	codeInternal        errorCode = "internal_error"
	codeUnavailable     errorCode = "unavailable"
	codePayloadTooLarge errorCode = "payload_too_large"
)

type errorBody struct {
	Code    errorCode `json:"code"`
	Message string    `json:"message"`
	// Field names the offending payload field, when one is to blame.
	Field string `json:"field,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this cannot be turned into a 500.
		// Logging it is all that remains.
		slog.Error("write response body", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code errorCode, message, field string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message, Field: field}})
}
