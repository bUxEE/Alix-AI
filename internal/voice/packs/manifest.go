package packs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/voice/profile"
	voicepacks "github.com/buxee/alix-ai/models/voice-packs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SchemaID is the $id of the pack manifest schema.
const SchemaID = "urn:alixai:schema:voice:language-pack:v1"

// maxManifestBytes bounds one manifest file. A real one is a few KiB.
const maxManifestBytes = 256 << 10

// maxSchemaViolations bounds how many schema failures one manifest reports.
const maxSchemaViolations = 12

// installRecordFile is the install record the store writes into a pack
// directory next to the model files. A manifest file name starts with a letter
// or digit, so a model file can never take this name.
const installRecordFile = ".installed.json"

// Manifest is one language pack: the models for one language, each with its
// pinned files and recorded licence (master plan 13, 16.3). Its JSON form is
// models/voice-packs/pack-manifest.schema.json; the YAML file is converted to
// JSON before it is validated and decoded, so these tags are the only names.
type Manifest struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Language  string    `json:"language"`
	Status    string    `json:"status"`
	Models    Models    `json:"models"`
	Quality   Quality   `json:"quality"`
	Resources Resources `json:"resources"`
	Notes     []string  `json:"notes"`

	// Digest identifies what an install caches and pins: the id, language,
	// version, roles and every file's role, name, size and SHA-256. Install
	// records it, and a pack installed under another digest is stale
	// (Store.Verify). Quality, resources, licences, notes and the display name
	// are not in it: measuring a pack (M4-06) or revising its notes must not make
	// every installed copy stale, and the files themselves are re-hashed at every
	// verification whatever the digest says.
	Digest string `json:"-"`
}

// Models are the pack's models by voice role. A nil member is a role the pack
// does not provide: an STT-only pack is valid.
type Models struct {
	STT *Model `json:"stt,omitempty"`
	TTS *Model `json:"tts,omitempty"`
	VAD *Model `json:"vad,omitempty"`
}

// Model is one model of a pack: the files a runtime backend loads.
type Model struct {
	Name string `json:"name"`
	// Engine is the runtime backend that loads the files (whisper.cpp, piper).
	Engine string `json:"engine"`
	// RegistryID is the model registry entry the model is qualified under, nil
	// while the registry has none.
	RegistryID *string `json:"registry_id"`
	Files      []File  `json:"files"`
	Licence    Licence `json:"licence"`
}

// File is one pinned model file.
type File struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Revision  string `json:"revision"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// Licence is the recorded licence of a model.
type Licence struct {
	SPDX                string `json:"spdx"`
	URL                 string `json:"url"`
	RedistributionNotes string `json:"redistribution_notes"`
}

// Quality holds the quality scores M4-06 measures; nil until measured.
type Quality struct {
	STTWER                *float64     `json:"stt_wer"`
	TTSTimeToFirstAudioMS *float64     `json:"tts_time_to_first_audio_ms"`
	TTSRealTimeFactor     *float64     `json:"tts_real_time_factor"`
	Measurement           *Measurement `json:"measurement"`
}

// Measurement says where the quality scores come from.
type Measurement struct {
	Profile     string `json:"profile"`
	Emulated    bool   `json:"emulated"`
	MeasuredOn  string `json:"measured_on"`
	ResultsPath string `json:"results_path"`
}

// Resources is what the pack needs while it is the active pack.
type Resources struct {
	DiskBytes   int64 `json:"disk_bytes"`
	SteadyRSSMB *int  `json:"steady_rss_mb"`
	PeakRSSMB   *int  `json:"peak_rss_mb"`
	CPUCores    *int  `json:"cpu_cores"`
}

// Roles lists the voice roles the pack provides, in stt, tts, vad order.
func (m Manifest) Roles() []providers.Role {
	var out []providers.Role
	if m.Models.STT != nil {
		out = append(out, providers.RoleSTT)
	}
	if m.Models.TTS != nil {
		out = append(out, providers.RoleTTS)
	}
	if m.Models.VAD != nil {
		out = append(out, providers.RoleVAD)
	}
	return out
}

// Pack is the profile's view of the manifest: id, language, version, roles.
func (m Manifest) Pack() profile.Pack {
	return profile.Pack{ID: m.ID, Language: m.Language, Version: m.Version, Roles: m.Roles()}
}

// Files returns every file of the pack with the role it belongs to, in role
// order and then manifest order.
func (m Manifest) Files() []RoleFile {
	var out []RoleFile
	for _, r := range []struct {
		role providers.Role
		m    *Model
	}{{providers.RoleSTT, m.Models.STT}, {providers.RoleTTS, m.Models.TTS}, {providers.RoleVAD, m.Models.VAD}} {
		if r.m == nil {
			continue
		}
		for _, f := range r.m.Files {
			out = append(out, RoleFile{Role: r.role, File: f})
		}
	}
	return out
}

// RoleFile is a pack file and the voice role it serves.
type RoleFile struct {
	Role providers.Role
	File File
}

// DiskBytes is the sum of the file sizes.
func (m Manifest) DiskBytes() int64 {
	var n int64
	for _, f := range m.Files() {
		n += f.File.SizeBytes
	}
	return n
}

// validate applies the rules a schema cannot express.
func (m Manifest) validate() error {
	var errs []error
	if !profile.ValidPackID(m.ID) {
		errs = append(errs, fmt.Errorf("id %q is not a pack id", m.ID))
	}
	if !voice.ValidLanguage(m.Language) {
		errs = append(errs, fmt.Errorf("language %q is not a language tag", m.Language))
	}
	if len(m.Roles()) == 0 {
		errs = append(errs, errors.New("the pack provides no voice role"))
	}
	seen := map[string]bool{}
	for _, rf := range m.Files() {
		key := strings.ToLower(rf.File.Name)
		if seen[key] {
			errs = append(errs, fmt.Errorf("file name %q is used twice (names are compared without case)", rf.File.Name))
		}
		seen[key] = true
	}
	if got, want := m.Resources.DiskBytes, m.DiskBytes(); got != want {
		errs = append(errs, fmt.Errorf("resources.disk_bytes is %d but the file sizes add up to %d", got, want))
	}
	return errors.Join(errs...)
}

// registryHashMatches reports whether the registry entry pins the sha256 of one
// of the model's files (the entry pins the primary artefact; a voice such as
// Piper also carries a small config file).
func registryHashMatches(entry models.Entry, mod *Model) bool {
	if entry.Artifact.SHA256 == nil {
		return false
	}
	for _, f := range mod.Files {
		if f.SHA256 == *entry.Artifact.SHA256 {
			return true
		}
	}
	return false
}

// CheckRegistry refuses a manifest whose registry_id does not resolve in reg,
// or whose registry entry pins a different file than the manifest does. A nil
// registry_id is allowed: the entry does not exist yet.
func (m Manifest) CheckRegistry(reg *models.Registry) error {
	var errs []error
	for _, r := range []struct {
		role string
		m    *Model
	}{{"stt", m.Models.STT}, {"tts", m.Models.TTS}, {"vad", m.Models.VAD}} {
		if r.m == nil || r.m.RegistryID == nil {
			continue
		}
		id := *r.m.RegistryID
		if reg == nil {
			errs = append(errs, fmt.Errorf("%s names registry entry %q but there is no registry to check it against", r.role, id))
			continue
		}
		entry, ok := reg.Get(id)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s names registry entry %q, which is not in the registry", r.role, id))
		case !registryHashMatches(entry, r.m):
			errs = append(errs, fmt.Errorf("%s: registry entry %q pins a sha256 that none of the pack's files has", r.role, id))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return contracts.Wrap(contracts.ErrSchemaInvalid, err, fmt.Sprintf("voice: language pack %q disagrees with the model registry", m.ID))
	}
	return nil
}

var (
	schemaOnce     sync.Once
	schemaCompiled *jsonschema.Schema
	schemaErr      error
)

// compiledSchema compiles the embedded manifest schema once, as JSON Schema
// 2020-12 with format assertions on.
func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		raw, err := fs.ReadFile(voicepacks.FS, voicepacks.SchemaFile)
		if err != nil {
			schemaErr = fmt.Errorf("voice packs: embedded schema: %w", err)
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			schemaErr = fmt.Errorf("voice packs: embedded schema is not valid JSON: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		if err := c.AddResource(SchemaID, doc); err != nil {
			schemaErr = fmt.Errorf("voice packs: embedded schema: %w", err)
			return
		}
		if schemaCompiled, err = c.Compile(SchemaID); err != nil {
			schemaErr = fmt.Errorf("voice packs: embedded schema does not compile: %w", err)
		}
	})
	return schemaCompiled, schemaErr
}

// ParseManifest parses and validates one manifest file. file is the file name
// (en.yaml): the pack id must equal its stem, so a pack cannot be shipped under
// another pack's name. The checks, in order: one plain YAML document, the JSON
// Schema (which rejects unknown keys and a missing hash), a strict decode and
// the pack's own rules. Every failure is schema_invalid.
func ParseManifest(file string, data []byte) (Manifest, error) {
	fail := func(format string, args ...any) (Manifest, error) {
		return Manifest{}, contracts.Newf(contracts.ErrSchemaInvalid, "voice: language pack manifest %s: %s", file, fmt.Sprintf(format, args...))
	}
	if len(data) > maxManifestBytes {
		return fail("%d bytes is over the %d byte limit", len(data), maxManifestBytes)
	}
	generic, err := yamlToValue(data)
	if err != nil {
		return fail("%v", err)
	}
	asJSON, err := json.Marshal(generic)
	if err != nil {
		return fail("%v", err)
	}
	sch, err := compiledSchema()
	if err != nil {
		return Manifest{}, contracts.Wrap(contracts.ErrInternal, err, "voice: the manifest schema is unusable")
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return fail("%v", err)
	}
	if err := sch.Validate(instance); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return fail("%v", err)
		}
		return fail("%s", strings.Join(schemaViolations(ve), "; "))
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(asJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fail("%v", err)
	}
	if want := strings.TrimSuffix(file, ".yaml"); m.ID != want || !strings.HasSuffix(file, ".yaml") {
		return fail("the id %q must match the file name (want %s.yaml)", m.ID, m.ID)
	}
	if err := m.validate(); err != nil {
		return fail("%v", err)
	}
	m.Digest = m.digest()
	return m, nil
}

// digest is the SHA-256 of a canonical encoding of what an install caches and
// pins (see Manifest.Digest).
func (m Manifest) digest() string {
	type pinned struct {
		Role   providers.Role `json:"role"`
		Name   string         `json:"name"`
		Size   int64          `json:"size_bytes"`
		SHA256 string         `json:"sha256"`
	}
	canon := struct {
		ID       string           `json:"id"`
		Language string           `json:"language"`
		Version  string           `json:"version"`
		Roles    []providers.Role `json:"roles"`
		Files    []pinned         `json:"files"`
	}{ID: m.ID, Language: m.Language, Version: m.Version, Roles: m.Roles(), Files: []pinned{}}
	for _, rf := range m.Files() {
		canon.Files = append(canon.Files, pinned{rf.Role, rf.File.Name, rf.File.SizeBytes, rf.File.SHA256})
	}
	b, _ := json.Marshal(canon) // plain strings, numbers and slices: cannot fail
	sum := sha256.Sum256(append([]byte("alixai voice pack digest v1\n"), b...))
	return hex.EncodeToString(sum[:])
}

// schemaViolations lists the leaf problems of a failed validation, capped.
func schemaViolations(ve *jsonschema.ValidationError) []string {
	var out []string
	var walk func(u jsonschema.OutputUnit)
	walk = func(u jsonschema.OutputUnit) {
		if len(out) >= maxSchemaViolations {
			return
		}
		if len(u.Errors) == 0 {
			if u.Error != nil {
				loc := u.InstanceLocation
				if loc == "" {
					loc = "/"
				}
				out = append(out, loc+": "+u.Error.String())
			}
			return
		}
		for _, c := range u.Errors {
			walk(c)
		}
	}
	walk(*ve.DetailedOutput())
	if len(out) == 0 {
		out = append(out, ve.Error())
	}
	return out
}

// yamlToValue reads exactly one YAML document with a mapping at the top into
// plain values. Anchors, aliases, duplicate keys, non-string keys and custom
// tags are refused, and timestamps stay strings (the schema's date format
// applies to them).
func yamlToValue(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("more than one YAML document; want exactly one")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the top level must be a mapping of field name to value")
	}
	return nodeValue(doc.Content[0])
}

func nodeValue(n *yaml.Node) (any, error) {
	if n.Anchor != "" {
		return nil, fmt.Errorf("line %d: YAML anchors are not allowed", n.Line)
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nil, fmt.Errorf("line %d: YAML aliases are not allowed", n.Line)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" || k.Anchor != "" {
				return nil, fmt.Errorf("line %d: mapping keys must be plain strings", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: key %q is defined twice", k.Line, k.Value)
			}
			val, err := nodeValue(v)
			if err != nil {
				return nil, err
			}
			m[k.Value] = val
		}
		return m, nil
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			val, err := nodeValue(c)
			if err != nil {
				return nil, err
			}
			s = append(s, val)
		}
		return s, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str", "!!timestamp":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("line %d: %w", n.Line, err)
			}
			return v, nil
		}
		return nil, fmt.Errorf("line %d: tag %s is not allowed", n.Line, strings.TrimSpace(n.Tag))
	}
	return nil, fmt.Errorf("line %d: unexpected YAML node", n.Line)
}

// Manifests is a set of parsed manifests: the approved packs. Packs that are
// not in the set cannot be installed (master plan 16.3: no runtime activation of
// unapproved artefacts).
type Manifests struct {
	byID map[string]Manifest
}

// LoadManifests parses every *.yaml file at the root of fsys, normally
// voicepacks.FS. It returns all of them or an error: one bad manifest rejects
// the set rather than being skipped.
func LoadManifests(fsys fs.FS) (Manifests, error) {
	des, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return Manifests{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "voice: cannot read the language pack manifests")
	}
	set := Manifests{byID: map[string]Manifest{}}
	var errs []error
	for _, de := range des {
		name := de.Name()
		switch {
		case de.IsDir():
			errs = append(errs, fmt.Errorf("%s: subdirectories are not part of the manifest set", name))
			continue
		case strings.HasSuffix(name, ".yml"):
			errs = append(errs, fmt.Errorf("%s: use the .yaml extension", name))
			continue
		case !strings.HasSuffix(name, ".yaml"):
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		m, err := ParseManifest(name, data)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		set.byID[m.ID] = m
	}
	if err := errors.Join(errs...); err != nil {
		return Manifests{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "voice: the language pack manifests are invalid")
	}
	if len(set.byID) == 0 {
		return Manifests{}, contracts.New(contracts.ErrSchemaInvalid, "voice: there are no language pack manifests")
	}
	return set, nil
}

// Shipped loads the manifests embedded in the binary.
func Shipped() (Manifests, error) { return LoadManifests(voicepacks.FS) }

// Lookup returns the approved manifest with the given pack id.
func (s Manifests) Lookup(id string) (Manifest, bool) {
	m, ok := s.byID[id]
	return m, ok
}

// All returns the manifests in id order.
func (s Manifests) All() []Manifest {
	out := make([]Manifest, 0, len(s.byID))
	for _, m := range s.byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CheckRegistry applies Manifest.CheckRegistry to every manifest.
func (s Manifests) CheckRegistry(reg *models.Registry) error {
	for _, m := range s.All() {
		if err := m.CheckRegistry(reg); err != nil {
			return err
		}
	}
	return nil
}
