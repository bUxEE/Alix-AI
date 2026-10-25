package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// typeKind is the kind of a type expression used at a use site (a field type,
// an array element, a map value).
type typeKind uint8

const (
	tString typeKind = iota
	tInteger
	tNumber
	tBool
	tNull
	tAny       // unconstrained value: Go any, TypeScript unknown
	tObjectAny // open object: map[string]any, Record<string, unknown>
	tNamed     // a generated declaration (Decl)
	tArray     // Elem[]
	tMap       // map[string]Elem
	tNullable  // Elem or null
	tLoose     // a union the generator cannot discriminate: Go any, TypeScript union of Members
	tConst     // a fixed value (Const)
)

// Type is a use-site type expression.
type Type struct {
	Kind    typeKind
	Decl    *Decl
	Elem    *Type
	Members []*Type
	Const   *Const
}

// Const is the value of a "const" keyword.
type Const struct {
	Kind typeKind // tString, tInteger, tNumber or tBool
	Str  string
	Num  string
	Bool bool
}

// declKind says what a Decl becomes in the target languages.
type declKind uint8

const (
	dStruct  declKind = iota // object with properties: Go struct, TS interface
	dEnum                    // string or integer enum
	dUnion                   // oneOf discriminated by a const property
	dDefined                 // a named primitive or composite (type X string)
	dAlias                   // an alias for another type (type X = Y, type X = any)
)

// Decl is one named, generated declaration: a schema root, a $defs entry, or
// any other schema location that is referenced or needs its own type.
type Decl struct {
	File *File
	Ptr  string // JSON pointer of the schema inside File
	Name string
	Kind declKind
	Doc  string

	// dDefined, dAlias
	Under       *Type
	Constraints string

	// dStruct
	Fields  []*Field
	Overlay bool // has cross-field rules (if/then/else, ...) the type cannot express

	// dEnum
	EnumKind typeKind // tString or tInteger
	Values   []EnumValue

	// dUnion
	Disc     string // discriminator property
	Variants []*Variant
}

// EnumValue is one member of an enum.
type EnumValue struct {
	Raw  string // string value or integer literal
	Name string // Go constant name
}

// Variant is one member of a discriminated union.
type Variant struct {
	Value string // discriminator value
	Decl  *Decl
}

// Field is one property of a struct.
type Field struct {
	JSON      string
	GoName    string
	Type      *Type
	Required  bool
	Doc       string
	Authority string
}

// schemaRef is the "$id#pointer" spelling of a declaration's origin.
func (d *Decl) schemaRef() string {
	if d.Ptr == "" {
		return d.File.ID
	}
	return d.File.ID + "#" + d.Ptr
}

// class is how the generator reads one schema node.
type class uint8

const (
	cAny class = iota
	cRef
	cConst
	cEnum
	cUnion
	cAllOf
	cStruct
	cMap
	cObjectAny
	cArray
	cPrim
	cPrimList
)

// unsupportedKeywords make the generator stop instead of guess: each would
// change the shape of the data in a way the generated types would not show.
var unsupportedKeywords = []string{
	"prefixItems", "patternProperties", "$dynamicRef", "$recursiveRef", "$anchor",
	"unevaluatedItems", "contentSchema", "dependentSchemas",
}

// typeBearing reports whether a oneOf/anyOf/allOf member says something about
// the type of the data (it has $ref, type, const or enum) as opposed to only
// constraining it (required, properties, not, contains, if/then/else ...). The
// same predicate is used for every combinator: members that fail it are
// overlays, which the generated types do not express and runtime schema
// validation keeps enforcing.
func typeBearing(n *Node) bool {
	return n.isObject() && (n.Has("$ref") || n.Has("type") || n.Has("const") || n.Has("enum"))
}

// countTypeBearing counts the type-bearing members of a combinator list.
func countTypeBearing(list *Node) (bearing, total int) {
	if list == nil || list.Kind != nArray {
		return 0, 0
	}
	for _, m := range list.Items {
		if typeBearing(m) {
			bearing++
		}
	}
	return bearing, len(list.Items)
}

var primNames = map[string]typeKind{
	"string": tString, "integer": tInteger, "number": tNumber, "boolean": tBool, "null": tNull,
}

// classify decides how a schema node is read. Overlay-only combinators are
// ignored; mixing type-bearing and overlay members in one list is an error.
func classify(n *Node) (class, error) {
	if n.Kind == nBool {
		return cAny, nil
	}
	if !n.isObject() {
		return 0, fmt.Errorf("a schema must be an object or a boolean")
	}
	for _, kw := range unsupportedKeywords {
		if n.Has(kw) {
			return 0, fmt.Errorf("unsupported keyword %q", kw)
		}
	}
	if n.Has("$ref") {
		if n.Get("$ref").Kind != nString {
			return 0, fmt.Errorf("$ref must be a string")
		}
		return cRef, nil
	}
	if n.Has("const") {
		return cConst, nil
	}
	if n.Has("enum") {
		return cEnum, nil
	}
	hasUnion := false
	for _, kw := range []string{"oneOf", "anyOf"} {
		list := n.Get(kw)
		if list == nil {
			continue
		}
		if list.Kind != nArray {
			return 0, fmt.Errorf("%s must be an array", kw)
		}
		bearing, total := countTypeBearing(list)
		switch {
		case bearing == 0:
			// Overlay only (required-only alternatives and the like).
		case bearing == total:
			if hasUnion {
				return 0, fmt.Errorf("oneOf and anyOf both present as type unions")
			}
			hasUnion = true
		default:
			return 0, fmt.Errorf("%s mixes type-bearing members with overlay-only members", kw)
		}
	}
	if hasUnion {
		return cUnion, nil
	}
	if list := n.Get("allOf"); list != nil {
		if list.Kind != nArray {
			return 0, fmt.Errorf("allOf must be an array")
		}
		if bearing, _ := countTypeBearing(list); bearing > 0 {
			if n.Has("type") || n.Has("properties") {
				return 0, fmt.Errorf("allOf composition next to type/properties is not supported")
			}
			return cAllOf, nil
		}
	}
	t := n.Get("type")
	typeName := ""
	switch {
	case t == nil:
	case t.Kind == nString:
		typeName = t.Str
	case t.Kind == nArray:
		for _, it := range t.Items {
			if _, ok := primNames[it.str()]; !ok {
				return 0, fmt.Errorf("type list may only hold primitive types")
			}
		}
		return cPrimList, nil
	default:
		return 0, fmt.Errorf("type must be a string or an array of strings")
	}
	switch typeName {
	case "object":
		return classifyObject(n)
	case "array":
		return cArray, nil
	case "string", "integer", "number", "boolean", "null":
		return cPrim, nil
	case "":
		if n.Has("properties") {
			return classifyObject(n)
		}
		if n.Has("items") {
			return cArray, nil
		}
		return cAny, nil
	}
	return 0, fmt.Errorf("unsupported type %q", typeName)
}

// classifyObject tells a struct (properties, closed) from a map (a schema for
// every value, keys constrained by propertyNames) from an open object.
func classifyObject(n *Node) (class, error) {
	ap := n.Get("additionalProperties")
	mapLike := ap != nil && (ap.Kind != nBool || ap.Bool)
	if n.Has("properties") {
		if mapLike {
			return 0, fmt.Errorf("an object with both properties and an additionalProperties schema is not supported: use a closed struct or a propertyNames map")
		}
		return cStruct, nil
	}
	if mapLike {
		return cMap, nil
	}
	if ap.isFalse() {
		return cStruct, nil // closed and empty
	}
	return cObjectAny, nil
}

// builder turns the schema registry into declarations.
type builder struct {
	reg        *Registry
	decls      map[string]*Decl
	forced     map[string]string // key -> name chosen by the caller (inline union variants)
	inProgress map[string]bool
}

func newBuilder(reg *Registry) *builder {
	return &builder{
		reg:        reg,
		decls:      map[string]*Decl{},
		forced:     map[string]string{},
		inProgress: map[string]bool{},
	}
}

func declKey(f *File, ptr string) string { return f.key() + "#" + ptr }

// at prefixes an error with the schema location it concerns.
func at(f *File, ptr string, err error) error {
	loc := f.Rel
	if ptr != "" {
		loc += "#" + ptr
	}
	return fmt.Errorf("%s: %w", loc, err)
}

// build declares every schema root and every $defs entry of every file.
func (b *builder) build() error {
	for _, area := range b.reg.areas {
		for _, f := range b.reg.byArea[area] {
			c, err := classify(f.Root)
			if err != nil {
				return at(f, "", err)
			}
			if c != cAny { // a root with no type information is a definitions library
				if _, err := b.declare(f, "", f.Root); err != nil {
					return err
				}
			}
			defs := f.Root.Get("$defs")
			if defs == nil {
				continue
			}
			if !defs.isObject() {
				return at(f, "/$defs", fmt.Errorf("$defs must be an object"))
			}
			for _, k := range defs.Keys {
				if _, err := b.declare(f, joinPointer("", "$defs", k), defs.Vals[k]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// sorted returns every declaration in a stable order: by area, file, then the
// root first and the rest by name.
func (b *builder) sorted() []*Decl {
	out := make([]*Decl, 0, len(b.decls))
	for _, d := range b.decls {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, c := out[i], out[j]
		if a.File.Area != c.File.Area {
			return a.File.Area < c.File.Area
		}
		if a.File.Stem != c.File.Stem {
			return a.File.Stem < c.File.Stem
		}
		if (a.Ptr == "") != (c.Ptr == "") {
			return a.Ptr == ""
		}
		if a.Name != c.Name {
			return a.Name < c.Name
		}
		return a.Ptr < c.Ptr
	})
	return out
}

// structRoot reports whether the file's root schema is a struct, in which case
// its $defs are named after the root (TaskProposalEntityRef) so a name never
// depends on which other schemas exist.
func (b *builder) structRoot(f *File) bool {
	c, err := classify(f.Root)
	return err == nil && c == cStruct
}

// nameFor derives a type name from a schema location.
//
//	root                         PascalCase(file stem)           TaskProposal
//	$defs/x, struct-root file    Root + X                        TaskProposalEntityRef
//	$defs/x, library/union file  X                               Fact, TokenEvent
//	.../properties/p             parent + P                      ErrorCode
//	.../items                    parent + Item
//	.../additionalProperties     parent + Value
func (b *builder) nameFor(f *File, ptr string) string {
	if n, ok := b.forced[declKey(f, ptr)]; ok {
		return n
	}
	base := pascal(f.Stem)
	if ptr == "" {
		return base
	}
	structRoot := b.structRoot(f)
	name := ""
	segs := splitPointer(ptr)
	for i := 0; i < len(segs); i++ {
		switch seg := segs[i]; seg {
		case "$defs":
			i++
			if i >= len(segs) {
				break
			}
			if structRoot {
				name = base + pascal(segs[i])
			} else {
				name = pascal(segs[i])
			}
		case "properties":
			i++
			if i >= len(segs) {
				break
			}
			if name == "" {
				name = base
			}
			name += pascal(segs[i])
		case "items":
			name += "Item"
		case "additionalProperties":
			name += "Value"
		case "oneOf", "anyOf", "allOf":
			i++
			if i < len(segs) {
				name += "Variant" + segs[i]
			}
		default:
			name += pascal(seg)
		}
		// An ancestor with a chosen name (an inline union variant) renames
		// everything below it, so its nested types read Union+Value+Member.
		if i < len(segs) {
			if n, ok := b.forced[declKey(f, joinPointer("", segs[:i+1]...))]; ok {
				name = n
			}
		}
	}
	return name
}

func descOf(n *Node) string {
	if d := n.Get("description").str(); d != "" {
		return d
	}
	return n.Get("title").str()
}

func newDecl(f *File, ptr, name string, k declKind, n *Node) *Decl {
	return &Decl{File: f, Ptr: ptr, Name: name, Kind: k, Doc: descOf(n)}
}

func named(d *Decl) *Type { return &Type{Kind: tNamed, Decl: d} }

// declare returns the declaration for the schema at f#ptr, creating it on first
// use. Every reference to a location, from anywhere, shares one declaration.
func (b *builder) declare(f *File, ptr string, n *Node) (*Decl, error) {
	key := declKey(f, ptr)
	if d, ok := b.decls[key]; ok {
		return d, nil
	}
	if n.isFalse() {
		return nil, at(f, ptr, fmt.Errorf("the false schema has no type"))
	}
	c, err := classify(n)
	if err != nil {
		return nil, at(f, ptr, err)
	}
	switch c {
	case cEnum:
		return b.declareEnum(f, ptr, n)
	case cStruct:
		return b.declareStruct(f, ptr, n)
	case cUnion:
		d, ok, err := b.declareUnion(f, ptr, n)
		if err != nil {
			return nil, err
		}
		if ok {
			return d, nil
		}
	}

	// Anything else becomes a named primitive, composite or alias.
	if b.inProgress[key] {
		return nil, at(f, ptr, fmt.Errorf("recursive definition of a non-object type is not supported"))
	}
	b.inProgress[key] = true
	defer delete(b.inProgress, key)
	t, err := b.typeOf(f, ptr, n)
	if err != nil {
		return nil, err
	}
	d := newDecl(f, ptr, b.nameFor(f, ptr), dDefined, n)
	d.Under = t
	switch t.Kind {
	case tString, tInteger, tNumber, tBool, tArray, tMap:
		d.Kind = dDefined
	case tConst:
		d.Kind = dDefined
		d.Under = &Type{Kind: t.Const.Kind}
	default: // named, any, loose, nullable, null, open object
		d.Kind = dAlias
	}
	d.Constraints = constraintDoc(n)
	b.decls[key] = d
	return d, nil
}

// constraintDoc lists the value constraints of a primitive schema so they show
// up in the generated documentation.
func constraintDoc(n *Node) string {
	var parts []string
	if v := n.Get("format").str(); v != "" {
		parts = append(parts, "format "+v)
	}
	if v := n.Get("pattern").str(); v != "" {
		parts = append(parts, "pattern "+quotePattern(v))
	}
	for _, kw := range []string{"minLength", "maxLength", "minimum", "maximum", "minItems", "maxItems"} {
		if v := n.Get(kw); v != nil && v.Kind == nNumber {
			parts = append(parts, kw+" "+v.Num)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "JSON Schema constraints (enforced by schema validation, not by this type): " + strings.Join(parts, ", ") + "."
}

// quotePattern spells a regular expression so it is safe inside a comment:
// plain printable ASCII goes in backticks, anything else is escaped.
func quotePattern(p string) string {
	plain := !strings.Contains(p, "`") && !strings.Contains(p, "*/")
	for _, r := range p {
		if r < 0x20 || r > 0x7e {
			plain = false
			break
		}
	}
	if plain {
		return "`" + p + "`"
	}
	q := strconv.QuoteToASCII(p)
	return strings.ReplaceAll(q, "*/", `*\/`)
}

func (b *builder) declareEnum(f *File, ptr string, n *Node) (*Decl, error) {
	list := n.Get("enum")
	if list.Kind != nArray || len(list.Items) == 0 {
		return nil, at(f, ptr, fmt.Errorf("enum must be a non-empty array"))
	}
	d := newDecl(f, ptr, b.nameFor(f, ptr), dEnum, n)
	seenNames := map[string]string{}
	for i, it := range list.Items {
		var kind typeKind
		var ev EnumValue
		switch it.Kind {
		case nString:
			kind = tString
			ev = EnumValue{Raw: it.Str, Name: d.Name + enumSuffix(it.Str)}
		case nNumber:
			kind = tInteger
			if _, err := strconv.ParseInt(it.Num, 10, 64); err != nil {
				return nil, at(f, ptr, fmt.Errorf("enum value %s is not an integer", it.Num))
			}
			suffix := it.Num
			if strings.HasPrefix(suffix, "-") {
				suffix = "Neg" + suffix[1:]
			}
			ev = EnumValue{Raw: it.Num, Name: d.Name + suffix}
		default:
			return nil, at(f, ptr, fmt.Errorf("enum values must be strings or integers"))
		}
		if i == 0 {
			d.EnumKind = kind
		} else if kind != d.EnumKind {
			return nil, at(f, ptr, fmt.Errorf("enum mixes strings and integers"))
		}
		if prev, dup := seenNames[ev.Name]; dup {
			return nil, at(f, ptr, fmt.Errorf("enum values %q and %q both map to the name %s", prev, ev.Raw, ev.Name))
		}
		seenNames[ev.Name] = ev.Raw
		d.Values = append(d.Values, ev)
	}
	b.decls[declKey(f, ptr)] = d
	return d, nil
}

// enumSuffix converts an enum string into the tail of a constant name.
func enumSuffix(v string) string {
	if s := pascal(v); s != "" {
		return s
	}
	return "Empty"
}

func (b *builder) declareStruct(f *File, ptr string, n *Node) (*Decl, error) {
	key := declKey(f, ptr)
	d := newDecl(f, ptr, b.nameFor(f, ptr), dStruct, n)
	b.decls[key] = d // registered first so a recursive reference finds it
	for _, kw := range []string{"if", "then", "else", "dependentRequired", "not", "contains", "oneOf", "anyOf", "allOf", "minProperties", "maxProperties"} {
		if n.Has(kw) {
			d.Overlay = true
		}
	}
	required := map[string]bool{}
	if r := n.Get("required"); r != nil {
		if r.Kind != nArray {
			return nil, at(f, ptr, fmt.Errorf("required must be an array"))
		}
		for _, it := range r.Items {
			if it.Kind != nString {
				return nil, at(f, ptr, fmt.Errorf("required must list property names"))
			}
			required[it.Str] = true
		}
	}
	props := n.Get("properties")
	seenNames := map[string]string{}
	have := map[string]bool{}
	if props != nil {
		if !props.isObject() {
			return nil, at(f, ptr, fmt.Errorf("properties must be an object"))
		}
		for _, key := range props.Keys {
			pn := props.Vals[key]
			have[key] = true
			if pn.isFalse() {
				continue // a forbidden property is not a field
			}
			pptr := joinPointer(ptr, "properties", key)
			t, err := b.typeOf(f, pptr, pn)
			if err != nil {
				return nil, err
			}
			goName := pascal(key)
			if goName == "" {
				return nil, at(f, pptr, fmt.Errorf("property name %q has no usable identifier", key))
			}
			if prev, dup := seenNames[goName]; dup {
				return nil, at(f, ptr, fmt.Errorf("properties %q and %q both map to the field name %s", prev, key, goName))
			}
			if goName == "MarshalJSON" {
				return nil, at(f, pptr, fmt.Errorf("property %q would produce a field named MarshalJSON, which clashes with the generated method", key))
			}
			seenNames[goName] = key
			fld := &Field{
				JSON: key, GoName: goName, Type: t, Required: required[key],
				Doc: descOf(pn), Authority: pn.Get("x-alixai-authority").str(),
			}
			d.Fields = append(d.Fields, fld)
		}
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !have[name] {
			return nil, at(f, ptr, fmt.Errorf("required property %q is not declared in properties", name))
		}
	}
	return d, nil
}

func isNullSchema(n *Node) bool {
	return n.isObject() && n.Get("type").str() == "null" && !n.Has("$ref")
}

// variantInfo is one alternative of a oneOf/anyOf, with $ref chains followed.
type variantInfo struct {
	file   *File
	ptr    string
	node   *Node
	viaRef bool
	disc   string // discriminator value, once known
}

// discriminate finds the property that tells the alternatives apart: a string
// const present in every alternative, with a distinct value in each. It returns
// ok=false (no error) when the union is not of that shape.
func (b *builder) discriminate(f *File, ptr, kw string, members []*Node) (string, []variantInfo, bool, error) {
	vs := make([]variantInfo, len(members))
	for i, m := range members {
		v := variantInfo{file: f, ptr: joinPointer(ptr, kw, strconv.Itoa(i)), node: m}
		for hops := 0; v.node.isObject() && v.node.Has("$ref"); hops++ {
			if hops > 8 {
				return "", nil, false, at(f, ptr, fmt.Errorf("$ref chain too long"))
			}
			r, err := b.reg.resolve(v.file, v.node.Get("$ref").Str)
			if err != nil {
				return "", nil, false, at(v.file, v.ptr, err)
			}
			v.file, v.ptr, v.node, v.viaRef = r.file, r.ptr, r.node, true
		}
		if !v.node.isObject() || !v.node.Has("properties") {
			return "", nil, false, nil
		}
		vs[i] = v
	}
	consts := func(v variantInfo, prop string) (string, bool) {
		c := v.node.Get("properties").Get(prop).Get("const")
		if c == nil || c.Kind != nString {
			return "", false
		}
		return c.Str, true
	}
	var candidates []string
	for _, k := range vs[0].node.Get("properties").Keys {
		ok := true
		seen := map[string]bool{}
		for _, v := range vs {
			val, has := consts(v, k)
			if !has || seen[val] {
				ok = false
				break
			}
			seen[val] = true
		}
		if ok {
			candidates = append(candidates, k)
		}
	}
	if len(candidates) == 0 {
		return "", nil, false, nil
	}
	disc := candidates[0]
	for _, preferred := range []string{"type", "kind", "transport", "source"} {
		found := false
		for _, c := range candidates {
			if c == preferred {
				disc, found = c, true
				break
			}
		}
		if found {
			break
		}
	}
	for i := range vs {
		vs[i].disc, _ = consts(vs[i], disc)
	}
	return disc, vs, true, nil
}

// unionMembers returns the keyword (oneOf or anyOf) and members of a type union.
func unionMembers(n *Node) (string, []*Node) {
	for _, kw := range []string{"oneOf", "anyOf"} {
		if list := n.Get(kw); list != nil {
			if bearing, total := countTypeBearing(list); bearing == total && total > 0 {
				return kw, list.Items
			}
		}
	}
	return "", nil
}

// declareUnion declares a discriminated union. ok is false when the union has
// no discriminator (or contains null), in which case the caller types it
// loosely.
func (b *builder) declareUnion(f *File, ptr string, n *Node) (*Decl, bool, error) {
	if d, ok := b.decls[declKey(f, ptr)]; ok && d.Kind == dUnion {
		return d, true, nil
	}
	kw, members := unionMembers(n)
	for _, m := range members {
		if isNullSchema(m) {
			return nil, false, nil
		}
	}
	disc, vs, ok, err := b.discriminate(f, ptr, kw, members)
	if err != nil || !ok {
		return nil, false, err
	}
	key := declKey(f, ptr)
	d := newDecl(f, ptr, b.nameFor(f, ptr), dUnion, n)
	d.Disc = disc
	b.decls[key] = d
	for _, v := range vs {
		if !v.viaRef {
			b.forced[declKey(v.file, v.ptr)] = d.Name + pascal(v.disc)
		}
		vd, err := b.declare(v.file, v.ptr, v.node)
		if err != nil {
			return nil, false, err
		}
		if vd.Kind != dStruct {
			return nil, false, at(f, ptr, fmt.Errorf("union variant %s is not an object type", vd.Name))
		}
		if vd.File.Area != f.Area {
			return nil, false, at(f, ptr, fmt.Errorf("union variant %s lives in area %s but the union is in %s: variants must share the union's package", vd.Name, vd.File.Area, f.Area))
		}
		d.Variants = append(d.Variants, &Variant{Value: v.disc, Decl: vd})
	}
	return d, true, nil
}

// typeOf returns the use-site type of the schema at f#ptr.
func (b *builder) typeOf(f *File, ptr string, n *Node) (*Type, error) {
	if n.isTrue() {
		return &Type{Kind: tAny}, nil
	}
	if n.isFalse() {
		return nil, at(f, ptr, fmt.Errorf("the false schema has no type"))
	}
	c, err := classify(n)
	if err != nil {
		return nil, at(f, ptr, err)
	}
	switch c {
	case cAny:
		return &Type{Kind: tAny}, nil
	case cRef:
		r, err := b.reg.resolve(f, n.Get("$ref").Str)
		if err != nil {
			return nil, at(f, ptr, err)
		}
		d, err := b.declare(r.file, r.ptr, r.node)
		if err != nil {
			return nil, err
		}
		return named(d), nil
	case cConst:
		cv := n.Get("const")
		switch cv.Kind {
		case nString:
			return &Type{Kind: tConst, Const: &Const{Kind: tString, Str: cv.Str}}, nil
		case nBool:
			return &Type{Kind: tConst, Const: &Const{Kind: tBool, Bool: cv.Bool}}, nil
		case nNumber:
			kind := tNumber
			if _, err := strconv.ParseInt(cv.Num, 10, 64); err == nil {
				kind = tInteger
			}
			return &Type{Kind: tConst, Const: &Const{Kind: kind, Num: cv.Num}}, nil
		}
		return nil, at(f, ptr, fmt.Errorf("const must be a string, number or boolean"))
	case cEnum, cStruct:
		d, err := b.declare(f, ptr, n)
		if err != nil {
			return nil, err
		}
		return named(d), nil
	case cUnion:
		return b.unionType(f, ptr, n)
	case cAllOf:
		return b.allOfType(f, ptr, n)
	case cMap:
		ap := n.Get("additionalProperties")
		elem := &Type{Kind: tAny}
		if !ap.isTrue() {
			if elem, err = b.typeOf(f, joinPointer(ptr, "additionalProperties"), ap); err != nil {
				return nil, err
			}
		}
		return &Type{Kind: tMap, Elem: elem}, nil
	case cObjectAny:
		return &Type{Kind: tObjectAny}, nil
	case cArray:
		elem := &Type{Kind: tAny}
		if items := n.Get("items"); items != nil {
			if elem, err = b.typeOf(f, joinPointer(ptr, "items"), items); err != nil {
				return nil, err
			}
		}
		return &Type{Kind: tArray, Elem: elem}, nil
	case cPrim:
		return &Type{Kind: primNames[n.Get("type").Str]}, nil
	case cPrimList:
		var members []*Type
		nullable := false
		for _, it := range n.Get("type").Items {
			if it.Str == "null" {
				nullable = true
				continue
			}
			members = append(members, &Type{Kind: primNames[it.Str]})
		}
		switch {
		case len(members) == 0:
			return &Type{Kind: tNull}, nil
		case len(members) == 1:
			return nullableOf(members[0], nullable), nil
		}
		if nullable {
			members = append(members, &Type{Kind: tNull})
		}
		return &Type{Kind: tLoose, Members: members}, nil
	}
	return nil, at(f, ptr, fmt.Errorf("unhandled schema shape"))
}

func nullableOf(t *Type, nullable bool) *Type {
	if !nullable || t.Kind == tNullable {
		return t
	}
	return &Type{Kind: tNullable, Elem: t}
}

// unionType types a oneOf/anyOf: [X, null] is "X or null"; alternatives
// discriminated by a const property become a union declaration; anything else
// is typed loosely (Go any, a TypeScript union) and left to schema validation.
func (b *builder) unionType(f *File, ptr string, n *Node) (*Type, error) {
	kw, members := unionMembers(n)
	var nonNull []int
	nullable := false
	for i, m := range members {
		if isNullSchema(m) {
			nullable = true
			continue
		}
		nonNull = append(nonNull, i)
	}
	typeAt := func(i int) (*Type, error) {
		return b.typeOf(f, joinPointer(ptr, kw, strconv.Itoa(i)), members[i])
	}
	switch len(nonNull) {
	case 0:
		return &Type{Kind: tNull}, nil
	case 1:
		inner, err := typeAt(nonNull[0])
		if err != nil {
			return nil, err
		}
		return nullableOf(inner, nullable), nil
	}
	d, ok, err := b.declareUnion(f, ptr, n)
	if err != nil {
		return nil, err
	}
	if ok {
		return named(d), nil
	}
	if nullable {
		if _, _, disc, _ := b.discriminate(f, ptr, kw, withoutNull(members)); disc {
			return nil, at(f, ptr, fmt.Errorf("a nullable discriminated union is not supported"))
		}
	}
	loose := &Type{Kind: tLoose}
	for i, m := range members {
		if isNullSchema(m) {
			loose.Members = append(loose.Members, &Type{Kind: tNull})
			continue
		}
		t, err := typeAt(i)
		if err != nil {
			return nil, err
		}
		loose.Members = append(loose.Members, t)
	}
	return loose, nil
}

func withoutNull(members []*Node) []*Node {
	var out []*Node
	for _, m := range members {
		if !isNullSchema(m) {
			out = append(out, m)
		}
	}
	return out
}

// allOfType merges the type-bearing members of an allOf. The only merge the
// schemas need is "every member is the same primitive" (a string that is both
// an identifier and a slug); anything else is an error rather than a guess.
func (b *builder) allOfType(f *File, ptr string, n *Node) (*Type, error) {
	var ts []*Type
	for i, m := range n.Get("allOf").Items {
		if !typeBearing(m) {
			continue // overlay
		}
		t, err := b.typeOf(f, joinPointer(ptr, "allOf", strconv.Itoa(i)), m)
		if err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	if len(ts) == 1 {
		return ts[0], nil
	}
	var kind typeKind
	for i, t := range ts {
		k, ok := primKind(t)
		if !ok || (i > 0 && k != kind) {
			return nil, at(f, ptr, fmt.Errorf("allOf of more than one type is only supported when every member is the same primitive type"))
		}
		kind = k
	}
	return &Type{Kind: kind}, nil
}

// primKind returns the primitive kind a type resolves to, looking through
// named declarations.
func primKind(t *Type) (typeKind, bool) {
	switch t.Kind {
	case tString, tInteger, tNumber, tBool:
		return t.Kind, true
	case tConst:
		return t.Const.Kind, true
	case tNamed:
		switch t.Decl.Kind {
		case dEnum:
			return t.Decl.EnumKind, true
		case dDefined, dAlias:
			if t.Decl.Under != nil {
				return primKind(t.Decl.Under)
			}
		}
	}
	return 0, false
}
