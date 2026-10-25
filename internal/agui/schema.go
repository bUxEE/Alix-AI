package agui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The vendored AG-UI schemas (schemas/agui, written by tools/aguischema from the pinned @ag-ui/core release) are the
// contract every emitted event is validated against. Schemas loads them: the gateway's and the golden-stream tests of
// the next tasks validate with it, and so do this package's own tests. It reads the files; nothing here reaches the
// network.

// BundleFile and VersionFile are the files of schemas/agui.
const (
	BundleFile  = "ag-ui-core.json"
	VersionFile = "VERSION"
)

// Version is the parsed schemas/agui/VERSION.
type Version struct {
	Package         string
	Version         string
	ProtocolVersion string
	Integrity       string
	SchemaID        string
	// BundleSHA256 is the recorded sha256 of the bundle file.
	BundleSHA256 string
}

// FindSchemaDir looks for schemas/agui from the working directory upwards: where a test runs inside the repository.
func FindSchemaDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 10 {
		cand := filepath.Join(dir, "schemas", "agui")
		if _, err := os.Stat(filepath.Join(cand, VersionFile)); err == nil {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("agui: schemas/agui was not found above the working directory")
}

// ParseVersion reads a VERSION file.
func ParseVersion(data []byte) (Version, error) {
	var v Version
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			return Version{}, fmt.Errorf("agui: VERSION line %q is not key: value", line)
		}
		switch key {
		case "package":
			v.Package = val
		case "version":
			v.Version = val
		case "protocol_version":
			v.ProtocolVersion = val
		case "integrity":
			v.Integrity = val
		case "schema_id":
			v.SchemaID = val
		case "file":
			name, sum, ok := strings.Cut(val, " sha256:")
			if ok && name == BundleFile {
				v.BundleSHA256 = sum
			}
		}
	}
	if v.Package == "" || v.Version == "" || v.ProtocolVersion == "" || v.Integrity == "" || v.SchemaID == "" || v.BundleSHA256 == "" {
		return Version{}, errors.New("agui: VERSION is incomplete")
	}
	return v, nil
}

// Schemas is the compiled vendored bundle.
type Schemas struct {
	Version Version
	comp    *jsonschema.Compiler
	defs    map[string]any
	root    string
}

// LoadSchemas reads schemas/agui from dir, checks that the bundle is the one VERSION records (its sha256) and that the
// release it names is the one this package implements, and prepares it for validation.
func LoadSchemas(dir string) (*Schemas, error) {
	vdata, err := os.ReadFile(filepath.Join(dir, VersionFile))
	if err != nil {
		return nil, err
	}
	ver, err := ParseVersion(vdata)
	if err != nil {
		return nil, err
	}
	if ver.ProtocolVersion != ProtocolVersion {
		return nil, fmt.Errorf("agui: the vendored schemas are protocol %s, this package implements %s", ver.ProtocolVersion, ProtocolVersion)
	}
	bundle, err := os.ReadFile(filepath.Join(dir, BundleFile))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bundle)
	if hex.EncodeToString(sum[:]) != ver.BundleSHA256 {
		return nil, errors.New("agui: the bundle does not match the sha256 recorded in VERSION: regenerate it with tools/aguischema")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundle))
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["$id"] != ver.SchemaID {
		return nil, errors.New("agui: the bundle's $id is not the one VERSION records")
	}
	defs, _ := obj["$defs"].(map[string]any)
	if len(defs) == 0 {
		return nil, errors.New("agui: the bundle has no definitions")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(ver.SchemaID, doc); err != nil {
		return nil, err
	}
	return &Schemas{Version: ver, comp: c, defs: defs, root: ver.SchemaID}, nil
}

// Has reports whether the bundle defines name (Event, RunAgentInput, RunStartedEvent, ...).
func (s *Schemas) Has(name string) bool { _, ok := s.defs[name]; return ok }

// Definitions returns the names of the definitions of the bundle.
func (s *Schemas) Definitions() []string {
	out := make([]string, 0, len(s.defs))
	for k := range s.defs {
		out = append(out, k)
	}
	return out
}

// Validate checks a JSON document against the definition name of the bundle.
func (s *Schemas) Validate(name string, data []byte) error {
	if !s.Has(name) {
		return fmt.Errorf("agui: the bundle has no definition %q", name)
	}
	sch, err := s.comp.Compile(s.root + "#/$defs/" + name)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return sch.Validate(inst)
}

// ValidateEvent checks an event against the Event union and against the definition of its own type, and checks that it
// has no member the definition does not declare. The release's schemas are open (a misspelled member would pass), so
// the closed-world check is what catches a typo in an event this package emits.
func (s *Schemas) ValidateEvent(data []byte) error {
	if err := s.Validate("Event", data); err != nil {
		return err
	}
	var inst any
	if err := json.Unmarshal(data, &inst); err != nil {
		return err
	}
	if extra := s.unknownMembers(map[string]any{"$ref": "#/$defs/Event"}, inst, "", 0); len(extra) > 0 {
		return fmt.Errorf("agui: members the schema does not declare: %s", strings.Join(extra, ", "))
	}
	return nil
}

// ValidateInput checks a RunAgentInput.
func (s *Schemas) ValidateInput(data []byte) error { return s.Validate("RunAgentInput", data) }

// unknownMembers lists the members of inst (at the paths of the schema's declared object properties) that the schema
// does not declare. It follows $ref, items and the declared properties, and picks a oneOf branch by its const
// discriminator (type, role, op, outcome type); members typed as any ({} schema) are not entered.
func (s *Schemas) unknownMembers(sch map[string]any, inst any, path string, depth int) []string {
	if depth > 16 {
		return nil
	}
	if ref, ok := sch["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		def, ok := s.defs[name].(map[string]any)
		if !ok {
			return nil
		}
		return s.unknownMembers(def, inst, path, depth+1)
	}
	if alts, ok := sch["oneOf"].([]any); ok {
		obj, isObj := inst.(map[string]any)
		if !isObj {
			return nil
		}
		for _, a := range alts {
			alt, _ := a.(map[string]any)
			if s.discriminates(alt, obj) {
				return s.unknownMembers(alt, inst, path, depth+1)
			}
		}
		return nil
	}
	switch v := inst.(type) {
	case []any:
		items, _ := sch["items"].(map[string]any)
		var out []string
		for i, e := range v {
			out = append(out, s.unknownMembers(items, e, fmt.Sprintf("%s/%d", path, i), depth+1)...)
		}
		return out
	case map[string]any:
		props, _ := sch["properties"].(map[string]any)
		if len(props) == 0 {
			return nil
		}
		var out []string
		for k, e := range v {
			sub, declared := props[k].(map[string]any)
			if !declared {
				out = append(out, path+"/"+k)
				continue
			}
			out = append(out, s.unknownMembers(sub, e, path+"/"+k, depth+1)...)
		}
		return out
	}
	return nil
}

// discriminates reports whether the object's const-typed members all equal the alternative's: the alternative is the
// one the instance is an instance of.
func (s *Schemas) discriminates(alt map[string]any, obj map[string]any) bool {
	if ref, ok := alt["$ref"].(string); ok {
		def, ok := s.defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			return false
		}
		return s.discriminates(def, obj)
	}
	props, _ := alt["properties"].(map[string]any)
	matched := false
	for _, key := range []string{"type", "role", "op"} {
		p, _ := props[key].(map[string]any)
		c, has := p["const"]
		if !has {
			continue
		}
		if obj[key] != c {
			return false
		}
		matched = true
	}
	return matched
}
