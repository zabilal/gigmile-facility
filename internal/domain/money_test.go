package domain

import (
	"errors"
	"testing"
)

func TestParseNairaAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want Kobo
		err  error
	}{
		{name: "whole naira, as the sample payload sends it", in: "10000", want: 1_000_000},
		{name: "kobo precision", in: "10000.50", want: 1_000_050},
		{name: "one kobo", in: "0.01", want: 1},
		{name: "single decimal place", in: "0.5", want: 50},
		{name: "surrounding whitespace is tolerated", in: "  10000  ", want: 1_000_000},
		{name: "a full deployment", in: "1000000", want: 100_000_000},
		{name: "at the ceiling", in: "100000000", want: MaxPaymentKobo},

		{name: "empty", in: "", err: ErrAmountMalformed},
		{name: "not a number", in: "abc", err: ErrAmountMalformed},
		{name: "thousands separators are ambiguous, not helpful", in: "10,000", err: ErrAmountMalformed},
		{name: "currency symbol", in: "N10000", err: ErrAmountMalformed},
		// Exponent notation would parse, but a payment amount written that way
		// means the sender generated it wrong; interpreting it hides the bug.
		{name: "exponent notation", in: "1e5", err: ErrAmountMalformed},
		{name: "zero moves no money", in: "0", err: ErrAmountNotPositive},
		{name: "zero with decimals", in: "0.00", err: ErrAmountNotPositive},
		// A negative credit notification is a reversal, which travels a different
		// path. Accepting it here would let a malformed payload increase a debt.
		{name: "negative", in: "-5000", err: ErrAmountNotPositive},
		{name: "sub-kobo precision", in: "10.001", err: ErrAmountPrecision},
		{name: "above the ceiling", in: "100000001", err: ErrAmountTooLarge},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseNairaAmount(tc.in)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("ParseNairaAmount(%q) error = %v, want %v", tc.in, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNairaAmount(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseNairaAmount(%q) = %d kobo, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestKoboString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   Kobo
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{50, "0.50"},
		{1_000_000, "10000.00"},
		{1_000_050, "10000.50"},
		{100_000_000, "1000000.00"},
		{-1_000_050, "-10000.50"},
	}

	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Kobo(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// FuzzParseNairaAmount asserts the parser is total: for any input it either
// rejects cleanly or returns an amount inside the permitted range. It must never
// panic and never yield a non-positive or oversized amount, because everything
// downstream -- the CHECK constraints, the overflow-free running totals -- is
// built on that guarantee holding at the boundary.
func FuzzParseNairaAmount(f *testing.F) {
	for _, seed := range []string{"10000", "0.01", "1000000.00", "", "-1", "abc", "1e5", "10.001", "999999999999"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := ParseNairaAmount(in)
		if err != nil {
			if got != 0 {
				t.Fatalf("ParseNairaAmount(%q) returned %d alongside error %v; want 0", in, got, err)
			}
			return
		}
		if got <= 0 {
			t.Fatalf("ParseNairaAmount(%q) = %d; a successful parse must be positive", in, got)
		}
		if got > MaxPaymentKobo {
			t.Fatalf("ParseNairaAmount(%q) = %d; exceeds MaxPaymentKobo %d", in, got, MaxPaymentKobo)
		}
	})
}
