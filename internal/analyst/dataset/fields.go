package dataset

import (
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// Rank returns how restrictive c is (higher = more restrictive).
func Rank(c analyst.DataClass) int {
	switch c {
	case analyst.DataClassPublic:
		return 0
	case analyst.DataClassInternal:
		return 1
	case analyst.DataClassConfidential:
		return 2
	case analyst.DataClassRestricted:
		return 3
	default:
		return 3
	}
}

// MaxClass returns the most restrictive of the given classes.
func MaxClass(classes ...analyst.DataClass) analyst.DataClass {
	out := analyst.DataClassPublic
	for _, c := range classes {
		if Rank(c) > Rank(out) {
			out = c
		}
	}
	return out
}

// FieldByName returns the field named n, or false.
func FieldByName(ds *analyst.Dataset, n common.FieldName) (analyst.DatasetDatasetField, bool) {
	if ds == nil {
		return analyst.DatasetDatasetField{}, false
	}
	for _, f := range ds.Fields {
		if f.Name == n {
			return f, true
		}
	}
	return analyst.DatasetDatasetField{}, false
}

// Index maps field name → column index.
func Index(ds *analyst.Dataset) map[string]int {
	m := make(map[string]int, len(ds.Fields))
	for i, f := range ds.Fields {
		m[string(f.Name)] = i
	}
	return m
}

// Cell returns the named cell, or nil when absent.
func Cell(row analyst.DatasetRow, name string) any {
	if row == nil {
		return nil
	}
	v, ok := row[name]
	if !ok {
		return nil
	}
	return v
}

// IsNull reports a JSON null or missing cell.
func IsNull(v any) bool {
	return v == nil
}

// AsString coerces a cell to string for eq/contains/join keys.
func AsString(v any) (string, bool) {
	if v == nil {
		return "", true
	}
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64:
		return formatNumber(x), true
	case int64:
		return fmt.Sprintf("%d", x), true
	case int:
		return fmt.Sprintf("%d", x), true
	case jsonNumber:
		return string(x), true
	default:
		return fmt.Sprint(x), true
	}
}

// jsonNumber is unused locally; kept for clarity if encoding/json numbers appear.
type jsonNumber string

// AsRat parses a numeric cell into a rational. Null → ok=false, null=true.
func AsRat(v any) (r *big.Rat, null, ok bool) {
	if v == nil {
		return nil, true, true
	}
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false, false
		}
		r = new(big.Rat).SetFloat64(x)
		if r == nil {
			return nil, false, false
		}
		return r, false, true
	case int64:
		return new(big.Rat).SetInt64(x), false, true
	case int:
		return new(big.Rat).SetInt64(int64(x)), false, true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, true, true
		}
		r = new(big.Rat)
		if _, ok := r.SetString(s); !ok {
			return nil, false, false
		}
		return r, false, true
	case bool:
		return nil, false, false
	default:
		return nil, false, false
	}
}

func formatNumber(f float64) string {
	r := new(big.Rat).SetFloat64(f)
	if r == nil {
		return fmt.Sprintf("%g", f)
	}
	return r.FloatString(8)
}

// CloneRow deep-copies a row map.
func CloneRow(row analyst.DatasetRow) analyst.DatasetRow {
	if row == nil {
		return analyst.DatasetRow{}
	}
	out := make(analyst.DatasetRow, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

// CloneFields copies the field slice.
func CloneFields(fs []analyst.DatasetDatasetField) []analyst.DatasetDatasetField {
	out := make([]analyst.DatasetDatasetField, len(fs))
	copy(out, fs)
	return out
}

// UnitOf returns the unit pointer string or "".
func UnitOf(f analyst.DatasetDatasetField) string {
	if f.Unit == nil {
		return ""
	}
	return *f.Unit
}

// CurrencyOf returns the currency or "".
func CurrencyOf(f analyst.DatasetDatasetField) string {
	if f.Currency == nil {
		return ""
	}
	return *f.Currency
}

// SameMeasure reports whether two fields share unit and currency (both empty is fine).
func SameMeasure(a, b analyst.DatasetDatasetField) bool {
	return UnitOf(a) == UnitOf(b) && CurrencyOf(a) == CurrencyOf(b)
}

// Ptr helpers.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func FieldNamePtr(n common.FieldName) *common.FieldName { return &n }
