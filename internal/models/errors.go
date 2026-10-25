package models

import (
	"errors"
	"fmt"
	"strings"
)

// Rule names the check a Violation failed. Tests and callers match on the rule,
// never on message text.
type Rule string

const (
	// RuleYAML: the file is not one well-formed YAML document of the plain
	// subset the registry allows (no aliases, anchors, duplicate keys, non-string
	// keys or custom tags).
	RuleYAML Rule = "yaml"
	// RuleSchema: the document does not satisfy registry-entry.schema.json
	// (unknown or missing field, wrong type, bad pattern or enum value).
	RuleSchema Rule = "schema"
	// RuleDecode: the document satisfied the schema but not the Go type. This
	// means the embedded schema and the Entry type have drifted.
	RuleDecode Rule = "decode"
	// RuleFile: the file name is not <id>.yaml, the file is too large, or the
	// directory holds a file the registry refuses (for example .yml).
	RuleFile Rule = "file"
	// RuleStatus: the status is not one of the defined lifecycle states.
	RuleStatus Rule = "status"
	// RuleLicence: the licence record is missing or incomplete (SEC-091).
	RuleLicence Rule = "licence"
	// RulePinned: a qualified-emulated or certified entry lacks a pinned
	// artefact (file name, revision or sha256), or a recorded hash is malformed
	// (SEC-063).
	RulePinned Rule = "pinned"
	// RuleEvidence: a qualified-emulated or certified entry lacks its
	// efficiency-review date and evidence path, an evidence path is unsafe, or
	// the file it names does not exist.
	RuleEvidence Rule = "evidence"
	// RuleEmulated: a certified entry whose measurement is marked emulated.
	RuleEmulated Rule = "emulated"
	// RuleDefault: a default_for item names a role the entry does not have,
	// is listed twice, or sits on a rejected or retired entry.
	RuleDefault Rule = "default"
	// RuleDefaultUnique: more than one entry is the default for the same
	// (role, profile).
	RuleDefaultUnique Rule = "default-unique"
	// RuleDuplicateID: two entries share an id.
	RuleDuplicateID Rule = "duplicate-id"
	// RuleFallback: a fallback names an unknown entry, itself, an entry with no
	// shared role, a rejected or retired entry, or closes a cycle.
	RuleFallback Rule = "fallback"
	// RuleEmbedding: an embedding model without its vector dimension.
	RuleEmbedding Rule = "embedding"
	// RuleScorer: a decision scorer lacks its scorer block, a bi-encoder has no
	// frozen-encoder dependency, a decision-encoder is a default, or a scorer is
	// the default for a profile it has no evidence or room for.
	RuleScorer Rule = "scorer"
	// RuleDependency: a dependency names an unknown, unpinned, rejected, retired
	// or wrong-role entry, itself, or the relation does not fit the dependent.
	RuleDependency Rule = "dependency"
	// RuleEmpty: the registry directory holds no entries.
	RuleEmpty Rule = "empty"
)

// Violation is one failed check.
type Violation struct {
	File   string // source file name; empty for a programmatic Entry
	Entry  string // entry id when known
	Rule   Rule
	Field  string // JSON pointer or dotted field the rule is about; may be empty
	Detail string
}

// Error renders "file: entry: [rule] field: detail", skipping empty parts.
func (v Violation) Error() string {
	var parts []string
	if v.File != "" {
		parts = append(parts, v.File)
	}
	if v.Entry != "" && v.Entry != v.File {
		parts = append(parts, v.Entry)
	}
	head := strings.Join(parts, ": ")
	body := "[" + string(v.Rule) + "]"
	if v.Field != "" {
		body += " " + v.Field
	}
	body += ": " + v.Detail
	if head == "" {
		return body
	}
	return head + ": " + body
}

// ErrInvalidRegistry is wrapped by every LoadError, so callers can test
// errors.Is(err, ErrInvalidRegistry) without importing the type.
var ErrInvalidRegistry = errors.New("models: invalid registry")

// LoadError reports every violation found, not just the first, so one run
// shows an operator everything to fix. A registry that produces a LoadError is
// never partially loaded.
type LoadError struct {
	Violations []Violation
}

// Error lists the violations, one per line.
func (e *LoadError) Error() string {
	if e == nil || len(e.Violations) == 0 {
		return ErrInvalidRegistry.Error()
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %d violation(s)", ErrInvalidRegistry.Error(), len(e.Violations))
	for _, v := range e.Violations {
		sb.WriteString("\n  ")
		sb.WriteString(v.Error())
	}
	return sb.String()
}

// Is makes errors.Is(err, ErrInvalidRegistry) true.
func (e *LoadError) Is(target error) bool { return target == ErrInvalidRegistry }

// Has reports whether any violation carries the rule.
func (e *LoadError) Has(rule Rule) bool {
	if e == nil {
		return false
	}
	for _, v := range e.Violations {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

// Count returns how many violations carry the rule.
func (e *LoadError) Count(rule Rule) int {
	if e == nil {
		return 0
	}
	n := 0
	for _, v := range e.Violations {
		if v.Rule == rule {
			n++
		}
	}
	return n
}

// newLoadError returns nil for no violations, so callers can return it directly.
func newLoadError(vs []Violation) error {
	if len(vs) == 0 {
		return nil
	}
	return &LoadError{Violations: vs}
}

// Verification sentinels. models.Verify wraps one of these in a
// contracts.Error with code unavailable, so the caller reports the model as
// unavailable and errors.Is can still tell the reasons apart.
var (
	// ErrInvalidEntry: the entry itself breaks a registry rule, so it cannot be
	// trusted as the reference for a hash.
	ErrInvalidEntry = errors.New("models: entry is invalid")
	// ErrNotActivatable: the entry is rejected or retired.
	ErrNotActivatable = errors.New("models: entry status does not allow activation")
	// ErrNotPinned: the entry has no sha256 to compare against. There is no
	// "unverified but allowed": fail closed.
	ErrNotPinned = errors.New("models: entry has no pinned sha256")
	// ErrArtefactUnreadable: the file is missing, not a regular file, or could
	// not be read in full.
	ErrArtefactUnreadable = errors.New("models: artefact cannot be read")
	// ErrSizeMismatch: the file's size differs from the registry's.
	ErrSizeMismatch = errors.New("models: artefact size does not match the registry")
	// ErrHashMismatch: the file's sha256 differs from the registry's.
	ErrHashMismatch = errors.New("models: artefact sha256 does not match the registry")
	// ErrRuntimeNotAllowed: the runtime, backend or version is not on the
	// allowlist for this entry.
	ErrRuntimeNotAllowed = errors.New("models: runtime or backend version is not allowed for this entry")
)
