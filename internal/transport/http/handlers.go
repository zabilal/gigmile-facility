package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/zabilal/gigmile-facility/internal/domain"
	"github.com/zabilal/gigmile-facility/internal/store/postgres"
)

const dateLayout = "2006-01-02"

type paymentResponse struct {
	Outcome              string            `json:"outcome"`
	TransactionReference string            `json:"transaction_reference"`
	PaymentID            int64             `json:"payment_id"`
	Reason               string            `json:"reason,omitempty"`
	AppliedAmount        money             `json:"applied_amount"`
	ExcessAmount         money             `json:"excess_amount"`
	Position             *positionResponse `json:"position,omitempty"`
}

type positionResponse struct {
	CustomerID string `json:"customer_id"`
	AccountID  string `json:"account_id"`
	Status     string `json:"status"`

	TotalPayable money `json:"total_payable"`
	TotalPaid    money `json:"total_paid"`
	Outstanding  money `json:"outstanding"`
	Overpayment  money `json:"overpayment"`
	WeeklyDue    money `json:"weekly_due"`

	TermWeeks           int    `json:"term_weeks"`
	StartDate           string `json:"start_date"`
	ScheduledCompletion string `json:"scheduled_completion"`

	WeeksElapsed    int   `json:"weeks_elapsed"`
	InstalmentsMet  int   `json:"instalments_met"`
	NextDueWeek     int   `json:"next_due_week"`
	PartPaidCurrent money `json:"part_paid_current"`

	// Repayment health: the balance alone cannot tell someone four weeks ahead
	// from four weeks behind, and only the second needs collections.
	ExpectedToDate money `json:"expected_to_date"`
	Arrears        money `json:"arrears"`
	AheadBy        money `json:"ahead_by"`
	WeeksBehind    int   `json:"weeks_behind"`
}

func renderPosition(p domain.Position) *positionResponse {
	return &positionResponse{
		CustomerID:          p.CustomerID,
		AccountID:           p.AccountID.String(),
		Status:              string(p.Status),
		TotalPayable:        amount(p.TotalPayable),
		TotalPaid:           amount(p.TotalPaid),
		Outstanding:         amount(p.Outstanding),
		Overpayment:         amount(p.Overpayment),
		WeeklyDue:           amount(p.WeeklyDue),
		TermWeeks:           p.TermWeeks,
		StartDate:           p.StartDate.Format(dateLayout),
		ScheduledCompletion: p.ScheduledCompletion.Format(dateLayout),
		WeeksElapsed:        p.WeeksElapsed,
		InstalmentsMet:      p.InstalmentsMet,
		NextDueWeek:         p.NextDueWeek,
		PartPaidCurrent:     amount(p.PartPaidCurrent),
		ExpectedToDate:      amount(p.ExpectedToDate),
		Arrears:             amount(p.Arrears),
		AheadBy:             amount(p.AheadBy),
		WeeksBehind:         p.WeeksBehind,
	}
}

// handlePayment is the webhook the bank calls on every successful transfer.
func (s *Server) handlePayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw := rawBodyFrom(ctx)

	var input domain.NotificationInput
	if err := json.Unmarshal(raw, &input); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidPayload, "body is not valid JSON", "")
		return
	}

	now := time.Now()
	notification, err := domain.ParseNotification(input, now)
	if err != nil {
		var invalid domain.ValidationError
		if errors.As(err, &invalid) {
			writeError(w, http.StatusBadRequest, codeInvalidPayload, invalid.Err.Error(), invalid.Field)
			return
		}
		writeError(w, http.StatusBadRequest, codeInvalidPayload, err.Error(), "")
		return
	}

	result, err := s.store.ApplyPayment(ctx, notification, raw, now)
	if err != nil {
		if errors.Is(err, postgres.ErrAmountMismatch) {
			referenceAnomalies.Inc()
			// Loud, because this is either a provider defect or an attempt to
			// clear a debt by replaying a reference with a bigger number.
			s.logger.Error("reference replayed with a different amount",
				"reference", notification.Reference,
				"customer_id", notification.CustomerID,
				"received_amount_kobo", int64(notification.Amount),
				"request_id", requestIDFrom(ctx))
			writeError(w, http.StatusConflict, codeAmountMismatch,
				"this transaction_reference is already recorded with a different amount", "transaction_amount")
			return
		}

		// We did not durably record it, so the provider must retry. 503 asks for
		// that; a 500 invites some providers to give up on a real credit.
		s.logger.Error("apply payment",
			"error", err,
			"reference", notification.Reference,
			"request_id", requestIDFrom(ctx))
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeUnavailable,
			"could not record the payment; please retry", "")
		return
	}

	observeOutcome(result.Outcome)

	if result.Outcome == postgres.OutcomeSuspense {
		s.logger.Warn("payment routed to suspense",
			"reference", result.Reference,
			"customer_id", notification.CustomerID,
			"reason", result.Reason,
			"request_id", requestIDFrom(ctx))
	}

	response := paymentResponse{
		Outcome:              string(result.Outcome),
		TransactionReference: result.Reference,
		PaymentID:            result.PaymentID,
		Reason:               result.Reason,
		AppliedAmount:        amount(result.Applied),
		ExcessAmount:         amount(result.Excess),
	}
	if result.Position != nil {
		response.Position = renderPosition(*result.Position)
	}

	// 202 for suspense: recorded and acknowledged, deliberately not acted upon.
	// Either way a 2xx tells the provider to stop retrying.
	status := http.StatusOK
	if result.Outcome == postgres.OutcomeSuspense {
		status = http.StatusAccepted
	}
	writeJSON(w, status, response)
}

func (s *Server) handlePosition(w http.ResponseWriter, r *http.Request) {
	customerID := r.PathValue("customerID")

	position, err := s.store.Position(r.Context(), customerID, time.Now())
	if errors.Is(err, postgres.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "no deployment found for this customer", "")
		return
	}
	if err != nil {
		s.logger.Error("load position", "error", err, "customer_id", customerID)
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "could not load position", "")
		return
	}

	writeJSON(w, http.StatusOK, renderPosition(position))
}

type ledgerEntryResponse struct {
	ID                   int64  `json:"id"`
	EntryType            string `json:"entry_type"`
	TransactionReference string `json:"transaction_reference,omitempty"`
	Amount               money  `json:"amount"`
	BalanceAfter         money  `json:"balance_after"`
	PostedAt             string `json:"posted_at"`
}

type ledgerResponse struct {
	Entries []ledgerEntryResponse `json:"entries"`
	// NextCursor is the `before` value for the next page, absent on the last.
	// Keyset paging, so depth costs nothing.
	NextCursor *int64 `json:"next_cursor,omitempty"`
}

const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 200
)

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	customerID := r.PathValue("customerID")

	limit := defaultLedgerLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, codeInvalidPayload, "limit must be a positive integer", "limit")
			return
		}
		limit = min(parsed, maxLedgerLimit)
	}

	var before *int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidPayload, "before must be an integer cursor", "before")
			return
		}
		before = &parsed
	}

	entries, err := s.store.Ledger(r.Context(), customerID, before, limit)
	if errors.Is(err, postgres.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "no deployment found for this customer", "")
		return
	}
	if err != nil {
		s.logger.Error("load ledger", "error", err, "customer_id", customerID)
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "could not load ledger", "")
		return
	}

	response := ledgerResponse{Entries: make([]ledgerEntryResponse, 0, len(entries))}
	for _, e := range entries {
		response.Entries = append(response.Entries, ledgerEntryResponse{
			ID:                   e.ID,
			EntryType:            e.EntryType,
			TransactionReference: e.Reference,
			Amount:               amount(e.Amount),
			BalanceAfter:         amount(e.BalanceAfter),
			PostedAt:             e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	// A full page implies there may be more; a short page is definitively the end.
	if len(entries) == limit {
		last := entries[len(entries)-1].ID
		response.NextCursor = &last
	}

	writeJSON(w, http.StatusOK, response)
}

// handleLive answers whether the process is running, deliberately without
// touching the database: an outage should drain replicas, not restart them all.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady answers whether the instance can serve traffic right now.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		s.logger.Warn("readiness probe failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
