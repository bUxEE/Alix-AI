package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tsFile accumulates the body and the imports of one generated TypeScript file.
type tsFile struct {
	area     string
	stem     string
	declared map[string]bool              // names declared in this file
	taken    map[string]string            // local name -> origin, for imported names
	imports  map[string]map[string]string // module specifier -> imported name -> local name
	body     strings.Builder
}

func newTSFile(area, stem string, decls []*Decl) *tsFile {
	t := &tsFile{
		area: area, stem: stem,
		declared: map[string]bool{},
		taken:    map[string]string{},
		imports:  map[string]map[string]string{},
	}
	for _, d := range decls {
		t.declared[d.Name] = true
	}
	return t
}

func (t *tsFile) w(format string, args ...any) { fmt.Fprintf(&t.body, format, args...) }

// ref spells a declaration, adding an import when it lives in another file.
// When the imported name would clash with a name declared in this file or
// imported from elsewhere (application-package/defs declares its own
// ErrorCode, common declares another), the import is aliased with the area
// name in front: ErrorCode as CommonErrorCode.
func (t *tsFile) ref(d *Decl) string {
	if d.File.Area == t.area && d.File.Stem == t.stem {
		return d.Name
	}
	spec := "./" + d.File.Stem + ".js"
	if d.File.Area != t.area {
		spec = "../" + d.File.Area + "/" + d.File.Stem + ".js"
	}
	if t.imports[spec] == nil {
		t.imports[spec] = map[string]string{}
	}
	if local, ok := t.imports[spec][d.Name]; ok {
		return local
	}
	origin := d.File.key() + "#" + d.Name
	local := d.Name
	if t.declared[local] || (t.taken[local] != "" && t.taken[local] != origin) {
		local = pascal(d.File.Area) + d.Name
		if t.declared[local] || (t.taken[local] != "" && t.taken[local] != origin) {
			local = pascal(d.File.Area) + pascal(d.File.Stem) + d.Name
		}
	}
	t.taken[local] = origin
	t.imports[spec][d.Name] = local
	return local
}

var tsIdent = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func tsPropName(name string) string {
	if tsIdent.MatchString(name) {
		return name
	}
	return strconv.Quote(name)
}

// typeExpr renders a use-site type. Compound forms are parenthesised where
// they sit inside an array.
func (t *tsFile) typeExpr(ty *Type) string {
	switch ty.Kind {
	case tString:
		return "string"
	case tInteger, tNumber:
		return "number"
	case tBool:
		return "boolean"
	case tNull:
		return "null"
	case tAny:
		return "unknown"
	case tObjectAny:
		return "Record<string, unknown>"
	case tConst:
		switch ty.Const.Kind {
		case tString:
			return strconv.Quote(ty.Const.Str)
		case tBool:
			return strconv.FormatBool(ty.Const.Bool)
		}
		return ty.Const.Num
	case tArray:
		inner := t.typeExpr(ty.Elem)
		if strings.Contains(inner, " | ") {
			inner = "(" + inner + ")"
		}
		return inner + "[]"
	case tMap:
		return "Record<string, " + t.typeExpr(ty.Elem) + ">"
	case tNullable:
		return t.typeExpr(ty.Elem) + " | null"
	case tLoose:
		parts := make([]string, len(ty.Members))
		for i, m := range ty.Members {
			parts[i] = t.typeExpr(m)
		}
		return strings.Join(parts, " | ")
	case tNamed:
		return t.ref(ty.Decl)
	}
	panic("schemagen: unknown type kind")
}

// tsComment renders doc lines as a JSDoc block.
func tsComment(indent string, lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(indent + "/**\n")
	for _, l := range lines {
		l = strings.ReplaceAll(l, "*/", `*\/`)
		if l == "" {
			b.WriteString(indent + " *\n")
		} else {
			b.WriteString(indent + " * " + l + "\n")
		}
	}
	b.WriteString(indent + " */\n")
	return b.String()
}

func emitTSDecl(t *tsFile, d *Decl) {
	t.w("%s", tsComment("", declDoc(d, 96)))
	switch d.Kind {
	case dStruct:
		if len(d.Fields) == 0 {
			t.w("export type %s = Record<string, never>;\n\n", d.Name)
			return
		}
		t.w("export interface %s {\n", d.Name)
		for i, f := range d.Fields {
			if i > 0 {
				t.w("\n")
			}
			doc := docLines(88, f.Doc)
			if f.Authority != "" {
				if len(doc) > 0 {
					doc = append(doc, "")
				}
				doc = append(doc, "@authority "+f.Authority)
			}
			t.w("%s", tsComment("  ", doc))
			opt := ""
			if !f.Required {
				opt = "?"
			}
			t.w("  %s%s: %s;\n", tsPropName(f.JSON), opt, t.typeExpr(f.Type))
		}
		t.w("}\n\n")
	case dEnum:
		parts := make([]string, len(d.Values))
		for i, v := range d.Values {
			if d.EnumKind == tInteger {
				parts[i] = v.Raw
			} else {
				parts[i] = strconv.Quote(v.Raw)
			}
		}
		t.w("export type %s =\n", d.Name)
		for i, p := range parts {
			end := ""
			if i == len(parts)-1 {
				end = ";"
			}
			t.w("  | %s%s\n", p, end)
		}
		t.w("\n")
	case dUnion:
		t.w("export type %s =\n", d.Name)
		for i, v := range d.Variants {
			end := ""
			if i == len(d.Variants)-1 {
				end = ";"
			}
			t.w("  | %s%s\n", t.ref(v.Decl), end)
		}
		t.w("\n")
	case dDefined, dAlias:
		t.w("export type %s = %s;\n\n", d.Name, t.typeExpr(d.Under))
	}
}

// source finishes a file: header, imports, body.
func (t *tsFile) source(schemaRel string) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "// %s from %s. DO NOT EDIT.\n\n", generatedMarker, schemaRel)
	specs := make([]string, 0, len(t.imports))
	for s := range t.imports {
		specs = append(specs, s)
	}
	sort.Strings(specs)
	for _, s := range specs {
		names := make([]string, 0, len(t.imports[s]))
		for n := range t.imports[s] {
			names = append(names, n)
		}
		sort.Strings(names)
		for i, n := range names {
			if local := t.imports[s][n]; local != n {
				names[i] = n + " as " + local
			}
		}
		fmt.Fprintf(&out, "import type { %s } from %s;\n", strings.Join(names, ", "), strconv.Quote(s))
	}
	if len(specs) > 0 {
		out.WriteString("\n")
	}
	out.WriteString(strings.TrimRight(t.body.String(), "\n"))
	out.WriteString("\n")
	return []byte(out.String())
}

// emitTSAreaIndex renders <area>/index.ts, re-exporting every file of the area.
func emitTSAreaIndex(area string, stems []string) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "// %s. DO NOT EDIT.\n\n", generatedMarker)
	for _, s := range stems {
		fmt.Fprintf(&out, "export * from %s;\n", strconv.Quote("./"+s+".js"))
	}
	return []byte(out.String())
}

// emitTSRootIndex renders index.ts, one namespace per area so identically named
// types of different areas never clash.
func emitTSRootIndex(areas []string) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "// %s. DO NOT EDIT.\n\n", generatedMarker)
	for _, a := range areas {
		fmt.Fprintf(&out, "export * as %s from %s;\n", tsNamespace(a), strconv.Quote("./"+a+"/index.js"))
	}
	return []byte(out.String())
}

// tsNamespace turns an area name into a camelCase identifier:
// application-package becomes applicationPackage.
func tsNamespace(area string) string {
	parts := strings.Split(area, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}
