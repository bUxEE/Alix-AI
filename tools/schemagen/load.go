package main

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// draft2020 is the only dialect AlixAI schemas may declare.
const draft2020 = "https://json-schema.org/draft/2020-12/schema"

// schemaIDPattern is the AlixAI schema id convention
// urn:alixai:schema:<area>:<name>:v<N>, the same one tools/schemacheck enforces.
var schemaIDPattern = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9]+(?:-[a-z0-9]+)*):([a-z0-9]+(?:-[a-z0-9]+)*):v[1-9][0-9]*$`)

// File is one loaded schema file.
type File struct {
	Area string // schemas/<area>/
	Stem string // <stem>.schema.json
	ID   string // the $id
	Rel  string // slash path relative to the repository root, used in headers
	Root *Node
}

// key identifies the file in registries and maps.
func (f *File) key() string { return f.Area + "/" + f.Stem }

// Registry holds every schema file found under the schemas root.
type Registry struct {
	files  map[string]*File   // area/stem -> file
	areas  []string           // sorted
	byArea map[string][]*File // sorted by stem
}

// loadRegistry reads every <area>/<name>.schema.json directly below the root of
// fsys. relPrefix is the slash path of that root inside the repository
// ("schemas"); it only decorates generated headers.
//
// Areas without any *.schema.json file (for example schemas/api, which holds
// OpenAPI YAML) are skipped, and so is every subdirectory of an area
// (examples/ and anything else): only files directly inside an area are schemas.
func loadRegistry(fsys fs.FS, relPrefix string) (*Registry, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read schemas root: %w", err)
	}
	reg := &Registry{files: map[string]*File{}, byArea: map[string][]*File{}}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		area := e.Name()
		files, err := fs.ReadDir(fsys, area)
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", relPrefix, area, err)
		}
		var names []string
		for _, fe := range files {
			if !fe.IsDir() && strings.HasSuffix(fe.Name(), ".schema.json") {
				names = append(names, fe.Name())
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		if !schemaIDPattern.MatchString("urn:alixai:schema:" + area + ":x:v1") {
			return nil, fmt.Errorf("%s/%s: area name must be lower-case letters, digits and single hyphens", relPrefix, area)
		}
		reg.areas = append(reg.areas, area)
		for _, name := range names {
			rel := path.Join(relPrefix, area, name)
			data, err := fs.ReadFile(fsys, path.Join(area, name))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			root, err := parseNode(data)
			if err != nil {
				return nil, fmt.Errorf("%s: not valid JSON: %w", rel, err)
			}
			if !root.isObject() {
				return nil, fmt.Errorf("%s: a schema must be a JSON object", rel)
			}
			if s := root.Get("$schema").str(); s != draft2020 {
				return nil, fmt.Errorf("%s: $schema must be %q (got %q)", rel, draft2020, s)
			}
			stem := strings.TrimSuffix(name, ".schema.json")
			id := root.Get("$id").str()
			m := schemaIDPattern.FindStringSubmatch(id)
			if m == nil || m[1] != area || m[2] != stem {
				return nil, fmt.Errorf("%s: $id %q must be urn:alixai:schema:%s:%s:v<N>", rel, id, area, stem)
			}
			f := &File{Area: area, Stem: stem, ID: id, Rel: rel, Root: root}
			reg.files[f.key()] = f
			reg.byArea[area] = append(reg.byArea[area], f)
		}
	}
	sort.Strings(reg.areas)
	return reg, nil
}

// ref is a parsed $ref: the file it points into and the JSON pointer inside it.
type ref struct {
	file *File
	ptr  string
	node *Node
}

// resolve follows a $ref found in file from. Two forms exist: a pointer inside
// the same file ("#/$defs/x") and a URN of another AlixAI schema with an
// optional pointer ("urn:alixai:schema:common:ids:v1#/$defs/identifier",
// "urn:alixai:schema:common:error:v1#/properties/code"). Anything else (file
// paths, URLs, anchors) is refused: the generator is as hermetic as
// tools/schemacheck.
func (r *Registry) resolve(from *File, refStr string) (ref, error) {
	base, frag, _ := strings.Cut(refStr, "#")
	target := from
	if base != "" {
		m := schemaIDPattern.FindStringSubmatch(base)
		if m == nil {
			return ref{}, fmt.Errorf("$ref %q: only same-file pointers and urn:alixai:schema:<area>:<name>:v<N> references can be resolved", refStr)
		}
		target = r.files[m[1]+"/"+m[2]]
		if target == nil || target.ID != base {
			return ref{}, fmt.Errorf("$ref %q: no schema with that $id under the schemas root", refStr)
		}
	}
	if frag != "" && !strings.HasPrefix(frag, "/") {
		return ref{}, fmt.Errorf("$ref %q: only JSON pointer fragments are supported", refStr)
	}
	n, err := pointerGet(target.Root, frag)
	if err != nil {
		return ref{}, fmt.Errorf("$ref %q: %w", refStr, err)
	}
	return ref{file: target, ptr: frag, node: n}, nil
}

// pointerGet walks a JSON pointer ("" is the document itself).
func pointerGet(root *Node, ptr string) (*Node, error) {
	n := root
	if ptr == "" {
		return n, nil
	}
	for _, seg := range splitPointer(ptr) {
		switch {
		case n.isObject():
			next := n.Get(seg)
			if next == nil {
				return nil, fmt.Errorf("pointer %q: no member %q", ptr, seg)
			}
			n = next
		case n != nil && n.Kind == nArray:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n.Items) {
				return nil, fmt.Errorf("pointer %q: bad array index %q", ptr, seg)
			}
			n = n.Items[i]
		default:
			return nil, fmt.Errorf("pointer %q: cannot descend into %q", ptr, seg)
		}
	}
	return n, nil
}

// splitPointer splits "/a/b~1c" into its unescaped segments.
func splitPointer(ptr string) []string {
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		parts[i] = strings.ReplaceAll(p, "~0", "~")
	}
	return parts
}

// joinPointer appends unescaped segments to a pointer.
func joinPointer(ptr string, segs ...string) string {
	for _, s := range segs {
		s = strings.ReplaceAll(s, "~", "~0")
		s = strings.ReplaceAll(s, "/", "~1")
		ptr += "/" + s
	}
	return ptr
}
