package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Corpus categories (schemas/bench/corpus-case.schema.json).
const (
	CatRouting    = "routing"
	CatExtraction = "extraction"
	CatRefusal    = "refusal"
	CatInjection  = "injection"
)

// Categories lists the categories in their fixed reporting order.
var Categories = []string{CatRouting, CatExtraction, CatRefusal, CatInjection}

// Languages lists the declared benchmark languages in reporting order.
var Languages = []string{"en", "it"}

// TaskProposal intents (schemas/conversation/task-proposal.schema.json).
const (
	IntentTask       = "task"
	IntentCorrection = "correction"
	IntentCancel     = "cancel"
	IntentHelp       = "help"
	IntentOutOfScope = "out_of_scope"
)

var intents = []string{IntentTask, IntentCorrection, IntentCancel, IntentHelp, IntentOutOfScope}

// Field types a catalogue may declare.
const (
	TypeString   = "string"
	TypeInteger  = "integer"
	TypeNumber   = "number"
	TypeBoolean  = "boolean"
	TypeDate     = "date"
	TypeTime     = "time"
	TypeDatetime = "datetime"
	TypeEnum     = "enum"
)

var fieldTypes = []string{TypeString, TypeInteger, TypeNumber, TypeBoolean, TypeDate, TypeTime, TypeDatetime, TypeEnum}

// Field is one typed field of a capability.
type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Enum     []string `json:"enum,omitempty"`
}

// Capability is one entry of a case's catalogue.
type Capability struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

// field returns the named field of the capability.
func (c Capability) field(name string) (Field, bool) {
	for _, f := range c.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Expected is the model-authority part of the TaskProposal the case wants. A
// number in Fields is a json.Number, so 48.50 and 4 keep their exact text.
type Expected struct {
	CapabilityID *string        `json:"capability_id"`
	Intent       string         `json:"intent"`
	Fields       map[string]any `json:"fields"`
}

// Attack says what falling for a prompt injection means for one case.
type Attack struct {
	ForbiddenCapabilities []string `json:"forbidden_capabilities"`
	ForbiddenValues       []string `json:"forbidden_values"`
}

// Case is one line of a corpus.
type Case struct {
	ID        string       `json:"id"`
	Language  string       `json:"language"`
	Category  string       `json:"category"`
	Today     string       `json:"today,omitempty"`
	Catalogue []Capability `json:"catalogue"`
	Text      string       `json:"text"`
	Expected  Expected     `json:"expected"`
	Attack    *Attack      `json:"attack,omitempty"`
}

// capability returns the catalogue entry with the given id.
func (c Case) capability(id string) (Capability, bool) {
	for _, k := range c.Catalogue {
		if k.ID == id {
			return k, true
		}
	}
	return Capability{}, false
}

// Corpus is a loaded, validated corpus.
type Corpus struct {
	// ID is the file name without its extension.
	ID string
	// SHA256 is the hash of the file's bytes.
	SHA256 string
	Cases  []Case
}

// maxCorpusBytes bounds what LoadCorpus reads.
const maxCorpusBytes = 16 << 20

var (
	caseIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	corpusIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	capabilityIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	fieldNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	timePattern         = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// LoadCorpus reads a JSONL corpus: one case per line, blank lines ignored.
// Decoding is strict (unknown fields are errors) and every case must pass
// Validate; the error lists every problem with its line number. A corpus that
// does not load is never partly used.
func LoadCorpus(path string) (*Corpus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCorpusBytes+1))
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	if len(data) > maxCorpusBytes {
		return nil, fmt.Errorf("corpus: %s is larger than %d bytes", filepath.Base(path), maxCorpusBytes)
	}
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return ParseCorpus(id, data)
}

// ParseCorpus is LoadCorpus for bytes already in memory. id names the corpus.
func ParseCorpus(id string, data []byte) (*Corpus, error) {
	if !corpusIDPattern.MatchString(id) {
		return nil, fmt.Errorf("corpus: id %q must match %s (name the file in lower case)", id, corpusIDPattern)
	}
	sum := sha256.Sum256(data)
	c := &Corpus{ID: id, SHA256: hex.EncodeToString(sum[:])}

	var problems []string
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var cs Case
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		dec.UseNumber()
		if err := dec.Decode(&cs); err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			problems = append(problems, fmt.Sprintf("line %d: more than one JSON value on the line", line))
			continue
		}
		for _, p := range cs.Validate() {
			problems = append(problems, fmt.Sprintf("line %d (%s): %s", line, cs.ID, p))
		}
		if first, dup := seen[cs.ID]; dup {
			problems = append(problems, fmt.Sprintf("line %d: duplicate case id %q (first on line %d)", line, cs.ID, first))
		}
		seen[cs.ID] = line
		c.Cases = append(c.Cases, cs)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	if len(c.Cases) == 0 && len(problems) == 0 {
		problems = append(problems, "the corpus holds no cases")
	}
	if len(problems) > 0 {
		const maxShown = 30
		more := ""
		if len(problems) > maxShown {
			more = fmt.Sprintf("\n  ... and %d more", len(problems)-maxShown)
			problems = problems[:maxShown]
		}
		return nil, fmt.Errorf("corpus %s is invalid:\n  %s%s", id, strings.Join(problems, "\n  "), more)
	}
	return c, nil
}

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Validate returns every way the case is malformed or inconsistent, so a
// corpus author sees them all at once. It checks what the JSON Schema cannot:
// that the expected proposal only names capabilities and fields the catalogue
// declares, with values of the declared types.
func (c Case) Validate() []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, fmt.Sprintf(format, args...)) }

	if !caseIDPattern.MatchString(c.ID) || len(c.ID) < 3 || len(c.ID) > 48 {
		add("id %q must be 3 to 48 characters of lower case words joined by hyphens", c.ID)
	}
	if !inList(Languages, c.Language) {
		add("language %q must be one of %v", c.Language, Languages)
	}
	if !inList(Categories, c.Category) {
		add("category %q must be one of %v", c.Category, Categories)
	}
	if c.Today != "" {
		if _, err := time.Parse("2006-01-02", c.Today); err != nil {
			add("today %q is not a YYYY-MM-DD date", c.Today)
		}
	}
	if n := utf8.RuneCountInString(c.Text); n < 1 || n > 2000 || !utf8.ValidString(c.Text) {
		add("text must be 1 to 2000 characters of valid UTF-8")
	}
	for _, r := range c.Text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			add("text holds a control character (U+%04X)", r)
			break
		}
	}

	if n := len(c.Catalogue); n < 2 || n > 6 {
		add("the catalogue holds %d capabilities; it must hold 2 to 6", n)
	}
	seenCap := map[string]bool{}
	for _, k := range c.Catalogue {
		if !capabilityIDPattern.MatchString(k.ID) {
			add("capability id %q must match %s", k.ID, capabilityIDPattern)
		}
		if seenCap[k.ID] {
			add("capability %q is listed twice", k.ID)
		}
		seenCap[k.ID] = true
		if d := utf8.RuneCountInString(k.Description); d < 1 || d > 120 {
			add("capability %q: description must be 1 to 120 characters", k.ID)
		}
		if len(k.Fields) > 12 {
			add("capability %q: more than 12 fields", k.ID)
		}
		seenField := map[string]bool{}
		for _, f := range k.Fields {
			if !fieldNamePattern.MatchString(f.Name) {
				add("capability %q: field name %q must match %s", k.ID, f.Name, fieldNamePattern)
			}
			if seenField[f.Name] {
				add("capability %q: field %q is listed twice", k.ID, f.Name)
			}
			seenField[f.Name] = true
			if !inList(fieldTypes, f.Type) {
				add("capability %q: field %q has unknown type %q", k.ID, f.Name, f.Type)
			}
			if (f.Type == TypeEnum) != (len(f.Enum) > 0) {
				add("capability %q: field %q: an enum list is required for type enum and forbidden otherwise", k.ID, f.Name)
			}
			if f.Type == TypeEnum && len(f.Enum) < 2 {
				add("capability %q: field %q: an enum needs at least two values", k.ID, f.Name)
			}
		}
	}

	ps = append(ps, c.validateExpected()...)

	switch c.Category {
	case CatRefusal:
		if c.Expected.Intent != IntentOutOfScope {
			add("a refusal case must expect intent %s", IntentOutOfScope)
		}
	case CatExtraction:
		if c.Expected.Intent != IntentTask || len(c.Expected.Fields) == 0 {
			add("an extraction case must expect a task with at least one field")
		}
	case CatRouting:
		if c.Expected.Intent == IntentOutOfScope || c.Expected.Intent == IntentCorrection {
			add("a routing case must expect task, cancel or help, not %s", c.Expected.Intent)
		}
	}
	if (c.Category == CatInjection) != (c.Attack != nil) {
		add("an attack is required for category injection and forbidden otherwise")
	}
	if c.Attack != nil {
		a := c.Attack
		if len(a.ForbiddenCapabilities)+len(a.ForbiddenValues) == 0 {
			add("attack names no forbidden capability and no forbidden value")
		}
		for _, id := range a.ForbiddenCapabilities {
			if !capabilityIDPattern.MatchString(id) {
				add("attack: forbidden capability %q must match %s", id, capabilityIDPattern)
			}
			if c.Expected.CapabilityID != nil && *c.Expected.CapabilityID == id {
				add("attack: the expected capability %q is also forbidden", id)
			}
		}
		for _, v := range a.ForbiddenValues {
			if utf8.RuneCountInString(v) < 3 {
				add("attack: forbidden value %q is shorter than 3 characters and would match innocent text", v)
			}
		}
	}
	return ps
}

func (c Case) validateExpected() []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, "expected: "+fmt.Sprintf(format, args...)) }
	e := c.Expected
	if !inList(intents, e.Intent) {
		add("intent %q must be one of %v", e.Intent, intents)
		return ps
	}
	if e.Fields == nil {
		add("fields must be an object (use {} when the message states no value)")
	}
	switch {
	case e.Intent == IntentOutOfScope:
		if e.CapabilityID != nil || len(e.Fields) > 0 {
			add("an out_of_scope proposal has no capability and no fields")
		}
		return ps
	case e.Intent == IntentTask || e.Intent == IntentCorrection:
		if e.CapabilityID == nil {
			add("intent %s needs a capability_id", e.Intent)
			return ps
		}
	}
	if e.CapabilityID == nil {
		if len(e.Fields) > 0 {
			add("fields without a capability")
		}
		return ps
	}
	k, ok := c.capability(*e.CapabilityID)
	if !ok {
		add("capability %q is not in the catalogue", *e.CapabilityID)
		return ps
	}
	for name, v := range e.Fields {
		f, ok := k.field(name)
		if !ok {
			add("field %q is not declared by %s", name, k.ID)
			continue
		}
		if err := valueMatchesType(f, v); err != nil {
			add("field %q: %v", name, err)
		}
	}
	return ps
}

// valueMatchesType checks a decoded JSON value against a declared field.
func valueMatchesType(f Field, v any) error {
	switch f.Type {
	case TypeInteger:
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("must be a JSON number (integer)")
		}
		if _, err := strconv.ParseInt(n.String(), 10, 64); err != nil {
			return fmt.Errorf("%s is not an integer", n)
		}
	case TypeNumber:
		if _, ok := v.(json.Number); !ok {
			return fmt.Errorf("must be a JSON number")
		}
	case TypeBoolean:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("must be a JSON boolean")
		}
	case TypeString, TypeDate, TypeTime, TypeDatetime, TypeEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("must be a JSON string")
		}
		switch f.Type {
		case TypeDate:
			if _, err := time.Parse("2006-01-02", s); err != nil {
				return fmt.Errorf("%q is not a YYYY-MM-DD date", s)
			}
		case TypeTime:
			if !timePattern.MatchString(s) {
				return fmt.Errorf("%q is not an HH:MM time", s)
			}
		case TypeDatetime:
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				return fmt.Errorf("%q is not an RFC 3339 timestamp", s)
			}
		case TypeEnum:
			if !inList(f.Enum, s) {
				return fmt.Errorf("%q is not one of %v", s, f.Enum)
			}
		}
	default:
		return fmt.Errorf("unknown type %q", f.Type)
	}
	return nil
}
