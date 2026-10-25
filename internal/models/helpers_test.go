package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }
func i64p(i int64) *int64   { return &i }
func boolp(b bool) *bool    { return &b }

const (
	testSHA  = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // sha256("abc")
	testRev  = "0123456789abcdef0123456789abcdef01234567"
	testRev2 = "fedcba9876543210fedcba9876543210fedcba98"
)

// candidateEntry is a complete, valid candidate: every key present, nothing
// measured, no pinned hash. Tests derive every other entry from it.
func candidateEntry(id string) Entry {
	return Entry{
		ID:           id,
		Name:         "Test model " + id,
		Version:      "1.0",
		Status:       StatusCandidate,
		Roles:        []Role{RoleConversationalController},
		Capabilities: []string{"chat"},
		Upstream:     Upstream{RepoURL: "https://huggingface.co/example/model", Revision: strp(testRev)},
		Artifact: Artifact{
			Format:      "gguf",
			RepoURL:     strp("https://huggingface.co/example/model-GGUF"),
			Filename:    strp("model-Q4_K_M.gguf"),
			Revision:    strp(testRev2),
			SizeBytes:   nil,
			SHA256:      nil,
			QuantizedBy: "example",
			FirstParty:  true,
			CheckedOn:   "2026-10-02",
		},
		Licence: Licence{
			SPDX:                "Apache-2.0",
			URL:                 "https://www.apache.org/licenses/LICENSE-2.0",
			RedistributionNotes: "Redistribution permitted with the licence text and notices.",
		},
		Quantization: Quant{Scheme: "Q4_K_M", NominalBits: 4},
		Backend:      Backend{Runtime: "localai", Name: "llama-cpp", AllowedRuntimeVersions: []string{}},
		Languages:    Languages{Declared: []string{"en", "it"}, Qualified: []string{}},
		Fallback:     Fallback{DegradedMode: "deterministic-routing"},
		DefaultFor:   []Default{},
		Notes:        []string{},
	}
}

// qualifiedEntry is a valid qualified-emulated entry: pinned and reviewed.
func qualifiedEntry(id string) Entry {
	e := candidateEntry(id)
	e.Status = StatusQualifiedEmulated
	e.Artifact.SHA256 = strp(testSHA)
	e.Artifact.SizeBytes = i64p(3)
	e.Review = Review{Date: strp("2026-10-03"), EvidencePath: strp("docs/efficiency/M0-13.md")}
	e.Measurement = Measure{Emulated: boolp(true), ResultsPath: strp("benchmarks/results/m0/")}
	return e
}

// certifiedEntry is a valid certified entry: pinned, reviewed, measured on
// real hardware.
func certifiedEntry(id string) Entry {
	e := qualifiedEntry(id)
	e.Status = StatusCertified
	e.Measurement.Emulated = boolp(false)
	return e
}

// jsonOf marshals an entry; JSON is valid YAML, so it can be a registry file.
func jsonOf(t *testing.T, e Entry) []byte {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mutated marshals e, applies f to the generic document and re-marshals it, so
// a test can delete or retype any key.
func mutated(t *testing.T, e Entry, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(jsonOf(t, e), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// walk returns the map that holds the last element of a dotted path.
func walk(t *testing.T, m map[string]any, dotted string) (map[string]any, string) {
	t.Helper()
	parts := strings.Split(dotted, ".")
	cur := m
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("path %q: %q is not an object", dotted, p)
		}
		cur = next
	}
	return cur, parts[len(parts)-1]
}

func setKey(t *testing.T, m map[string]any, dotted string, v any) {
	t.Helper()
	cur, k := walk(t, m, dotted)
	cur[k] = v
}

func delKey(t *testing.T, m map[string]any, dotted string) {
	t.Helper()
	cur, k := walk(t, m, dotted)
	if _, ok := cur[k]; !ok {
		t.Fatalf("path %q does not exist", dotted)
	}
	delete(cur, k)
}

// registryFS builds a registry directory from file bodies keyed by file name.
func registryFS(files map[string][]byte) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// loadOne loads a registry holding one file named <id>.yaml.
func loadOne(t *testing.T, id string, data []byte, opts ...LoadOption) (*Registry, error) {
	t.Helper()
	return LoadFS(registryFS(map[string][]byte{id + ".yaml": data}), opts...)
}

// loadError extracts the *LoadError behind err, failing the test if err is
// nil or of another kind.
func loadError(t *testing.T, err error) *LoadError {
	t.Helper()
	if err == nil {
		t.Fatal("expected the registry to be rejected, got no error")
	}
	if !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("error is not ErrInvalidRegistry: %v", err)
	}
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("error is not a *LoadError: %v", err)
	}
	return le
}

// requireViolation fails unless some violation has the rule and mentions
// substr in its field or detail. The rule is the contract; the substring only
// pins down which field it is about, so an unrelated failure cannot satisfy it.
func requireViolation(t *testing.T, le *LoadError, rule Rule, substr string) {
	t.Helper()
	for _, v := range le.Violations {
		if v.Rule == rule && strings.Contains(v.Field+" "+v.Detail, substr) {
			return
		}
	}
	t.Fatalf("no [%s] violation mentioning %q in:\n%v", rule, substr, le)
}

// hasRuleField reports whether vs has a violation with the rule and field.
func hasRuleField(vs []Violation, rule Rule, field string) bool {
	for _, v := range vs {
		if v.Rule == rule && v.Field == field {
			return true
		}
	}
	return false
}
