package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Quality case kinds for Italian meeting / governance / company-data hosts.
const (
	KindSummarise   = "summarise"
	KindExtract     = "extract"
	KindFacts       = "facts"
	KindDomain      = "domain"
	KindJSONTask    = "json_task_proposal"
	KindJSONAnalyst = "json_analyst_step"
	KindVerify      = "verify"
)

// QualityCase is one Italian quality evaluation item with a reference answer.
type QualityCase struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Language  string           `json:"language"` // it
	Messages  []map[string]any `json:"messages"`
	MaxTokens int              `json:"max_tokens"`
	Reference QualityRef       `json:"reference"`
}

// QualityRef holds deterministic scoring references (not latency figures).
type QualityRef struct {
	// MustContain substrings that should appear in free-text answers.
	MustContain []string `json:"must_contain,omitempty"`
	// MustNumbers digit strings that must appear (Italian or plain forms).
	MustNumbers []string `json:"must_numbers,omitempty"`
	// AllowedNumbers is the closed set of numbers the answer may state.
	// Empty means only MustNumbers are required; any extra digits fail accuracy.
	AllowedNumbers []string `json:"allowed_numbers,omitempty"`
	// DomainLabel is "in" or "out" for domain classification cases.
	DomainLabel string `json:"domain_label,omitempty"`
	// JSONSchema selects task_proposal or analyst_step for constrained JSON cases.
	JSONSchema string `json:"json_schema,omitempty"`
	// ExpectedIntent / ExpectedKind optional soft checks on parsed JSON.
	ExpectedIntent string `json:"expected_intent,omitempty"`
	ExpectedKind   string `json:"expected_kind,omitempty"`
	// Facts drives VerifyAnalyst for kind=verify (derived facts only; simplified).
	Facts *VerifyFacts `json:"facts,omitempty"`
}

// VerifyFacts is a compact corpus encoding of verify.AnalystFactSet inputs.
type VerifyFacts struct {
	Derived []VerifyDerived `json:"derived"`
}

// VerifyDerived is one derived numeric/text fact for verifier scoring.
type VerifyDerived struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Display  string  `json:"display,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// LoadQualityCorpus reads every *.json case under dir (non-recursive).
func LoadQualityCorpus(dir string) ([]QualityCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []QualityCase
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var c QualityCase
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if c.ID == "" {
			c.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		if c.Language == "" {
			c.Language = "it"
		}
		if c.MaxTokens <= 0 {
			c.MaxTokens = 400
		}
		switch c.Kind {
		case KindJSONTask:
			c.Reference.JSONSchema = "task_proposal"
		case KindJSONAnalyst:
			c.Reference.JSONSchema = "analyst_step"
		}
		if err := validateQualityCase(c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no quality cases in %s", dir)
	}
	return out, nil
}

func validateQualityCase(c QualityCase) error {
	switch c.Kind {
	case KindSummarise, KindExtract, KindFacts, KindDomain, KindJSONTask, KindJSONAnalyst, KindVerify:
	default:
		return fmt.Errorf("unknown kind %q", c.Kind)
	}
	if len(c.Messages) == 0 {
		return fmt.Errorf("messages required")
	}
	switch c.Kind {
	case KindJSONTask:
		if c.Reference.JSONSchema != "" && c.Reference.JSONSchema != "task_proposal" {
			return fmt.Errorf("json_schema must be task_proposal")
		}
	case KindJSONAnalyst:
		if c.Reference.JSONSchema != "" && c.Reference.JSONSchema != "analyst_step" {
			return fmt.Errorf("json_schema must be analyst_step")
		}
	case KindDomain:
		if c.Reference.DomainLabel != "in" && c.Reference.DomainLabel != "out" {
			return fmt.Errorf("domain_label must be in|out")
		}
	case KindVerify:
		if c.Reference.Facts == nil || len(c.Reference.Facts.Derived) == 0 {
			return fmt.Errorf("verify cases need reference.facts.derived")
		}
	}
	return nil
}
