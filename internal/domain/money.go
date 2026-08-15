package domain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Kobo is money in minor units (1 naira = 100 kobo). Never a float: binary
// floating point cannot represent 0.10, and a ledger is repeated addition.
type Kobo int64

// KobosPerNaira is the minor-unit scale of the naira.
const KobosPerNaira = 100

// MaxPaymentKobo bounds one payment at 100 million naira. Deployments are worth
// about 1 million, so anything near this is malformed, mis-scaled, or an attack.
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

// ParseNairaAmount converts a payload amount into Kobo. Deliberately strict:
// anything ambiguous means sender and receiver disagree about what was sent.
func ParseNairaAmount(s string) (Kobo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrAmountMalformed
	}

	// An amount written in exponent notation is a generator bug, not a number.
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

// String renders the amount in naira, e.g. "10000.00". Kobo stays the wire and
// storage representation; this is for humans and logs.
func (k Kobo) String() string {
	neg := ""
	v := int64(k)
	if v < 0 {
		neg, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", neg, v/KobosPerNaira, v%KobosPerNaira)
}
