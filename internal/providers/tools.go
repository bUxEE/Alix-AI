package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// A tool call is a PROPOSAL. The model sees an allowlist of ToolSpec values and
// may answer with candidate ToolCallProposal values; nothing in this package
// can run one. There is no callback, handle or adapter reference in either
// type, and no method that executes. A proposal is untrusted data: the caller
// validates the arguments against the capability schema it owns, then goes
// through the normal prepare / confirm / commit path.

// Limits on tool specs and proposals.
const (
	maxToolSpecs      = 32
	maxToolDescLen    = 1024
	maxToolSchemaSize = 16 << 10
	maxToolArgsSize   = 64 << 10
	maxProposals      = 16
)

var (
	toolNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	callIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// ToolSpec offers one tool to the model. It is data only: a name, a
// description and a JSON Schema for the arguments.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is a JSON Schema document (a JSON object).
	Parameters json.RawMessage `json:"parameters"`
}

// ToolChoice says whether the model may, must or must not propose a call.
type ToolChoice string

const (
	// ToolChoiceAuto (the default) lets the model decide.
	ToolChoiceAuto ToolChoice = ""
	// ToolChoiceNone forbids proposals.
	ToolChoiceNone ToolChoice = "none"
	// ToolChoiceRequired demands a proposal.
	ToolChoiceRequired ToolChoice = "required"
)

// Validate checks the spec's shape.
func (s ToolSpec) Validate() error {
	if !toolNamePattern.MatchString(s.Name) {
		return invalidRequest("tool spec has an invalid name")
	}
	if len(s.Description) > maxToolDescLen {
		return invalidRequest("tool description is too long")
	}
	trimmed := bytes.TrimSpace(s.Parameters)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return invalidRequest("tool parameters are not a JSON object")
	}
	if len(trimmed) > maxToolSchemaSize {
		return invalidRequest("tool parameters schema is too large")
	}
	return nil
}

// ValidateToolSpecs checks an allowlist: bounded, valid and unique by name.
func ValidateToolSpecs(specs []ToolSpec) error {
	if len(specs) > maxToolSpecs {
		return invalidRequest(fmt.Sprintf("more than %d tool specs", maxToolSpecs))
	}
	seen := make(map[string]bool, len(specs))
	for i, s := range specs {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("tool spec %d: %w", i, err)
		}
		if seen[s.Name] {
			return invalidRequest(fmt.Sprintf("duplicate tool name %q", s.Name))
		}
		seen[s.Name] = true
	}
	return nil
}

// ToolCallProposal is a candidate structured call returned by a model. It is
// never an executed call.
type ToolCallProposal struct {
	// ID correlates the proposal with the tool-result message that answers it.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is a JSON object. It is syntactically valid JSON, NOT
	// validated against the tool's schema.
	Arguments json.RawMessage `json:"arguments"`
}

// Clone returns a deep copy.
func (c ToolCallProposal) Clone() ToolCallProposal {
	c.Arguments = append(json.RawMessage(nil), c.Arguments...)
	return c
}

func schemaInvalid(msg string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "providers: "+msg)
}

// structural checks a proposal without an allowlist. Empty arguments are
// normalised to {}.
func (c ToolCallProposal) structural() (ToolCallProposal, error) {
	if !callIDPattern.MatchString(c.ID) {
		return c, schemaInvalid("tool call proposal has an invalid id")
	}
	if !toolNamePattern.MatchString(c.Name) {
		return c, schemaInvalid("tool call proposal has an invalid tool name")
	}
	args := bytes.TrimSpace(c.Arguments)
	if len(args) == 0 {
		args = []byte("{}")
	}
	if len(args) > maxToolArgsSize {
		return c, schemaInvalid("tool call arguments are too large")
	}
	if args[0] != '{' || !json.Valid(args) {
		return c, schemaInvalid("tool call arguments are not a JSON object")
	}
	c.Arguments = append(json.RawMessage(nil), args...)
	return c, nil
}

// ValidateProposals checks the calls a model returned against the allowlist
// the request offered and the request's tool choice. It fails closed: a call
// to a tool that was not offered, malformed arguments, a duplicate id, or any
// call while tools are off rejects the whole answer (schema_invalid). The
// error never echoes the model's output. Arguments are returned normalised.
func ValidateProposals(calls []ToolCallProposal, allow []ToolSpec, choice ToolChoice) ([]ToolCallProposal, error) {
	if len(calls) == 0 {
		if choice == ToolChoiceRequired {
			return nil, schemaInvalid("tool call required but the model proposed none")
		}
		return nil, nil
	}
	if choice == ToolChoiceNone || len(allow) == 0 {
		return nil, schemaInvalid("model proposed a tool call but no tool was offered")
	}
	if len(calls) > maxProposals {
		return nil, schemaInvalid("model proposed too many tool calls")
	}
	offered := make(map[string]bool, len(allow))
	for _, s := range allow {
		offered[s.Name] = true
	}
	out := make([]ToolCallProposal, 0, len(calls))
	ids := make(map[string]bool, len(calls))
	for i, c := range calls {
		n, err := c.structural()
		if err != nil {
			return nil, annotateIndex(err, i)
		}
		if !offered[n.Name] {
			return nil, annotateIndex(schemaInvalid("model proposed a tool that was not offered"), i)
		}
		if ids[n.ID] {
			return nil, annotateIndex(schemaInvalid("duplicate tool call id"), i)
		}
		ids[n.ID] = true
		out = append(out, n)
	}
	return out, nil
}

func annotateIndex(err error, i int) error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.WithDetails("call_index", i)
	}
	return err
}

// validateMessageCalls checks the proposals echoed in an assistant message of
// a request (structure only: they were validated when first received).
func validateMessageCalls(calls []ToolCallProposal) error {
	if len(calls) > maxProposals {
		return invalidRequest("assistant message carries too many tool calls")
	}
	for _, c := range calls {
		if _, err := c.structural(); err != nil {
			return invalidRequest("assistant message carries a malformed tool call")
		}
	}
	return nil
}

func toolSpecText(s ToolSpec) string {
	return strings.Join([]string{s.Name, s.Description, string(s.Parameters)}, " ")
}
