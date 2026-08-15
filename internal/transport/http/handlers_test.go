package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zabilal/gigmile-facility/internal/domain"
	"github.com/zabilal/gigmile-facility/internal/platform/config"
	"github.com/zabilal/gigmile-facility/internal/store/postgres"
)

const testSecret = "test-signing-secret"

func testServer(t *testing.T) (*httptest.Server, *postgres.Store) {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		url = "postgres://facility:facility@localhost:5433/facility?sslmode=disable"
	}

	store, err := postgres.New(context.Background(), url, 10)
	if err != nil {
		t.Skipf("transport tests need Postgres (run `make up`): %v", err)
	}
	t.Cleanup(store.Close)

	cfg := config.Config{HMACSecret: testSecret, ApplyMode: config.ApplySync, Environment: "development"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := httptest.NewServer(NewServer(store, logger, cfg).Handler())
	t.Cleanup(srv.Close)

	return srv, store
}

func newCustomer(t *testing.T) string {
	t.Helper()
	return "GIG" + uuid.NewString()[:8]
}

func deploy(t *testing.T, store *postgres.Store, customerID string) {
	t.Helper()

	_, err := store.Deploy(context.Background(), postgres.Deployment{
		CustomerID:   customerID,
		AssetValue:   80_000_000,
		TotalPayable: 100_000_000,
		TermWeeks:    50,
		StartDate:    time.Now().AddDate(0, 0, -7),
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
}

func payload(customerID, reference, amount string) string {
	return fmt.Sprintf(
		`{"customer_id":%q,"payment_status":"COMPLETE","transaction_amount":%q,"transaction_date":%q,"transaction_reference":%q}`,
		customerID, amount, time.Now().In(domain.WAT).Format(domain.TransactionDateLayout), reference)
}

func sign(t *testing.T, secret, timestamp, body string) string {
	t.Helper()

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// post sends a correctly signed request unless a mutator says otherwise.
func post(t *testing.T, srv *httptest.Server, body string, mutate ...func(*http.Request)) *http.Response {
	t.Helper()

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/payments", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerTimestamp, timestamp)
	req.Header.Set(headerSignature, sign(t, testSecret, timestamp, body))
	for _, m := range mutate {
		m(req)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()

	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

func TestPaymentEndpointAppliesAndReportsPosition(t *testing.T) {
	t.Parallel()

	srv, store := testServer(t)
	customer := newCustomer(t)
	deploy(t, store, customer)

	resp := post(t, srv, payload(customer, "REF-"+uuid.NewString(), "20000"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := decode[paymentResponse](t, resp)
	if got.Outcome != string(postgres.OutcomeApplied) {
		t.Errorf("outcome = %q, want applied", got.Outcome)
	}
	if got.AppliedAmount.Kobo != 2_000_000 {
		t.Errorf("applied = %d kobo, want 2000000", got.AppliedAmount.Kobo)
	}
	if got.AppliedAmount.Naira != "20000.00" {
		t.Errorf("applied naira = %q, want \"20000.00\"", got.AppliedAmount.Naira)
	}
	// The position rides along so the caller needs no second round trip.
	if got.Position == nil {
		t.Fatal("position missing from response")
	}
	if got.Position.Outstanding.Kobo != 98_000_000 {
		t.Errorf("outstanding = %d kobo, want 98000000", got.Position.Outstanding.Kobo)
	}
}

// TestSignatureIsRequired covers the control that makes this endpoint safe to
// expose: without it, anyone who can reach the service can clear a debt.
func TestSignatureIsRequired(t *testing.T) {
	t.Parallel()

	srv, store := testServer(t)
	customer := newCustomer(t)
	deploy(t, store, customer)
	body := payload(customer, "REF-"+uuid.NewString(), "20000")

	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"no signature at all", func(r *http.Request) { r.Header.Del(headerSignature) }},
		{"no timestamp", func(r *http.Request) { r.Header.Del(headerTimestamp) }},
		{"signature is not hex", func(r *http.Request) { r.Header.Set(headerSignature, "not-hex!!") }},
		{"wrong signing key", func(r *http.Request) {
			ts := r.Header.Get(headerTimestamp)
			r.Header.Set(headerSignature, sign(t, "attacker-guess", ts, body))
		}},
		{
			// Signed correctly but for a different body: raising the amount
			// after signing, the classic tampering attempt.
			name: "body tampered after signing",
			mutate: func(r *http.Request) {
				tampered := payload(customer, "REF-"+uuid.NewString(), "999999")
				r.Body = io.NopCloser(strings.NewReader(tampered))
				r.ContentLength = int64(len(tampered))
			},
		},
		{
			// A valid signature captured earlier must not stay valid forever.
			name: "timestamp outside the replay window",
			mutate: func(r *http.Request) {
				stale := strconv.FormatInt(time.Now().Add(-2*signatureWindow).Unix(), 10)
				r.Header.Set(headerTimestamp, stale)
				r.Header.Set(headerSignature, sign(t, testSecret, stale, body))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := post(t, srv, body, tc.mutate)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}

			got := decode[errorResponse](t, resp)
			if got.Error.Code != codeUnauthorised {
				t.Errorf("code = %q, want %q", got.Error.Code, codeUnauthorised)
			}
			// The response must not reveal which half of the check failed.
			if strings.Contains(strings.ToLower(got.Error.Message), "timestamp") {
				t.Errorf("message %q tells a prober why it failed", got.Error.Message)
			}
		})
	}
}

func TestPaymentEndpointRejectsBadPayloads(t *testing.T) {
	t.Parallel()

	srv, store := testServer(t)
	customer := newCustomer(t)
	deploy(t, store, customer)

	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"not JSON", `{"customer_id":`, ""},
		{"amount is not a number", payload(customer, "R1-"+uuid.NewString(), "many"), "transaction_amount"},
		{"amount is zero", payload(customer, "R2-"+uuid.NewString(), "0"), "transaction_amount"},
		{"amount is negative", payload(customer, "R3-"+uuid.NewString(), "-5000"), "transaction_amount"},
		{"amount has sub-kobo precision", payload(customer, "R4-"+uuid.NewString(), "100.005"), "transaction_amount"},
		{"reference missing", `{"customer_id":"X","payment_status":"COMPLETE","transaction_amount":"100","transaction_date":"2026-01-01 00:00:00","transaction_reference":""}`, "transaction_reference"},
		{"customer missing", `{"customer_id":"","payment_status":"COMPLETE","transaction_amount":"100","transaction_date":"2026-01-01 00:00:00","transaction_reference":"R5"}`, "customer_id"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := post(t, srv, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}

			got := decode[errorResponse](t, resp)
			if got.Error.Code != codeInvalidPayload {
				t.Errorf("code = %q, want %q", got.Error.Code, codeInvalidPayload)
			}
			if got.Error.Field != tc.wantField {
				t.Errorf("field = %q, want %q", got.Error.Field, tc.wantField)
			}
		})
	}
}

func TestPaymentEndpointIdempotency(t *testing.T) {
	t.Parallel()

	srv, store := testServer(t)
	customer := newCustomer(t)
	deploy(t, store, customer)

	body := payload(customer, "REF-"+uuid.NewString(), "20000")

	first := decode[paymentResponse](t, post(t, srv, body))
	if first.Outcome != string(postgres.OutcomeApplied) {
		t.Fatalf("first outcome = %q, want applied", first.Outcome)
	}

	// A retry must return 200 with the original result: a 409 makes
	// well-behaved providers keep retrying forever.
	resp := post(t, srv, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", resp.StatusCode)
	}

	repeat := decode[paymentResponse](t, resp)
	if repeat.Outcome != string(postgres.OutcomeDuplicate) {
		t.Errorf("retry outcome = %q, want duplicate", repeat.Outcome)
	}
	if repeat.PaymentID != first.PaymentID {
		t.Errorf("retry payment_id = %d, want %d", repeat.PaymentID, first.PaymentID)
	}
	if repeat.Position.TotalPaid.Kobo != first.Position.TotalPaid.Kobo {
		t.Error("retry reports a moved balance")
	}
}

func TestPaymentEndpointAmountMismatchConflicts(t *testing.T) {
	t.Parallel()

	srv, store := testServer(t)
	customer := newCustomer(t)
	deploy(t, store, customer)

	reference := "REF-" + uuid.NewString()
	post(t, srv, payload(customer, reference, "20000"))

	resp := post(t, srv, payload(customer, reference, "500000"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := decode[errorResponse](t, resp); got.Error.Code != codeAmountMismatch {
		t.Errorf("code = %q, want %q", got.Error.Code, codeAmountMismatch)
	}
}

// TestUnmatchedPaymentIsAcceptedNotRejected: the money arrived. Refusing it
// because we cannot map it puts a real credit at risk of being dropped.
func TestUnmatchedPaymentIsAcceptedNotRejected(t *testing.T) {
	t.Parallel()

	srv, _ := testServer(t)

	resp := post(t, srv, payload(newCustomer(t), "REF-"+uuid.NewString(), "20000"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	got := decode[paymentResponse](t, resp)
	if got.Outcome != string(postgres.OutcomeSuspense) {
		t.Errorf("outcome = %q, want suspense", got.Outcome)
	}
	if got.PaymentID == 0 {
		t.Error("payment was not persisted; an unmatched credit must still be recorded")
	}
	if got.Reason == "" {
		t.Error("reason is empty; ops cannot triage without one")
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	t.Parallel()

	srv, _ := testServer(t)

	resp := post(t, srv, `{"padding":"`+strings.Repeat("x", maxBodyBytes+1)+`"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestPositionAndLedgerEndpoints(t *testing.T) {
	t.Parallel()

	srv, store := testServer(t)
	customer := newCustomer(t)
	deploy(t, store, customer)
	post(t, srv, payload(customer, "REF-"+uuid.NewString(), "20000"))

	resp, err := srv.Client().Get(srv.URL + "/v1/customers/" + customer + "/position")
	if err != nil {
		t.Fatalf("get position: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("position status = %d, want 200", resp.StatusCode)
	}
	position := decode[positionResponse](t, resp)
	if position.TotalPaid.Kobo != 2_000_000 {
		t.Errorf("total_paid = %d kobo, want 2000000", position.TotalPaid.Kobo)
	}
	if position.NextDueWeek != 2 {
		t.Errorf("next_due_week = %d, want 2", position.NextDueWeek)
	}

	ledgerResp, err := srv.Client().Get(srv.URL + "/v1/customers/" + customer + "/ledger")
	if err != nil {
		t.Fatalf("get ledger: %v", err)
	}
	defer ledgerResp.Body.Close()

	statement := decode[ledgerResponse](t, ledgerResp)
	if len(statement.Entries) != 1 {
		t.Fatalf("statement has %d entries, want 1", len(statement.Entries))
	}
	if statement.Entries[0].BalanceAfter.Kobo != 98_000_000 {
		t.Errorf("balance_after = %d kobo, want 98000000", statement.Entries[0].BalanceAfter.Kobo)
	}
}

func TestUnknownCustomerReadsAre404(t *testing.T) {
	t.Parallel()

	srv, _ := testServer(t)

	for _, path := range []string{"/position", "/ledger"} {
		resp, err := srv.Client().Get(srv.URL + "/v1/customers/" + newCustomer(t) + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	srv, _ := testServer(t)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}
