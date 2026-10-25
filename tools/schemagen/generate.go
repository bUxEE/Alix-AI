package main

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Config says where the schemas are and where the output goes.
type Config struct {
	// Schemas is the schemas root: <area>/<name>.schema.json lives directly
	// inside it.
	Schemas fs.FS
	// SchemasRel is the slash path of that root relative to the repository root
	// ("schemas"); it only appears in generated headers.
	SchemasRel string
	// ModulePath is the Go module path, used to import generated packages from
	// each other.
	ModulePath string
	// GoOut and TSOut are the slash paths, relative to the repository root, of
	// the Go and TypeScript output directories.
	GoOut string
	TSOut string
}

// Generate renders every schema area as Go and TypeScript. The result maps a
// slash path relative to the repository root to the file's content. It is a
// pure function of the schema files: the same input always yields the same
// bytes (nothing is time-, path- or map-order-dependent).
//
// Generation never reads the existing output, and nothing here imports it: a
// stale or broken pkg/contracts/gen cannot stop it from being regenerated.
func Generate(cfg Config) (map[string][]byte, error) {
	reg, err := loadRegistry(cfg.Schemas, cfg.SchemasRel)
	if err != nil {
		return nil, err
	}
	b := newBuilder(reg)
	if err := b.build(); err != nil {
		return nil, err
	}
	decls := b.sorted()
	if err := checkAreaCycles(decls); err != nil {
		return nil, err
	}
	if err := checkSymbols(decls); err != nil {
		return nil, err
	}

	byFile := map[string][]*Decl{}
	for _, d := range decls {
		byFile[d.File.key()] = append(byFile[d.File.key()], d)
	}

	goModule := cfg.ModulePath + "/" + cfg.GoOut
	out := map[string][]byte{}
	var tsAreas []string
	for _, area := range reg.areas {
		hasUnion := false
		var tsStems []string
		for _, f := range reg.byArea[area] {
			fd := byFile[f.key()]
			if len(fd) == 0 {
				continue
			}
			g := newGoFile(area, goModule)
			t := newTSFile(area, f.Stem, fd)
			for _, d := range fd {
				emitGoDecl(g, d)
				emitTSDecl(t, d)
				hasUnion = hasUnion || d.Kind == dUnion
			}
			src, err := g.source(f.Rel)
			if err != nil {
				return nil, err
			}
			out[path.Join(cfg.GoOut, goPkgName(area), snake(f.Stem)+".gen.go")] = src
			out[path.Join(cfg.TSOut, area, f.Stem+".ts")] = t.source(f.Rel)
			tsStems = append(tsStems, f.Stem)
		}
		if len(tsStems) == 0 {
			continue
		}
		doc, err := emitGoPackageDoc(area, hasUnion)
		if err != nil {
			return nil, err
		}
		out[path.Join(cfg.GoOut, goPkgName(area), "doc.gen.go")] = doc
		out[path.Join(cfg.TSOut, area, "index.ts")] = emitTSAreaIndex(area, tsStems)
		tsAreas = append(tsAreas, area)
	}
	if len(tsAreas) > 0 {
		out[path.Join(cfg.TSOut, "index.ts")] = emitTSRootIndex(tsAreas)
	}
	return out, nil
}

// deps lists the declarations d refers to.
func deps(d *Decl) []*Decl {
	var out []*Decl
	var walk func(t *Type)
	walk = func(t *Type) {
		if t == nil {
			return
		}
		switch t.Kind {
		case tNamed:
			out = append(out, t.Decl)
		case tArray, tMap, tNullable:
			walk(t.Elem)
		case tLoose:
			for _, m := range t.Members {
				walk(m)
			}
		}
	}
	switch d.Kind {
	case dStruct:
		for _, f := range d.Fields {
			walk(f.Type)
		}
	case dUnion:
		for _, v := range d.Variants {
			out = append(out, v.Decl)
		}
	case dDefined, dAlias:
		walk(d.Under)
	}
	return out
}

// checkAreaCycles rejects schemas whose areas refer to each other in a circle:
// Go packages cannot import each other, so such a layout cannot be generated.
func checkAreaCycles(decls []*Decl) error {
	edges := map[string]map[string]bool{}
	for _, d := range decls {
		for _, dep := range deps(d) {
			if dep.File.Area == d.File.Area {
				continue
			}
			if edges[d.File.Area] == nil {
				edges[d.File.Area] = map[string]bool{}
			}
			edges[d.File.Area][dep.File.Area] = true
		}
	}
	const (
		white = iota
		grey
		black
	)
	state := map[string]int{}
	var stack []string
	var visit func(a string) error
	visit = func(a string) error {
		state[a] = grey
		stack = append(stack, a)
		next := make([]string, 0, len(edges[a]))
		for n := range edges[a] {
			next = append(next, n)
		}
		sort.Strings(next)
		for _, n := range next {
			switch state[n] {
			case grey:
				return fmt.Errorf("schema areas refer to each other in a cycle (%s -> %s): Go packages cannot import each other", strings.Join(stack, " -> "), n)
			case white:
				if err := visit(n); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[a] = black
		return nil
	}
	areas := make([]string, 0, len(edges))
	for a := range edges {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	for _, a := range areas {
		if state[a] == white {
			if err := visit(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSymbols fails when two declarations of one area would share a name
// (types, enum constants, union interfaces and the strict decoder all live in
// one Go package and one TypeScript index). A new schema that collides with an
// existing name must rename its definition: names never change to make room.
func checkSymbols(decls []*Decl) error {
	seen := map[string]map[string]string{} // area -> symbol -> origin
	claim := func(d *Decl, sym string) error {
		area := d.File.Area
		if seen[area] == nil {
			seen[area] = map[string]string{"decodeStrict": "the strict decoder"}
		}
		origin := d.schemaRef()
		if prev, dup := seen[area][sym]; dup {
			return fmt.Errorf("name %s in area %s is produced by both %s and %s: rename one definition in its schema", sym, area, prev, origin)
		}
		seen[area][sym] = origin
		return nil
	}
	for _, d := range decls {
		if err := claim(d, d.Name); err != nil {
			return err
		}
		switch d.Kind {
		case dEnum:
			for _, v := range d.Values {
				if err := claim(d, v.Name); err != nil {
					return err
				}
			}
		case dUnion:
			if err := claim(d, d.Name+"Value"); err != nil {
				return err
			}
		}
	}
	return nil
}
