package providerbudget

import (
	"fmt"
	"math/bits"
	"regexp"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// MaxAmount is the largest money amount or counter: 2^53-1, so every JSON implementation represents
// it exactly (schemas/providers shared microUnits and counter).
const MaxAmount int64 = 9007199254740991

// Units is the metered consumption of one call: an upper bound when reserving, the metered figures
// when settling.
type Units = providers.UsageRecordMetering

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// unitAmount is one meter unit and its count.
type unitAmount struct {
	unit providers.MeterUnit
	n    int64
}

// unitList returns the nine units of u in a fixed order.
func unitList(u Units) []unitAmount {
	return []unitAmount{
		{providers.MeterUnitToksIn, int64(u.ToksIn)},
		{providers.MeterUnitToksOut, int64(u.ToksOut)},
		{providers.MeterUnitToksInCached, int64(u.ToksInCached)},
		{providers.MeterUnitImagesIn, int64(u.ImagesIn)},
		{providers.MeterUnitImagesOut, int64(u.ImagesOut)},
		{providers.MeterUnitAudioSecondsIn, int64(u.AudioSecondsIn)},
		{providers.MeterUnitAudioSecondsOut, int64(u.AudioSecondsOut)},
		{providers.MeterUnitFileBytesIn, int64(u.FileBytesIn)},
		{providers.MeterUnitRequests, int64(u.Requests)},
	}
}

// validateUnits refuses a negative or over-cap count.
func validateUnits(u Units) error {
	for _, a := range unitList(u) {
		if a.n < 0 || a.n > MaxAmount {
			return invalid("bad_units", fmt.Sprintf("providerbudget: %s is outside 0..2^53-1", a.unit))
		}
	}
	return nil
}

// price returns the table's price of a unit, or nil.
func price(t providers.PriceTable, u providers.MeterUnit) *providers.PriceTablePrice {
	p := t.Prices
	switch u {
	case providers.MeterUnitToksIn:
		return p.ToksIn
	case providers.MeterUnitToksOut:
		return p.ToksOut
	case providers.MeterUnitToksInCached:
		return p.ToksInCached
	case providers.MeterUnitImagesIn:
		return p.ImagesIn
	case providers.MeterUnitImagesOut:
		return p.ImagesOut
	case providers.MeterUnitAudioSecondsIn:
		return p.AudioSecondsIn
	case providers.MeterUnitAudioSecondsOut:
		return p.AudioSecondsOut
	case providers.MeterUnitFileBytesIn:
		return p.FileBytesIn
	case providers.MeterUnitRequests:
		return p.Requests
	}
	return nil
}

// validatePrices checks the table's currency and every price it carries.
func validatePrices(t providers.PriceTable) error {
	if !currencyRE.MatchString(string(t.Currency)) {
		return invalid("bad_price_table", "providerbudget: the price table currency is not three upper case letters")
	}
	for _, u := range []providers.MeterUnit{
		providers.MeterUnitToksIn, providers.MeterUnitToksOut, providers.MeterUnitToksInCached,
		providers.MeterUnitImagesIn, providers.MeterUnitImagesOut, providers.MeterUnitAudioSecondsIn,
		providers.MeterUnitAudioSecondsOut, providers.MeterUnitFileBytesIn, providers.MeterUnitRequests,
	} {
		p := price(t, u)
		if p == nil {
			continue
		}
		if p.PerUnits < 1 || p.PerUnits > MaxAmount || p.MicroUnits < 0 || int64(p.MicroUnits) > MaxAmount {
			return invalid("bad_price_table", fmt.Sprintf("providerbudget: the price of %s is out of range", u))
		}
	}
	return nil
}

// priceOf is ceil(n * micro / per) in exact integer arithmetic, refused above 2^53-1.
func priceOf(p providers.PriceTablePrice, n int64) (int64, error) {
	hi, lo := bits.Mul64(uint64(n), uint64(p.MicroUnits))
	per := uint64(p.PerUnits)
	if hi >= per {
		return 0, invalid("cost_overflow", "providerbudget: a cost does not fit in 2^53-1 micro-units")
	}
	q, r := bits.Div64(hi, lo, per)
	if r > 0 {
		q++
	}
	if q > uint64(MaxAmount) {
		return 0, invalid("cost_overflow", "providerbudget: a cost does not fit in 2^53-1 micro-units")
	}
	return int64(q), nil
}

func addCost(total, c int64) (int64, error) {
	if total > MaxAmount-c {
		return 0, invalid("cost_overflow", "providerbudget: a cost does not fit in 2^53-1 micro-units")
	}
	return total + c, nil
}

func unpriced(u providers.MeterUnit) error {
	return invalid("unpriced_unit", fmt.Sprintf("providerbudget: the price table has no price for %s, so the request cannot be estimated and is not sent", u)).
		WithDetails("unit", string(u))
}

// Cost prices metered units from the price table: each used unit rounds up on its own and the
// results are summed. A used unit with no price is an error, except requests: a table with no
// per-request price bills nothing per request (the same rule as internal/providers).
func Cost(t providers.PriceTable, u Units) (providers.MicroUnits, error) {
	if err := validatePrices(t); err != nil {
		return 0, err
	}
	if err := validateUnits(u); err != nil {
		return 0, err
	}
	var total int64
	for _, a := range unitList(u) {
		if a.n == 0 {
			continue
		}
		p := price(t, a.unit)
		if p == nil {
			if a.unit == providers.MeterUnitRequests {
				continue
			}
			return 0, unpriced(a.unit)
		}
		c, err := priceOf(*p, a.n)
		if err != nil {
			return 0, err
		}
		if total, err = addCost(total, c); err != nil {
			return 0, err
		}
	}
	return providers.MicroUnits(total), nil
}

// Estimate prices the largest a request can consume, as an upper bound (ADR-0025 point 3): units is
// the bounded input, the declared output cap and so on. Input toks use the higher of the toks_in and
// toks_in_cached rates, since the caller cannot know the split in advance, so whatever the provider
// later reports within these units costs no more than the estimate. A used unit with no price is
// refused; so is toks_in without a toks_in price even when a cached price exists.
func Estimate(t providers.PriceTable, units Units) (providers.MicroUnits, error) {
	if err := validatePrices(t); err != nil {
		return 0, err
	}
	if err := validateUnits(units); err != nil {
		return 0, err
	}
	rest := units
	rest.ToksIn = 0
	base, err := Cost(t, rest)
	if err != nil {
		return 0, err
	}
	total := int64(base)
	if units.ToksIn > 0 {
		p := price(t, providers.MeterUnitToksIn)
		if p == nil {
			return 0, unpriced(providers.MeterUnitToksIn)
		}
		c, err := priceOf(*p, int64(units.ToksIn))
		if err != nil {
			return 0, err
		}
		if cp := price(t, providers.MeterUnitToksInCached); cp != nil {
			cc, err := priceOf(*cp, int64(units.ToksIn))
			if err != nil {
				return 0, err
			}
			c = max(c, cc)
		}
		if total, err = addCost(total, c); err != nil {
			return 0, err
		}
	}
	return providers.MicroUnits(total), nil
}

// invalid is a validation_failed error carrying a fixed reason code.
func invalid(reason, msg string) *contracts.Error {
	return contracts.New(contracts.ErrValidationFailed, msg).WithDetails("reason", reason)
}
