package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var digitRunRE = regexp.MustCompile(`\d+(?:[.,]\d+)*`)

// Italian / English function-word lists for a cheap language-stay check.
var itStops = map[string]bool{
	"il": true, "lo": true, "la": true, "i": true, "gli": true, "le": true,
	"di": true, "a": true, "da": true, "in": true, "con": true, "su": true,
	"per": true, "tra": true, "fra": true, "che": true, "non": true, "è": true,
	"un": true, "una": true, "uno": true, "del": true, "della": true, "dei": true,
	"delle": true, "degli": true, "al": true, "alla": true, "ai": true, "alle": true,
	"nel": true, "nella": true, "nei": true, "nelle": true, "sul": true, "sulla": true,
	"questo": true, "questa": true, "questi": true, "queste": true, "sono": true,
	"come": true, "più": true, "anche": true, "ma": true, "se": true, "o": true,
	"e": true, "si": true, "ci": true, "mi": true, "ti": true, "ha": true, "hanno": true,
	"essere": true, "stato": true, "stata": true,
	"verso": true, "dopo": true, "prima": true, "durante": true, "ogni": true,
	"tutti": true, "tutto": true, "solo": true, "già": true, "ancora": true,
}

var enStops = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "of": true, "to": true, "in": true,
	"for": true, "and": true, "or": true, "with": true, "on": true, "at": true,
	"by": true, "from": true, "this": true, "that": true, "these": true, "those": true,
	"it": true, "as": true, "but": true, "not": true, "have": true, "has": true,
	"had": true, "will": true, "would": true, "can": true, "could": true, "should": true,
	"we": true, "you": true, "they": true, "their": true, "our": true, "your": true,
}

// CaseScore is per-case automatic scoring (no human).
type CaseScore struct {
	CaseID           string   `json:"case_id"`
	Kind             string   `json:"kind"`
	OK               bool     `json:"ok"`
	SchemaValid      *bool    `json:"schema_valid,omitempty"`
	VerifierPassed   *bool    `json:"verifier_passed,omitempty"`
	VerifierReplaced *bool    `json:"verifier_replaced,omitempty"`
	FactAccuracy     *float64 `json:"fact_accuracy,omitempty"`
	ItalianOK        *bool    `json:"italian_ok,omitempty"`
	ItalianRatio     *float64 `json:"italian_stop_ratio,omitempty"`
	JudgeScore       *float64 `json:"judge_score,omitempty"`
	Problems         []string `json:"problems,omitempty"`
	OutputPreview    string   `json:"output_preview,omitempty"`
	Err              string   `json:"error,omitempty"`
}

func scoreCase(c QualityCase, text string, schemas *QualitySchemas, judge *float64) CaseScore {
	s := CaseScore{CaseID: c.ID, Kind: c.Kind, OutputPreview: truncate(text, 240)}
	if text == "" {
		s.Problems = append(s.Problems, "empty output")
		return s
	}

	switch c.Kind {
	case KindJSONTask:
		ok, probs := scoreJSON(text, schemas.TaskProposal, c.Reference.ExpectedIntent, "")
		s.SchemaValid = boolPtr(ok)
		s.Problems = append(s.Problems, probs...)
		s.OK = ok
	case KindJSONAnalyst:
		ok, probs := scoreJSON(text, schemas.AnalystStep, "", c.Reference.ExpectedKind)
		s.SchemaValid = boolPtr(ok)
		s.Problems = append(s.Problems, probs...)
		s.OK = ok
	case KindVerify:
		passed, replaced, probs := scoreVerify(text, c.Reference.Facts)
		s.VerifierPassed = boolPtr(passed)
		s.VerifierReplaced = boolPtr(replaced)
		s.Problems = append(s.Problems, probs...)
		itOK, ratio := italianCheck(text)
		s.ItalianOK = boolPtr(itOK)
		s.ItalianRatio = floatPtr(ratio)
		if !itOK {
			s.Problems = append(s.Problems, "italian language check failed")
		}
		s.OK = passed && itOK
	default:
		acc, probs := factAccuracy(text, c.Reference)
		s.FactAccuracy = floatPtr(acc)
		s.Problems = append(s.Problems, probs...)
		itOK, ratio := italianCheck(text)
		s.ItalianOK = boolPtr(itOK)
		s.ItalianRatio = floatPtr(ratio)
		if !itOK {
			s.Problems = append(s.Problems, "italian language check failed")
		}
		domainOK := true
		if c.Kind == KindDomain {
			domainOK = domainMatch(text, c.Reference.DomainLabel)
			if !domainOK {
				s.Problems = append(s.Problems, "domain label mismatch")
			}
		}
		s.OK = acc >= 1.0 && itOK && domainOK && len(probs) == 0
		if acc < 1.0 && len(probs) == 0 {
			s.Problems = append(s.Problems, fmt.Sprintf("fact_accuracy=%.2f", acc))
		}
	}

	if judge != nil {
		s.JudgeScore = judge
	}
	return s
}

func scoreJSON(text string, sch *jsonschema.Schema, wantIntent, wantKind string) (bool, []string) {
	var probs []string
	v, err := parseSingleJSON(text)
	if err != nil {
		return false, []string{err.Error()}
	}
	if sch == nil {
		return false, []string{"schema not loaded"}
	}
	if err := sch.Validate(v); err != nil {
		return false, []string{"schema: " + firstSchemaProblem(err)}
	}
	obj, _ := v.(map[string]any)
	if wantIntent != "" {
		if got, _ := obj["intent"].(string); got != wantIntent {
			probs = append(probs, fmt.Sprintf("intent=%q want %q", got, wantIntent))
		}
	}
	if wantKind != "" {
		if got, _ := obj["kind"].(string); got != wantKind {
			probs = append(probs, fmt.Sprintf("kind=%q want %q", got, wantKind))
		}
	}
	return len(probs) == 0, probs
}

func scoreVerify(text string, facts *VerifyFacts) (passed, replaced bool, probs []string) {
	if facts == nil {
		return false, false, []string{"no facts"}
	}
	set := toAnalystFactSet(facts)
	out, err := verify.VerifyAnalyst(verify.AnalystAnswer{Text: text}, set, verify.AnalystOptions{Language: verify.LangIT})
	if err != nil {
		return false, false, []string{err.Error()}
	}
	passed = out.Source == verify.SourceModel
	replaced = out.Source == verify.SourceTemplate
	if !passed {
		for _, v := range out.Violations {
			probs = append(probs, string(v.Kind))
		}
		if len(probs) == 0 {
			probs = append(probs, "verifier replaced answer")
		}
	}
	return passed, replaced, probs
}

func toAnalystFactSet(f *VerifyFacts) verify.AnalystFactSet {
	sl := "computed"
	var derived []analyst.DerivedFact
	for _, d := range f.Derived {
		df := analyst.DerivedFact{
			ID: d.ID, Label: d.Label, Value: d.Value,
			DataClass: analyst.DataClassInternal, SourceLabel: &sl,
			Provenance: analyst.DerivedFactDerivedProvenance{
				Op: "aggregate", InputDatasetIDs: []analyst.DatasetID{"ds_llmbench_01"},
				Params:   map[string]any{"agg": "sum"},
				Rounding: analyst.DerivedFactRounding{Mode: analyst.RoundingModeHalfUp, Decimals: 0},
			},
		}
		if d.Display != "" {
			disp := d.Display
			df.Display = &disp
		}
		if d.Currency != "" {
			cur := d.Currency
			df.Currency = &cur
		}
		derived = append(derived, df)
	}
	return verify.AnalystFactSet{Derived: derived}
}

func factAccuracy(text string, ref QualityRef) (float64, []string) {
	var probs []string
	low := strings.ToLower(text)
	for _, m := range ref.MustContain {
		if m != "" && !strings.Contains(low, strings.ToLower(m)) {
			probs = append(probs, "missing phrase: "+m)
		}
	}
	found := extractNumberTokens(text)
	must := normalizeNumberSet(ref.MustNumbers)
	allowed := normalizeNumberSet(ref.AllowedNumbers)
	if len(allowed) == 0 {
		allowed = must
	}
	hit := 0
	for n := range must {
		if found[n] {
			hit++
		} else {
			probs = append(probs, "missing number: "+n)
		}
	}
	for n := range found {
		if len(allowed) > 0 && !allowed[n] {
			probs = append(probs, "unexpected number: "+n)
		}
	}
	if len(must) == 0 && len(ref.MustContain) == 0 {
		if len(probs) == 0 {
			return 1, nil
		}
		return 0, probs
	}
	den := len(must)
	if den == 0 {
		if len(probs) == 0 {
			return 1, nil
		}
		return 0, probs
	}
	acc := float64(hit) / float64(den)
	if len(probs) > 0 && hit == den {
		// unexpected numbers pull accuracy down
		acc = acc * 0.5
	}
	return acc, probs
}

func domainMatch(text, label string) bool {
	low := strings.ToLower(text)
	switch label {
	case "in":
		return strings.Contains(low, "in ambito") || strings.Contains(low, "nel dominio") ||
			strings.Contains(low, "pertinente") || strings.Contains(low, `"in"`) ||
			strings.Contains(low, "in_scope") || strings.Contains(low, "in scope")
	case "out":
		return strings.Contains(low, "fuori ambito") || strings.Contains(low, "fuori dominio") ||
			strings.Contains(low, "non pertinente") || strings.Contains(low, "out_of_scope") ||
			strings.Contains(low, `"out"`) || strings.Contains(low, "out of scope")
	}
	return false
}

func italianCheck(text string) (bool, float64) {
	words := tokenizeWords(text)
	if len(words) == 0 {
		return false, 0
	}
	var itN, enN int
	for _, w := range words {
		if itStops[w] {
			itN++
		}
		if enStops[w] {
			enN++
		}
	}
	den := itN + enN
	if den == 0 {
		// No stop words matched: treat as pass if Latin letters dominate and English stops absent.
		return true, 1
	}
	ratio := float64(itN) / float64(den)
	// Fail on language switch: English stop words dominate.
	ok := ratio >= 0.55 && float64(enN) <= float64(itN)*1.2
	return ok, ratio
}

func tokenizeWords(s string) []string {
	var b strings.Builder
	var out []string
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, strings.ToLower(b.String()))
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || r == '\'' || r == '’' {
			b.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return out
}

func extractNumberTokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range digitRunRE.FindAllString(text, -1) {
		if n := normalizeNumber(m); n != "" {
			out[n] = true
		}
	}
	return out
}

func normalizeNumberSet(nums []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range nums {
		if x := normalizeNumber(n); x != "" {
			out[x] = true
		}
	}
	return out
}

// normalizeNumber strips grouping so "45.000" and "45000" and "45,000" align when unambiguous.
func normalizeNumber(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Italian thousands: 12.400 → 12400; decimal: 12,5 → 12.5
	if strings.Contains(s, ".") && strings.Contains(s, ",") {
		s = strings.ReplaceAll(s, ".", "")
		s = strings.ReplaceAll(s, ",", ".")
		return s
	}
	if strings.Count(s, ".") == 1 {
		parts := strings.Split(s, ".")
		if len(parts[1]) == 3 && len(parts[0]) <= 3 {
			// likely thousands grouping
			return parts[0] + parts[1]
		}
	}
	if strings.Count(s, ",") == 1 {
		parts := strings.Split(s, ",")
		if len(parts[1]) == 3 && !strings.Contains(parts[0], ".") {
			return parts[0] + parts[1]
		}
		return parts[0] + "." + parts[1]
	}
	return strings.ReplaceAll(s, " ", "")
}

func parseSingleJSON(text string) (any, error) {
	trimmed := strings.TrimSpace(text)
	// Allow a single markdown fence around JSON.
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```JSON")
		trimmed = strings.TrimPrefix(trimmed, "```")
		if i := strings.LastIndex(trimmed, "```"); i >= 0 {
			trimmed = trimmed[:i]
		}
		trimmed = strings.TrimSpace(trimmed)
	}
	if trimmed == "" {
		return nil, errors.New("output is empty")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("output is not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("output is not exactly one JSON document")
	}
	return v, nil
}

func firstSchemaProblem(err error) string {
	best, bestDepth := "", -1
	for _, line := range strings.Split(err.Error(), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		if depth := len(line) - len(trimmed); depth > bestDepth {
			best, bestDepth = strings.TrimPrefix(trimmed, "- "), depth
		}
	}
	if best == "" {
		best = strings.SplitN(err.Error(), "\n", 2)[0]
	}
	if len(best) > 300 {
		best = best[:300] + "…"
	}
	return best
}

func boolPtr(v bool) *bool        { return &v }
func floatPtr(v float64) *float64 { return &v }
