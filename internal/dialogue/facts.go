package dialogue

import (
	"encoding/json"
	"strconv"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// canonicalText writes a canonical value as text: the value in its canonical
// spelling, with the currency after a money amount.
func canonicalText(v act.CanonicalValue) string {
	switch x := v.Value.(type) {
	case act.TextValue:
		return x.Value
	case act.IntegerValue:
		return strconv.FormatInt(x.Value, 10)
	case act.DecimalValue:
		return x.Value
	case act.BooleanValue:
		return strconv.FormatBool(x.Value)
	case act.DateValue:
		return x.Value
	case act.TimeValue:
		return x.Value
	case act.DatetimeValue:
		return string(x.Value)
	case act.MoneyValue:
		return x.Value + " " + x.Currency
	case act.IdentifierValue:
		return string(x.Value)
	}
	return ""
}

// factText writes a fact's value for the model: the host's display form when it
// has one, else the canonical value.
func factText(f act.Fact) string {
	if f.Display != nil && *f.Display != "" {
		return *f.Display
	}
	return canonicalText(f.Value)
}

// plainValue returns the Go value a canonical value stands for as a field value
// (what a selected choice supplies): a string, a json.Number or a bool.
func plainValue(v act.CanonicalValue) (any, bool) {
	switch x := v.Value.(type) {
	case act.TextValue:
		return x.Value, true
	case act.IntegerValue:
		return json.Number(strconv.FormatInt(x.Value, 10)), true
	case act.DecimalValue:
		return json.Number(x.Value), true
	case act.BooleanValue:
		return x.Value, true
	case act.DateValue:
		return x.Value, true
	case act.TimeValue:
		return x.Value, true
	case act.DatetimeValue:
		return string(x.Value), true
	case act.IdentifierValue:
		return string(x.Value), true
	}
	return nil, false
}

func textValue(s string) act.CanonicalValue {
	return act.CanonicalValue{Value: act.TextValue{Value: s}}
}

func identifierValue(s string) act.CanonicalValue {
	return act.CanonicalValue{Value: act.IdentifierValue{Value: common.Identifier(s)}}
}

// textFact is a core-written fact whose value is a host-supplied sentence.
func textFact(field, value string) act.Fact {
	return act.Fact{Field: common.FieldName(field), Value: textValue(value)}
}

// boundFacts keeps at most max facts. A fact that is dropped is not stated: the
// response never carries a value the directive does not.
func boundFacts(in []act.Fact, max int) []act.Fact {
	out := make([]act.Fact, 0, len(in))
	for _, f := range in {
		if len(out) == max {
			break
		}
		out = append(out, cloneFact(f))
	}
	return out
}

func cloneFact(f act.Fact) act.Fact {
	if f.Display != nil {
		d := *f.Display
		f.Display = &d
	}
	return f
}

// mustStateAll names the field of every fact: the response states them all. A name
// that several facts share (a read that lists records, each with its label) is named
// once: must_state is a set of names (the directive schema says so), and the verifier
// and the templates already take every fact of a named field.
func mustStateAll(facts []act.Fact) []common.FieldName {
	out := make([]common.FieldName, 0, len(facts))
	seen := make(map[common.FieldName]bool, len(facts))
	for _, f := range facts {
		if seen[f.Field] {
			continue
		}
		seen[f.Field] = true
		out = append(out, f.Field)
	}
	return out
}

func errorCode(c contracts.ErrCode) *common.ErrorCode {
	e := common.ErrorCode(c)
	return &e
}

func fieldNames(in []string) []common.FieldName {
	out := make([]common.FieldName, 0, len(in))
	for _, s := range in {
		out = append(out, common.FieldName(s))
	}
	return out
}

func strPtr(s string) *string { return &s }
