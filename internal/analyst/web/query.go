package web

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// SensitiveValue is one dataset cell that must not appear in a web_query.
type SensitiveValue struct {
	Field string
	Value string
	Class an.DataClass
}

// CollectSensitiveValues gathers confidential and restricted cell values from
// datasets read in the turn.
func CollectSensitiveValues(datasets []an.Dataset) []SensitiveValue {
	var out []SensitiveValue
	for _, ds := range datasets {
		classOf := map[string]an.DataClass{}
		for _, f := range ds.Fields {
			classOf[string(f.Name)] = f.DataClass
		}
		for _, row := range ds.Rows {
			for name, cell := range row {
				cls := classOf[name]
				if cls != an.DataClassConfidential && cls != an.DataClassRestricted {
					continue
				}
				if s := cellString(cell); s != "" {
					out = append(out, SensitiveValue{Field: name, Value: s, Class: cls})
				}
			}
		}
	}
	return out
}

func cellString(v an.DatasetCell) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		// Exact decimal form without scientific notation so "1234567" matches.
		return strings.TrimSpace(strconv.FormatFloat(x, 'f', -1, 64))
	case json.Number:
		return strings.TrimSpace(x.String())
	case bool:
		return fmt.Sprint(x)
	case int, int64, int32:
		return fmt.Sprint(x)
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

// CheckQuery refuses a web_query that embeds any confidential/restricted value
// (exact and normalised match). Tenant policy must still allow the category
// separately via PolicyAllowsWebQuery.
func CheckQuery(query string, datasets []an.Dataset) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return contracts.New(contracts.ErrValidationFailed, "web: search query is empty")
	}
	if len(q) > 512 {
		return contracts.New(contracts.ErrValidationFailed, "web: search query is too long")
	}
	nq := normalizeMatch(q)
	for _, sv := range CollectSensitiveValues(datasets) {
		if sv.Value == "" {
			continue
		}
		if strings.Contains(q, sv.Value) || strings.Contains(nq, normalizeMatch(sv.Value)) {
			return contracts.New(contracts.ErrWebQueryRefused, "web: query embeds a confidential or restricted value").
				WithDetails("reason", "confidential_value").
				WithDetails("field", sv.Field).
				WithDetails("data_class", string(sv.Class))
		}
	}
	return nil
}

func normalizeMatch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PolicyAllowsWebQuery is the tenant/deployment gate for category web_query.
// When allowed is false the search is refused (fail closed).
func PolicyAllowsWebQuery(allowed bool) error {
	if !allowed {
		return contracts.New(contracts.ErrWebQueryRefused, "web: egress policy does not allow web_query").
			WithDetails("reason", "category_denied")
	}
	return nil
}
