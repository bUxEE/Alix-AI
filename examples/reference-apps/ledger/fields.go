package ledger

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// fields are the candidate values of a capability: JSON scalars as the bridge
// decodes them (strings, booleans and json.Number). Every value is untrusted:
// AlixAI validated it, the ledger validates it again.
type fields map[string]any

// parseFields refuses a member that is not a field of the operation, so a field a
// newer package relies on is never silently ignored.
func parseFields(raw map[string]any, allowed ...string) (fields, *refusal) {
	f := fields{}
	for k, v := range raw {
		known := false
		for _, a := range allowed {
			if k == a {
				known = true
			}
		}
		if !known {
			return nil, refuse("validation_failed", k, "this operation has no field of this name")
		}
		f[k] = v
	}
	return f, nil
}

func (f fields) text(name string, required bool, minLen, maxLen int) (string, *refusal) {
	v, present := f[name]
	if !present || v == nil {
		if required {
			return "", refuse("missing_fields", name, name+" is required")
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", refuse("validation_failed", name, name+" must be text")
	}
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < minLen || n > maxLen {
		return "", refuse("validation_failed", name, fmt.Sprintf("%s must be %d to %d characters", name, minLen, maxLen))
	}
	return s, nil
}

func (f fields) enum(name string, required bool, values ...string) (string, *refusal) {
	s, bad := f.text(name, required, 0, 64)
	if bad != nil || s == "" {
		return "", bad
	}
	for _, v := range values {
		if s == v {
			return s, nil
		}
	}
	return "", refuse("validation_failed", name, name+" is not one of the allowed values")
}

// integer reads a whole number in [min, max]; def applies when the field is absent
// and !required. It never accepts a fraction or an exponent.
func (f fields) integer(name string, required bool, min, max, def int64) (int64, *refusal) {
	v, present := f[name]
	if !present || v == nil {
		if required {
			return 0, refuse("missing_fields", name, name+" is required")
		}
		return def, nil
	}
	n, err := minorFrom(v)
	if err != nil {
		return 0, refuse("validation_failed", name, name+" must be a whole number")
	}
	if n < min || n > max {
		return 0, refuse("validation_failed", name, fmt.Sprintf("%s must be from %d to %d", name, min, max))
	}
	return n, nil
}

var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func parseDate(s string) (time.Time, bool) {
	if !datePattern.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

func addDays(date string, days int64) string {
	t, _ := parseDate(date)
	return t.AddDate(0, 0, int(days)).Format("2006-01-02")
}
