package blocks

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// FactSet is the authoritative numbers of a turn: the integer, decimal and
// money facts of the DialogueDirective (and of the receipt, when the caller
// adds it). A chart that presents application results may show a number only
// if it is in the set. Numbers are compared as exact rationals, never as
// floats and never as spellings, so 12, "12.00" and 1.2e1 are one number and
// 0.1 is not 0.10000000000000002.
//
// The zero value and a nil *FactSet hold nothing.
type FactSet struct {
	nums map[string]struct{}
}

// NewFactSet collects the numbers of the facts. Facts that carry no number
// (text, boolean, date, time, datetime, identifier) add nothing: an identifier
// that looks like a number is a name, not a quantity.
func NewFactSet(facts []actions.Fact) *FactSet {
	s := &FactSet{nums: map[string]struct{}{}}
	s.AddFacts(facts)
	return s
}

// AddFacts adds the numbers of more facts.
func (s *FactSet) AddFacts(facts []actions.Fact) {
	if s.nums == nil {
		s.nums = map[string]struct{}{}
	}
	for _, f := range facts {
		switch v := f.Value.Value.(type) {
		case actions.IntegerValue:
			s.nums[new(big.Rat).SetInt64(v.Value).RatString()] = struct{}{}
		case actions.DecimalValue:
			s.addDecimal(v.Value)
		case actions.MoneyValue:
			s.addDecimal(v.Value)
		}
	}
}

func (s *FactSet) addDecimal(text string) {
	if r, ok := parseDecimal(text); ok {
		s.nums[r.RatString()] = struct{}{}
	}
}

// Len is the number of distinct numbers in the set.
func (s *FactSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.nums)
}

// has reports whether the exact value is in the set.
func (s *FactSet) has(r *big.Rat) bool {
	if s == nil {
		return false
	}
	_, ok := s.nums[r.RatString()]
	return ok
}

// parseDecimal reads the canonical decimal string of a Decimal or Money fact
// ("-?digits(.digits)?").
func parseDecimal(text string) (*big.Rat, bool) {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > 40 {
		return nil, false
	}
	for i, c := range t {
		if (c < '0' || c > '9') && c != '.' && !(i == 0 && (c == '-' || c == '+')) {
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(t)
	return r, ok
}

// maxExactInteger is the largest integer a JSON number carries exactly.
const maxExactInteger = 1 << 53

// asNumber reads a chart or table value as an exact number. ok is false for
// anything that is not a number; finite is false for a NaN, an infinity or a
// magnitude beyond the JSON-safe range.
func asNumber(v any) (r *big.Rat, isNumber, finite bool) {
	switch n := v.(type) {
	case float64:
		return floatRat(n)
	case float32:
		return floatRat(float64(n))
	case int:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case int8:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case int16:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case int32:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case int64:
		return intRat(n)
	case uint:
		return uintRat(uint64(n))
	case uint8:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case uint16:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case uint32:
		return new(big.Rat).SetInt64(int64(n)), true, true
	case uint64:
		return uintRat(n)
	case json.Number:
		rat, ok := new(big.Rat).SetString(string(n))
		if !ok {
			return nil, true, false
		}
		if new(big.Rat).Abs(rat).Cmp(new(big.Rat).SetInt64(maxExactInteger)) > 0 {
			return nil, true, false
		}
		return rat, true, true
	}
	return nil, false, false
}

func floatRat(f float64) (*big.Rat, bool, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > maxExactInteger {
		return nil, true, false
	}
	// The shortest representation that round-trips is the decimal the producer
	// meant: 0.1 is 1/10, not the nearest binary fraction.
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	if !ok {
		return nil, true, false
	}
	return r, true, true
}

func intRat(n int64) (*big.Rat, bool, bool) {
	if n > maxExactInteger || n < -maxExactInteger {
		return nil, true, false
	}
	return new(big.Rat).SetInt64(n), true, true
}

func uintRat(n uint64) (*big.Rat, bool, bool) {
	if n > maxExactInteger {
		return nil, true, false
	}
	return new(big.Rat).SetInt64(int64(n)), true, true
}

// numericLiteral reports whether a string is nothing but a number ("12",
// "-3.5"). A string value that reads as a number presents a number, whatever
// its JSON type.
func numericLiteral(s string) (*big.Rat, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, false
	}
	digits := 0
	for i, c := range t {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' || ((c == '-' || c == '+') && i == 0):
		default:
			return nil, false
		}
	}
	if digits == 0 {
		return nil, false
	}
	return parseDecimal(t)
}
