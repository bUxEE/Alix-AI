package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// nodeKind is the JSON type of a Node.
type nodeKind uint8

const (
	nObject nodeKind = iota
	nArray
	nString
	nNumber
	nBool
	nNull
)

// Node is a parsed JSON value that keeps object keys in source order, which a
// map[string]any does not. The generator needs that order so struct fields and
// union variants come out in the order the schema author wrote them, and so
// that output never depends on Go's randomised map iteration.
type Node struct {
	Kind  nodeKind
	Keys  []string         // object: keys in source order
	Vals  map[string]*Node // object: key -> value
	Items []*Node          // array
	Str   string           // string
	Num   string           // number: the literal text
	Bool  bool
}

// parseNode parses exactly one JSON value. Duplicate object keys and trailing
// data are errors: a schema that says two things about one key is ambiguous.
func parseNode(data []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := decodeNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the JSON value")
	}
	return n, nil
}

func decodeNode(dec *json.Decoder) (*Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &Node{Kind: nObject, Vals: map[string]*Node{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				if _, dup := n.Vals[key]; dup {
					return nil, fmt.Errorf("duplicate object key %q", key)
				}
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.Keys = append(n.Keys, key)
				n.Vals[key] = v
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return n, nil
		case '[':
			n := &Node{Kind: nArray}
			for dec.More() {
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case string:
		return &Node{Kind: nString, Str: t}, nil
	case json.Number:
		return &Node{Kind: nNumber, Num: t.String()}, nil
	case bool:
		return &Node{Kind: nBool, Bool: t}, nil
	case nil:
		return &Node{Kind: nNull}, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// Get returns the member of an object, or nil when n is nil, not an object, or
// has no such member.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != nObject {
		return nil
	}
	return n.Vals[key]
}

// Has reports whether n is an object with the given member.
func (n *Node) Has(key string) bool { return n.Get(key) != nil }

// str returns the string value of n, or "" when n is nil or not a string.
func (n *Node) str() string {
	if n == nil || n.Kind != nString {
		return ""
	}
	return n.Str
}

// isObject reports whether n is a JSON object.
func (n *Node) isObject() bool { return n != nil && n.Kind == nObject }

// isTrue and isFalse recognise the boolean schemas.
func (n *Node) isTrue() bool  { return n != nil && n.Kind == nBool && n.Bool }
func (n *Node) isFalse() bool { return n != nil && n.Kind == nBool && !n.Bool }
