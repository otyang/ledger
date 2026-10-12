package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// maxDecimals bounds Currency.Decimals. ETH has 18 and NEAR 24.
const maxDecimals = 36

// Currency says how to read amounts in a currency. Every account's currency
// must be registered with RegisterCurrency first.
type Currency struct {
	bun.BaseModel `bun:"table:ledger.currencies,alias:c"`

	Code string `bun:",pk" json:"code"`
	// Decimals is the number of digits after the decimal point: 2 for USD
	// (amount 540323 is 5403.23 USD), 0 for JPY, 18 for ETH. It is nil only
	// for a currency already in use before currencies were registered, until
	// RegisterCurrency sets it. Once set it never changes.
	Decimals  *int      `json:"decimals"`
	CreatedAt time.Time `bun:",notnull" json:"created_at"`
}

// Parse converts a decimal value such as "5403.23" into an Amount in minor
// units (540323 for USD). It never rounds: "5403.234" USD fails with
// ErrInvalidParams, though extra trailing zeros ("5403.230") are fine.
func (c *Currency) Parse(value string) (Amount, error) {
	d, err := c.decimals()
	if err != nil {
		return Amount{}, err
	}
	return parseValue(value, d)
}

// Format writes a as a decimal value with exactly Decimals digits after the
// point: 540323 USD is "5403.23", 5 USD is "0.05".
func (c *Currency) Format(a Amount) (string, error) {
	d, err := c.decimals()
	if err != nil {
		return "", err
	}
	return formatValue(a, d), nil
}

func (c *Currency) decimals() (int, error) {
	switch {
	case c.Decimals == nil:
		return 0, fmt.Errorf("%w: %s has no decimals registered yet; call RegisterCurrency", ErrUnknownCurrency, c.Code)
	case *c.Decimals < 0 || *c.Decimals > maxDecimals:
		return 0, fmt.Errorf("%w: %s decimals must be 0-%d, not %d", ErrInvalidParams, c.Code, maxDecimals, *c.Decimals)
	}
	return *c.Decimals, nil
}

// clone returns a deep copy, so values handed out never share Decimals with
// the cache.
func (c Currency) clone() *Currency {
	if c.Decimals != nil {
		d := *c.Decimals
		c.Decimals = &d
	}
	return &c
}

func parseValue(value string, decimals int) (Amount, error) {
	digits := strings.TrimPrefix(value, "-")
	whole, frac, hasPoint := strings.Cut(digits, ".")
	if whole == "" || (hasPoint && frac == "") || !isDigits(whole) || !isDigits(frac) {
		return Amount{}, fmt.Errorf("%w: value %q must look like 5403.23", ErrInvalidParams, value)
	}
	if len(frac) > decimals {
		if strings.Trim(frac[decimals:], "0") != "" {
			return Amount{}, fmt.Errorf("%w: value %q has more than %d decimals", ErrInvalidParams, value, decimals)
		}
		frac = frac[:decimals]
	}
	return ParseAmount(value[:len(value)-len(digits)] + whole + frac + strings.Repeat("0", decimals-len(frac)))
}

func isDigits(s string) bool { return strings.Trim(s, "0123456789") == "" }

func formatValue(a Amount, decimals int) string {
	s := a.big().String()
	sign, digits := "", s
	if a.Sign() < 0 {
		sign, digits = "-", s[1:]
	}
	if decimals == 0 {
		return s
	}
	if pad := decimals + 1 - len(digits); pad > 0 {
		digits = strings.Repeat("0", pad) + digits
	}
	return sign + digits[:len(digits)-decimals] + "." + digits[len(digits)-decimals:]
}

type CurrencyParams struct {
	Code     string `json:"code"`
	Decimals int    `json:"decimals"`
}

// RegisterCurrency registers a currency and its decimals. Registering it again
// with the same decimals returns it unchanged; different decimals fail with
// ErrCurrencyConflict, since stored amounts would change meaning.
func (s *Service) RegisterCurrency(ctx context.Context, p CurrencyParams) (*Currency, error) {
	if err := validateCurrency(p.Code); err != nil {
		return nil, err
	}
	if p.Decimals < 0 || p.Decimals > maxDecimals {
		return nil, fmt.Errorf("%w: decimals must be 0-%d", ErrInvalidParams, maxDecimals)
	}
	c := new(Currency)
	err := inTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		// Sets the decimals of a currency that was in use before registration.
		err := tx.NewRaw(`
			INSERT INTO ledger.currencies AS c (code, decimals, created_at) VALUES (?, ?, ?)
			ON CONFLICT (code) DO UPDATE SET decimals = EXCLUDED.decimals WHERE c.decimals IS NULL
			RETURNING c.code, c.decimals, c.created_at`, p.Code, p.Decimals, now()).Scan(ctx, c)
		if err != nil {
			return err
		}
		return emit(ctx, tx, EventCurrencyRegistered, nil, c.Code, c, c.CreatedAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		// Already registered with decimals.
		if c, err = s.GetCurrency(ctx, p.Code); err != nil {
			return nil, err
		}
		if *c.Decimals != p.Decimals {
			return nil, fmt.Errorf("%w: %s has %d decimals, not %d", ErrCurrencyConflict, p.Code, *c.Decimals, p.Decimals)
		}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: register currency: %w", err)
	}
	s.currencies.Store(c.Code, c.clone())
	return c, nil
}

// GetCurrency returns a registered currency, or ErrUnknownCurrency.
func (s *Service) GetCurrency(ctx context.Context, code string) (*Currency, error) {
	c, err := s.getCurrency(ctx, s.db, code)
	return c, classify(ctx, err)
}

// getCurrency reads through db. Currencies with decimals are cached: they
// never change. Only reads through a *bun.DB are cached, never ones through a
// caller's transaction, which could still roll back.
func (s *Service) getCurrency(ctx context.Context, db bun.IDB, code string) (*Currency, error) {
	if v, ok := s.currencies.Load(code); ok {
		return v.(*Currency).clone(), nil
	}
	if err := validateCurrency(code); err != nil {
		return nil, err
	}
	c := new(Currency)
	err := db.NewSelect().Model(c).Where("c.code = ?", code).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s; register it with RegisterCurrency", ErrUnknownCurrency, code)
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: get currency: %w", err)
	}
	if _, committed := db.(*bun.DB); committed && c.Decimals != nil {
		s.currencies.Store(code, c.clone())
	}
	return c, nil
}

// resolveAmount returns value converted to minor units of currency if it is
// set, and amount otherwise. If both are set they must agree.
func (s *Service) resolveAmount(ctx context.Context, db bun.IDB, currency string, amount Amount, value string) (Amount, error) {
	if value == "" {
		return amount, nil
	}
	c, err := s.getCurrency(ctx, db, currency)
	if err != nil {
		return Amount{}, err
	}
	v, err := c.Parse(value)
	if err != nil {
		return Amount{}, err
	}
	if !amount.IsZero() && amount.Cmp(v) != 0 {
		return Amount{}, fmt.Errorf("%w: amount %s and value %q disagree", ErrInvalidParams, amount, value)
	}
	return v, nil
}

// resolveLegs returns legs with every Value converted into Amount and then
// cleared, so validation, hashing and storage only ever see Amount: a request
// with values and one with the same amounts are the same request. legs itself
// is not modified.
func (s *Service) resolveLegs(ctx context.Context, db bun.IDB, currency string, legs []Leg) ([]Leg, error) {
	if !hasValues(legs) {
		return legs, nil
	}
	out := slices.Clone(legs)
	for i := range out {
		amount, err := s.resolveAmount(ctx, db, currency, out[i].Amount, out[i].Value)
		if err != nil {
			return nil, fmt.Errorf("leg %d: %w", i, err)
		}
		out[i].Amount, out[i].Value = amount, ""
	}
	return out, nil
}

func hasValues(legs []Leg) bool {
	return slices.ContainsFunc(legs, func(l Leg) bool { return l.Value != "" })
}
