package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	frozenRoot   = "schemas/_frozen"
	frozenSuffix = ".frozen.json"
	openapiFile  = "openapi" + frozenSuffix
)

// Snapshot is the public contract surface at one moment: every public JSON Schema and the merged OpenAPI document.
type Snapshot struct {
	// Schemas maps "<area>/<name>" to the decoded schema; Raw keeps the original bytes for freezing.
	Schemas map[string]any
	Raw     map[string][]byte
	OpenAPI object
}

// schemaKey is "<area>/<name>" for schemas/<area>/<name>.schema.json (or <name>.json for a vendored schema).
func schemaKey(area, file string) string {
	name := strings.TrimSuffix(file, ".json")
	name = strings.TrimSuffix(name, ".schema")
	return area + "/" + name
}

// LoadCurrent reads the public contracts of the working tree below repo.
func LoadCurrent(repo string) (*Snapshot, error) {
	s := &Snapshot{Schemas: map[string]any{}, Raw: map[string][]byte{}}
	root := filepath.Join(repo, "schemas")
	areas, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	for _, a := range areas {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") || strings.HasPrefix(a.Name(), "_") {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, a.Name(), f.Name()))
			if err != nil {
				return nil, err
			}
			doc, err := decodeJSON(data)
			if err != nil {
				return nil, fmt.Errorf("schemas/%s/%s: %w", a.Name(), f.Name(), err)
			}
			key := schemaKey(a.Name(), f.Name())
			s.Schemas[key] = doc
			s.Raw[key] = data
		}
	}
	oa, err := mergeOpenAPI(filepath.Join(root, "api"))
	if err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	s.OpenAPI = oa
	return s, nil
}

// LoadFrozen reads a frozen tree (schemas/_frozen/<version>). A missing or empty tree is an error: an absent freeze must
// never read as "nothing to protect".
func LoadFrozen(dir string) (*Snapshot, error) {
	s := &Snapshot{Schemas: map[string]any{}, Raw: map[string][]byte{}}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("no frozen contract set at %s: %w (run `go run ./tools/compat freeze` once to create it)", dir, err)
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), frozenSuffix) {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := decodeJSON(data)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if rel == openapiFile {
			m, ok := doc.(object)
			if !ok {
				return fmt.Errorf("%s: not an object", rel)
			}
			s.OpenAPI = m
			return nil
		}
		area, file := path.Split(rel)
		key := strings.TrimSuffix(area, "/") + "/" + strings.TrimSuffix(file, frozenSuffix)
		s.Schemas[key] = doc
		s.Raw[key] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(s.Schemas) == 0 || s.OpenAPI == nil {
		return nil, fmt.Errorf("the frozen set at %s is incomplete (%d schemas, openapi present: %v)", dir, len(s.Schemas), s.OpenAPI != nil)
	}
	return s, nil
}

// WriteFrozen replaces the frozen tree at dir with the snapshot.
func WriteFrozen(dir string, s *Snapshot) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	for key, raw := range s.Raw {
		p := filepath.Join(dir, filepath.FromSlash(key)+frozenSuffix)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.OpenAPI); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, openapiFile), buf.Bytes(), 0o644)
}

// Compare classifies every difference between the frozen snapshot and the current one.
func Compare(frozen, current *Snapshot) Report {
	var r Report
	keys := map[string]bool{}
	for k := range frozen.Schemas {
		keys[k] = true
	}
	for k := range current.Schemas {
		keys[k] = true
	}
	for _, k := range sortedKeys(keys) {
		subject := "schema " + k
		o, oHas := frozen.Schemas[k]
		n, nHas := current.Schemas[k]
		switch {
		case oHas && !nHas:
			r.Changes = append(r.Changes, Change{Severity: Breaking, Subject: subject, Code: "schema_removed", Message: "the public schema " + k + " was removed"})
		case !oHas && nHas:
			r.Changes = append(r.Changes, Change{Severity: Additive, Subject: subject, Code: "schema_added", Message: "the public schema " + k + " is new and not yet frozen"})
		default:
			r.Changes = append(r.Changes, diffSchemaDocs(subject, o, n)...)
		}
	}
	r.Changes = append(r.Changes, diffOpenAPI(frozen.OpenAPI, current.OpenAPI)...)
	r.sort()
	return r
}

var errNoAreas = errors.New("x-alixai-areas is missing")

// mergeOpenAPI assembles schemas/api/openapi.yaml and its area fragments into one document, the way the gateway spec
// tests do: paths and components are unioned, a duplicate is an error, and relative $refs are rewritten so the merged
// document does not depend on where the fragments live.
func mergeOpenAPI(apiDir string) (object, error) {
	root, err := readYAML(filepath.Join(apiDir, "openapi.yaml"))
	if err != nil {
		return nil, err
	}

	areas := asObject(root["x-alixai-areas"])
	if len(areas) == 0 {
		return nil, errNoAreas
	}
	delete(root, "x-alixai-areas")
	rewriteRefs(root, "schemas/api")
	paths := asObject(root["paths"])
	root["paths"] = paths
	comps := asObject(root["components"])
	root["components"] = comps

	names := make([]string, 0, len(areas))
	for name := range areas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ref, _ := asObject(areas[name])["$ref"].(string)
		if ref == "" {
			return nil, fmt.Errorf("area %s has no $ref", name)
		}
		frag, err := readYAML(filepath.Join(apiDir, filepath.FromSlash(ref)))
		if err != nil {
			return nil, err
		}
		rewriteRefs(frag, path.Dir(path.Join("schemas/api", ref)))
		for p, item := range asObject(frag["paths"]) {
			existing, dup := paths[p]
			if !dup {
				paths[p] = item
				continue
			}
			em := asObject(existing)
			for k, v := range asObject(item) {
				if _, has := em[k]; has {
					return nil, fmt.Errorf("area %s: %s %s is defined twice", name, k, p)
				}
				em[k] = v
			}
		}
		for section, entries := range asObject(frag["components"]) {
			dst := asObject(comps[section])
			comps[section] = dst
			for k, v := range asObject(entries) {
				if _, dup := dst[k]; dup {
					return nil, fmt.Errorf("area %s: component %s/%s is defined twice", name, section, k)
				}
				dst[k] = v
			}
		}
	}
	// Round-trip through JSON so numbers are json.Number like every other document.
	b, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	doc, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	return doc.(object), nil
}

func readYAML(file string) (object, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	m, ok := normalize(v).(object)
	if !ok {
		return nil, fmt.Errorf("%s: not a mapping", file)
	}
	return m, nil
}

// normalize turns YAML maps with any key type into string-keyed maps.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalize(e)
		}
		return t
	case map[any]any:
		out := make(object, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = normalize(e)
		}
		return out
	case []any:
		for i, e := range t {
			t[i] = normalize(e)
		}
		return t
	}
	return v
}

// rewriteRefs makes every $ref independent of the file it was written in: a reference into openapi.yaml or another
// fragment becomes a local "#/..." one, a reference to a schema file becomes its repository path.
func rewriteRefs(v any, dir string) {
	switch t := v.(type) {
	case object:
		for k, e := range t {
			if s, ok := e.(string); ok && k == "$ref" {
				t[k] = rewriteRef(s, dir)
				continue
			}
			rewriteRefs(e, dir)
		}
	case []any:
		for _, e := range t {
			rewriteRefs(e, dir)
		}
	}
}

func rewriteRef(ref, dir string) string {
	if strings.HasPrefix(ref, "#") || strings.Contains(ref, "://") || strings.HasPrefix(ref, "urn:") {
		return ref
	}
	file, frag, _ := strings.Cut(ref, "#")
	target := path.Clean(path.Join(dir, file))
	if strings.HasSuffix(target, ".yaml") && strings.HasPrefix(target, "schemas/api/") {
		if frag == "" {
			return ref // a whole-fragment reference has no merged equivalent; keep it visible
		}
		return "#" + frag
	}
	if frag != "" {
		return target + "#" + frag
	}
	return target
}
