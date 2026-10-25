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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The corpus is a directory:
//
//	corpus.json      the manifest: tenants, principals, sources, their versions and states
//	questions.jsonl  the labelled questions, one JSON object per line
//	docs/...         the Markdown documents the manifest names
//
// Everything in it is synthetic. Loading is strict: unknown keys, a dangling
// reference, a label whose anchor text is not in the document it points at and
// a question that cannot be asked all fail the load, so a corpus that has
// drifted from its labels cannot produce numbers.

const (
	manifestFile  = "corpus.json"
	questionsFile = "questions.jsonl"

	corpusFormatVersion = 1

	// maxDocBytes bounds one document and maxQuestions the question file.
	maxDocBytes  = 1 << 20
	maxQuestions = 10_000
)

// Source states. active is the ordinary case; the others are what the corpus
// exists to exercise.
const (
	StateActive      = "active"      // indexed, available and current
	StateStale       = "stale"       // indexed, but the source has changed since: citations say so
	StateUnavailable = "unavailable" // indexed, but the principal cannot open it now: citations say so
	StateDeleted     = "deleted"     // indexed, then deleted: nothing of it may be found again
)

// Question outcomes: what a correct system does with the question.
const (
	OutcomeAnswer  = "answer"  // a grounded answer from the labelled evidence
	OutcomeDecline = "decline" // no_supported_evidence, and nothing forbidden anywhere
)

// Question kinds. The set is open to the corpus (a kind is a label for the
// per-kind breakdown), but these are the ones the report explains.
const (
	KindIdentifier   = "identifier"
	KindTable        = "table"
	KindSemantic     = "semantic"
	KindCrossLang    = "crosslang"
	KindConflict     = "conflict"
	KindStale        = "stale"
	KindUnavailable  = "unavailable"
	KindDeleted      = "deleted"
	KindPermission   = "permission"
	KindCrossTenant  = "crosstenant"
	KindUnanswerable = "unanswerable"
)

// Manifest is corpus.json.
type Manifest struct {
	FormatVersion int         `json:"format_version"`
	ID            string      `json:"id"`
	Description   string      `json:"description"`
	Application   string      `json:"application"`
	Tenants       []TenantDef `json:"tenants"`
	Sources       []SourceDef `json:"sources"`
}

// TenantDef is one tenant of the corpus, with its single workspace and the
// principals that may ask questions in it.
type TenantDef struct {
	Key         string   `json:"key"`
	TenantID    string   `json:"tenant_id"`
	WorkspaceID string   `json:"workspace_id"`
	Principals  []string `json:"principals"`
}

// SourceDef is one registered source. Versions are indexed in order, each one
// replacing the one before it (a new index generation), so after the last one
// only its content is searchable. Readers, when not empty, are the only
// principals of the tenant that may read it.
type SourceDef struct {
	Key      string       `json:"key"`
	Tenant   string       `json:"tenant"`
	Name     string       `json:"name"`
	Language string       `json:"language"`
	State    string       `json:"state"`
	Readers  []string     `json:"readers,omitempty"`
	Versions []VersionDef `json:"versions"`
}

// VersionDef is one version of a source and the documents it holds.
type VersionDef struct {
	Version   string   `json:"version"`
	Documents []DocDef `json:"documents"`
}

// DocDef is one document: its id inside the source (the same id in two
// versions is the same document) and its file, relative to the corpus
// directory.
type DocDef struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

// Evidence labels where an answer to a question is. A returned passage hits
// the entry when it comes from this document of this source and its text holds
// every anchor (compared after case, accent and whitespace folding).
type Evidence struct {
	Source  string   `json:"source"`
	Doc     string   `json:"doc"`
	Anchors []string `json:"anchors"`
}

// CitationLabel states what a correct citation of the answer looks like. Every
// field is optional.
type CitationLabel struct {
	Source       string `json:"source,omitempty"`
	Version      string `json:"version,omitempty"`
	Availability string `json:"availability,omitempty"`
	Staleness    string `json:"staleness,omitempty"`
}

// Question is one labelled question.
type Question struct {
	ID        string `json:"id"`
	Language  string `json:"language"`
	Kind      string `json:"kind"`
	Tenant    string `json:"tenant"`
	Principal string `json:"principal"`
	Text      string `json:"text"`
	Outcome   string `json:"outcome"`
	// Expect is the labelled evidence of an answerable question; empty for a
	// question that must be declined.
	Expect []Evidence `json:"expect,omitempty"`
	// Facts must all appear in the answer's claims.
	Facts []string `json:"facts,omitempty"`
	// Forbidden strings must appear nowhere: not in the answer, not in any
	// passage handed to the model.
	Forbidden []string `json:"forbidden,omitempty"`
	// ForbiddenSources are sources the evidence package and the citations must
	// not touch.
	ForbiddenSources []string `json:"forbidden_sources,omitempty"`
	// Citation labels a correct citation (version, availability, staleness).
	Citation *CitationLabel `json:"citation,omitempty"`
}

// Corpus is a loaded corpus.
type Corpus struct {
	Dir       string
	Manifest  Manifest
	Questions []Question
	// SHA256 identifies the corpus: the digest of every file's relative path
	// and bytes, in path order.
	SHA256 string
	// Files and Bytes count the corpus files and their size.
	Files int
	Bytes int64

	docs    map[string][]byte // relative path -> bytes
	tenants map[string]*TenantDef
	sources map[string]*SourceDef
}

var (
	keyPattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	verPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,31}$`)
	docIDPatten = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// LoadCorpus reads and validates the corpus in dir.
func LoadCorpus(dir string) (*Corpus, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	c := &Corpus{Dir: root, docs: map[string][]byte{}, tenants: map[string]*TenantDef{}, sources: map[string]*SourceDef{}}

	hash := sha256.New()
	var paths []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("corpus %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("corpus %s: no files", dir)
	}
	sort.Strings(paths)
	raw := map[string][]byte{}
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		b, err := readCapped(p, 8<<20)
		if err != nil {
			return nil, fmt.Errorf("corpus %s: %w", rel, err)
		}
		b = normaliseEOL(b)
		raw[rel] = b
		fmt.Fprintf(hash, "%s\x00%d\x00", rel, len(b))
		hash.Write(b)
		c.Files++
		c.Bytes += int64(len(b))
	}
	c.SHA256 = hex.EncodeToString(hash.Sum(nil))

	mb, ok := raw[manifestFile]
	if !ok {
		return nil, fmt.Errorf("corpus %s: no %s", dir, manifestFile)
	}
	if err := decodeStrict(mb, &c.Manifest); err != nil {
		return nil, fmt.Errorf("corpus %s: %w", manifestFile, err)
	}
	qb, ok := raw[questionsFile]
	if !ok {
		return nil, fmt.Errorf("corpus %s: no %s", dir, questionsFile)
	}
	if c.Questions, err = decodeQuestions(qb); err != nil {
		return nil, fmt.Errorf("corpus %s: %w", questionsFile, err)
	}
	for rel, b := range raw {
		if strings.HasPrefix(rel, "docs/") {
			c.docs[rel] = b
		}
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("corpus %s: %w", dir, err)
	}
	return c, nil
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}

// normaliseEOL makes the corpus independent of the checkout's line endings, so
// its digest and the span offsets of every chunk are the same everywhere.
func normaliseEOL(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON document")
	}
	return nil
}

func decodeQuestions(b []byte) ([]Question, error) {
	var out []Question
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var q Question
		if err := decodeStrict([]byte(text), &q); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, q)
		if len(out) > maxQuestions {
			return nil, fmt.Errorf("more than %d questions", maxQuestions)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no questions")
	}
	return out, nil
}

// Doc returns the text of a document file named by the manifest.
func (c *Corpus) Doc(file string) string { return string(c.docs[file]) }

// Tenant and Source look a definition up by key.
func (c *Corpus) Tenant(key string) (*TenantDef, bool) { t, ok := c.tenants[key]; return t, ok }

// Source looks a source definition up by key.
func (c *Corpus) Source(key string) (*SourceDef, bool) { s, ok := c.sources[key]; return s, ok }

// Latest is the version of the source that is indexed last, and so the one that
// is searchable.
func (s *SourceDef) Latest() VersionDef { return s.Versions[len(s.Versions)-1] }

// Languages counts the questions per language.
func (c *Corpus) Languages() map[string]int {
	m := map[string]int{}
	for _, q := range c.Questions {
		m[q.Language]++
	}
	return m
}

// Documents counts the documents of the latest versions (what is searchable).
func (c *Corpus) Documents() int {
	n := 0
	for _, s := range c.Manifest.Sources {
		n += len(s.Latest().Documents)
	}
	return n
}

func (c *Corpus) validate() error {
	m := &c.Manifest
	bad := func(format string, args ...any) error { return fmt.Errorf(format, args...) }
	switch {
	case m.FormatVersion != corpusFormatVersion:
		return bad("%s: format_version %d is not %d", manifestFile, m.FormatVersion, corpusFormatVersion)
	case !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`).MatchString(m.ID):
		return bad("%s: id %q is not a corpus id", manifestFile, m.ID)
	case !idPattern.MatchString(m.Application):
		return bad("%s: application %q is not an identifier", manifestFile, m.Application)
	case len(m.Tenants) == 0 || len(m.Sources) == 0:
		return bad("%s: it needs tenants and sources", manifestFile)
	}
	principalTenant := map[string]string{}
	for i := range m.Tenants {
		t := &m.Tenants[i]
		switch {
		case !keyPattern.MatchString(t.Key):
			return bad("tenant key %q is not a key", t.Key)
		case !idPattern.MatchString(t.TenantID) || !idPattern.MatchString(t.WorkspaceID):
			return bad("tenant %s: tenant_id and workspace_id must be identifiers", t.Key)
		case len(t.Principals) == 0:
			return bad("tenant %s has no principal", t.Key)
		}
		if _, dup := c.tenants[t.Key]; dup {
			return bad("tenant %s appears twice", t.Key)
		}
		c.tenants[t.Key] = t
		for _, p := range t.Principals {
			if !idPattern.MatchString(p) {
				return bad("tenant %s: principal %q is not an identifier", t.Key, p)
			}
			if other, dup := principalTenant[p]; dup {
				return bad("principal %s belongs to tenants %s and %s", p, other, t.Key)
			}
			principalTenant[p] = t.Key
		}
	}
	for i := range m.Sources {
		s := &m.Sources[i]
		if err := c.validateSource(s); err != nil {
			return bad("source %s: %w", s.Key, err)
		}
		c.sources[s.Key] = s
	}
	seen := map[string]bool{}
	for i := range c.Questions {
		q := &c.Questions[i]
		if seen[q.ID] {
			return bad("question %s appears twice", q.ID)
		}
		seen[q.ID] = true
		if err := c.validateQuestion(q, principalTenant); err != nil {
			return bad("question %s: %w", q.ID, err)
		}
	}
	return nil
}

func (c *Corpus) validateSource(s *SourceDef) error {
	switch {
	case !keyPattern.MatchString(s.Key):
		return errors.New("the key is not a key")
	case c.sources[s.Key] != nil:
		return errors.New("the key appears twice")
	case s.Name == "" || len(s.Name) > 200:
		return errors.New("the name is empty or over 200 bytes")
	case s.Language != "en" && s.Language != "it":
		return fmt.Errorf("language %q is not en or it", s.Language)
	case len(s.Versions) == 0:
		return errors.New("it has no version")
	}
	t, ok := c.tenants[s.Tenant]
	if !ok {
		return fmt.Errorf("tenant %q is not defined", s.Tenant)
	}
	switch s.State {
	case StateActive, StateStale, StateUnavailable, StateDeleted:
	default:
		return fmt.Errorf("state %q is unknown", s.State)
	}
	for _, r := range s.Readers {
		if !contains(t.Principals, r) {
			return fmt.Errorf("reader %q is not a principal of tenant %s", r, s.Tenant)
		}
	}
	versions := map[string]bool{}
	for _, v := range s.Versions {
		if !verPattern.MatchString(v.Version) || versions[v.Version] {
			return fmt.Errorf("version %q is malformed or repeated", v.Version)
		}
		versions[v.Version] = true
		if len(v.Documents) == 0 {
			return fmt.Errorf("version %s has no document", v.Version)
		}
		ids := map[string]bool{}
		for _, d := range v.Documents {
			if !docIDPatten.MatchString(d.ID) || ids[d.ID] {
				return fmt.Errorf("document id %q is malformed or repeated in version %s", d.ID, v.Version)
			}
			ids[d.ID] = true
			text, ok := c.docs[d.File]
			switch {
			case !ok:
				return fmt.Errorf("document file %q does not exist under docs/", d.File)
			case len(text) > maxDocBytes:
				return fmt.Errorf("document %s is over %d bytes", d.File, maxDocBytes)
			case strings.TrimSpace(string(text)) == "":
				return fmt.Errorf("document %s is empty", d.File)
			}
		}
	}
	return nil
}

func (c *Corpus) validateQuestion(q *Question, principalTenant map[string]string) error {
	switch {
	case !keyPattern.MatchString(q.ID) || len(q.ID) > 48:
		return errors.New("the id is not a bench case id (lower-case words joined by hyphens, at most 48 bytes)")
	case q.Language != "en" && q.Language != "it":
		return fmt.Errorf("language %q is not en or it", q.Language)
	case !keyPattern.MatchString(q.Kind):
		return fmt.Errorf("kind %q is not a key", q.Kind)
	case strings.TrimSpace(q.Text) == "":
		return errors.New("the text is empty")
	case q.Outcome != OutcomeAnswer && q.Outcome != OutcomeDecline:
		return fmt.Errorf("outcome %q is not answer or decline", q.Outcome)
	}
	if _, ok := c.tenants[q.Tenant]; !ok {
		return fmt.Errorf("tenant %q is not defined", q.Tenant)
	}
	if principalTenant[q.Principal] != q.Tenant {
		return fmt.Errorf("principal %q is not a principal of tenant %s", q.Principal, q.Tenant)
	}
	for _, f := range q.Facts {
		if strings.TrimSpace(f) == "" {
			return errors.New("an empty fact")
		}
	}
	for _, f := range q.Forbidden {
		if strings.TrimSpace(f) == "" {
			return errors.New("an empty forbidden string")
		}
	}
	for _, k := range q.ForbiddenSources {
		if c.sources[k] == nil {
			return fmt.Errorf("forbidden source %q is not defined", k)
		}
	}
	switch q.Outcome {
	case OutcomeDecline:
		if len(q.Expect) > 0 || len(q.Facts) > 0 || q.Citation != nil {
			return errors.New("a question to decline carries no evidence, facts or citation label")
		}
		return nil
	}
	if len(q.Expect) == 0 {
		return errors.New("an answerable question needs at least one evidence label")
	}
	for _, e := range q.Expect {
		s, ok := c.sources[e.Source]
		if !ok {
			return fmt.Errorf("evidence source %q is not defined", e.Source)
		}
		if s.Tenant != q.Tenant {
			return fmt.Errorf("evidence source %s belongs to tenant %s, not %s", e.Source, s.Tenant, q.Tenant)
		}
		if s.State == StateDeleted {
			return fmt.Errorf("evidence source %s is deleted: the question should be declined", e.Source)
		}
		if len(s.Readers) > 0 && !contains(s.Readers, q.Principal) {
			return fmt.Errorf("principal %s may not read evidence source %s", q.Principal, e.Source)
		}
		if len(e.Anchors) == 0 {
			return fmt.Errorf("evidence %s/%s has no anchor", e.Source, e.Doc)
		}
		var doc string
		for _, d := range s.Latest().Documents {
			if d.ID == e.Doc {
				doc = string(c.docs[d.File])
			}
		}
		if doc == "" {
			return fmt.Errorf("evidence document %s is not in the latest version of source %s", e.Doc, e.Source)
		}
		for _, a := range e.Anchors {
			if strings.TrimSpace(a) == "" || !containsFold(doc, a) {
				return fmt.Errorf("anchor %q is not in document %s of source %s", a, e.Doc, e.Source)
			}
		}
	}
	if q.Citation != nil {
		if q.Citation.Source != "" && c.sources[q.Citation.Source] == nil {
			return fmt.Errorf("citation source %q is not defined", q.Citation.Source)
		}
		switch q.Citation.Availability {
		case "", "available", "unavailable", "removed":
		default:
			return fmt.Errorf("citation availability %q is unknown", q.Citation.Availability)
		}
		switch q.Citation.Staleness {
		case "", "current", "stale", "unknown":
		default:
			return fmt.Errorf("citation staleness %q is unknown", q.Citation.Staleness)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
