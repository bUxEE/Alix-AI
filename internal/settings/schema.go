package settings

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// schemaFS holds byte-identical copies of the published schemas the documents
// are validated against (go:embed cannot reach outside the package): the three
// document schemas, the shared definitions and the common identifiers they
// reference. TestEmbeddedSchemasMatchPublished fails when a copy and its
// original differ.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

// MaxDocumentBytes bounds a settings document. A larger one is refused before it
// is parsed. The schemas bound every list and number, so a valid document is a
// fraction of this.
const MaxDocumentBytes = 16 << 10

// maxDocumentDepth bounds the nesting of a document: the deepest valid one is
// three levels (root, section, list).
const maxDocumentDepth = 6

// maxReported bounds the paths a refusal names.
const maxReported = 10

type schemaSet struct {
	byLevel map[Level]*jsonschema.Schema
}

var (
	schemaOnce sync.Once
	schemaVal  *schemaSet
	schemaErr  error
)

// loadSchemas compiles the embedded schemas once.
func loadSchemas() (*schemaSet, error) {
	schemaOnce.Do(func() { schemaVal, schemaErr = compileSchemas() })
	return schemaVal, schemaErr
}

// refuseLoader resolves nothing: every schema is registered up front, so a $ref
// that is not is a bug, never a reason to read a file or reach the network.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("settings: schema reference %q is not available", url)
}

func compileSchemas() (*schemaSet, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(refuseLoader{})
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, fmt.Errorf("settings: embedded schemas: %w", err)
	}
	for _, e := range entries {
		raw, err := schemaFS.ReadFile("schemas/" + e.Name())
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", e.Name(), err)
		}
		m, _ := doc.(map[string]any)
		id, _ := m["$id"].(string)
		if id == "" {
			return nil, fmt.Errorf("settings: %s has no $id", e.Name())
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("settings: %s: %w", e.Name(), err)
		}
	}
	set := &schemaSet{byLevel: map[Level]*jsonschema.Schema{}}
	for _, l := range Levels() {
		s, err := c.Compile(l.SchemaID())
		if err != nil {
			return nil, fmt.Errorf("settings: compile %s: %w", l.SchemaID(), err)
		}
		set.byLevel[l] = s
	}
	return set, nil
}

// refuse builds the refusal of a document: validation_failed with fixed wording,
// never the caller's values.
func refuse(format string, args ...any) *contracts.Error {
	return contracts.Newf(contracts.ErrValidationFailed, format, args...)
}

// checkStrictJSON reads raw as exactly one JSON object, with no duplicate member
// anywhere (a duplicate is how a second, hidden value rides along), at most
// maxDocumentDepth deep and no longer than MaxDocumentBytes.
func checkStrictJSON(raw []byte) error {
	if len(raw) == 0 {
		return refuse("the settings document is missing")
	}
	if len(raw) > MaxDocumentBytes {
		return refuse("the settings document is larger than %d bytes", MaxDocumentBytes)
	}
	if !utf8.Valid(raw) {
		return refuse("the settings document is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return refuse("the settings document is not valid JSON")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return refuse("the settings document must be a JSON object")
	}
	if err := skipObject(dec, 1); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse("the settings document has data after the JSON object")
	}
	return nil
}

// skipObject reads the rest of an object whose '{' was consumed.
func skipObject(dec *json.Decoder, depth int) error {
	if depth > maxDocumentDepth {
		return refuse("the settings document is nested more than %d levels deep", maxDocumentDepth)
	}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return refuse("the settings document is not valid JSON")
		}
		key, ok := keyTok.(string)
		if !ok {
			return refuse("the settings document is not valid JSON")
		}
		if seen[key] {
			return refuse("the settings document names a member twice")
		}
		seen[key] = true
		if err := skipValue(dec, depth); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return refuse("the settings document is not valid JSON")
	}
	return nil
}

func skipValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return refuse("the settings document is not valid JSON")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		return skipObject(dec, depth+1)
	case '[':
		if depth+1 > maxDocumentDepth {
			return refuse("the settings document is nested more than %d levels deep", maxDocumentDepth)
		}
		for dec.More() {
			if err := skipValue(dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return refuse("the settings document is not valid JSON")
		}
	}
	return nil
}

// validateAgainstSchema checks raw (already strict JSON) against the schema of the
// level. A failure is a refusal naming only paths and keywords.
func validateAgainstSchema(level Level, raw []byte) error {
	set, err := loadSchemas()
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "settings: the schemas cannot be compiled")
	}
	s := set.byLevel[level]
	if s == nil {
		return contracts.New(contracts.ErrInternal, "settings: no schema for the level")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return refuse("the settings document is not valid JSON")
	}
	verr := s.Validate(inst)
	if verr == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return contracts.Wrap(contracts.ErrInternal, verr, "settings: the document could not be checked")
	}
	return refusalOf(ve)
}

// refusalOf turns a schema failure into the refusal sent to the client: the
// members that do not exist (with the guard each one runs into, when it is one of
// the things a setting can never do) and the members that are wrong, as paths.
func refusalOf(ve *jsonschema.ValidationError) error {
	var unknown, invalid []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		loc := cleanPath(strings.Join(e.InstanceLocation, "."))
		if ap, ok := e.ErrorKind.(*kind.AdditionalProperties); ok {
			for _, p := range ap.Properties {
				unknown = append(unknown, cleanPath(joinPath(loc, p)))
			}
			return
		}
		keyword := "schema"
		if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
			keyword = kp[len(kp)-1]
		}
		if loc == "" {
			loc = "(document)"
		}
		invalid = append(invalid, loc+": "+cleanPath(keyword))
	}
	walk(ve)
	sort.Strings(unknown)
	sort.Strings(invalid)
	unknown, invalid = clipList(unknown), clipList(invalid)

	var err *contracts.Error
	switch {
	case len(unknown) > 0:
		err = refuse("the settings document names a member that does not exist: %s", unknown[0]).
			WithDetails("members", unknown).
			WithDetails("reason", "unknown_member")
		if g := guardOf(unknown); g != "" {
			err = err.WithDetails("guard", g)
		}
	default:
		first := "(document)"
		if len(invalid) > 0 {
			first = invalid[0]
		}
		err = refuse("the settings document does not match its schema at %s", first).WithDetails("reason", "schema")
	}
	if len(invalid) > 0 {
		err = err.WithDetails("invalid", invalid)
	}
	return err
}

func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}

var pathCharsRE = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// cleanPath makes a caller-supplied member name safe to name in a message and a
// log: identifier characters only, at most 64 of them.
func cleanPath(s string) string {
	s = pathCharsRE.ReplaceAllString(s, "?")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func clipList(in []string) []string {
	if len(in) > maxReported {
		return in[:maxReported]
	}
	return in
}

// The things a setting can never do (master plan 14.2 last paragraph, the
// external-mode and decision-layer guards). They are not special cases in the
// validator: the schemas have no member for any of them, so a document that names
// one is refused as an unknown member like any other. The table only says which
// of them the unknown member looks like, so the refusal can name the rule and the
// log line is useful. The first match wins, most specific first.
var guards = []struct {
	name string
	re   *regexp.Regexp
}{
	{"decision_layer", regexp.MustCompile(`decision|clm|scorer|candidate`)},
	{"runtime_admin", regexp.MustCompile(`runtime|localai|gallery|backend|webui|admin|base_?url|endpoint|model_?path|inference`)},
	{"confirmation", regexp.MustCompile(`confirm|step_?up|risk|floor|approv|skip|bypass`)},
	{"credentials", regexp.MustCompile(`credential|secret|token|api_?key|password|oauth|auth|bearer`)},
	{"budget", regexp.MustCompile(`budget|quota|spend|cost|price|micro|billing`)},
	{"data_egress", regexp.MustCompile(`egress|destination|allow_?list|data_?class|classification`)},
	{"external_mode", regexp.MustCompile(`external|hybrid|(^|[._])mode($|[._])|provider`)},
	{"scope", regexp.MustCompile(`scope|role|permission|grant|tenant|workspace|application|principal|session|deployment`)},
	{"tool", regexp.MustCompile(`tool|mcp|agent|plugin|function|shell|exec|enable|capabilit`)},
}

// guardOf names the rule the first recognisable unknown member runs into, or "".
func guardOf(paths []string) string {
	for _, p := range paths {
		p = strings.ToLower(p)
		for _, g := range guards {
			if g.re.MatchString(p) {
				return g.name
			}
		}
	}
	return ""
}
