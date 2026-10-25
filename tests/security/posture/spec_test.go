package posturesec

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

// ---- the OpenAPI document ----------------------------------------------------

const specRoot = "schemas/api"

// specOp is one operation of the public API as the document defines it.
type specOp struct {
	Method, Path, OperationID string
	Op                        map[string]any
	// File is the document the operation was defined in (a fragment or the root).
	File string
}

func (o specOp) Route() string { return o.Method + " " + o.Path }

// specDocs loads and caches the YAML and JSON documents of the specification.
type specDocs struct {
	t     testing.TB
	root  string
	cache map[string]any
	ids   map[string]string
}

func newSpecDocs(t testing.TB) *specDocs {
	t.Helper()
	return &specDocs{t: t, root: filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(specRoot)), cache: map[string]any{}}
}

func (s *specDocs) load(file string) any {
	s.t.Helper()
	file = filepath.Clean(file)
	if d, ok := s.cache[file]; ok {
		return d
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		s.t.Fatalf("reading %s: %v", file, err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		s.t.Fatalf("parsing %s: %v", file, err)
	}
	s.cache[file] = doc
	return doc
}

// operations returns every operation of the document: the root's own paths and those of
// every area fragment x-alixai-areas lists. It fails on an area fragment that is not
// listed, so a fragment cannot hide from the inventory.
func (s *specDocs) operations() []specOp {
	t := s.t
	t.Helper()
	rootFile := filepath.Join(s.root, "openapi.yaml")
	root, _ := s.load(rootFile).(map[string]any)
	if root == nil {
		t.Fatal("the root document is not a mapping")
	}
	var ops []specOp
	add := func(file string, paths map[string]any) {
		for p, item := range paths {
			m, _ := item.(map[string]any)
			for method, op := range m {
				switch strings.ToUpper(method) {
				case "GET", "POST", "PUT", "PATCH", "DELETE":
					om, _ := op.(map[string]any)
					id, _ := om["operationId"].(string)
					ops = append(ops, specOp{Method: strings.ToUpper(method), Path: p, OperationID: id, Op: om, File: file})
				}
			}
		}
	}
	rp, _ := root["paths"].(map[string]any)
	add(rootFile, rp)
	areas, _ := root["x-alixai-areas"].(map[string]any)
	listed := map[string]bool{}
	for name, v := range areas {
		ref, _ := v.(map[string]any)["$ref"].(string)
		if ref == "" {
			t.Fatalf("area %s has no $ref", name)
		}
		listed[filepath.Base(ref)] = true
		file := filepath.Join(s.root, filepath.FromSlash(ref))
		frag, _ := s.load(file).(map[string]any)
		fp, _ := frag["paths"].(map[string]any)
		add(file, fp)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "paths"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !listed[e.Name()] {
			t.Fatalf("paths/%s is not listed in x-alixai-areas: a fragment the root does not list is never served", e.Name())
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Route() < ops[j].Route() })
	if len(ops) < 20 {
		t.Fatalf("the specification holds only %d operations", len(ops))
	}
	return ops
}

// deref follows a $ref (a pointer in the same document, in another document, or both).
func (s *specDocs) deref(file string, node any) (string, any) {
	for i := 0; i < 32; i++ {
		m, ok := node.(map[string]any)
		if !ok {
			return file, node
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return file, node
		}
		target, pointer, _ := strings.Cut(ref, "#")
		next := file
		switch {
		case strings.HasPrefix(target, "urn:"):
			next = s.urn(target)
		case target != "":
			next = filepath.Join(filepath.Dir(file), filepath.FromSlash(target))
		}
		cur := s.load(next)
		if pointer != "" {
			for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
				part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
				mm, ok := cur.(map[string]any)
				if !ok {
					s.t.Fatalf("%s: the pointer %q of %s leaves the document", file, pointer, ref)
				}
				if cur, ok = mm[part]; !ok {
					s.t.Fatalf("%s: $ref %q has no member %q", file, ref, part)
				}
			}
		}
		file, node = next, cur
	}
	s.t.Fatalf("%s: $ref chain too long", file)
	return "", nil
}

// urn resolves a schema $id (urn:alixai:schema:...) to the file that declares it.
func (s *specDocs) urn(id string) string {
	s.t.Helper()
	if s.ids == nil {
		s.ids = map[string]string{}
		schemas := filepath.Join(filepath.Dir(s.root))
		_ = filepath.WalkDir(schemas, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".schema.json") {
				return nil
			}
			if m, ok := s.load(p).(map[string]any); ok {
				if v, ok := m["$id"].(string); ok {
					s.ids[v] = p
				}
			}
			return nil
		})
	}
	p, ok := s.ids[id]
	if !ok {
		s.t.Fatalf("no schema declares the $id %s", id)
	}
	return p
}

// bodySchema is one media type of a request body with its resolved schema.
type bodySchema struct {
	Media string
	File  string
	// Schema is nil when the media type declares none.
	Schema any
}

func (s *specDocs) requestBodies(o specOp) []bodySchema {
	file, rb := s.deref(o.File, o.Op["requestBody"])
	m, _ := rb.(map[string]any)
	if m == nil {
		return nil
	}
	content, _ := m["content"].(map[string]any)
	var out []bodySchema
	for media, v := range content {
		mm, _ := v.(map[string]any)
		b := bodySchema{Media: media}
		if sc, ok := mm["schema"]; ok {
			b.File, b.Schema = s.deref(file, sc)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Media < out[j].Media })
	return out
}

// ---- the free-form definition detector ---------------------------------------

// definitionNames are the member names under which a client could hand the service a
// server, tool, agent or command definition. A request body schema that declares one,
// at any depth, is a route that accepts a free-form definition.
var definitionNames = map[string]bool{
	"command": true, "cmd": true, "args": true, "argv": true, "exec": true, "shell": true, "script": true, "code": true,
	"mcp": true, "mcp_server": true, "mcp_servers": true, "mcp_url": true, "mcp_tool": true, "mcpservers": true,
	"server": true, "servers": true, "tool": true, "tools": true, "tool_definition": true, "tool_definitions": true,
	"agent": true, "agents": true, "agent_definition": true, "transport": true,
	"url": true, "base_url": true, "baseurl": true, "endpoint": true, "endpoints": true, "uri": true, "webhook": true,
	"openapi": true, "openapi_spec": true, "spec": true, "manifest": true, "definition": true, "plugin": true, "plugins": true,
	"image": true, "container": true, "model_url": true, "gallery": true, "backend": true, "backends": true,
}

// freeform walks a resolved request body schema and describes every way it accepts a
// definition nobody declared: a member whose name is in definitionNames, a member
// accepted by pattern, and an object that declares no members and does not forbid others.
func (s *specDocs) freeform(file string, node any) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(file string, node any, where string, depth int)
	walk = func(file string, node any, where string, depth int) {
		if depth > 40 {
			out = append(out, where+": nesting too deep to check")
			return
		}
		file, node = s.deref(file, node)
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		key := fmt.Sprintf("%s|%p", file, m)
		if seen[key] {
			return
		}
		seen[key] = true
		props, _ := m["properties"].(map[string]any)
		for name, sub := range props {
			if definitionNames[strings.ToLower(name)] {
				out = append(out, fmt.Sprintf("%s declares the member %q", where, name))
			}
			walk(file, sub, where+"."+name, depth+1)
		}
		if pp, ok := m["patternProperties"].(map[string]any); ok {
			for pat := range pp {
				out = append(out, fmt.Sprintf("%s accepts members by pattern %q", where, pat))
			}
		}
		ap, hasAP := m["additionalProperties"]
		if m["type"] == "object" || props != nil {
			switch v := ap.(type) {
			case bool:
				if v {
					out = append(out, where+" accepts any additional member")
				}
			case map[string]any:
				walk(file, v, where+".*", depth+1)
			case nil:
				if !hasAP && len(props) == 0 {
					out = append(out, where+" is an object that declares no member and does not forbid others")
				}
			}
		}
		if items, ok := m["items"]; ok {
			walk(file, items, where+"[]", depth+1)
		}
		for _, k := range []string{"allOf", "anyOf", "oneOf"} {
			if list, ok := m[k].([]any); ok {
				for i, sub := range list {
					walk(file, sub, fmt.Sprintf("%s.%s[%d]", where, k, i), depth+1)
				}
			}
		}
	}
	walk(file, node, "body", 0)
	sort.Strings(out)
	return out
}
