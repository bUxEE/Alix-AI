package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Money in this application is an integer count of minor units and an ISO 4217
// currency, and nothing else: there is no float anywhere between the wire and the
// store. The decimal text a result carries is derived from the integer by integer
// arithmetic, so 1234 EUR cents is "12.34", 1234 yen is "1234" and 1234 Kuwaiti
// fils is "1.234", exactly, every time.

// currencyExponents is the closed set of currencies the ledger bills in, with the
// number of minor-unit digits of each. The set is deliberately mixed (0, 2 and 3
// digits) so no code can assume cents.
var currencyExponents = map[string]int{
	"CHF": 2,
	"EUR": 2,
	"GBP": 2,
	"JPY": 0,
	"KWD": 3,
	"USD": 2,
}

// Currencies lists the supported ISO codes in order.
func Currencies() []string {
	out := make([]string, 0, len(currencyExponents))
	for c := range currencyExponents {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// MaxMinor bounds one amount: 10^14 minor units, so a sum of a few thousand of
// them still fits an int64 and the decimal text fits the canonical money pattern.
const MaxMinor int64 = 100_000_000_000_000

// Money is an amount of one currency in minor units.
type Money struct {
	Minor    int64
	Currency string
}

// Exponent returns the minor-unit digits of a currency.
func Exponent(currency string) (int, bool) {
	e, ok := currencyExponents[currency]
	return e, ok
}

// Decimal renders the amount as the canonical decimal string of a money fact. It
// panics on an unsupported currency or a negative amount: both are programming
// errors, because every Money is built from validated input.
func (m Money) Decimal() string {
	exp, ok := currencyExponents[m.Currency]
	if !ok || m.Minor < 0 {
		panic("ledger: a Money with an unsupported currency or a negative amount")
	}
	if exp == 0 {
		return strconv.FormatInt(m.Minor, 10)
	}
	unit := int64(1)
	for i := 0; i < exp; i++ {
		unit *= 10
	}
	return fmt.Sprintf("%d.%0*d", m.Minor/unit, exp, m.Minor%unit)
}

// ParseDecimal is the inverse of Decimal: it reads "12.34" as 1234 minor units of
// a two-digit currency. It refuses a different number of fraction digits, a sign,
// an exponent and anything out of range, so an amount is never rounded.
func ParseDecimal(s, currency string) (int64, error) {
	exp, ok := currencyExponents[currency]
	if !ok {
		return 0, errors.New("unsupported currency")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac != (exp > 0) || len(frac) != exp || whole == "" || (len(whole) > 1 && whole[0] == '0') {
		return 0, errors.New("the amount does not have the digits of the currency")
	}
	v, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil || v < 0 || strings.ContainsAny(whole+frac, "+-eE ") || v > MaxMinor {
		return 0, errors.New("the amount is not a valid amount")
	}
	return v, nil
}

// Add returns a+b. The currencies must match and the sum must stay within
// MaxMinor.
func Add(a, b Money) (Money, error) {
	if a.Currency != b.Currency {
		return Money{}, errors.New("currencies differ")
	}
	if a.Minor > math.MaxInt64-b.Minor || a.Minor+b.Minor > MaxMinor {
		return Money{}, errors.New("the sum is out of range")
	}
	return Money{Minor: a.Minor + b.Minor, Currency: a.Currency}, nil
}

// Sub returns a-b; it refuses a negative result.
func Sub(a, b Money) (Money, error) {
	if a.Currency != b.Currency {
		return Money{}, errors.New("currencies differ")
	}
	if b.Minor > a.Minor {
		return Money{}, errors.New("the difference is negative")
	}
	return Money{Minor: a.Minor - b.Minor, Currency: a.Currency}, nil
}

// minorFrom reads a JSON integer, and only an integer, as minor units: a string,
// a fraction ("12.50"), an exponent ("1e3") and a number that does not fit are all
// refused, never rounded or truncated. Values arrive as json.Number from the
// bridge; native ints are accepted for callers that build fields in Go.
func minorFrom(v any) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return 0, errors.New("an amount is a whole number of minor units")
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, errors.New("an amount is a whole number of minor units")
		}
		return n, nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	default:
		return 0, errors.New("an amount is a whole number of minor units")
	}
}
