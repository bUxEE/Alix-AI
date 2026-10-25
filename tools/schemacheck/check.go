package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// draft2020 is the only dialect AlixAI schemas may declare.
const draft2020 = "https://json-schema.org/draft/2020-12/schema"

// schemaIDPattern is the AlixAI schema id convention:
// urn:alixai:schema:<area>:<name>:v<N>.
var schemaIDPattern = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9]+(?:-[a-z0-9]+)*):([a-z0-9]+(?:-[a-z0-9]+)*):v[1-9][0-9]*$`)

// FindingKind classifies one problem found by Check.
type FindingKind string

const (
	// KindSchema: a schema file is unreadable, malformed, has a bad $id or
	// $schema, duplicates another $id, fails to compile, or a directory holds
	// no schemas at all.
	KindSchema FindingKind = "schema"
	// KindValidRejected: a file in examples/<name>/valid/ failed validation.
	KindValidRejected FindingKind = "valid-example-rejected"
	// KindInvalidAccepted: a file in examples/<name>/invalid/ passed validation.
	KindInvalidAccepted FindingKind = "invalid-example-accepted"
	// KindExampleUnreadable: an example is not parseable JSON. It is never
	// counted as a correctly-rejected invalid example.
	KindExampleUnreadable FindingKind = "example-unreadable"
	// KindLayout: examples that do not follow
	// examples/<schema-name>/{valid,invalid}/*.json, or that belong to no schema.
	KindLayout FindingKind = "layout"
	// KindNoExamples: only reported with Options.RequireExamples.
	KindNoExamples FindingKind = "no-examples"
)

// Finding is one problem.
type Finding struct {
	Kind   FindingKind
	Path   string // file or directory the problem is about (display form)
	Detail string
}

// Report is the outcome of Check.
type Report struct {
	Schemas         int // schemas that compiled
	ValidExamples   int // valid examples that passed
	InvalidExamples int // invalid examples that were correctly rejected
	Findings        []Finding
	// Rejections maps an invalid example's path to the validation error that
	// rejected it (shown with -v, so a reviewer can see WHY it is invalid).
	Rejections map[string]string
}

// OK reports whether nothing was wrong.
func (r *Report) OK() bool { return len(r.Findings) == 0 }

// Has reports whether a finding of the given kind exists.
func (r *Report) Has(kind FindingKind) bool {
	for _, f := range r.Findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// Options tunes Check.
type Options struct {
	// Roots are extra schema roots (a directory laid out as
	// <root>/<area>/<name>.schema.json) used to resolve $refs to schemas that
	// are not under the checked directories. Any ancestor named "schemas" of a
	// checked directory is added automatically, so `schemacheck
	// schemas/conversation` resolves refs into schemas/common.
	Roots []string
	// RequireExamples makes a schema without any example a finding.
	RequireExamples bool
}

// Write prints the report in a human-readable form.
func (r *Report) Write(w io.Writer, verbose bool) {
	if r.OK() {
		fmt.Fprintf(w, "schemacheck: ok - %d schemas, %d valid and %d invalid examples checked\n",
			r.Schemas, r.ValidExamples, r.InvalidExamples)
	} else {
		fmt.Fprintf(w, "schemacheck: FAIL - %d problem(s)\n", len(r.Findings))
		for _, f := range r.Findings {
			fmt.Fprintf(w, "\n  [%s] %s\n", f.Kind, f.Path)
			for _, line := range strings.Split(strings.TrimRight(f.Detail, "\n"), "\n") {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
		fmt.Fprintf(w, "\nchecked: %d schemas compiled, %d valid and %d invalid examples behaved as declared\n",
			r.Schemas, r.ValidExamples, r.InvalidExamples)
	}
	if verbose && len(r.Rejections) > 0 {
		paths := make([]string, 0, len(r.Rejections))
		for p := range r.Rejections {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		fmt.Fprintln(w, "\ninvalid examples and why each is rejected:")
		for _, p := range paths {
			fmt.Fprintf(w, "  %s\n", p)
			for _, line := range strings.Split(strings.TrimRight(r.Rejections[p], "\n"), "\n") {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
	}
}

// loadedSchema is one schema file that parsed and passed the static checks.
type loadedSchema struct {
	path string // absolute
	id   string
	doc  any
}

type checker struct {
	opts   Options
	report *Report
	base   string // directory used to render relative display paths
}

// Check loads every *.schema.json under dirs (skipping examples/ trees),
// compiles each as JSON Schema 2020-12 with format assertions enabled, and for
// each schema <dir>/<name>.schema.json checks the files in
// <dir>/examples/<name>/valid/ all pass and those in
// <dir>/examples/<name>/invalid/ all fail. An error is returned only when the
// working directory cannot be determined; everything else is a Finding.
func Check(dirs []string, opts Options) (*Report, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	c := &checker{opts: opts, report: &Report{Rejections: map[string]string{}}, base: wd}

	var schemaFiles []string
	exampleParents := map[string]bool{}
	roots := map[string]bool{}
	for _, r := range opts.Roots {
		if abs, err := filepath.Abs(r); err == nil {
			roots[abs] = true
		}
	}

	seenDir := map[string]bool{}
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			c.add(KindSchema, d, "cannot resolve path: %v", err)
			continue
		}
		if seenDir[abs] {
			continue
		}
		seenDir[abs] = true
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			c.add(KindSchema, abs, "not a directory that exists (check the path)")
			continue
		}
		if root := schemasAncestor(abs); root != "" {
			roots[root] = true
		}
		files, parents, err := walk(abs)
		if err != nil {
			c.add(KindSchema, abs, "cannot read directory: %v", err)
			continue
		}
		if len(files) == 0 {
			c.add(KindSchema, abs, "no *.schema.json files found (typo in the path?)")
		}
		schemaFiles = append(schemaFiles, files...)
		for _, p := range parents {
			exampleParents[p] = true
		}
	}
	schemaFiles = dedupe(schemaFiles)

	// Load and statically check every schema; register the good ones by $id.
	var loaded []loadedSchema
	byID := map[string]string{}
	for _, f := range schemaFiles {
		ls, ok := c.loadSchema(f)
		if !ok {
			continue
		}
		if prev, dup := byID[ls.id]; dup {
			c.add(KindSchema, f, "duplicate $id %s (also declared by %s)", ls.id, c.rel(prev))
			continue
		}
		byID[ls.id] = f
		loaded = append(loaded, ls)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(urnLoader{roots: sortedKeys(roots)})
	registered := map[string]bool{}
	for _, ls := range loaded {
		if err := compiler.AddResource(ls.id, ls.doc); err != nil {
			c.add(KindSchema, ls.path, "cannot register schema: %v", err)
			continue
		}
		registered[ls.id] = true
	}

	compiled := map[string]*jsonschema.Schema{}
	for _, ls := range loaded {
		if !registered[ls.id] {
			continue
		}
		sch, err := compiler.Compile(ls.id)
		if err != nil {
			c.add(KindSchema, ls.path, "does not compile as JSON Schema 2020-12:\n%v", err)
			continue
		}
		compiled[ls.path] = sch
		c.report.Schemas++
	}

	// Examples, per schema.
	stems := map[string]map[string]bool{} // dir -> schema stems discovered there
	for _, f := range schemaFiles {
		dir := filepath.Dir(f)
		if stems[dir] == nil {
			stems[dir] = map[string]bool{}
		}
		stems[dir][schemaStem(f)] = true
	}
	for _, ls := range loaded {
		sch := compiled[ls.path]
		if sch == nil {
			continue // already reported
		}
		c.checkExamples(ls.path, sch)
	}

	// Examples that belong to no schema.
	parents := sortedKeys(exampleParents)
	for _, dir := range parents {
		c.checkOrphans(dir, stems[dir])
	}

	sort.SliceStable(c.report.Findings, func(i, j int) bool {
		a, b := c.report.Findings[i], c.report.Findings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Kind < b.Kind
	})
	return c.report, nil
}

func (c *checker) add(kind FindingKind, path, format string, args ...any) {
	c.report.Findings = append(c.report.Findings, Finding{
		Kind:   kind,
		Path:   c.rel(path),
		Detail: fmt.Sprintf(format, args...),
	})
}

// rel renders a path relative to the working directory, with forward slashes.
func (c *checker) rel(p string) string {
	if filepath.IsAbs(p) {
		if r, err := filepath.Rel(c.base, p); err == nil && !strings.HasPrefix(r, "..") {
			p = r
		}
	}
	return filepath.ToSlash(p)
}

// loadSchema reads one schema file and applies the static checks: valid JSON
// object, declared 2020-12, a conforming and path-consistent $id.
func (c *checker) loadSchema(path string) (loadedSchema, bool) {
	f, err := os.Open(path)
	if err != nil {
		c.add(KindSchema, path, "cannot open: %v", err)
		return loadedSchema{}, false
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		c.add(KindSchema, path, "not valid JSON: %v", err)
		return loadedSchema{}, false
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		c.add(KindSchema, path, "a schema must be a JSON object")
		return loadedSchema{}, false
	}
	if s, _ := obj["$schema"].(string); s != draft2020 {
		c.add(KindSchema, path, "$schema must be %q (got %q)", draft2020, s)
		return loadedSchema{}, false
	}
	id, _ := obj["$id"].(string)
	m := schemaIDPattern.FindStringSubmatch(id)
	if m == nil {
		c.add(KindSchema, path, "$id %q does not match urn:alixai:schema:<area>:<name>:v<N>", id)
		return loadedSchema{}, false
	}
	// schemas/<area>/<name>.schema.json must carry the matching id: the $ref
	// loader finds schemas by this convention.
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) == "schemas" {
		if area, name := filepath.Base(dir), schemaStem(path); m[1] != area || m[2] != name {
			c.add(KindSchema, path, "$id %q does not match its location (expected urn:alixai:schema:%s:%s:v<N>)", id, area, name)
			return loadedSchema{}, false
		}
	}
	return loadedSchema{path: path, id: id, doc: doc}, true
}

// checkExamples runs the valid/ and invalid/ examples of one schema.
func (c *checker) checkExamples(schemaPath string, sch *jsonschema.Schema) {
	exDir := filepath.Join(filepath.Dir(schemaPath), "examples", schemaStem(schemaPath))
	entries, err := os.ReadDir(exDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if c.opts.RequireExamples {
				c.add(KindNoExamples, schemaPath, "no examples: expected %s/{valid,invalid}/*.json", c.rel(exDir))
			}
			return
		}
		c.add(KindLayout, exDir, "cannot read examples directory: %v", err)
		return
	}
	for _, e := range entries {
		if e.Name() == "valid" && e.IsDir() || e.Name() == "invalid" && e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		c.add(KindLayout, filepath.Join(exDir, e.Name()),
			"unexpected entry: %s may contain only the directories valid/ and invalid/", c.rel(exDir))
	}

	total := 0
	for _, kind := range []string{"valid", "invalid"} {
		files := c.exampleFiles(filepath.Join(exDir, kind))
		total += len(files)
		for _, ef := range files {
			inst, err := readJSON(ef)
			if err != nil {
				c.add(KindExampleUnreadable, ef, "cannot parse the example as JSON (it must be valid JSON even when it is an invalid example): %v", err)
				continue
			}
			verr := sch.Validate(inst)
			switch {
			case kind == "valid" && verr != nil:
				c.add(KindValidRejected, ef, "the example is in valid/ but %s rejects it:\n%s", c.rel(schemaPath), verr)
			case kind == "valid":
				c.report.ValidExamples++
			case verr == nil:
				c.add(KindInvalidAccepted, ef, "the example is in invalid/ but %s accepts it", c.rel(schemaPath))
			default:
				c.report.InvalidExamples++
				c.report.Rejections[c.rel(ef)] = verr.Error()
			}
		}
	}
	if total == 0 && c.opts.RequireExamples {
		c.add(KindNoExamples, schemaPath, "%s holds no *.json examples", c.rel(exDir))
	}
}

// exampleFiles lists the *.json files directly inside dir. A missing directory
// yields none; subdirectories and other non-hidden files are layout findings.
func (c *checker) exampleFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "."):
		case e.IsDir():
			c.add(KindLayout, filepath.Join(dir, e.Name()), "examples must sit directly in %s, not in a subdirectory", c.rel(dir))
		case strings.HasSuffix(e.Name(), ".json"):
			out = append(out, filepath.Join(dir, e.Name()))
		default:
			c.add(KindLayout, filepath.Join(dir, e.Name()), "not a .json file; only *.json examples are allowed in %s", c.rel(dir))
		}
	}
	sort.Strings(out)
	return out
}

// checkOrphans flags examples/<x>/ directories with no <x>.schema.json beside
// the examples directory, and stray files directly in examples/.
func (c *checker) checkOrphans(dir string, stems map[string]bool) {
	exRoot := filepath.Join(dir, "examples")
	entries, err := os.ReadDir(exRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "."):
		case !e.IsDir():
			c.add(KindLayout, filepath.Join(exRoot, e.Name()), "unexpected file: %s may contain only <schema-name>/ directories", c.rel(exRoot))
		case !stems[e.Name()]:
			c.add(KindLayout, filepath.Join(exRoot, e.Name()),
				"orphan examples: there is no %s.schema.json in %s", e.Name(), c.rel(dir))
		}
	}
}

// walk returns the schema files under root (skipping examples/, hidden and
// node_modules directories) and the parents of every examples/ directory met.
func walk(root string) (schemas, exampleParents []string, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		name := d.Name()
		if d.IsDir() {
			switch {
			case name == "examples":
				exampleParents = append(exampleParents, filepath.Dir(p))
				return filepath.SkipDir
			case p != root && (strings.HasPrefix(name, ".") || name == "node_modules"):
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".schema.json") {
			schemas = append(schemas, p)
		}
		return nil
	})
	sort.Strings(schemas)
	return schemas, exampleParents, err
}

// schemasAncestor returns the nearest directory named "schemas" that is dir or
// one of its ancestors, or "".
func schemasAncestor(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if filepath.Base(d) == "schemas" {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func schemaStem(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".schema.json")
}

func readJSON(path string) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jsonschema.UnmarshalJSON(f)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// urnLoader resolves urn:alixai:schema:<area>:<name>:v<N> references to
// <root>/<area>/<name>.schema.json under the known schema roots. It is the only
// loader: file: and network references are refused, so checking is hermetic.
type urnLoader struct {
	roots []string
}

func (l urnLoader) Load(url string) (any, error) {
	m := schemaIDPattern.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("only urn:alixai:schema:<area>:<name>:v<N> references can be resolved; got %q", url)
	}
	for _, root := range l.roots {
		p := filepath.Join(root, m[1], m[2]+".schema.json")
		doc, err := readJSON(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if obj, ok := doc.(map[string]any); !ok || obj["$id"] != url {
			return nil, fmt.Errorf("%s does not declare $id %q", p, url)
		}
		return doc, nil
	}
	return nil, fmt.Errorf("no schema file for %s under %s", url, strings.Join(l.roots, ", "))
}
