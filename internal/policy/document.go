package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// FormatPolicyV1 is the value of the optional format key of a host policy.
const FormatPolicyV1 = "policy/v1"

// MaxPolicyBytes bounds a policy document. A document larger than this is
// refused before it is parsed.
const MaxPolicyBytes = 1 << 20

// maxJSONDepth bounds the nesting the strict reader accepts. The deepest valid
// document nests rules[].when.fields.<name>.in[], seven levels from the root.
const maxJSONDepth = 12

// Limits mirrored from schemas/policy/host-policy.schema.json.
const (
	maxGrants       = 256
	maxOverrides    = 256
	maxProjections  = 256
	maxRules        = 256
	maxListItems    = 64
	maxRuleCaps     = 64
	maxFieldPreds   = 16
	maxScalarLen    = 256
	maxIdentifierLn = contracts.MaxScopeValueLen
	maxTextLen      = 2000
)

var (
	slugRE       = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	fieldNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	dottedIDRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,3}$`)
	sha256HexLen = 64
)

// HostPolicy is a parsed policy/v1 document: the application-package policy of
// M1-01 plus optional conditional Rules. Its JSON form is
// schemas/policy/host-policy.schema.json.
//
// The type has no field that can grant a capability other than RoleGrants and
// none that can lower a risk level: a policy can only narrow or raise.
type HostPolicy struct {
	Format          string            `json:"format,omitempty"`
	PolicyID        string            `json:"policy_id"`
	Description     string            `json:"description,omitempty"`
	RoleGrants      []RoleGrant       `json:"role_grants"`
	RiskOverrides   []RiskOverride    `json:"risk_overrides"`
	FieldProjection []FieldProjection `json:"field_projection"`
	Rules           []Rule            `json:"rules,omitempty"`
}

// RoleGrant lets one role use the listed capabilities.
type RoleGrant struct {
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
}

// RiskOverride raises the risk level of one capability to at least
// MinimumRiskLevel. Raising to a level at or below the declared one changes
// nothing; there is no way to lower anything.
type RiskOverride struct {
	CapabilityID     string              `json:"capability_id"`
	MinimumRiskLevel contracts.RiskLevel `json:"minimum_risk_level"`
	Reason           string              `json:"reason"`
}

// FieldProjection lists the only fields of a capability the model may see.
type FieldProjection struct {
	CapabilityID       string   `json:"capability_id"`
	Roles              []string `json:"roles,omitempty"`
	ModelVisibleFields []string `json:"model_visible_fields"`
}

// Rule is a conditional narrowing or raising rule.
type Rule struct {
	ID           string    `json:"id"`
	Capabilities []string  `json:"capabilities"`
	When         Condition `json:"when"`
	Then         Effect    `json:"then"`
	Reason       string    `json:"reason"`
}

// Condition is the AND of the keys that are set.
type Condition struct {
	Roles      *SetCondition             `json:"roles,omitempty"`
	Scopes     *SetCondition             `json:"scopes,omitempty"`
	Tenants    *ValueCondition           `json:"tenants,omitempty"`
	Workspaces *ValueCondition           `json:"workspaces,omitempty"`
	Fields     map[string]FieldPredicate `json:"fields,omitempty"`
}

// SetCondition tests a set the context holds several values of.
type SetCondition struct {
	Any  []string `json:"any,omitempty"`
	All  []string `json:"all,omitempty"`
	None []string `json:"none,omitempty"`
}

// ValueCondition tests the single value the context holds.
type ValueCondition struct {
	In    []string `json:"in,omitempty"`
	NotIn []string `json:"not_in,omitempty"`
}

// FieldPredicate is the AND of the operators that are set.
type FieldPredicate struct {
	Eq      *Scalar  `json:"eq,omitempty"`
	Ne      *Scalar  `json:"ne,omitempty"`
	In      []Scalar `json:"in,omitempty"`
	NotIn   []Scalar `json:"not_in,omitempty"`
	Gt      *float64 `json:"gt,omitempty"`
	Gte     *float64 `json:"gte,omitempty"`
	Lt      *float64 `json:"lt,omitempty"`
	Lte     *float64 `json:"lte,omitempty"`
	Present *bool    `json:"present,omitempty"`
}

// Effect is what a rule does: deny, or raise the risk level and hide fields.
type Effect struct {
	Deny       bool                 `json:"deny,omitempty"`
	RiskMin    *contracts.RiskLevel `json:"risk_min,omitempty"`
	HideFields []string             `json:"hide_fields,omitempty"`
}

// ScalarKind is the type of a Scalar.
type ScalarKind uint8

// Scalar kinds. The zero kind is invalid so a zero Scalar never compares equal
// to anything.
const (
	ScalarInvalid ScalarKind = iota
	ScalarString
	ScalarNumber
	ScalarBool
)

// Scalar is a comparison value in a field predicate: a string, a number or a
// boolean. Never null, an array or an object.
type Scalar struct {
	Kind ScalarKind
	Str  string
	Num  float64
	Bool bool
}

// String, Number and Bool build scalars.
func String(s string) Scalar  { return Scalar{Kind: ScalarString, Str: s} }
func Number(n float64) Scalar { return Scalar{Kind: ScalarNumber, Num: n} }
func Bool(b bool) Scalar      { return Scalar{Kind: ScalarBool, Bool: b} }

// MarshalJSON encodes the scalar as its JSON value.
func (s Scalar) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case ScalarString:
		return json.Marshal(s.Str)
	case ScalarNumber:
		if math.IsNaN(s.Num) || math.IsInf(s.Num, 0) {
			return nil, contracts.New(contracts.ErrSchemaInvalid, "policy scalar number is not finite")
		}
		return json.Marshal(s.Num)
	case ScalarBool:
		return json.Marshal(s.Bool)
	}
	return nil, contracts.New(contracts.ErrSchemaInvalid, "policy scalar has no value")
}

// MarshalJSON writes the three required arrays as [] when empty, never null,
// so the canonical form (and its digest) does not depend on nil versus empty.
func (h HostPolicy) MarshalJSON() ([]byte, error) {
	type alias HostPolicy
	a := alias(h)
	if a.RoleGrants == nil {
		a.RoleGrants = []RoleGrant{}
	}
	if a.RiskOverrides == nil {
		a.RiskOverrides = []RiskOverride{}
	}
	if a.FieldProjection == nil {
		a.FieldProjection = []FieldProjection{}
	}
	return json.Marshal(a)
}

// ParseHostPolicy parses and validates one policy/v1 document from its JSON
// form (the YAML in policies/*.yaml is converted to the JSON data model by the
// package loader first). It is strict: any key it does not know, a duplicate
// key, a null, a value of the wrong type, a number with a fraction where an
// integer is required, an out-of-range value or an invalid identifier is an
// error. Every error is a schema_invalid *contracts.Error naming the JSON path.
//
// A document that parses is also valid against host-policy.schema.json, and a
// test keeps that true for the whole example corpus.
func ParseHostPolicy(data []byte) (*HostPolicy, error) {
	if len(data) > MaxPolicyBytes {
		return nil, schemaErr("", "document larger than %d bytes", MaxPolicyBytes)
	}
	tree, err := readStrict(data)
	if err != nil {
		return nil, err
	}
	h, err := decodeHostPolicy(tree)
	if err != nil {
		return nil, err
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h, nil
}

// Validate checks the grammar and cardinality limits of the schema on a
// document that was built in code or parsed. Cross-document checks (duplicate
// roles, overlapping projections, references to the package) happen when the
// policies are compiled into a package.
func (h *HostPolicy) Validate() error {
	if h == nil {
		return schemaErr("", "policy is nil")
	}
	if h.Format != "" && h.Format != FormatPolicyV1 {
		return schemaErr("format", "unsupported policy format; only %q is defined", FormatPolicyV1)
	}
	if !slugRE.MatchString(h.PolicyID) {
		return schemaErr("policy_id", "must be a slug")
	}
	if h.Description != "" && !validText(h.Description) {
		return schemaErr("description", "must be 1 to %d characters without control characters", maxTextLen)
	}
	if n := len(h.RoleGrants); n < 1 || n > maxGrants {
		return schemaErr("role_grants", "must hold 1 to %d grants", maxGrants)
	}
	for i, g := range h.RoleGrants {
		p := fmt.Sprintf("role_grants[%d]", i)
		if !validIdentifier(g.Role) {
			return schemaErr(p+".role", "must be an identifier")
		}
		if err := validCapabilityList(p+".capabilities", g.Capabilities, 1, maxGrants); err != nil {
			return err
		}
	}
	if len(h.RiskOverrides) > maxOverrides {
		return schemaErr("risk_overrides", "must hold at most %d entries", maxOverrides)
	}
	for i, o := range h.RiskOverrides {
		p := fmt.Sprintf("risk_overrides[%d]", i)
		if !validCapabilityID(o.CapabilityID) {
			return schemaErr(p+".capability_id", "must be a capability ID")
		}
		if !o.MinimumRiskLevel.Valid() {
			return schemaErr(p+".minimum_risk_level", "must be 0 to 3")
		}
		if !validText(o.Reason) {
			return schemaErr(p+".reason", "must be 1 to %d characters without control characters", maxTextLen)
		}
	}
	if len(h.FieldProjection) > maxProjections {
		return schemaErr("field_projection", "must hold at most %d entries", maxProjections)
	}
	for i, fp := range h.FieldProjection {
		p := fmt.Sprintf("field_projection[%d]", i)
		if !validCapabilityID(fp.CapabilityID) {
			return schemaErr(p+".capability_id", "must be a capability ID")
		}
		if fp.Roles != nil {
			if err := validIdentifierList(p+".roles", fp.Roles, 1, 64); err != nil {
				return err
			}
		}
		if err := validFieldNameList(p+".model_visible_fields", fp.ModelVisibleFields, 1, 64); err != nil {
			return err
		}
	}
	if len(h.Rules) > maxRules {
		return schemaErr("rules", "must hold at most %d rules", maxRules)
	}
	seen := make(map[string]bool, len(h.Rules))
	for i, r := range h.Rules {
		p := fmt.Sprintf("rules[%d]", i)
		if err := r.validate(p); err != nil {
			return err
		}
		if seen[r.ID] {
			return schemaErr(p+".id", "duplicate rule ID")
		}
		seen[r.ID] = true
	}
	return nil
}

func (r Rule) validate(p string) error {
	if !slugRE.MatchString(r.ID) {
		return schemaErr(p+".id", "must be a slug")
	}
	if err := validCapabilityList(p+".capabilities", r.Capabilities, 1, maxRuleCaps); err != nil {
		return err
	}
	if !validText(r.Reason) {
		return schemaErr(p+".reason", "must be 1 to %d characters without control characters", maxTextLen)
	}
	if err := r.When.validate(p + ".when"); err != nil {
		return err
	}
	return r.Then.validate(p + ".then")
}

func (c Condition) validate(p string) error {
	if c.Roles == nil && c.Scopes == nil && c.Tenants == nil && c.Workspaces == nil && len(c.Fields) == 0 {
		return schemaErr(p, "must state at least one condition; an unconditional rule is a role grant, a risk override or a projection")
	}
	if c.Roles != nil {
		if err := c.Roles.validate(p + ".roles"); err != nil {
			return err
		}
	}
	if c.Scopes != nil {
		if err := c.Scopes.validate(p + ".scopes"); err != nil {
			return err
		}
	}
	if c.Tenants != nil {
		if err := c.Tenants.validate(p + ".tenants"); err != nil {
			return err
		}
	}
	if c.Workspaces != nil {
		if err := c.Workspaces.validate(p + ".workspaces"); err != nil {
			return err
		}
	}
	if c.Fields != nil {
		if len(c.Fields) < 1 || len(c.Fields) > maxFieldPreds {
			return schemaErr(p+".fields", "must hold 1 to %d fields", maxFieldPreds)
		}
		for _, name := range sortedKeys(c.Fields) {
			if !fieldNameRE.MatchString(name) {
				return schemaErr(p+".fields", "field name %q is not lower snake_case", name)
			}
			if err := c.Fields[name].validate(p + ".fields." + name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s SetCondition) validate(p string) error {
	if len(s.Any) == 0 && len(s.All) == 0 && len(s.None) == 0 {
		return schemaErr(p, "must state any, all or none")
	}
	for _, kv := range []struct {
		k string
		v []string
	}{{"any", s.Any}, {"all", s.All}, {"none", s.None}} {
		if kv.v != nil {
			if err := validIdentifierList(p+"."+kv.k, kv.v, 1, maxListItems); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v ValueCondition) validate(p string) error {
	if len(v.In) == 0 && len(v.NotIn) == 0 {
		return schemaErr(p, "must state in or not_in")
	}
	if v.In != nil {
		if err := validIdentifierList(p+".in", v.In, 1, maxListItems); err != nil {
			return err
		}
	}
	if v.NotIn != nil {
		if err := validIdentifierList(p+".not_in", v.NotIn, 1, maxListItems); err != nil {
			return err
		}
	}
	return nil
}

func (f FieldPredicate) validate(p string) error {
	if f.Eq == nil && f.Ne == nil && f.In == nil && f.NotIn == nil &&
		f.Gt == nil && f.Gte == nil && f.Lt == nil && f.Lte == nil && f.Present == nil {
		return schemaErr(p, "must state at least one operator")
	}
	for _, kv := range []struct {
		k string
		v *Scalar
	}{{"eq", f.Eq}, {"ne", f.Ne}} {
		if kv.v != nil {
			if err := kv.v.validate(p + "." + kv.k); err != nil {
				return err
			}
		}
	}
	for _, kv := range []struct {
		k string
		v []Scalar
	}{{"in", f.In}, {"not_in", f.NotIn}} {
		if kv.v == nil {
			continue
		}
		if len(kv.v) < 1 || len(kv.v) > maxListItems {
			return schemaErr(p+"."+kv.k, "must hold 1 to %d values", maxListItems)
		}
		for i := range kv.v {
			if err := kv.v[i].validate(fmt.Sprintf("%s.%s[%d]", p, kv.k, i)); err != nil {
				return err
			}
		}
	}
	for _, kv := range []struct {
		k string
		v *float64
	}{{"gt", f.Gt}, {"gte", f.Gte}, {"lt", f.Lt}, {"lte", f.Lte}} {
		if kv.v != nil && (math.IsNaN(*kv.v) || math.IsInf(*kv.v, 0)) {
			return schemaErr(p+"."+kv.k, "must be a finite number")
		}
	}
	return nil
}

func (s Scalar) validate(p string) error {
	switch s.Kind {
	case ScalarString:
		if utf8.RuneCountInString(s.Str) > maxScalarLen || !utf8.ValidString(s.Str) {
			return schemaErr(p, "string longer than %d characters or not valid UTF-8", maxScalarLen)
		}
	case ScalarNumber:
		if math.IsNaN(s.Num) || math.IsInf(s.Num, 0) {
			return schemaErr(p, "must be a finite number")
		}
	case ScalarBool:
	default:
		return schemaErr(p, "must be a string, number or boolean")
	}
	return nil
}

func (e Effect) validate(p string) error {
	n := 0
	if e.Deny {
		n++
	}
	if e.RiskMin != nil {
		n++
		if *e.RiskMin < contracts.RiskReversible || *e.RiskMin > contracts.RiskPrivileged {
			return schemaErr(p+".risk_min", "must be 1 to 3; a raise to 0 raises nothing")
		}
	}
	if e.HideFields != nil {
		n++
		if err := validFieldNameList(p+".hide_fields", e.HideFields, 1, 64); err != nil {
			return err
		}
	}
	if n == 0 {
		return schemaErr(p, "must state deny, risk_min or hide_fields")
	}
	if e.Deny && n > 1 {
		return schemaErr(p, "deny cannot be combined with another effect")
	}
	return nil
}

// ---- identifier and list grammars (mirrors of the schema patterns) ----

func validIdentifier(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > maxIdentifierLn || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validCapabilityID(s string) bool {
	return len(s) <= maxIdentifierLn && validIdentifier(s) && dottedIDRE.MatchString(s)
}

// validText mirrors the schema's text grammar: 1 to 2000 characters, line
// breaks and tabs allowed, no other control characters.
func validText(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > maxTextLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validCapabilityList(p string, v []string, lo, hi int) error {
	if len(v) < lo || len(v) > hi {
		return schemaErr(p, "must hold %d to %d capability IDs", lo, hi)
	}
	seen := make(map[string]bool, len(v))
	for i, c := range v {
		if !validCapabilityID(c) {
			return schemaErr(fmt.Sprintf("%s[%d]", p, i), "must be an exact capability ID; wildcards do not exist")
		}
		if seen[c] {
			return schemaErr(fmt.Sprintf("%s[%d]", p, i), "duplicate entry")
		}
		seen[c] = true
	}
	return nil
}

func validIdentifierList(p string, v []string, lo, hi int) error {
	if len(v) < lo || len(v) > hi {
		return schemaErr(p, "must hold %d to %d identifiers", lo, hi)
	}
	seen := make(map[string]bool, len(v))
	for i, s := range v {
		if !validIdentifier(s) {
			return schemaErr(fmt.Sprintf("%s[%d]", p, i), "must be an identifier; wildcards do not exist")
		}
		if seen[s] {
			return schemaErr(fmt.Sprintf("%s[%d]", p, i), "duplicate entry")
		}
		seen[s] = true
	}
	return nil
}

func validFieldNameList(p string, v []string, lo, hi int) error {
	if len(v) < lo || len(v) > hi {
		return schemaErr(p, "must hold %d to %d field names", lo, hi)
	}
	seen := make(map[string]bool, len(v))
	for i, s := range v {
		if !fieldNameRE.MatchString(s) {
			return schemaErr(fmt.Sprintf("%s[%d]", p, i), "must be a lower snake_case field name")
		}
		if seen[s] {
			return schemaErr(fmt.Sprintf("%s[%d]", p, i), "duplicate entry")
		}
		seen[s] = true
	}
	return nil
}

func schemaErr(path, format string, args ...any) *contracts.Error {
	msg := fmt.Sprintf(format, args...)
	e := contracts.New(contracts.ErrSchemaInvalid, "policy document invalid: "+withPath(path, msg))
	if path != "" {
		e = e.WithDetails("path", path)
	}
	return e
}

func withPath(path, msg string) string {
	if path == "" {
		return msg
	}
	return path + ": " + msg
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- strict JSON reading ----

// readStrict decodes exactly one JSON value into a tree of map[string]any,
// []any, string, json.Number and bool. Unlike encoding/json it rejects
// duplicate object keys, null, trailing data and excessive nesting, and keeps
// numbers as their literal text. Key matching is exact and case-sensitive
// because the caller reads the tree by key.
func readStrict(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := readValue(dec, "", 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, schemaErr("", "unexpected data after the document")
	}
	return v, nil
}

func readValue(dec *json.Decoder, path string, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, schemaErr(path, "nested deeper than %d levels", maxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, schemaErr(path, "not valid JSON")
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, schemaErr(path, "not valid JSON")
				}
				key, ok := kt.(string)
				if !ok {
					return nil, schemaErr(path, "not valid JSON")
				}
				kp := key
				if path != "" {
					kp = path + "." + key
				}
				if _, dup := m[key]; dup {
					return nil, schemaErr(kp, "duplicate key")
				}
				v, err := readValue(dec, kp, depth+1)
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, schemaErr(path, "not valid JSON")
			}
			return m, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := readValue(dec, fmt.Sprintf("%s[%d]", path, len(arr)), depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, schemaErr(path, "not valid JSON")
			}
			return arr, nil
		}
		return nil, schemaErr(path, "not valid JSON")
	case nil:
		return nil, schemaErr(path, "null is not a value in this format; leave the key out")
	default:
		return tok, nil // string, json.Number, bool
	}
}

// ---- tree to document ----

func decodeHostPolicy(tree any) (*HostPolicy, error) {
	m, err := asObject("", tree, []string{"policy_id", "role_grants", "risk_overrides", "field_projection"},
		"format", "description", "rules")
	if err != nil {
		return nil, err
	}
	h := &HostPolicy{}
	if v, ok := m["format"]; ok {
		if h.Format, err = asString("format", v); err != nil {
			return nil, err
		}
		if h.Format != FormatPolicyV1 {
			return nil, schemaErr("format", "unsupported policy format; only %q is defined", FormatPolicyV1)
		}
	}
	if h.PolicyID, err = asString("policy_id", m["policy_id"]); err != nil {
		return nil, err
	}
	if v, ok := m["description"]; ok {
		if h.Description, err = asString("description", v); err != nil {
			return nil, err
		}
		if h.Description == "" {
			return nil, schemaErr("description", "must not be empty; leave the key out")
		}
	}
	if h.RoleGrants, err = decodeList("role_grants", m["role_grants"], decodeRoleGrant); err != nil {
		return nil, err
	}
	if h.RiskOverrides, err = decodeList("risk_overrides", m["risk_overrides"], decodeRiskOverride); err != nil {
		return nil, err
	}
	if h.FieldProjection, err = decodeList("field_projection", m["field_projection"], decodeFieldProjection); err != nil {
		return nil, err
	}
	if v, ok := m["rules"]; ok {
		if h.Rules, err = decodeList("rules", v, decodeRule); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func decodeRoleGrant(p string, v any) (RoleGrant, error) {
	m, err := asObject(p, v, []string{"role", "capabilities"})
	if err != nil {
		return RoleGrant{}, err
	}
	var g RoleGrant
	if g.Role, err = asString(p+".role", m["role"]); err != nil {
		return g, err
	}
	g.Capabilities, err = asStrings(p+".capabilities", m["capabilities"])
	return g, err
}

func decodeRiskOverride(p string, v any) (RiskOverride, error) {
	m, err := asObject(p, v, []string{"capability_id", "minimum_risk_level", "reason"})
	if err != nil {
		return RiskOverride{}, err
	}
	var o RiskOverride
	if o.CapabilityID, err = asString(p+".capability_id", m["capability_id"]); err != nil {
		return o, err
	}
	if o.MinimumRiskLevel, err = asRisk(p+".minimum_risk_level", m["minimum_risk_level"], contracts.RiskRead); err != nil {
		return o, err
	}
	o.Reason, err = asString(p+".reason", m["reason"])
	return o, err
}

func decodeFieldProjection(p string, v any) (FieldProjection, error) {
	m, err := asObject(p, v, []string{"capability_id", "model_visible_fields"}, "roles")
	if err != nil {
		return FieldProjection{}, err
	}
	var fp FieldProjection
	if fp.CapabilityID, err = asString(p+".capability_id", m["capability_id"]); err != nil {
		return fp, err
	}
	if rv, ok := m["roles"]; ok {
		if fp.Roles, err = asStrings(p+".roles", rv); err != nil {
			return fp, err
		}
	}
	fp.ModelVisibleFields, err = asStrings(p+".model_visible_fields", m["model_visible_fields"])
	return fp, err
}

func decodeRule(p string, v any) (Rule, error) {
	m, err := asObject(p, v, []string{"id", "capabilities", "when", "then", "reason"})
	if err != nil {
		return Rule{}, err
	}
	var r Rule
	if r.ID, err = asString(p+".id", m["id"]); err != nil {
		return r, err
	}
	if r.Capabilities, err = asStrings(p+".capabilities", m["capabilities"]); err != nil {
		return r, err
	}
	if r.Reason, err = asString(p+".reason", m["reason"]); err != nil {
		return r, err
	}
	if r.When, err = decodeCondition(p+".when", m["when"]); err != nil {
		return r, err
	}
	r.Then, err = decodeEffect(p+".then", m["then"])
	return r, err
}

func decodeCondition(p string, v any) (Condition, error) {
	m, err := asObject(p, v, nil, "roles", "scopes", "tenants", "workspaces", "fields")
	if err != nil {
		return Condition{}, err
	}
	var c Condition
	if x, ok := m["roles"]; ok {
		s, err := decodeSetCondition(p+".roles", x)
		if err != nil {
			return c, err
		}
		c.Roles = &s
	}
	if x, ok := m["scopes"]; ok {
		s, err := decodeSetCondition(p+".scopes", x)
		if err != nil {
			return c, err
		}
		c.Scopes = &s
	}
	if x, ok := m["tenants"]; ok {
		s, err := decodeValueCondition(p+".tenants", x)
		if err != nil {
			return c, err
		}
		c.Tenants = &s
	}
	if x, ok := m["workspaces"]; ok {
		s, err := decodeValueCondition(p+".workspaces", x)
		if err != nil {
			return c, err
		}
		c.Workspaces = &s
	}
	if x, ok := m["fields"]; ok {
		fm, ok := x.(map[string]any)
		if !ok {
			return c, schemaErr(p+".fields", "must be an object")
		}
		c.Fields = make(map[string]FieldPredicate, len(fm))
		for _, name := range sortedKeys(fm) {
			fp, err := decodeFieldPredicate(p+".fields."+name, fm[name])
			if err != nil {
				return c, err
			}
			c.Fields[name] = fp
		}
	}
	return c, nil
}

func decodeSetCondition(p string, v any) (SetCondition, error) {
	m, err := asObject(p, v, nil, "any", "all", "none")
	if err != nil {
		return SetCondition{}, err
	}
	var s SetCondition
	for _, t := range []struct {
		k   string
		dst *[]string
	}{{"any", &s.Any}, {"all", &s.All}, {"none", &s.None}} {
		if x, ok := m[t.k]; ok {
			if *t.dst, err = asStrings(p+"."+t.k, x); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}

func decodeValueCondition(p string, v any) (ValueCondition, error) {
	m, err := asObject(p, v, nil, "in", "not_in")
	if err != nil {
		return ValueCondition{}, err
	}
	var s ValueCondition
	for _, t := range []struct {
		k   string
		dst *[]string
	}{{"in", &s.In}, {"not_in", &s.NotIn}} {
		if x, ok := m[t.k]; ok {
			if *t.dst, err = asStrings(p+"."+t.k, x); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}

func decodeFieldPredicate(p string, v any) (FieldPredicate, error) {
	m, err := asObject(p, v, nil, "eq", "ne", "in", "not_in", "gt", "gte", "lt", "lte", "present")
	if err != nil {
		return FieldPredicate{}, err
	}
	var f FieldPredicate
	for _, t := range []struct {
		k   string
		dst **Scalar
	}{{"eq", &f.Eq}, {"ne", &f.Ne}} {
		if x, ok := m[t.k]; ok {
			s, err := asScalar(p+"."+t.k, x)
			if err != nil {
				return f, err
			}
			*t.dst = &s
		}
	}
	for _, t := range []struct {
		k   string
		dst *[]Scalar
	}{{"in", &f.In}, {"not_in", &f.NotIn}} {
		if x, ok := m[t.k]; ok {
			*t.dst, err = decodeList(p+"."+t.k, x, asScalar)
			if err != nil {
				return f, err
			}
		}
	}
	for _, t := range []struct {
		k   string
		dst **float64
	}{{"gt", &f.Gt}, {"gte", &f.Gte}, {"lt", &f.Lt}, {"lte", &f.Lte}} {
		if x, ok := m[t.k]; ok {
			n, err := asFloat(p+"."+t.k, x)
			if err != nil {
				return f, err
			}
			*t.dst = &n
		}
	}
	if x, ok := m["present"]; ok {
		b, ok := x.(bool)
		if !ok {
			return f, schemaErr(p+".present", "must be a boolean")
		}
		f.Present = &b
	}
	return f, nil
}

func decodeEffect(p string, v any) (Effect, error) {
	m, err := asObject(p, v, nil, "deny", "risk_min", "hide_fields")
	if err != nil {
		return Effect{}, err
	}
	var e Effect
	if x, ok := m["deny"]; ok {
		if b, ok := x.(bool); !ok || !b {
			return e, schemaErr(p+".deny", "must be true; to not deny, leave the key out")
		}
		e.Deny = true
	}
	if x, ok := m["risk_min"]; ok {
		lvl, err := asRisk(p+".risk_min", x, contracts.RiskReversible)
		if err != nil {
			return e, err
		}
		e.RiskMin = &lvl
	}
	if x, ok := m["hide_fields"]; ok {
		if e.HideFields, err = asStrings(p+".hide_fields", x); err != nil {
			return e, err
		}
	}
	return e, nil
}

// asObject checks that v is an object whose keys are exactly the required ones
// plus any of the optional ones.
func asObject(p string, v any, required []string, optional ...string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaErr(p, "must be an object")
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, k := range required {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			return nil, schemaErr(p, "missing key %q", k)
		}
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for _, k := range sortedKeys(m) {
		if !allowed[k] {
			return nil, schemaErr(p, "unknown key %q", k)
		}
	}
	return m, nil
}

func asString(p string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", schemaErr(p, "must be a string")
	}
	return s, nil
}

func asStrings(p string, v any) ([]string, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, schemaErr(p, "must be an array")
	}
	out := make([]string, len(a))
	for i, x := range a {
		s, ok := x.(string)
		if !ok {
			return nil, schemaErr(fmt.Sprintf("%s[%d]", p, i), "must be a string")
		}
		out[i] = s
	}
	return out, nil
}

func decodeList[T any](p string, v any, f func(string, any) (T, error)) ([]T, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, schemaErr(p, "must be an array")
	}
	out := make([]T, len(a))
	for i, x := range a {
		t, err := f(fmt.Sprintf("%s[%d]", p, i), x)
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

func asFloat(p string, v any) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaErr(p, "must be a number")
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, schemaErr(p, "must be a finite number")
	}
	return f, nil
}

// asRisk reads a bare integer 0..3 (a plain integer literal: no fraction, no
// exponent, no sign), and refuses anything below lowest.
func asRisk(p string, v any, lowest contracts.RiskLevel) (contracts.RiskLevel, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, schemaErr(p, "must be an integer")
	}
	var lvl contracts.RiskLevel
	if err := lvl.UnmarshalJSON([]byte(n.String())); err != nil {
		return 0, schemaErr(p, "must be one of the integers 0, 1, 2, 3")
	}
	if lvl < lowest {
		return 0, schemaErr(p, "must be at least %d", lowest)
	}
	return lvl, nil
}

func asScalar(p string, v any) (Scalar, error) {
	switch x := v.(type) {
	case string:
		return Scalar{Kind: ScalarString, Str: x}, nil
	case bool:
		return Scalar{Kind: ScalarBool, Bool: x}, nil
	case json.Number:
		f, err := asFloat(p, x)
		if err != nil {
			return Scalar{}, err
		}
		return Scalar{Kind: ScalarNumber, Num: f}, nil
	}
	return Scalar{}, schemaErr(p, "must be a string, number or boolean")
}

// ---- canonical form and digest ----

// canonicalJSON re-encodes a decoded JSON tree with sorted keys and no
// insignificant whitespace.
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		b.WriteByte('{')
		for i, k := range sortedKeys(x) {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		eb, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(eb)
	}
	return nil
}

// Digest returns the SHA-256 (lower-case hex) of the canonical JSON form of
// the policy: sorted keys, no insignificant whitespace, the required arrays
// written as [] when empty. Equal policies have equal digests however they were
// spelled; the audit record carries it so a decision can be tied to the exact
// signed policy.
func (h *HostPolicy) Digest() (string, error) {
	raw, err := json.Marshal(h)
	if err != nil {
		return "", contracts.Wrap(contracts.ErrInternal, err, "policy cannot be encoded")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return "", contracts.Wrap(contracts.ErrInternal, err, "policy cannot be re-read")
	}
	canon, err := canonicalJSON(tree)
	if err != nil {
		return "", contracts.Wrap(contracts.ErrInternal, err, "policy cannot be canonicalised")
	}
	sum := sha256.Sum256(canon)
	d := hex.EncodeToString(sum[:])
	if len(d) != sha256HexLen {
		return "", contracts.New(contracts.ErrInternal, "unexpected digest length")
	}
	return d, nil
}
