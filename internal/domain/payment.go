package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// PaymentStatus is the provider's view of the transfer.
type PaymentStatus string

const (
	// PaymentComplete is a settled transfer. Only this moves money.
	PaymentComplete PaymentStatus = "COMPLETE"
	// PaymentPending is in flight and may still fail.
	PaymentPending PaymentStatus = "PENDING"
	// PaymentFailed did not settle.
	PaymentFailed PaymentStatus = "FAILED"
	// PaymentReversed unwinds a previously settled transfer.
	PaymentReversed PaymentStatus = "REVERSED"
)

// Disposition is what the service should do with a notification.
type Disposition string

const (
	// DispositionApply moves money against the customer's obligation.
	DispositionApply Disposition = "APPLY"
	// DispositionIgnore records the notification without touching the ledger.
	DispositionIgnore Disposition = "IGNORE"
	// DispositionReview records it and routes to ops, for anything where
	// applying or discarding would both be a guess.
	DispositionReview Disposition = "REVIEW"
)

// WAT is West Africa Time. A fixed offset rather than a tzdata lookup: Nigeria
// has no daylight saving, and this stays correct in a scratch container.
var WAT = time.FixedZone("WAT", 1*60*60)

// TransactionDateLayout is the payload's timestamp format. It carries no zone,
// so it is read as WAT.
const TransactionDateLayout = "2006-01-02 15:04:05"

const (
	maxReferenceLen  = 128
	maxCustomerIDLen = 64
	// futureSkewTolerance accepts provider clock drift while still rejecting a
	// timestamp that is obviously wrong.
	futureSkewTolerance = 24 * time.Hour
	// maxBackdating rejects timestamps too old to be a live notification.
	maxBackdating = 10 * 365 * 24 * time.Hour
)

// ErrUnknownStatus means the provider sent a payment_status we do not model.
var ErrUnknownStatus = errors.New("unrecognised payment_status")

// ValidationError names the rejected field, so the transport layer can return
// something more useful than "bad request".
type ValidationError struct {
	Field string
	Err   error
}

func (e ValidationError) Error() string { return fmt.Sprintf("%s: %v", e.Field, e.Err) }
func (e ValidationError) Unwrap() error { return e.Err }

// NotificationInput is the payload as it arrives: every field a string, nothing
// coerced. Parsing lives here, not in the handler, so it is testable standalone.
type NotificationInput struct {
	CustomerID           string `json:"customer_id"`
	PaymentStatus        string `json:"payment_status"`
	TransactionAmount    string `json:"transaction_amount"`
	TransactionDate      string `json:"transaction_date"`
	TransactionReference string `json:"transaction_reference"`
}

// Notification is a validated payment notification.
type Notification struct {
	Reference     string
	CustomerID    string
	Amount        Kobo
	Status        PaymentStatus
	TransactionAt time.Time
}

// ParseNotification validates and converts an inbound payload. now is passed in
// so the timestamp sanity window does not depend on the wall clock.
func ParseNotification(in NotificationInput, now time.Time) (Notification, error) {
	ref := strings.TrimSpace(in.TransactionReference)
	switch {
	case ref == "":
		return Notification{}, ValidationError{"transaction_reference", errors.New("must not be empty")}
	case len(ref) > maxReferenceLen:
		return Notification{}, ValidationError{"transaction_reference", fmt.Errorf("must be at most %d characters", maxReferenceLen)}
	}

	customerID := strings.TrimSpace(in.CustomerID)
	switch {
	case customerID == "":
		return Notification{}, ValidationError{"customer_id", errors.New("must not be empty")}
	case len(customerID) > maxCustomerIDLen:
		return Notification{}, ValidationError{"customer_id", fmt.Errorf("must be at most %d characters", maxCustomerIDLen)}
	}

	amount, err := ParseNairaAmount(in.TransactionAmount)
	if err != nil {
		return Notification{}, ValidationError{"transaction_amount", err}
	}

	// The payload carries no timezone. Reading it as WAT is an assumption, and a
	// wrong guess shifts a payment across a week boundary into false arrears.
	txAt, err := time.ParseInLocation(TransactionDateLayout, strings.TrimSpace(in.TransactionDate), WAT)
	if err != nil {
		return Notification{}, ValidationError{"transaction_date", fmt.Errorf("must match %q", TransactionDateLayout)}
	}
	if txAt.After(now.Add(futureSkewTolerance)) {
		return Notification{}, ValidationError{"transaction_date", errors.New("is implausibly far in the future")}
	}
	if txAt.Before(now.Add(-maxBackdating)) {
		return Notification{}, ValidationError{"transaction_date", errors.New("is implausibly far in the past")}
	}

	status := PaymentStatus(strings.ToUpper(strings.TrimSpace(in.PaymentStatus)))
	if status == "" {
		return Notification{}, ValidationError{"payment_status", errors.New("must not be empty")}
	}

	return Notification{
		Reference:     ref,
		CustomerID:    customerID,
		Amount:        amount,
		Status:        status,
		TransactionAt: txAt,
	}, nil
}

// Disposition decides what happens to the notification. An unknown status is
// reviewed, never ignored: a queue costs less than a vanished repayment.
func (n Notification) Disposition() Disposition {
	switch n.Status {
	case PaymentComplete:
		return DispositionApply
	case PaymentPending, PaymentFailed:
		// Nothing settled. The matching COMPLETE arrives under the same
		// reference and is applied then.
		return DispositionIgnore
	case PaymentReversed:
		// The payload carries one reference and no link to the original entry,
		// and guessing which to compensate is how a ledger loses integrity.
		return DispositionReview
	default:
		return DispositionReview
	}
}
