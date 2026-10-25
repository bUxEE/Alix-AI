package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A small matcher for the GBNF subset Grammar emits, used to check that the
// grammar accepts what a correct model writes and refuses what it must not.
// It understands rules (name ::= expression, one per line), alternation,
// sequences, groups, string literals, character classes (with ranges and
// negation), the quantifiers ? * + {m} {m,n} and the escapes \" \\ \n \t \r
// \xHH. It is not llama.cpp, so it cannot prove the backend accepts the
// grammar; it proves the grammar means what it was written to mean.

type gbnfNode struct {
	kind     string // "lit", "class", "ref", "seq", "alt", "rep"
	lit      []rune
	ranges   [][2]rune
	negate   bool
	name     string
	children []*gbnfNode
	min, max int // for rep; max -1 means unbounded
}

type gbnfParser struct {
	src []rune
	pos int
}

func parseGBNF(text string) (map[string]*gbnfNode, error) {
	rules := map[string]*gbnfNode{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, body, ok := strings.Cut(line, "::=")
		if !ok {
			return nil, fmt.Errorf("not a rule: %q", line)
		}
		name = strings.TrimSpace(name)
		if !regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(name) {
			return nil, fmt.Errorf("bad rule name %q", name)
		}
		if _, dup := rules[name]; dup {
			return nil, fmt.Errorf("rule %q defined twice", name)
		}
		p := &gbnfParser{src: []rune(body)}
		n, err := p.alt()
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", name, err)
		}
		p.skip()
		if p.pos != len(p.src) {
			return nil, fmt.Errorf("rule %s: unexpected %q", name, string(p.src[p.pos:]))
		}
		rules[name] = n
	}
	if rules["root"] == nil {
		return nil, fmt.Errorf("no root rule")
	}
	// Every referenced rule must exist.
	var check func(n *gbnfNode) error
	check = func(n *gbnfNode) error {
		if n.kind == "ref" && rules[n.name] == nil {
			return fmt.Errorf("rule %q is used but not defined", n.name)
		}
		for _, c := range n.children {
			if err := check(c); err != nil {
				return err
			}
		}
		return nil
	}
	for name, n := range rules {
		if err := check(n); err != nil {
			return nil, fmt.Errorf("rule %s: %w", name, err)
		}
	}
	return rules, nil
}

func (p *gbnfParser) skip() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *gbnfParser) peek() rune {
	p.skip()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

// alt parses alternatives; an empty alternative is allowed (ws ::= | " " | ...).
func (p *gbnfParser) alt() (*gbnfNode, error) {
	var alts []*gbnfNode
	for {
		s, err := p.seq()
		if err != nil {
			return nil, err
		}
		alts = append(alts, s)
		if p.peek() != '|' {
			break
		}
		p.pos++
	}
	if len(alts) == 1 {
		return alts[0], nil
	}
	return &gbnfNode{kind: "alt", children: alts}, nil
}

func (p *gbnfParser) seq() (*gbnfNode, error) {
	n := &gbnfNode{kind: "seq"}
	for {
		c := p.peek()
		if c == 0 || c == '|' || c == ')' {
			return n, nil
		}
		atom, err := p.atom()
		if err != nil {
			return nil, err
		}
		atom, err = p.quantify(atom)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, atom)
	}
}

func (p *gbnfParser) quantify(a *gbnfNode) (*gbnfNode, error) {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '?':
			p.pos++
			a = &gbnfNode{kind: "rep", children: []*gbnfNode{a}, min: 0, max: 1}
		case '*':
			p.pos++
			a = &gbnfNode{kind: "rep", children: []*gbnfNode{a}, min: 0, max: -1}
		case '+':
			p.pos++
			a = &gbnfNode{kind: "rep", children: []*gbnfNode{a}, min: 1, max: -1}
		case '{':
			end := p.pos
			for end < len(p.src) && p.src[end] != '}' {
				end++
			}
			if end >= len(p.src) {
				return nil, fmt.Errorf("unterminated {")
			}
			spec := string(p.src[p.pos+1 : end])
			p.pos = end + 1
			lo, hi, hasComma := strings.Cut(spec, ",")
			min, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return nil, fmt.Errorf("bad repetition {%s}", spec)
			}
			max := min
			if hasComma {
				if strings.TrimSpace(hi) == "" {
					max = -1
				} else if max, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
					return nil, fmt.Errorf("bad repetition {%s}", spec)
				}
			}
			a = &gbnfNode{kind: "rep", children: []*gbnfNode{a}, min: min, max: max}
		default:
			return a, nil
		}
	}
	return a, nil
}

func (p *gbnfParser) escape() (rune, error) {
	// p.src[p.pos] is the character after a backslash.
	c := p.src[p.pos]
	p.pos++
	switch c {
	case 'n':
		return '\n', nil
	case 't':
		return '\t', nil
	case 'r':
		return '\r', nil
	case '\\', '"', '[', ']', '-', '^':
		return c, nil
	case 'x':
		if p.pos+2 > len(p.src) {
			return 0, fmt.Errorf("short \\x escape")
		}
		v, err := strconv.ParseUint(string(p.src[p.pos:p.pos+2]), 16, 32)
		p.pos += 2
		return rune(v), err
	case 'u':
		if p.pos+4 > len(p.src) {
			return 0, fmt.Errorf("short \\u escape")
		}
		v, err := strconv.ParseUint(string(p.src[p.pos:p.pos+4]), 16, 32)
		p.pos += 4
		return rune(v), err
	}
	return 0, fmt.Errorf("unknown escape \\%c", c)
}

func (p *gbnfParser) atom() (*gbnfNode, error) {
	switch c := p.peek(); {
	case c == '"':
		p.pos++
		n := &gbnfNode{kind: "lit"}
		for {
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("unterminated string")
			}
			r := p.src[p.pos]
			p.pos++
			if r == '"' {
				return n, nil
			}
			if r == '\\' {
				e, err := p.escape()
				if err != nil {
					return nil, err
				}
				r = e
			}
			n.lit = append(n.lit, r)
		}
	case c == '[':
		p.pos++
		n := &gbnfNode{kind: "class"}
		if p.pos < len(p.src) && p.src[p.pos] == '^' {
			n.negate = true
			p.pos++
		}
		readOne := func() (rune, error) {
			if p.pos >= len(p.src) {
				return 0, fmt.Errorf("unterminated class")
			}
			r := p.src[p.pos]
			p.pos++
			if r == '\\' {
				if p.pos >= len(p.src) {
					return 0, fmt.Errorf("unterminated class")
				}
				return p.escape()
			}
			return r, nil
		}
		for {
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("unterminated class")
			}
			if p.src[p.pos] == ']' {
				p.pos++
				return n, nil
			}
			lo, err := readOne()
			if err != nil {
				return nil, err
			}
			hi := lo
			if p.pos+1 < len(p.src) && p.src[p.pos] == '-' && p.src[p.pos+1] != ']' {
				p.pos++
				if hi, err = readOne(); err != nil {
					return nil, err
				}
			}
			n.ranges = append(n.ranges, [2]rune{lo, hi})
		}
	case c == '(':
		p.pos++
		n, err := p.alt()
		if err != nil {
			return nil, err
		}
		if p.peek() != ')' {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return n, nil
	case c >= 'a' && c <= 'z':
		start := p.pos
		for p.pos < len(p.src) && (p.src[p.pos] == '-' || p.src[p.pos] >= 'a' && p.src[p.pos] <= 'z' || p.src[p.pos] >= '0' && p.src[p.pos] <= '9') {
			p.pos++
		}
		return &gbnfNode{kind: "ref", name: string(p.src[start:p.pos])}, nil
	default:
		return nil, fmt.Errorf("unexpected %q", string(c))
	}
}

// gbnfMatches reports whether the grammar derives exactly input from root.
func gbnfMatches(rules map[string]*gbnfNode, input string) bool {
	in := []rune(input)
	for _, end := range gbnfMatch(rules, rules["root"], in, 0) {
		if end == len(in) {
			return true
		}
	}
	return false
}

// gbnfMatch returns every position the node can end at, starting at pos.
func gbnfMatch(rules map[string]*gbnfNode, n *gbnfNode, in []rune, pos int) []int {
	switch n.kind {
	case "lit":
		if pos+len(n.lit) > len(in) {
			return nil
		}
		for i, r := range n.lit {
			if in[pos+i] != r {
				return nil
			}
		}
		return []int{pos + len(n.lit)}
	case "class":
		if pos >= len(in) {
			return nil
		}
		hit := false
		for _, r := range n.ranges {
			if in[pos] >= r[0] && in[pos] <= r[1] {
				hit = true
			}
		}
		if hit == n.negate || !utf8.ValidRune(in[pos]) {
			return nil
		}
		return []int{pos + 1}
	case "ref":
		return gbnfMatch(rules, rules[n.name], in, pos)
	case "alt":
		var out []int
		for _, c := range n.children {
			out = append(out, gbnfMatch(rules, c, in, pos)...)
		}
		return dedupeInts(out)
	case "seq":
		cur := []int{pos}
		for _, c := range n.children {
			var next []int
			for _, p := range cur {
				next = append(next, gbnfMatch(rules, c, in, p)...)
			}
			cur = dedupeInts(next)
			if len(cur) == 0 {
				return nil
			}
		}
		return cur
	case "rep":
		var out []int
		cur := []int{pos}
		visited := map[int]bool{pos: true}
		if n.min == 0 {
			out = append(out, pos)
		}
		for count := 1; n.max < 0 || count <= n.max; count++ {
			var next []int
			for _, p := range cur {
				next = append(next, gbnfMatch(rules, n.children[0], in, p)...)
			}
			next = dedupeInts(next)
			if len(next) == 0 {
				break
			}
			if count >= n.min {
				out = append(out, next...)
			}
			fresh := false
			for _, p := range next {
				if !visited[p] {
					visited[p], fresh = true, true
				}
			}
			if !fresh && count >= n.min {
				break // further repetitions reach nothing new
			}
			cur = next
		}
		return dedupeInts(out)
	}
	return nil
}

func dedupeInts(xs []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
