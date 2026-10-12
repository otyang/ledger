package ledger

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// maxAmountDigits matches the NUMERIC(78,0) amount columns: 78 digits hold any
// 256-bit value, so token amounts in their smallest unit (e.g. wei) fit.
const maxAmountDigits = 78

var (
	bigZero   = new(big.Int)
	maxAmount = new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(maxAmountDigits), nil), big.NewInt(1))
)

// Amount is an exact whole number of a currency's minor unit: cents for USD,
// wei for ETH. Stored amounts range over ±(10^78 - 1).
//
// The zero value is 0. Amounts are immutable, so arithmetic returns a new
// value. They can't be compared with ==; use Cmp or Sign.
//
// In JSON an Amount is a string of digits ("5000"), so values beyond 2^53
// survive JavaScript clients. Integer JSON numbers are accepted on input.
type Amount struct {
	_ [0]func() // makes == a compile error; it would compare pointers
	v *big.Int  // nil means 0; never modified after construction
}

func NewAmount(n int64) Amount { return Amount{v: big.NewInt(n)} }

// AmountFromBig returns x as an Amount. It copies x.
func AmountFromBig(x *big.Int) Amount { return Amount{v: new(big.Int).Set(x)} }

// ParseAmount parses a base-10 integer such as "5000" or "-12". It fails with
// ErrInvalidParams for anything else (including "1.5" and "1e3") and with
// ErrAmountOverflow beyond 78 digits.
func ParseAmount(s string) (Amount, error) {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return Amount{}, fmt.Errorf("%w: amount %q must be a whole number of minor units", ErrInvalidParams, s)
	}
	// Check the length before parsing: big.Int parsing is superlinear, and
	// request bodies may be large.
	if len(strings.TrimLeft(digits, "0")) > maxAmountDigits {
		return Amount{}, fmt.Errorf("%w: amount has more than %d digits", ErrAmountOverflow, maxAmountDigits)
	}
	v, _ := new(big.Int).SetString(s, 10)
	return Amount{v: v}, nil
}

func (a Amount) big() *big.Int {
	if a.v == nil {
		return bigZero
	}
	return a.v
}

// Big returns a copy of a as a *big.Int.
func (a Amount) Big() *big.Int { return new(big.Int).Set(a.big()) }

// Int64 returns a as an int64, and whether it fits.
func (a Amount) Int64() (int64, bool) { return a.big().Int64(), a.big().IsInt64() }

func (a Amount) String() string { return a.big().String() }

// Sign returns -1, 0 or +1.
func (a Amount) Sign() int { return a.big().Sign() }

func (a Amount) IsZero() bool { return a.Sign() == 0 }

// isSet reports whether a was given a value, even 0, rather than left as the
// zero Amount{}. Every constructor, parser and arithmetic result is set. It
// tells "no amount given" (commit or refund everything) apart from an
// explicit 0, which must not mean everything.
func (a Amount) isSet() bool { return a.v != nil }

// Cmp returns -1, 0 or +1 as a is less than, equal to or greater than b.
func (a Amount) Cmp(b Amount) int { return a.big().Cmp(b.big()) }

func (a Amount) Add(b Amount) Amount { return Amount{v: new(big.Int).Add(a.big(), b.big())} }

func (a Amount) Sub(b Amount) Amount { return Amount{v: new(big.Int).Sub(a.big(), b.big())} }

func (a Amount) Neg() Amount { return Amount{v: new(big.Int).Neg(a.big())} }

// inRange reports whether a fits the NUMERIC(78,0) columns.
func (a Amount) inRange() bool { return a.big().CmpAbs(maxAmount) <= 0 }

func (a Amount) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "%q", a.String()), nil
}

// UnmarshalJSON accepts a string of digits or an integer JSON number.
func (a *Amount) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	v, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// Scan reads a NUMERIC (sent as text) or integer column.
func (a *Amount) Scan(src any) error {
	var s string
	switch src := src.(type) {
	case []byte:
		s = string(src)
	case string:
		s = src
	case int64:
		*a = NewAmount(src)
		return nil
	default:
		return fmt.Errorf("ledger: cannot scan %T into Amount", src)
	}
	// Sums computed in SQL may exceed 78 digits, so no length limit here.
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return fmt.Errorf("ledger: cannot scan %q into Amount", s)
	}
	*a = Amount{v: v}
	return nil
}

func (a Amount) Value() (driver.Value, error) { return a.String(), nil }
