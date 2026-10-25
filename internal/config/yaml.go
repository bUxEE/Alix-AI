package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeYAML decodes one YAML document into the working copies. The document
// must be a mapping whose keys are registered section names; every nested key
// must exist in its section's type (yaml.Decoder.KnownFields). Empty files and
// a bare null are accepted and change nothing.
func (r *Registry) decodeYAML(path string, data []byte, work []reflect.Value) error {
	// Pass 1: the top level, as a node tree, for clean errors with line numbers.
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file
		}
		return fmt.Errorf("config: %s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("config: %s: %w", path, err)
		}
		return fmt.Errorf("config: %s: multiple YAML documents; want exactly one", path)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return fmt.Errorf("config: %s: not a YAML document", path)
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return nil
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config: %s: line %d: top level must be a mapping of section name to section", path, root.Line)
	}
	seen := map[string]int{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("config: %s: line %d: section names must be plain strings", path, key.Line)
		}
		if first, dup := seen[key.Value]; dup {
			return fmt.Errorf("config: %s: line %d: section %q already defined at line %d", path, key.Line, key.Value, first)
		}
		seen[key.Value] = key.Line
		if _, ok := r.byName[key.Value]; !ok {
			return fmt.Errorf("config: %s: line %d: unknown section %q (known: %s)",
				path, key.Line, key.Value, strings.Join(r.sortedNames(), ", "))
		}
	}

	// Pass 2: strict decode. yaml.Node.Decode ignores KnownFields, so the
	// whole document is decoded through a struct synthesised from the
	// registered sections, one field per section, tagged with its name.
	fields := make([]reflect.StructField, len(r.entries))
	for i, e := range r.entries {
		fields[i] = reflect.StructField{
			Name: fmt.Sprintf("S%d", i),
			Type: work[i].Type(),
			Tag:  reflect.StructTag(fmt.Sprintf(`yaml:%q`, e.name)),
		}
	}
	holder := reflect.New(reflect.StructOf(fields))
	for i := range r.entries {
		holder.Elem().Field(i).Set(work[i])
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(holder.Interface()); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return nil
}
