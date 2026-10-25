package dialogue

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// fieldCheck is the outcome of the deterministic validation of candidate values.
type fieldCheck struct {
	// Clean holds the values that passed, normalised: a number the model wrote as
	// text is a json.Number, a datetime is UTC.
	Clean map[string]any
	// Issues are the values that failed, with a typed, constant reason.
	Issues []act.Issue
	// Dropped are the proposed names that were not kept: fields the capability
	// does not declare or the model may not see, and null values.
	Dropped []string
}

// maxEntityMentionRunes bounds an entity reference value.
const maxEntityMentionRunes = 512

var durationRE = regexp.MustCompile(`^P(?:\d+Y)?(?:\d+M)?(?:\d+W)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`)

// checkFields validates candidate field values against the capability's own field
// definitions (master plan 11.2: "the capability's own field schema decides what
// is valid"). A value is never repaired by guessing: it is accepted as stated,
// accepted after a lossless normalisation (the text "3" for an integer field), or
// it is an issue. Only fields the model may see are considered; the rest, and
// names the capability does not declare, are dropped.
func checkFields(spec ap.Capability, visible []string, in map[string]any, m *coreMsgs) fieldCheck {
	out := fieldCheck{Clean: map[string]any{}}
	vis := map[string]bool{}
	for _, f := range visible {
		vis[f] = true
	}
	defs := map[string]ap.CapabilityField{}
	for _, f := range spec.Fields {
		defs[string(f.Name)] = f
	}
	names := make([]string, 0, len(in))
	for n := range in {
		names = append(names, n)
	}
	sortStrings(names)
	for _, name := range names {
		v := in[name]
		def, declared := defs[name]
		if !declared || !vis[name] || v == nil {
			out.Dropped = append(out.Dropped, name)
			continue
		}
		clean, msg := checkValue(def, v, m)
		if msg != "" {
			out.Issues = append(out.Issues, issue(contracts.ErrValidationFailed, name, msg))
			continue
		}
		out.Clean[name] = clean
	}
	return out
}

func issue(code contracts.ErrCode, field, message string) act.Issue {
	is := act.Issue{Code: common.ErrorCode(code), Message: message}
	if field != "" {
		f := common.FieldName(field)
		is.Field = &f
	}
	return is
}

// checkValue validates one value against its field definition. It returns the
// normalised value, or a non-empty message.
func checkValue(def ap.CapabilityField, v any, m *coreMsgs) (any, string) {
	if def.Type == ap.CapabilityFieldTypeList {
		items, ok := v.([]any)
		if !ok {
			return nil, m.mustBeList
		}
		if def.MaxItems != nil && int64(len(items)) > *def.MaxItems {
			return nil, fmt.Sprintf(m.tooManyItems, *def.MaxItems)
		}
		it := ap.CapabilityFieldType("string")
		if def.ItemType != nil {
			it = ap.CapabilityFieldType(*def.ItemType)
		}
		itemDef := ap.CapabilityField{Name: def.Name, Type: it, Values: def.Values, EntityType: def.EntityType}
		out := make([]any, 0, len(items))
		for _, x := range items {
			c, msg := checkScalar(itemDef, x, m)
			if msg != "" {
				return nil, msg
			}
			out = append(out, c)
		}
		return out, ""
	}
	return checkScalar(def, v, m)
}

func checkScalar(def ap.CapabilityField, v any, m *coreMsgs) (any, string) {
	switch def.Type {
	case ap.CapabilityFieldTypeString:
		s, ok := v.(string)
		if !ok {
			return nil, m.mustBeText
		}
		n := utf8.RuneCountInString(s)
		if def.MinLength != nil && int64(n) < *def.MinLength {
			return nil, fmt.Sprintf(m.tooShort, *def.MinLength)
		}
		if def.MaxLength != nil && int64(n) > *def.MaxLength {
			return nil, fmt.Sprintf(m.tooLong, *def.MaxLength)
		}
		return s, ""
	case ap.CapabilityFieldTypeInteger:
		n, ok := asNumber(v)
		if !ok || n.f != math.Trunc(n.f) || math.Abs(n.f) > 1<<53 {
			return nil, m.mustBeInteger
		}
		if msg := checkRange(def, n.f, m); msg != "" {
			return nil, msg
		}
		return json.Number(strconv.FormatInt(int64(n.f), 10)), ""
	case ap.CapabilityFieldTypeNumber:
		n, ok := asNumber(v)
		if !ok {
			return nil, m.mustBeNumber
		}
		if msg := checkRange(def, n.f, m); msg != "" {
			return nil, msg
		}
		return n.num, ""
	case ap.CapabilityFieldTypeBoolean:
		switch x := v.(type) {
		case bool:
			return x, ""
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true":
				return true, ""
			case "false":
				return false, ""
			}
		}
		return nil, m.mustBeBoolean
	case ap.CapabilityFieldTypeDate:
		s, ok := v.(string)
		if !ok {
			return nil, m.mustBeDate
		}
		t, err := time.Parse("2006-01-02", s)
		if err != nil || t.Format("2006-01-02") != s {
			return nil, m.mustBeDate
		}
		return s, ""
	case ap.CapabilityFieldTypeDatetime:
		s, ok := v.(string)
		if !ok {
			return nil, m.mustBeDatetime
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, m.mustBeDatetime
		}
		return t.UTC().Format("2006-01-02T15:04:05Z"), ""
	case ap.CapabilityFieldTypeDuration:
		s, ok := v.(string)
		if !ok || s == "P" || strings.HasSuffix(s, "T") || !durationRE.MatchString(s) {
			return nil, m.mustBeDuration
		}
		return s, ""
	case ap.CapabilityFieldTypeEnum:
		s, ok := v.(string)
		if ok {
			for _, allowed := range def.Values {
				if s == allowed {
					return s, ""
				}
			}
		}
		return nil, fmt.Sprintf(m.mustBeOneOf, strings.Join(def.Values, ", "))
	case ap.CapabilityFieldTypeEntityRef:
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, m.mustBeText
		}
		if utf8.RuneCountInString(s) > maxEntityMentionRunes {
			return nil, fmt.Sprintf(m.tooLong, maxEntityMentionRunes)
		}
		return s, ""
	}
	return nil, m.mustBeText
}

type numeric struct {
	f   float64
	num json.Number
}

// asNumber reads a JSON number, or a string that is exactly one.
func asNumber(v any) (numeric, bool) {
	var n json.Number
	switch x := v.(type) {
	case json.Number:
		n = x
	case string:
		n = json.Number(strings.TrimSpace(x))
	default:
		return numeric{}, false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return numeric{}, false
	}
	return numeric{f: f, num: n}, true
}

func checkRange(def ap.CapabilityField, f float64, m *coreMsgs) string {
	if def.Minimum != nil && f < *def.Minimum {
		return fmt.Sprintf(m.belowMinimum, strconv.FormatFloat(*def.Minimum, 'f', -1, 64))
	}
	if def.Maximum != nil && f > *def.Maximum {
		return fmt.Sprintf(m.aboveMaximum, strconv.FormatFloat(*def.Maximum, 'f', -1, 64))
	}
	return ""
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
