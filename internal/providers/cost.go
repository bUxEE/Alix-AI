package providers

import (
	"fmt"
	"math/bits"
	"regexp"
)

// Money is integer micro-units of the operator's currency: 1_000_000 per unit.
// There are no floats anywhere in this file, and none in any type that carries
// a cost. Estimates round up, so they are upper bounds.

// maxMicroUnits mirrors the schemas (the JavaScript safe-integer range).
const maxMicroUnits = 9007199254740991

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// CostUsage is what one call consumed and what it cost. The JSON keys avoid
// the word "token" so the log redactor does not mask them.
type CostUsage struct {
	ToksIn    int `json:"toks_in"`
	ToksOut   int `json:"toks_out"`
	ImagesIn  int `json:"images_in,omitempty"`
	ImagesOut int `json:"images_out,omitempty"`
	// CostMicros is the cost in micro-units of Currency.
	CostMicros int64 `json:"cost_micros"`
	// Currency is an ISO 4217 code; empty only while CostMicros is zero.
	Currency string `json:"currency,omitempty"`
	// Estimated is true when AlixAI computed the figures from counts and a
	// price table (an upper bound) rather than the provider reporting them.
	Estimated bool `json:"estimated"`
}

// Validate rejects negative counts, a cost without a currency, a malformed
// currency, and a cost beyond the safe-integer range.
func (c CostUsage) Validate() error {
	if c.ToksIn < 0 || c.ToksOut < 0 || c.ImagesIn < 0 || c.ImagesOut < 0 {
		return schemaInvalid("usage has a negative count")
	}
	if c.CostMicros < 0 || c.CostMicros > maxMicroUnits {
		return schemaInvalid("usage cost is outside the micro-unit range")
	}
	if c.Currency != "" && !currencyPattern.MatchString(c.Currency) {
		return schemaInvalid("usage currency is not an ISO 4217 code")
	}
	if c.CostMicros > 0 && c.Currency == "" {
		return schemaInvalid("usage cost has no currency")
	}
	return nil
}

// Usage returns the plain token counts in the form the older contracts use.
func (c CostUsage) Usage() Usage { return Usage{InputTokens: c.ToksIn, OutputTokens: c.ToksOut} }

// Price is micro-units per PerUnits units, for example 150000 micro-units per
// 1000000 input toks.
type Price struct {
	PerUnits   int64 `json:"per_units"`
	MicroUnits int64 `json:"micro_units"`
}

// Validate checks the price is usable.
func (p Price) Validate() error {
	if p.PerUnits < 1 || p.PerUnits > maxMicroUnits || p.MicroUnits < 0 || p.MicroUnits > maxMicroUnits {
		return invalidRequest("price is outside the supported range")
	}
	return nil
}

// Of returns ceil(units * MicroUnits / PerUnits), computed in integers.
func (p Price) Of(units int64) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if units < 0 {
		return 0, invalidRequest("negative billed units")
	}
	hi, lo := bits.Mul64(uint64(units), uint64(p.MicroUnits))
	per := uint64(p.PerUnits)
	if hi >= per {
		return 0, invalidRequest("cost overflows")
	}
	q, r := bits.Div64(hi, lo, per)
	if r != 0 {
		q++
	}
	if q > maxMicroUnits {
		return 0, invalidRequest("cost overflows")
	}
	return int64(q), nil
}

// PriceTable is the pricing metadata of one provider model, mirroring the
// providers price-table schema. A unit with no price cannot be estimated:
// using it is an error, never a zero.
type PriceTable struct {
	Currency  string `json:"currency"`
	ToksIn    *Price `json:"toks_in,omitempty"`
	ToksOut   *Price `json:"toks_out,omitempty"`
	ImagesIn  *Price `json:"images_in,omitempty"`
	ImagesOut *Price `json:"images_out,omitempty"`
	Requests  *Price `json:"requests,omitempty"`
}

// Validate checks the currency and every price present.
func (t PriceTable) Validate() error {
	if !currencyPattern.MatchString(t.Currency) {
		return invalidRequest("price table currency is not an ISO 4217 code")
	}
	for _, p := range []*Price{t.ToksIn, t.ToksOut, t.ImagesIn, t.ImagesOut, t.Requests} {
		if p != nil {
			if err := p.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cost prices the usage u as an ESTIMATE (an upper bound, Estimated true).
// Each billed unit is rounded up on its own. A used unit with no price is an
// error, so a request that cannot be priced is not sent.
func (t PriceTable) Cost(u CostUsage) (CostUsage, error) {
	if err := t.Validate(); err != nil {
		return CostUsage{}, err
	}
	var total int64
	add := func(name string, p *Price, units int64) error {
		if units == 0 {
			return nil
		}
		if p == nil {
			return invalidRequest(fmt.Sprintf("no price for billed unit %s", name))
		}
		c, err := p.Of(units)
		if err != nil {
			return err
		}
		if total > maxMicroUnits-c {
			return invalidRequest("cost overflows")
		}
		total += c
		return nil
	}
	if u.ToksIn < 0 || u.ToksOut < 0 || u.ImagesIn < 0 || u.ImagesOut < 0 {
		return CostUsage{}, invalidRequest("negative billed units")
	}
	for _, step := range []struct {
		name  string
		p     *Price
		units int64
	}{
		{"toks_in", t.ToksIn, int64(u.ToksIn)},
		{"toks_out", t.ToksOut, int64(u.ToksOut)},
		{"images_in", t.ImagesIn, int64(u.ImagesIn)},
		{"images_out", t.ImagesOut, int64(u.ImagesOut)},
		{"requests", t.Requests, 1},
	} {
		units := step.units
		if step.name == "requests" && t.Requests == nil {
			units = 0 // a table with no per-request price bills nothing per request
		}
		if err := add(step.name, step.p, units); err != nil {
			return CostUsage{}, err
		}
	}
	u.CostMicros, u.Currency, u.Estimated = total, t.Currency, true
	return u, nil
}
