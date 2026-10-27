package verify

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// Reference tokens (ADR 0049) let the model name a withheld dataset cell in
// text_plan without ever seeing its value. Syntax (cannot collide with Markdown
// emphasis or user prose that uses single braces):
//
//	{{ref:<dataset_id>:<row>:<field>}}
//
// dataset_id is a core-minted id (ds_…); row is the 0-based row index the model
// can see from non-withheld columns; field is a dataset field name. Tokens are
// prose-only (not titles, not block plans). Core validates them, then resolves
// after verification for the client; model history keeps the token form.
const (
	RefTokenPrefix = "{{ref:"
	RefTokenSuffix = "}}"
	refPlaceholder = "⟦ref⟧" // claim-extraction stand-in; never a number or id
)

// RefTokenSyntaxExample is the documented example for prompts and schemas.
const RefTokenSyntaxExample = "{{ref:ds_example:0:attendee_name}}"

var (
	refTokenRE = regexp.MustCompile(`\{\{ref:(ds_[a-z0-9][a-z0-9_]{2,60}):(\d{1,6}):([a-z][a-z0-9_]{0,63})\}\}`)
	refOpenRE  = regexp.MustCompile(`\{\{ref:`)
)

// RefToken is one parsed {{ref:…}} occurrence.
type RefToken struct {
	Raw       string
	DatasetID analyst.DatasetID
	Row       int
	Field     string
	Start     int
	End       int
}

// ParseRefTokens returns well-formed tokens in order. Malformed {{ref:…}} forms
// are not returned; call ValidateRefTokens / CheckAnalystRefs for those.
func ParseRefTokens(text string) []RefToken {
	matches := refTokenRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]RefToken, 0, len(matches))
	for _, m := range matches {
		row, _ := strconv.Atoi(text[m[4]:m[5]])
		out = append(out, RefToken{
			Raw:       text[m[0]:m[1]],
			DatasetID: analyst.DatasetID(text[m[2]:m[3]]),
			Row:       row,
			Field:     text[m[6]:m[7]],
			Start:     m[0],
			End:       m[1],
		})
	}
	return out
}

// HasIncompleteRefToken reports a {{ref: with no closing }} (streaming hold).
func HasIncompleteRefToken(text string) bool {
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], RefTokenPrefix)
		if j < 0 {
			return false
		}
		j += i
		rest := text[j+len(RefTokenPrefix):]
		k := strings.Index(rest, RefTokenSuffix)
		if k < 0 {
			return true
		}
		i = j + len(RefTokenPrefix) + k + len(RefTokenSuffix)
	}
	return false
}

// MaskRefTokens replaces well-formed tokens so claim extraction ignores them.
func MaskRefTokens(text string) string {
	return refTokenRE.ReplaceAllString(text, refPlaceholder)
}

// CheckAnalystRefs validates every {{ref:…}} in text against the turn fact set.
// Malformed or unknown tokens become ViolationUnknownIdentifier (fingerprint only).
func CheckAnalystRefs(text string, facts AnalystFactSet) Result {
	if errTok := firstMalformedRef(text); errTok != "" {
		return Result{Violations: []Violation{{
			Kind: ViolationUnknownIdentifier, Fingerprint: fingerprint(errTok),
		}}}
	}
	tokens := ParseRefTokens(text)
	if len(tokens) == 0 {
		return Result{OK: true}
	}
	byID := map[analyst.DatasetID]*analyst.Dataset{}
	for i := range facts.Datasets {
		d := &facts.Datasets[i]
		byID[d.ID] = d
	}
	for _, tok := range tokens {
		ds, ok := byID[tok.DatasetID]
		if !ok || ds == nil {
			return Result{Violations: []Violation{{
				Kind: ViolationUnknownIdentifier, Fingerprint: fingerprint(tok.Raw),
			}}}
		}
		if tok.Row < 0 || tok.Row >= len(ds.Rows) {
			return Result{Violations: []Violation{{
				Kind: ViolationUnknownIdentifier, Fingerprint: fingerprint(tok.Raw),
			}}}
		}
		fieldOK := false
		for _, f := range ds.Fields {
			if string(f.Name) == tok.Field {
				fieldOK = true
				break
			}
		}
		if !fieldOK {
			return Result{Violations: []Violation{{
				Kind: ViolationUnknownIdentifier, Field: tok.Field, Fingerprint: fingerprint(tok.Raw),
			}}}
		}
		if _, ok := ds.Rows[tok.Row][tok.Field]; !ok {
			return Result{Violations: []Violation{{
				Kind: ViolationUnknownIdentifier, Field: tok.Field, Fingerprint: fingerprint(tok.Raw),
			}}}
		}
	}
	return Result{OK: true}
}

// CheckBlockPlansForRefs refuses tokens in titles/descriptions (prose-only rule).
func CheckBlockPlansForRefs(plans []analyst.StepProposalBlockPlan) Result {
	for _, p := range plans {
		if p.Title != nil && (refOpenRE.MatchString(*p.Title) || len(ParseRefTokens(*p.Title)) > 0) {
			return Result{Violations: []Violation{{Kind: ViolationUnknownIdentifier, Got: "title"}}}
		}
		if p.Description != nil && (refOpenRE.MatchString(*p.Description) || len(ParseRefTokens(*p.Description)) > 0) {
			return Result{Violations: []Violation{{Kind: ViolationUnknownIdentifier, Got: "description"}}}
		}
	}
	return Result{OK: true}
}

// ResolveAnalystRefs replaces validated tokens with language-formatted cell values.
// Call only after CheckAnalystRefs succeeded. Resolved values are app-sourced.
func ResolveAnalystRefs(text string, facts AnalystFactSet, lang Language) (string, error) {
	if errTok := firstMalformedRef(text); errTok != "" {
		return "", fmt.Errorf("malformed ref token")
	}
	tokens := ParseRefTokens(text)
	if len(tokens) == 0 {
		return text, nil
	}
	byID := map[analyst.DatasetID]*analyst.Dataset{}
	for i := range facts.Datasets {
		d := &facts.Datasets[i]
		byID[d.ID] = d
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, tok := range tokens {
		b.WriteString(text[prev:tok.Start])
		ds := byID[tok.DatasetID]
		if ds == nil || tok.Row < 0 || tok.Row >= len(ds.Rows) {
			return "", fmt.Errorf("unknown ref")
		}
		var field *analyst.DatasetDatasetField
		for i := range ds.Fields {
			if string(ds.Fields[i].Name) == tok.Field {
				field = &ds.Fields[i]
				break
			}
		}
		if field == nil {
			return "", fmt.Errorf("unknown field")
		}
		cell, ok := ds.Rows[tok.Row][tok.Field]
		if !ok {
			return "", fmt.Errorf("missing cell")
		}
		b.WriteString(FormatRefCell(cell, *field, lang))
		prev = tok.End
	}
	b.WriteString(text[prev:])
	return b.String(), nil
}

// FormatRefCell formats a dataset cell for insertion into analyst prose.
func FormatRefCell(cell analyst.DatasetCell, field analyst.DatasetDatasetField, lang Language) string {
	if cell == nil {
		return ""
	}
	switch field.Type {
	case analyst.FieldTypeBoolean:
		return formatRefBool(cell, lang)
	case analyst.FieldTypeNumber:
		return formatRefNumber(cell, field, lang)
	case analyst.FieldTypeDate:
		return formatRefDate(cell, lang)
	case analyst.FieldTypeDatetime:
		return formatRefDatetime(cell, lang)
	default:
		return strings.TrimSpace(fmt.Sprint(cellAsString(cell)))
	}
}

func formatRefBool(cell analyst.DatasetCell, lang Language) string {
	m := messagesFor(lang)
	switch v := cell.(type) {
	case bool:
		if v {
			return m.yes
		}
		return m.no
	case string:
		low := strings.ToLower(strings.TrimSpace(v))
		if low == "true" || low == "1" || low == "yes" {
			return m.yes
		}
		if low == "false" || low == "0" || low == "no" {
			return m.no
		}
	}
	return cellAsString(cell)
}

func formatRefNumber(cell analyst.DatasetCell, field analyst.DatasetDatasetField, lang Language) string {
	canon := canonFromAny(cell)
	if canon == "" {
		return cellAsString(cell)
	}
	s := localizeDecimal(canon, lang)
	if field.Currency != nil && *field.Currency != "" {
		return s + " " + *field.Currency
	}
	return s
}

func formatRefDate(cell analyst.DatasetCell, lang Language) string {
	s := strings.TrimSpace(cellAsString(cell))
	if t, ok := parseRefTime(s); ok {
		m := messagesFor(lang)
		return formatDate(m, atom{n1: t.Year(), n2: int(t.Month()), n3: t.Day()})
	}
	return s
}

func formatRefDatetime(cell analyst.DatasetCell, lang Language) string {
	s := strings.TrimSpace(cellAsString(cell))
	if t, ok := parseRefTime(s); ok {
		m := messagesFor(lang)
		d := formatDate(m, atom{n1: t.Year(), n2: int(t.Month()), n3: t.Day()})
		return d + ", " + pad(t.Hour(), 2) + ":" + pad(t.Minute(), 2)
	}
	return s
}

func parseRefTime(s string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func cellAsString(v analyst.DatasetCell) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

func firstMalformedRef(text string) string {
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], RefTokenPrefix)
		if j < 0 {
			return ""
		}
		j += i
		rest := text[j+len(RefTokenPrefix):]
		k := strings.Index(rest, RefTokenSuffix)
		if k < 0 {
			return text[j:]
		}
		raw := text[j : j+len(RefTokenPrefix)+k+len(RefTokenSuffix)]
		if !refTokenRE.MatchString(raw) {
			return raw
		}
		i = j + len(raw)
	}
	return ""
}
