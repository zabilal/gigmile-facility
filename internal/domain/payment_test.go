package domain

import (
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// validPayload is the sample from the brief.
func validPayload() NotificationInput {
	return NotificationInput{
		CustomerID:           "GIG00001",
		PaymentStatus:        "COMPLETE",
		TransactionAmount:    "10000",
		TransactionDate:      "2025-11-07 14:54:16",
		TransactionReference: "VPAY25110713542114478761522000",
	}
}

func TestParseNotification(t *testing.T) {
	t.Parallel()

	got, err := ParseNotification(validPayload(), testNow)
	if err != nil {
		t.Fatalf("ParseNotification: %v", err)
	}

	if got.Reference != "VPAY25110713542114478761522000" {
		t.Errorf("Reference = %q", got.Reference)
	}
	if got.CustomerID != "GIG00001" {
		t.Errorf("CustomerID = %q", got.CustomerID)
	}
	if got.Amount != 1_000_000 {
		t.Errorf("Amount = %d kobo, want 1000000 (10,000 naira)", got.Amount)
	}
	if got.Status != PaymentComplete {
		t.Errorf("Status = %q", got.Status)
	}

	// The payload carries no zone; it is read as WAT, so 14:54:16 in Lagos is
	// 13:54:16 UTC. Getting this wrong shifts payments across week boundaries
	// and mislabels customers as delinquent.
	wantUTC := time.Date(2025, 11, 7, 13, 54, 16, 0, time.UTC)
	if !got.TransactionAt.UTC().Equal(wantUTC) {
		t.Errorf("TransactionAt = %s, want %s", got.TransactionAt.UTC(), wantUTC)
	}
}

func TestParseNotificationRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*NotificationInput)
		wantField string
	}{
		{"empty reference", func(p *NotificationInput) { p.TransactionReference = "" }, "transaction_reference"},
		{"blank reference", func(p *NotificationInput) { p.TransactionReference = "   " }, "transaction_reference"},
		{"oversized reference", func(p *NotificationInput) {
			p.TransactionReference = string(make([]byte, maxReferenceLen+1))
		}, "transaction_reference"},
		{"empty customer", func(p *NotificationInput) { p.CustomerID = "" }, "customer_id"},
		{"oversized customer", func(p *NotificationInput) {
			p.CustomerID = string(make([]byte, maxCustomerIDLen+1))
		}, "customer_id"},
		{"unparseable amount", func(p *NotificationInput) { p.TransactionAmount = "ten thousand" }, "transaction_amount"},
		{"zero amount", func(p *NotificationInput) { p.TransactionAmount = "0" }, "transaction_amount"},
		{"negative amount", func(p *NotificationInput) { p.TransactionAmount = "-10000" }, "transaction_amount"},
		{"sub-kobo amount", func(p *NotificationInput) { p.TransactionAmount = "10000.005" }, "transaction_amount"},
		{"ISO 8601 instead of the agreed layout", func(p *NotificationInput) {
			p.TransactionDate = "2025-11-07T14:54:16Z"
		}, "transaction_date"},
		{"empty date", func(p *NotificationInput) { p.TransactionDate = "" }, "transaction_date"},
		{"implausibly future date", func(p *NotificationInput) { p.TransactionDate = "2030-01-01 00:00:00" }, "transaction_date"},
		{"implausibly old date", func(p *NotificationInput) { p.TransactionDate = "2000-01-01 00:00:00" }, "transaction_date"},
		{"empty status", func(p *NotificationInput) { p.PaymentStatus = "" }, "payment_status"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := validPayload()
			tc.mutate(&payload)

			_, err := ParseNotification(payload, testNow)
			if err == nil {
				t.Fatalf("ParseNotification(%+v) = nil error, want rejection", payload)
			}

			var ve ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error %v is not a ValidationError; the transport layer needs the field name", err)
			}
			if ve.Field != tc.wantField {
				t.Errorf("rejected field = %q, want %q", ve.Field, tc.wantField)
			}
		})
	}
}

func TestParseNotificationNormalises(t *testing.T) {
	t.Parallel()

	payload := validPayload()
	payload.PaymentStatus = "  complete  "
	payload.CustomerID = " GIG00001 "
	payload.TransactionReference = " VPAY-1 "
	payload.TransactionAmount = " 10000 "

	got, err := ParseNotification(payload, testNow)
	if err != nil {
		t.Fatalf("ParseNotification: %v", err)
	}
	if got.Status != PaymentComplete {
		t.Errorf("Status = %q, want %q: casing from the provider must not change the outcome", got.Status, PaymentComplete)
	}
	if got.CustomerID != "GIG00001" || got.Reference != "VPAY-1" {
		t.Errorf("whitespace not trimmed: customer %q reference %q", got.CustomerID, got.Reference)
	}
}

// TestClockSkewTolerance: providers' clocks drift. A few hours ahead is drift and
// must be accepted; years ahead is a corrupt payload and must not be.
func TestClockSkewTolerance(t *testing.T) {
	t.Parallel()

	payload := validPayload()
	payload.TransactionDate = testNow.In(WAT).Add(2 * time.Hour).Format(TransactionDateLayout)

	if _, err := ParseNotification(payload, testNow); err != nil {
		t.Errorf("two hours of clock skew should be tolerated, got %v", err)
	}
}

func TestDisposition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status PaymentStatus
		want   Disposition
		why    string
	}{
		{PaymentComplete, DispositionApply, "settled funds are the only thing that moves the balance"},
		{PaymentPending, DispositionIgnore, "not settled yet; the COMPLETE will follow"},
		{PaymentFailed, DispositionIgnore, "nothing settled, nothing to apply"},
		{PaymentReversed, DispositionReview, "cannot infer which entry to compensate from one reference"},
		{PaymentStatus("SOMETHING_NEW"), DispositionReview, "never guess about money"},
		{PaymentStatus("SUCCESS"), DispositionReview, "plausible-looking but unmodelled: still a guess"},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()

			got := Notification{Status: tc.status}.Disposition()
			if got != tc.want {
				t.Errorf("Disposition(%q) = %q, want %q (%s)", tc.status, got, tc.want, tc.why)
			}
		})
	}
}
