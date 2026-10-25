package apppkg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// parseYAML reads one package file as plain data (docs/integration/
// application-package.md section 2) and returns its JSON data model: nil,
// bool, int64, float64, string, []any and map[string]any.
//
// The loader refuses, rather than interprets, anything that would let a file
// mean something other than what it says:
//
//   - input that is not UTF-8, has a byte order mark, a carriage return or a
//     control character, or is larger than Limits.MaxYAMLBytes;
//   - an empty file, or more than one YAML document;
//   - anchors, aliases, merge keys (<<) and every explicit tag, which are the
//     mechanisms of expansion attacks and of type confusion;
//   - mapping keys that are not plain strings, and duplicate keys (never a
//     silent last-one-wins);
//   - plain scalars that resolve to a date or time, to a number written in
//     anything but plain decimal, to a number that does not fit int64, or to
//     infinity or not-a-number: the author must quote text that looks like
//     one of those, so what the file says is what is read;
//   - nesting deeper than Limits.MaxYAMLDepth or more nodes than
//     Limits.MaxYAMLNodes.
func parseYAML(data []byte, lim Limits) (any, error) {
	if int64(len(data)) > lim.MaxYAMLBytes {
		return nil, fmt.Errorf("file is larger than %d bytes", lim.MaxYAMLBytes)
	}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, errors.New("file starts with a byte order mark; use plain UTF-8")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("file is not valid UTF-8")
	}
	for i, b := range data {
		switch {
		case b == '\r':
			return nil, fmt.Errorf("line %d: carriage return found; use LF line endings", 1+bytes.Count(data[:i], []byte{'\n'}))
		case b == 0 || (b < 0x20 && b != '\n' && b != '\t') || b == 0x7f:
			return nil, fmt.Errorf("line %d: control character 0x%02x", 1+bytes.Count(data[:i], []byte{'\n'}), b)
		}
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file holds no YAML document")
		}
		return nil, cleanYAMLError(err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, errors.New("file holds more than one YAML document")
	case !errors.Is(err, io.EOF):
		return nil, cleanYAMLError(err)
	}

	w := &yamlWalker{lim: lim}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) != 1 {
			return nil, errors.New("file holds no YAML document")
		}
		root = doc.Content[0]
	}
	v, err := w.node(root, 1)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// cleanYAMLError keeps the parser's position and reason but drops anything
// that could quote file content at length.
func cleanYAMLError(err error) error {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return errors.New(msg)
}

type yamlWalker struct {
	lim   Limits
	nodes int
}

func (w *yamlWalker) fail(n *yaml.Node, format string, args ...any) error {
	return fmt.Errorf("line %d: %s", n.Line, fmt.Sprintf(format, args...))
}

var (
	decimalRE = regexp.MustCompile(`^[-+]?(0|[1-9][0-9]*)$`)
	floatRE   = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
)

func (w *yamlWalker) node(n *yaml.Node, depth int) (any, error) {
	w.nodes++
	if w.nodes > w.lim.MaxYAMLNodes {
		return nil, fmt.Errorf("document has more than %d nodes", w.lim.MaxYAMLNodes)
	}
	if depth > w.lim.MaxYAMLDepth {
		return nil, w.fail(n, "nesting deeper than %d levels", w.lim.MaxYAMLDepth)
	}
	if n.Anchor != "" {
		return nil, w.fail(n, "anchors are not allowed")
	}
	if n.Kind == yaml.AliasNode {
		return nil, w.fail(n, "aliases are not allowed")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return nil, w.fail(n, "explicit tags are not allowed")
	}

	switch n.Kind {
	case yaml.ScalarNode:
		return w.scalar(n)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := w.node(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			key, err := w.key(k)
			if err != nil {
				return nil, err
			}
			if _, dup := out[key]; dup {
				return nil, w.fail(k, "duplicate key %s", q(key))
			}
			val, err := w.node(v, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = val
		}
		return out, nil
	}
	return nil, w.fail(n, "unsupported YAML node")
}

func (w *yamlWalker) key(k *yaml.Node) (string, error) {
	w.nodes++
	if k.Anchor != "" {
		return "", w.fail(k, "anchors are not allowed")
	}
	if k.Kind == yaml.AliasNode {
		return "", w.fail(k, "aliases are not allowed")
	}
	if k.Kind != yaml.ScalarNode {
		return "", w.fail(k, "mapping keys must be plain strings")
	}
	if k.Style&yaml.TaggedStyle != 0 {
		return "", w.fail(k, "explicit tags are not allowed")
	}
	if k.ShortTag() == "!!merge" || (k.Value == "<<" && k.Style == 0) {
		return "", w.fail(k, "merge keys (<<) are not allowed")
	}
	if k.ShortTag() != "!!str" {
		return "", w.fail(k, "mapping key %s is not a string; quote it", q(k.Value))
	}
	return k.Value, nil
}

func (w *yamlWalker) scalar(n *yaml.Node) (any, error) {
	quoted := n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0
	if quoted {
		return n.Value, nil
	}
	switch tag := n.ShortTag(); tag {
	case "!!str":
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		switch n.Value {
		case "true", "True", "TRUE":
			return true, nil
		case "false", "False", "FALSE":
			return false, nil
		}
		return nil, w.fail(n, "ambiguous boolean %s; quote it if it is text", q(n.Value))
	case "!!int":
		if !decimalRE.MatchString(n.Value) {
			return nil, w.fail(n, "number %s is not plain decimal; quote it if it is text", q(n.Value))
		}
		i, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil {
			return nil, w.fail(n, "number %s does not fit a 64-bit integer; quote it if it is text", q(n.Value))
		}
		return i, nil
	case "!!float":
		if decimalRE.MatchString(n.Value) {
			// an integer literal the YAML library could only read as a float
			return nil, w.fail(n, "number %s does not fit a 64-bit integer; quote it if it is text", q(n.Value))
		}
		if !floatRE.MatchString(n.Value) {
			return nil, w.fail(n, "number %s is not a plain decimal number; quote it if it is text", q(n.Value))
		}
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, w.fail(n, "number %s is out of range; quote it if it is text", q(n.Value))
		}
		return f, nil
	case "!!timestamp":
		return nil, w.fail(n, "%s reads as a date or time; quote it if it is text", q(n.Value))
	default:
		return nil, w.fail(n, "value %s has the unsupported type %s", q(n.Value), tag)
	}
}

// toJSON encodes a tree produced by parseYAML. Map keys are sorted, so the
// same document always yields the same bytes.
func toJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
