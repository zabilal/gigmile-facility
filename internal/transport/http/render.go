package http

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/zabilal/gigmile-facility/internal/domain"
)

// money renders both representations on purpose: kobo is authoritative, naira
// is for humans. A formatted string alone invites clients to parse it as float.
type money struct {
	Kobo  int64  `json:"kobo"`
	Naira string `json:"naira"`
}

func amount(k domain.Kobo) money {
	return money{Kobo: int64(k), Naira: k.String()}
}

// errorCode is a stable, machine-readable classification that clients switch
// on; the message is for humans and may be reworded without notice.
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
		// The status line is already sent, so this cannot become a 500.
		slog.Error("write response body", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code errorCode, message, field string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message, Field: field}})
}
