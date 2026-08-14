package domain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Kobo is an amount of money in minor units (1 naira = 100 kobo).
//
// Money is never a float in this codebase. Binary floating point cannot
// represent 0.10 exactly, so repeated addition drifts -- and a repayment ledger
// is nothing but repeated addition. Making the unit a distinct type also means
// passing naira where kobo is expected is a compile error rather than a
// hundredfold error in production.
type Kobo int64

// KobosPerNaira is the minor-unit scale of the naira.
const KobosPerNaira = 100

// MaxPaymentKobo bounds a single inbound payment at 100 million naira.
//
// Deployments are valued around 1 million naira, so anything at this magnitude
// is a malformed payload, a unit confusion at the provider, or an attack. It is
// also well inside int64, which keeps the running-total arithmetic safe from
// overflow no matter how many payments an account accumulates.
const MaxPaymentKobo Kobo = 100_000_000 * KobosPerNaira

var (
	// ErrAmountMalformed means the amount string was not a plain decimal number.
	ErrAmountMalformed = errors.New("amount is not a valid decimal number")
	// ErrAmountNotPositive means the amount was zero or negative.
	ErrAmountNotPositive = errors.New("amount must be greater than zero")
	// ErrAmountPrecision means the amount carried sub-kobo precision.
	ErrAmountPrecision = errors.New("amount has more than two decimal places")
	// ErrAmountTooLarge means the amount exceeded MaxPaymentKobo.
	ErrAmountTooLarge = errors.New("amount exceeds the maximum permitted payment")
)

// ParseNairaAmount converts an amount string from a payment payload into Kobo.
//
// The payload delivers the amount as a string ("10000"), which is the right
// choice by the provider -- it avoids the JSON-number-to-float64 round trip that
// silently mangles money. This function is the only place a string becomes an
// amount, and it is deliberately strict: exponent notation, thousands
// separators, currency symbols, and sub-kobo precision are all rejected rather
// than coerced, because every one of them means the sender and receiver
// disagree about what was sent.
func ParseNairaAmount(s string) (Kobo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrAmountMalformed
	}

	// decimal.NewFromString accepts exponent notation ("1e5"); a payment amount
	// written that way is a generator bug, not a number we should interpret.
	if strings.ContainsAny(s, "eE") {
		return 0, ErrAmountMalformed
	}

	d, err := decimal.NewFromString(s)
	if err != nil {
		return 0, ErrAmountMalformed
	}
	if d.Exponent() < -2 {
		return 0, ErrAmountPrecision
	}
	if !d.IsPositive() {
		return 0, ErrAmountNotPositive
	}

	kobo := d.Mul(decimal.NewFromInt(KobosPerNaira))
	if !kobo.IsInteger() {
		return 0, ErrAmountPrecision
	}
	if kobo.GreaterThan(decimal.NewFromInt(int64(MaxPaymentKobo))) {
		return 0, ErrAmountTooLarge
	}

	return Kobo(kobo.IntPart()), nil
}

// String renders the amount in naira with two decimal places, e.g. "10000.00".
// Kobo remains the wire and storage representation; this is for humans and logs.
func (k Kobo) String() string {
	neg := ""
	v := int64(k)
	if v < 0 {
		neg, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", neg, v/KobosPerNaira, v%KobosPerNaira)
}
