package blocks

import (
	"errors"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Rule names the check a block failed. The names are stable: they are what a
// log line, an audit finding and a test assert on. A rule never carries text
// of the block, which is untrusted and may repeat a secret.
type Rule string

// The rules. A "schema" violation is the published JSON Schema (the location
// says where); every other rule is a check only the core can make.
const (
	RuleSchema           Rule = "schema"
	RuleNotJSON          Rule = "not_json"
	RuleNoVariant        Rule = "no_variant"
	RuleBlockTooLarge    Rule = "block_too_large"
	RuleBadEncoding      Rule = "bad_encoding"
	RuleControlChar      Rule = "control_char"
	RuleInvisibleChar    Rule = "invisible_char"
	RuleMarkdownTooLarge Rule = "markdown.too_large"
	RuleMarkdownTooDeep  Rule = "markdown.too_deep"
	RuleMarkdownEmpty    Rule = "markdown.empty"
	RuleMarkdownUnstable Rule = "markdown.unstable"
	RuleTableShape       Rule = "table.shape"
	RuleTableBounds      Rule = "table.bounds"
	RuleTableKeys        Rule = "table.duplicate_key"
	RuleTableKind        Rule = "table.cell_kind"
	RuleMarkup           Rule = "markup"
	RuleScheme           Rule = "scheme"
	RuleExpression       Rule = "expression"
	RuleChartBounds      Rule = "chart.bounds"
	RuleChartField       Rule = "chart.field_missing"
	RuleChartHidden      Rule = "chart.field_not_encoded"
	RuleChartType        Rule = "chart.channel_type"
	RuleChartShape       Rule = "chart.shape"
	RuleChartNumber      Rule = "chart.number_not_in_facts"
	RuleChartNumeric     Rule = "chart.not_a_number"
	RuleDiagramType      Rule = "diagram.unknown_type"
	RuleDiagramStatement Rule = "diagram.forbidden_statement"
	RuleDiagramBounds    Rule = "diagram.bounds"
	// RuleNotModelKind refuses, in the JSON a model wrote (ModelJSON), a block
	// of a kind only the core assembles from host and core data.
	RuleNotModelKind Rule = "not_model_kind"
	// RuleModelDatasetID refuses a model-authored block that carries dataset_id
	// (alixai authority): trust comes from who built the block, not a writable member.
	RuleModelDatasetID Rule = "model.dataset_id"
)

// Violation is one reason a block was refused: the rule and the JSON location
// of the offending member (for example "/data/3/booked").
type Violation struct {
	Rule Rule   `json:"rule"`
	Path string `json:"path,omitempty"`
}

func (v Violation) String() string {
	if v.Path == "" {
		return string(v.Rule)
	}
	return string(v.Rule) + " at " + v.Path
}

// Rejection is the cause of the error a refused block returns. The error
// itself is a contracts.Error with code schema_invalid, so a caller that only
// looks at the code fails closed; a caller that wants the reasons uses
// Violations.
type Rejection struct {
	// Block is the block type that was refused, or "" when it could not be read.
	Block      string
	Violations []Violation
}

// maxReportedViolations bounds how many violations one rejection lists.
const maxReportedViolations = 8

func (r *Rejection) Error() string {
	parts := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		parts = append(parts, v.String())
	}
	block := r.Block
	if block == "" {
		block = "block"
	}
	return fmt.Sprintf("rich block %q refused: %s", block, strings.Join(parts, "; "))
}

// reject builds the error for a refused block.
func reject(block string, vs ...Violation) error {
	if len(vs) > maxReportedViolations {
		vs = vs[:maxReportedViolations]
	}
	rej := &Rejection{Block: block, Violations: append([]Violation(nil), vs...)}
	e := contracts.Wrap(contracts.ErrSchemaInvalid, rej, "the rich block was refused")
	list := make([]map[string]string, 0, len(vs))
	for _, v := range vs {
		list = append(list, map[string]string{"rule": string(v.Rule), "path": v.Path})
	}
	return e.WithDetails("violations", list)
}

// Violations returns the reasons a block was refused, or nil when err is not
// a block rejection.
func Violations(err error) []Violation {
	var rej *Rejection
	if errors.As(err, &rej) {
		return append([]Violation(nil), rej.Violations...)
	}
	return nil
}

// HasRule reports whether err is a block rejection that includes the rule.
func HasRule(err error, rule Rule) bool {
	for _, v := range Violations(err) {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

// collector gathers violations while a block is checked, up to a bound, so a
// hostile block cannot make the report itself large.
type collector struct {
	vs []Violation
}

func (c *collector) add(rule Rule, path string) {
	if len(c.vs) < maxReportedViolations {
		c.vs = append(c.vs, Violation{Rule: rule, Path: path})
	}
}

func (c *collector) failed() bool { return len(c.vs) > 0 }

func (c *collector) err(block string) error {
	if len(c.vs) == 0 {
		return nil
	}
	return reject(block, c.vs...)
}
