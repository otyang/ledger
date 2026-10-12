package ledger

import "fmt"

type legKey struct {
	account   AccountID
	direction Direction
}

// aggregate sums legs per (account, direction), keeping first-seen order.
func aggregate(legs []Leg) []Leg {
	idx := make(map[legKey]int, len(legs))
	out := make([]Leg, 0, len(legs))
	for _, leg := range legs {
		k := legKey{leg.AccountID, leg.Direction}
		if i, ok := idx[k]; ok {
			out[i].Amount = out[i].Amount.Add(leg.Amount)
			continue
		}
		idx[k] = len(out)
		out = append(out, leg)
	}
	return out
}

// pickLegs chooses the legs to post against amounts still open per (account,
// direction), as used by hold commits and refunds:
//
//   - explicit legs: each (account, direction) total must not exceed what is open;
//   - amount: only when what is open is a simple transfer (one debit and one
//     credit leg), and posts amount on both legs. It must be positive: an
//     explicit 0 is an error, never "everything";
//   - neither (amount left unset): everything that is open.
//
// exceeded is the error wrapped when the request asks for more than is open.
func pickLegs(open []Leg, amount Amount, explicit []Leg, exceeded error) ([]Leg, error) {
	switch {
	case amount.isSet() && amount.Sign() <= 0:
		return nil, fmt.Errorf("%w: amount must be positive; leave it out to take everything that is left", ErrInvalidParams)
	case amount.isSet() && len(explicit) > 0:
		return nil, fmt.Errorf("%w: set amount or legs, not both", ErrInvalidParams)
	case len(open) == 0:
		return nil, fmt.Errorf("%w: nothing left", exceeded)
	}

	if len(explicit) > 0 {
		remaining := make(map[legKey]Amount, len(open))
		for _, leg := range open {
			remaining[legKey{leg.AccountID, leg.Direction}] = leg.Amount
		}
		for _, leg := range aggregate(explicit) {
			if left := remaining[legKey{leg.AccountID, leg.Direction}]; leg.Amount.Cmp(left) > 0 {
				return nil, fmt.Errorf("%w: %s of %s on account %s, %s left", exceeded, leg.Direction, leg.Amount, leg.AccountID, left)
			}
		}
		return explicit, nil
	}

	if amount.isSet() {
		if len(open) != 2 {
			return nil, fmt.Errorf("%w: amount only applies to two-leg transfers; pass legs instead", ErrInvalidParams)
		}
		legs := make([]Leg, 2)
		for i, leg := range open {
			if amount.Cmp(leg.Amount) > 0 {
				return nil, fmt.Errorf("%w: amount %s, %s left", exceeded, amount, leg.Amount)
			}
			legs[i] = Leg{AccountID: leg.AccountID, Direction: leg.Direction, Amount: amount}
		}
		return legs, nil
	}

	return open, nil
}
