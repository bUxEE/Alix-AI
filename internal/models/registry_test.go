package models

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

var (
	publishedSchema = filepath.Join("..", "..", "schemas", "models", "registry-entry.schema.json")
	realRegistry    = filepath.Join("..", "..", "models", "registry")
	repoRoot        = filepath.Join("..", "..")
)

// TestRegistryEntriesValid loads the real models/registry directory: the five
// candidate files of master plan 7.1, 7.2 and 8.2 must exist, parse, satisfy
// the schema and every cross-entry rule.
//
// It asserts invariants, not a snapshot. M0-13 (which cannot edit this
// package) changes statuses, measurements, defaults, allowed runtime versions
// and may add or reject entries, and this test has to keep passing when it does.
// formatExtension is the file extension each artefact format implies.
var formatExtension = map[string]string{"gguf": ".gguf", "onnx": ".onnx", "safetensors": ".safetensors", "pytorch": ".pt"}

func TestRegistryEntriesValid(t *testing.T) {
	// WithEvidenceFS makes any evidence or results path an entry names prove
	// it exists in the repository.
	r, err := LoadDir(realRegistry, WithEvidenceFS(repoEvidenceFS(t)))
	if err != nil {
		t.Fatalf("the shipped registry is invalid: %v", err)
	}

	// The registry contains the five required models; more are allowed.
	required := map[string]bool{ // id -> published by the model's own organisation
		"granite-4.0-350m-q4_k_m":                      true,
		"granite-4.0-1b-q4_k_m":                        true,
		"qwen3.5-0.8b-q4_k_m":                          false,
		"qwen3.5-2b-q4_k_m":                            false,
		"granite-embedding-97m-multilingual-r2-q4_k_m": false,
	}
	for id, firstParty := range required {
		e, ok := r.Get(id)
		if !ok {
			t.Errorf("required entry %s is missing", id)
			continue
		}
		if e.Artifact.FirstParty != firstParty {
			t.Errorf("%s: first_party = %v, want %v", id, e.Artifact.FirstParty, firstParty)
		}
	}

	for _, e := range r.Entries() {
		t.Run(e.ID, func(t *testing.T) {
			// SEC-091: a licence record on every entry, whatever its status.
			if blank(e.Licence.SPDX) || blank(e.Licence.URL) || blank(e.Licence.RedistributionNotes) {
				t.Errorf("licence is incomplete: %+v", e.Licence)
			}
			// The artefact each entry names is pinned and its file name has the
			// extension its format implies (GGUF for the LocalAI controllers,
			// safetensors or a PyTorch checkpoint for the decision layer).
			a := e.Artifact
			if a.RepoURL == nil || a.Filename == nil || a.Revision == nil || a.SHA256 == nil || a.SizeBytes == nil {
				t.Errorf("artefact is not fully pinned: %+v", a)
			}
			if ext, known := formatExtension[a.Format]; !known {
				t.Errorf("artefact format %q has no known file extension", a.Format)
			} else if a.Filename != nil && !strings.HasSuffix(*a.Filename, ext) {
				t.Errorf("artefact filename %q does not end in %s, which its format %s implies", *a.Filename, ext, a.Format)
			}
			if len(e.Notes) == 0 {
				t.Error("an entry records what it could not confirm in notes")
			}
			if e.HasRole(RoleEmbedding) && (e.Embedding == nil || e.Embedding.Dimensions == nil) {
				t.Error("an embedding entry must record its vector dimension")
			}
			// A candidate has not been measured: nothing is filled in before
			// M0-13 measures it, and no runtime version has qualified it.
			if e.Status == StatusCandidate {
				if e.Memory.PeakRSSMB != nil || e.Memory.SteadyRSSMB != nil ||
					e.Performance.LatencyMSP50 != nil || e.Performance.TokensPerSecond != nil ||
					e.Scores.Task != nil || e.Scores.Tool != nil || e.Scores.RAG != nil || e.Scores.Safety != nil ||
					e.LoadTime.ColdMS != nil || e.LoadTime.WarmMS != nil || e.Context.BudgetTokens != nil {
					t.Errorf("a candidate has measurements: %+v %+v %+v", e.Memory, e.Performance, e.Scores)
				}
				if len(e.Backend.AllowedRuntimeVersions) != 0 {
					t.Errorf("allowed_runtime_versions = %v on a candidate", e.Backend.AllowedRuntimeVersions)
				}
			}
		})
	}

	// The Granite embedding model produces 384-dimension vectors (upstream card).
	if e, ok := r.Get("granite-embedding-97m-multilingual-r2-q4_k_m"); ok {
		if e.Embedding == nil || e.Embedding.Dimensions == nil || *e.Embedding.Dimensions != 384 {
			t.Errorf("embedding facts = %+v, want 384 dimensions", e.Embedding)
		}
	}
}

// TestEmbeddedSchemaMatchesPublished keeps the copy of the schema that the
// loader embeds identical to the published one in schemas/models.
func TestEmbeddedSchemaMatchesPublished(t *testing.T) {
	published, err := os.ReadFile(publishedSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, SchemaJSON()) {
		t.Fatalf("internal/models/registry-entry.schema.json differs from schemas/models/registry-entry.schema.json; copy the published file over the embedded one")
	}
}

// TestRegistryRejectsMissingLicence covers SEC-091: an entry without a
// complete licence record never loads, whatever its status.
//
// SEC-091
func TestRegistryRejectsMissingLicence(t *testing.T) {
	// Positive control: the same entry with its licence intact loads, so the
	// failures below are caused by the licence and nothing else.
	for _, e := range []Entry{candidateEntry("mdl"), qualifiedEntry("mdl"), certifiedEntry("mdl")} {
		if _, err := loadOne(t, "mdl", jsonOf(t, e)); err != nil {
			t.Fatalf("%s entry with a licence was rejected: %v", e.Status, err)
		}
	}

	cases := []struct {
		name   string
		mutate func(t *testing.T, m map[string]any)
		field  string
	}{
		{"licence key absent", func(t *testing.T, m map[string]any) { delete(m, "licence") }, "licence"},
		{"licence null", func(t *testing.T, m map[string]any) { m["licence"] = nil }, "/licence"},
		{"licence empty object", func(t *testing.T, m map[string]any) { m["licence"] = map[string]any{} }, "/licence"},
		{"spdx absent", func(t *testing.T, m map[string]any) { delKey(t, m, "licence.spdx") }, "spdx"},
		{"spdx empty", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.spdx", "") }, "/licence/spdx"},
		{"spdx not an id", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.spdx", "Apache 2.0 or later") }, "/licence/spdx"},
		{"url absent", func(t *testing.T, m map[string]any) { delKey(t, m, "licence.url") }, "url"},
		{"url empty", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.url", "") }, "/licence/url"},
		{"url not https", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.url", "http://example.com/licence") }, "/licence/url"},
		{"notes absent", func(t *testing.T, m map[string]any) { delKey(t, m, "licence.redistribution_notes") }, "redistribution_notes"},
		{"notes empty", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.redistribution_notes", "") }, "/licence/redistribution_notes"},
		{"notes blank", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.redistribution_notes", "  \t ") }, "/licence/redistribution_notes"},
	}
	for _, status := range []Entry{candidateEntry("mdl"), certifiedEntry("mdl")} {
		for _, c := range cases {
			t.Run(string(status.Status)+"/"+c.name, func(t *testing.T) {
				_, err := loadOne(t, "mdl", mutated(t, status, func(m map[string]any) { c.mutate(t, m) }))
				le := loadError(t, err)
				requireViolation(t, le, RuleSchema, c.field)
			})
		}
	}

	// The same rule holds for an Entry built in code, which never passes
	// through the schema (Verify and the spike tools build entries).
	for _, c := range []struct {
		name  string
		apply func(e *Entry)
		field string
	}{
		{"spdx", func(e *Entry) { e.Licence.SPDX = "" }, "licence.spdx"},
		{"url", func(e *Entry) { e.Licence.URL = "" }, "licence.url"},
		{"url scheme", func(e *Entry) { e.Licence.URL = "ftp://example.com/l" }, "licence.url"},
		{"notes", func(e *Entry) { e.Licence.RedistributionNotes = "  " }, "licence.redistribution_notes"},
	} {
		t.Run("Validate/"+c.name, func(t *testing.T) {
			e := candidateEntry("mdl")
			if vs := e.Validate(); len(vs) != 0 {
				t.Fatalf("control entry is not valid: %v", vs)
			}
			c.apply(&e)
			if vs := e.Validate(); !hasRuleField(vs, RuleLicence, c.field) {
				t.Fatalf("want a [licence] violation on %s, got %v", c.field, vs)
			}
		})
	}
}

// TestCertifiedRequiresHashAndEvidence covers SEC-063 and SEC-091: certified
// (and qualified-emulated) needs a pinned artefact with its sha256 and a
// recorded review with an evidence path; a candidate may leave both null.
//
// SEC-063
func TestCertifiedRequiresHashAndEvidence(t *testing.T) {
	// Positive controls.
	if _, err := loadOne(t, "mdl", jsonOf(t, certifiedEntry("mdl"))); err != nil {
		t.Fatalf("a complete certified entry was rejected: %v", err)
	}
	if _, err := loadOne(t, "mdl", jsonOf(t, qualifiedEntry("mdl"))); err != nil {
		t.Fatalf("a complete qualified-emulated entry was rejected: %v", err)
	}
	if _, err := loadOne(t, "mdl", jsonOf(t, candidateEntry("mdl"))); err != nil {
		t.Fatalf("a candidate with no hash and no evidence must load: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(e *Entry)
		rule   Rule
		field  string
		schema string // where the schema reports it
	}{
		{"no sha256", func(e *Entry) { e.Artifact.SHA256 = nil }, RulePinned, "artifact.sha256", "/artifact/sha256"},
		{"no filename", func(e *Entry) { e.Artifact.Filename = nil }, RulePinned, "artifact.filename", "/artifact/filename"},
		{"no revision", func(e *Entry) { e.Artifact.Revision = nil }, RulePinned, "artifact.revision", "/artifact/revision"},
		{"no review date", func(e *Entry) { e.Review.Date = nil }, RuleEvidence, "last_efficiency_review.date", "/last_efficiency_review/date"},
		{"no evidence path", func(e *Entry) { e.Review.EvidencePath = nil }, RuleEvidence, "last_efficiency_review.evidence_path", "/last_efficiency_review/evidence_path"},
		{"blank evidence path", func(e *Entry) { e.Review.EvidencePath = strp(" ") }, RuleEvidence, "last_efficiency_review.evidence_path", "/last_efficiency_review/evidence_path"},
		{"short sha256", func(e *Entry) { e.Artifact.SHA256 = strp("abc123") }, RulePinned, "artifact.sha256", "/artifact/sha256"},
		{"upper-case sha256", func(e *Entry) { e.Artifact.SHA256 = strp(strings.ToUpper(testSHA)) }, RulePinned, "artifact.sha256", "/artifact/sha256"},
		{"branch as revision", func(e *Entry) { e.Artifact.Revision = strp("main") }, RulePinned, "artifact.revision", "/artifact/revision"},
		{"path as filename", func(e *Entry) { e.Artifact.Filename = strp("../models/x.gguf") }, RulePinned, "artifact.filename", "/artifact/filename"},
		{"parent-directory evidence", func(e *Entry) { e.Review.EvidencePath = strp("../outside.md") }, RuleEvidence, "last_efficiency_review.evidence_path", "/last_efficiency_review/evidence_path"},
		{"absolute evidence", func(e *Entry) { e.Review.EvidencePath = strp("/etc/passwd") }, RuleEvidence, "last_efficiency_review.evidence_path", "/last_efficiency_review/evidence_path"},
		{"backslash evidence", func(e *Entry) { e.Review.EvidencePath = strp(`docs\efficiency\M0-13.md`) }, RuleEvidence, "last_efficiency_review.evidence_path", "/last_efficiency_review/evidence_path"},
	}
	for _, status := range []Status{StatusCertified, StatusQualifiedEmulated} {
		for _, c := range cases {
			base := certifiedEntry("mdl")
			base.Status = status
			c.mutate(&base)

			t.Run(string(status)+"/"+c.name, func(t *testing.T) {
				// Entry.Validate names the rule and the field.
				if vs := base.Validate(); !hasRuleField(vs, c.rule, c.field) {
					t.Errorf("Validate: want [%s] on %s, got %v", c.rule, c.field, vs)
				}
				// The loader refuses it (the schema reports the same field).
				_, err := loadOne(t, "mdl", jsonOf(t, base))
				requireViolation(t, loadError(t, err), RuleSchema, c.schema)
			})
		}
	}

	// A certified entry cannot rest on an emulated measurement.
	t.Run("certified and emulated", func(t *testing.T) {
		e := certifiedEntry("mdl")
		e.Measurement.Emulated = boolp(true)
		if vs := e.Validate(); !hasRuleField(vs, RuleEmulated, "measurement.emulated") {
			t.Errorf("Validate: %v", vs)
		}
		_, err := loadOne(t, "mdl", jsonOf(t, e))
		requireViolation(t, loadError(t, err), RuleSchema, "/measurement/emulated")
	})

	// The evidence file has to exist when the loader is told where to look.
	t.Run("evidence file must exist", func(t *testing.T) {
		data := jsonOf(t, certifiedEntry("mdl"))
		root := fstest.MapFS{}
		_, err := loadOne(t, "mdl", data, WithEvidenceFS(root))
		requireViolation(t, loadError(t, err), RuleEvidence, "docs/efficiency/M0-13.md")

		root["docs/efficiency/M0-13.md"] = &fstest.MapFile{Data: []byte("review")}
		root["benchmarks/results/m0/run.json"] = &fstest.MapFile{Data: []byte("{}")}
		if _, err := loadOne(t, "mdl", data, WithEvidenceFS(root)); err != nil {
			t.Fatalf("with the evidence present the entry must load: %v", err)
		}
	})
}

// TestOnlyOneDefaultPerRoleAndProfile checks that across the registry a
// (role, profile) pair has at most one default entry.
func TestOnlyOneDefaultPerRoleAndProfile(t *testing.T) {
	ctrlLite := Default{Role: RoleConversationalController, Profile: ProfileLite}
	ctrlBal := Default{Role: RoleConversationalController, Profile: ProfileBalanced}
	routeLite := Default{Role: RoleTypedRouting, Profile: ProfileLite}

	entry := func(id string, defaults ...Default) Entry {
		e := candidateEntry(id)
		e.Roles = []Role{RoleConversationalController, RoleTypedRouting}
		e.DefaultFor = append([]Default{}, defaults...)
		return e
	}
	load := func(es ...Entry) (*Registry, error) {
		files := map[string][]byte{}
		for _, e := range es {
			files[e.ID+".yaml"] = jsonOf(t, e)
		}
		return LoadFS(registryFS(files))
	}

	// Positive control: distinct pairs, one default each.
	r, err := load(entry("aaa", ctrlLite), entry("bbb", ctrlBal, routeLite), entry("ccc"))
	if err != nil {
		t.Fatalf("non-conflicting defaults were rejected: %v", err)
	}
	for _, c := range []struct {
		d    Default
		want string
	}{{ctrlLite, "aaa"}, {ctrlBal, "bbb"}, {routeLite, "bbb"}} {
		if e, ok := r.DefaultFor(c.d.Role, c.d.Profile); !ok || e.ID != c.want {
			t.Errorf("DefaultFor(%s, %s) = %q, %v; want %q", c.d.Role, c.d.Profile, e.ID, ok, c.want)
		}
	}
	if _, ok := r.DefaultFor(RoleEmbedding, ProfileLite); ok {
		t.Error("DefaultFor returned an entry for a pair nobody is the default for")
	}

	// Two entries default for the same pair: rejected, naming both.
	_, err = load(entry("aaa", ctrlLite), entry("bbb", ctrlLite), entry("ccc", ctrlBal))
	le := loadError(t, err)
	requireViolation(t, le, RuleDefaultUnique, "aaa, bbb")
	requireViolation(t, le, RuleDefaultUnique, "conversational-controller, lite")
	if n := le.Count(RuleDefaultUnique); n != 1 {
		t.Errorf("want exactly one default-unique violation, got %d: %v", n, le)
	}

	// Three entries and two conflicting pairs: both are reported.
	_, err = load(entry("aaa", ctrlLite, ctrlBal), entry("bbb", ctrlLite), entry("ccc", ctrlBal))
	if n := loadError(t, err).Count(RuleDefaultUnique); n != 2 {
		t.Errorf("want two default-unique violations, got %d", n)
	}

	// One entry listing a pair twice. The schema's uniqueItems catches it in a
	// file; Validate catches it for an Entry built in code.
	dup := entry("aaa", ctrlLite, ctrlLite)
	if vs := dup.Validate(); !hasRuleField(vs, RuleDefault, "default_for[1]") {
		t.Errorf("Validate: duplicate pair in one entry not reported: %v", vs)
	}
	_, err = load(dup)
	requireViolation(t, loadError(t, err), RuleSchema, "default_for")

	// A default for a role the entry does not have.
	wrongRole := entry("aaa", Default{Role: RoleEmbedding, Profile: ProfileLite})
	if vs := wrongRole.Validate(); !hasRuleField(vs, RuleDefault, "default_for[0]") {
		t.Errorf("Validate: default for a role the entry lacks not reported: %v", vs)
	}

	// A rejected entry is nobody's default.
	rej := entry("aaa", ctrlLite)
	rej.Status = StatusRejected
	if vs := rej.Validate(); !hasRuleField(vs, RuleDefault, "default_for") {
		t.Errorf("Validate: rejected default not reported: %v", vs)
	}
	_, err = load(rej)
	requireViolation(t, loadError(t, err), RuleSchema, "default_for")
	rej.DefaultFor = []Default{}
	if _, err := load(rej); err != nil {
		t.Errorf("a rejected entry without defaults must load: %v", err)
	}
}

// TestRegistryRejectsUnknownFields: unknown fields are refused at every depth,
// and so is every other way a file can be shaped differently from the schema.
func TestRegistryRejectsUnknownFields(t *testing.T) {
	base := candidateEntry("mdl")
	if _, err := loadOne(t, "mdl", jsonOf(t, base)); err != nil {
		t.Fatalf("control entry rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(t *testing.T, m map[string]any)
		field  string
	}{
		{"top level", func(t *testing.T, m map[string]any) { m["is_safe"] = true }, "is_safe"},
		{"licence", func(t *testing.T, m map[string]any) { setKey(t, m, "licence.approved_by", "me") }, "approved_by"},
		{"artifact", func(t *testing.T, m map[string]any) { setKey(t, m, "artifact.mirror", "https://example.com") }, "mirror"},
		{"scores", func(t *testing.T, m map[string]any) { setKey(t, m, "scores.vibes", 0.9) }, "vibes"},
		{"memory", func(t *testing.T, m map[string]any) { setKey(t, m, "memory.p99_rss_mb", 1) }, "p99_rss_mb"},
		{"hardware spec", func(t *testing.T, m map[string]any) { setKey(t, m, "hardware.minimum.gpu_vram", 8) }, "gpu_vram"},
		{"default_for item", func(t *testing.T, m map[string]any) {
			m["default_for"] = []any{map[string]any{"role": "embedding", "profile": "lite", "weight": 2}}
		}, "weight"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadOne(t, "mdl", mutated(t, base, func(m map[string]any) { c.mutate(t, m) }))
			requireViolation(t, loadError(t, err), RuleSchema, c.field)
		})
	}

	// A missing field is rejected too: every key must be present, null when unknown.
	t.Run("missing field", func(t *testing.T) {
		_, err := loadOne(t, "mdl", mutated(t, base, func(m map[string]any) { delKey(t, m, "scores.safety") }))
		requireViolation(t, loadError(t, err), RuleSchema, "safety")
	})
}

// TestParseEntryRejectsUnsafeYAML: the registry accepts a plain subset of YAML.
func TestParseEntryRejectsUnsafeYAML(t *testing.T) {
	good := string(jsonOf(t, candidateEntry("mdl")))
	cases := []struct {
		name string
		data string
		rule Rule
		sub  string
	}{
		{"empty file", "", RuleYAML, "empty"},
		{"not a mapping", "- a\n- b\n", RuleYAML, "mapping"},
		{"two documents", good + "\n---\n" + good, RuleYAML, "more than one"},
		{"duplicate key", "id: m\nid: n\n", RuleYAML, "already defined"},
		{"anchor", "id: &x m\n", RuleYAML, "anchors"},
		{"alias", "a: &x 1\nid: *x\n", RuleYAML, "anchors"},
		{"merge key", "base: {}\n<<: {id: m}\n", RuleYAML, "plain strings"},
		{"non-string key", "1: x\n", RuleYAML, "plain strings"},
		{"custom tag", "id: !custom m\n", RuleYAML, "not allowed"},
		{"binary tag", "id: !!binary bQ==\n", RuleYAML, "not allowed"},
		{"not yaml", "id: [unterminated\n", RuleYAML, ""},
		{"oversized", strings.Repeat("# x\n", maxEntryBytes/4+1), RuleFile, "limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, vs := ParseEntry("mdl.yaml", []byte(c.data))
			if len(vs) == 0 {
				t.Fatalf("accepted; entry = %+v", e)
			}
			found := false
			for _, v := range vs {
				if v.Rule == c.rule && strings.Contains(v.Detail, c.sub) && v.File == "mdl.yaml" {
					found = true
				}
			}
			if !found {
				t.Fatalf("want a [%s] violation containing %q for m.yaml, got %v", c.rule, c.sub, vs)
			}
			if e.ID != "" {
				t.Errorf("a rejected file must not return an entry, got %+v", e)
			}
		})
	}
}

// TestParseEntryAcceptsBlockYAML: ordinary hand-written YAML works, including
// an unquoted date, which YAML would otherwise turn into a timestamp.
func TestParseEntryAcceptsBlockYAML(t *testing.T) {
	doc := `# a comment
id: mdl
name: "Test model"
version: "1.0"
status: candidate
roles: [conversational-controller]
capabilities:
  - chat
upstream: {repo_url: "https://huggingface.co/example/model", revision: null}
artifact:
  format: gguf
  repo_url: https://huggingface.co/example/model-GGUF
  filename: model-Q4_K_M.gguf
  revision: null
  size_bytes: null
  sha256: null
  quantized_by: example
  first_party: true
  checked_on: 2026-10-02
licence:
  spdx: MIT
  url: https://opensource.org/license/mit
  redistribution_notes: Keep the copyright notice.
quantization: {scheme: Q4_K_M, nominal_bits: 4}
backend: {runtime: localai, name: llama-cpp, allowed_runtime_versions: []}
hardware:
  minimum: {cpu_cores: 4, ram_mb: 1024, accelerator: none}
  recommended: {cpu_cores: null, ram_mb: null, accelerator: null}
memory: {peak_rss_mb: 812.5, steady_rss_mb: null}
context: {native_max_tokens: 32768, budget_tokens: 2048, kv_cache_mb: null}
load_time: {cold_ms: null, warm_ms: null}
performance: {latency_ms_p50: null, latency_ms_p95: null, ttft_ms_p50: null, tokens_per_second: null}
languages: {declared: [en, "no"], claimed_count: null, qualified: []}
scores: {task: 0.5, tool: null, rag: null, safety: null}
measurement: {profile: lite, emulated: true, results_path: null, measured_on: 2026-10-03}
fallback: {model_id: null, degraded_mode: refuse, notes: null}
last_efficiency_review: {date: null, evidence_path: null}
default_for: []
notes: []
`
	e, vs := ParseEntry("mdl.yaml", []byte(doc))
	if len(vs) != 0 {
		t.Fatalf("a valid hand-written file was rejected: %v", vs)
	}
	if e.Artifact.CheckedOn != "2026-10-02" {
		t.Errorf("unquoted date = %q, want 2026-10-02", e.Artifact.CheckedOn)
	}
	if e.Memory.PeakRSSMB == nil || *e.Memory.PeakRSSMB != 812.5 {
		t.Errorf("peak rss = %v", e.Memory.PeakRSSMB)
	}
	if e.Hardware.Minimum.CPUCores == nil || *e.Hardware.Minimum.CPUCores != 4 {
		t.Errorf("cpu cores = %v", e.Hardware.Minimum.CPUCores)
	}
	if diff := cmp.Diff([]string{"en", "no"}, e.Languages.Declared); diff != "" {
		t.Errorf("languages (-want +got):\n%s", diff)
	}
	if e.Measurement.Profile == nil || *e.Measurement.Profile != ProfileLite {
		t.Errorf("profile = %v", e.Measurement.Profile)
	}

	// A malformed date is rejected, not ignored (format assertions are on).
	_, vs = ParseEntry("mdl.yaml", []byte(strings.Replace(doc, "checked_on: 2026-10-02", "checked_on: 2026-13-45", 1)))
	if len(vs) == 0 {
		t.Error("an impossible date was accepted")
	}
}

// TestRegistryFileRules covers the rules about the directory rather than one
// entry: the file name is the id, .yml is refused, empty is an error, and one
// bad file blocks the whole registry.
func TestRegistryFileRules(t *testing.T) {
	good := jsonOf(t, candidateEntry("mdl"))

	t.Run("id must match file name", func(t *testing.T) {
		_, err := loadOne(t, "other", good)
		requireViolation(t, loadError(t, err), RuleFile, `id "mdl" must match the file name "other"`)
	})
	t.Run("yml refused", func(t *testing.T) {
		_, err := LoadFS(registryFS(map[string][]byte{"mdl.yml": good}))
		requireViolation(t, loadError(t, err), RuleFile, ".yaml")
	})
	t.Run("subdirectory refused", func(t *testing.T) {
		fsys := registryFS(map[string][]byte{"mdl.yaml": good, "old/x.yaml": good})
		_, err := LoadFS(fsys)
		requireViolation(t, loadError(t, err), RuleFile, "subdirector")
	})
	t.Run("other files ignored", func(t *testing.T) {
		r, err := LoadFS(registryFS(map[string][]byte{"mdl.yaml": good, "README.md": []byte("hi"), "registry.go": []byte("package x")}))
		if err != nil || r.Len() != 1 {
			t.Fatalf("Len = %v, err = %v", r, err)
		}
	})
	t.Run("empty registry", func(t *testing.T) {
		_, err := LoadFS(registryFS(map[string][]byte{"README.md": []byte("hi")}))
		requireViolation(t, loadError(t, err), RuleEmpty, "no .yaml")
	})
	t.Run("missing directory", func(t *testing.T) {
		if _, err := LoadDir(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("a missing directory loaded")
		}
	})
	t.Run("one bad file blocks everything", func(t *testing.T) {
		bad := mutated(t, candidateEntry("bad"), func(m map[string]any) { delete(m, "licence") })
		r, err := LoadFS(registryFS(map[string][]byte{"mdl.yaml": good, "bad.yaml": bad}))
		if r != nil {
			t.Error("a registry with an invalid file returned a partial result")
		}
		requireViolation(t, loadError(t, err), RuleSchema, "licence")
	})
	t.Run("all problems reported", func(t *testing.T) {
		a := mutated(t, candidateEntry("aaa"), func(m map[string]any) { delete(m, "licence") })
		b := mutated(t, candidateEntry("bbb"), func(m map[string]any) { m["extra"] = 1 })
		_, err := LoadFS(registryFS(map[string][]byte{"a.yaml": a, "b.yaml": b}))
		le := loadError(t, err)
		requireViolation(t, le, RuleSchema, "licence")
		requireViolation(t, le, RuleSchema, "extra")
	})
	t.Run("real directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "mdl.yaml"), good, 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := r.Get("mdl"); !ok || e.ID != "mdl" {
			t.Fatalf("Get(m) = %+v, %v", e, ok)
		}
		if _, ok := r.Get("absent"); ok {
			t.Error("Get returned an entry for an unknown id")
		}
	})
}

// TestRegistryFallbackRules: a fallback must name another live entry that
// serves the same role, and chains must not loop.
func TestRegistryFallbackRules(t *testing.T) {
	entry := func(id, fallback string, mods ...func(*Entry)) Entry {
		e := candidateEntry(id)
		if fallback != "" {
			e.Fallback.ModelID = strp(fallback)
		}
		for _, m := range mods {
			m(&e)
		}
		return e
	}
	load := func(es ...Entry) (*Registry, error) {
		files := map[string][]byte{}
		for _, e := range es {
			files[e.ID+".yaml"] = jsonOf(t, e)
		}
		return LoadFS(registryFS(files))
	}

	r, err := load(entry("aaa", "bbb"), entry("bbb", "ccc"), entry("ccc", ""))
	if err != nil {
		t.Fatalf("a valid chain was rejected: %v", err)
	}
	chain, err := r.FallbackChain("aaa")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range chain {
		ids = append(ids, e.ID)
	}
	if diff := cmp.Diff([]string{"bbb", "ccc"}, ids); diff != "" {
		t.Errorf("chain (-want +got):\n%s", diff)
	}
	if _, err := r.FallbackChain("absent"); err == nil {
		t.Error("FallbackChain of an unknown id returned no error")
	}

	_, err = load(entry("aaa", "ghost"))
	requireViolation(t, loadError(t, err), RuleFallback, `"ghost" is not in the registry`)

	self := entry("aaa", "aaa")
	if vs := self.Validate(); !hasRuleField(vs, RuleFallback, "fallback.model_id") {
		t.Errorf("Validate: self fallback not reported: %v", vs)
	}
	_, err = load(self)
	requireViolation(t, loadError(t, err), RuleFallback, "its own fallback")

	_, err = load(entry("aaa", "bbb"), entry("bbb", "aaa"))
	le := loadError(t, err)
	requireViolation(t, le, RuleFallback, "loops back")
	if n := le.Count(RuleFallback); n != 2 {
		t.Errorf("a two-entry loop must be reported once per member, got %d: %v", n, le)
	}

	_, err = load(entry("aaa", "bbb"), entry("bbb", "ccc"), entry("ccc", "bbb"))
	requireViolation(t, loadError(t, err), RuleFallback, "loops back")

	rejected := func(e *Entry) { e.Status = StatusRejected }
	_, err = load(entry("aaa", "bbb"), entry("bbb", "", rejected))
	requireViolation(t, loadError(t, err), RuleFallback, "is rejected")

	embedding := func(e *Entry) {
		e.Roles = []Role{RoleEmbedding}
		e.Embedding = &Embedding{Dimensions: intp(384)}
	}
	_, err = load(entry("aaa", "bbb"), entry("bbb", "", embedding))
	requireViolation(t, loadError(t, err), RuleFallback, "none of this entry's roles")
}

// TestRegistryCrossEntryDuplicateIDs: checkRegistry refuses two entries with
// one id even though LoadFS cannot produce that through file names alone.
func TestRegistryCrossEntryDuplicateIDs(t *testing.T) {
	items := []loaded{
		{file: "a.yaml", entry: candidateEntry("same")},
		{file: "b.yaml", entry: candidateEntry("same")},
	}
	vs := checkRegistry(items)
	if len(vs) != 1 || vs[0].Rule != RuleDuplicateID || vs[0].File != "b.yaml" || !strings.Contains(vs[0].Detail, "a.yaml") {
		t.Fatalf("violations = %v", vs)
	}
}

// TestEmbeddingRoleNeedsDimensions: the vector dimension fixes the index
// layout, so an embedding entry without it is rejected.
func TestEmbeddingRoleNeedsDimensions(t *testing.T) {
	e := candidateEntry("emb")
	e.Roles = []Role{RoleEmbedding}
	e.Capabilities = []string{"embedding"}
	if vs := e.Validate(); !hasRuleField(vs, RuleEmbedding, "embedding.dimensions") {
		t.Errorf("Validate: %v", vs)
	}
	_, err := loadOne(t, "emb", jsonOf(t, e))
	requireViolation(t, loadError(t, err), RuleSchema, "embedding")

	e.Embedding = &Embedding{Dimensions: intp(384), MaxInputTokens: intp(32768)}
	if vs := e.Validate(); len(vs) != 0 {
		t.Errorf("Validate with dimensions: %v", vs)
	}
	if _, err := loadOne(t, "emb", jsonOf(t, e)); err != nil {
		t.Errorf("embedding entry with dimensions rejected: %v", err)
	}
}

// TestStatusLifecycle: candidate -> qualified-emulated -> certified, with no
// way to skip qualification.
func TestStatusLifecycle(t *testing.T) {
	allow := map[Status][]Status{
		StatusCandidate:         {StatusQualifiedEmulated, StatusRejected},
		StatusQualifiedEmulated: {StatusCertified, StatusRejected},
		StatusCertified:         {StatusQualifiedEmulated, StatusRejected, StatusRetired},
		StatusRejected:          {StatusCandidate},
		StatusRetired:           nil,
	}
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			want := false
			for _, a := range allow[from] {
				if a == to {
					want = true
				}
			}
			if got := from.CanTransition(to); got != want {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, want)
			}
		}
	}
	if StatusCandidate.CanTransition(StatusCertified) {
		t.Error("a candidate must not become certified without qualification")
	}
	if Status("bogus").CanTransition(StatusCandidate) || StatusCandidate.CanTransition("bogus") {
		t.Error("an undefined status must never transition")
	}
	if Status("bogus").Valid() || !StatusCertified.Valid() {
		t.Error("Valid is wrong")
	}

	e := candidateEntry("mdl")
	e.Status = "bogus"
	if vs := e.Validate(); !hasRuleField(vs, RuleStatus, "status") {
		t.Errorf("Validate: unknown status not reported: %v", vs)
	}
	_, err := loadOne(t, "mdl", jsonOf(t, e))
	requireViolation(t, loadError(t, err), RuleSchema, "/status")
}

// TestEntriesAreSortedAndCopied: Entries and Get hand out copies in a stable order.
func TestEntriesAreSortedAndCopied(t *testing.T) {
	files := map[string][]byte{}
	for _, id := range []string{"ccc", "aaa", "bbb"} {
		files[id+".yaml"] = jsonOf(t, candidateEntry(id))
	}
	r, err := LoadFS(registryFS(files))
	if err != nil {
		t.Fatal(err)
	}
	es := r.Entries()
	if !sort.SliceIsSorted(es, func(i, j int) bool { return es[i].ID < es[j].ID }) || len(es) != 3 {
		t.Fatalf("entries not sorted: %v", es)
	}
	es[0].ID = "tampered"
	if first := r.Entries()[0]; first.ID != "aaa" {
		t.Errorf("changing the returned slice changed the registry: %q", first.ID)
	}
	if !errors.Is(&LoadError{}, ErrInvalidRegistry) {
		t.Error("LoadError must match ErrInvalidRegistry")
	}
}

// TestSchemaExamplesAgreeWithLoader: the examples that document the schema
// behave the same way through the Go loader, so the schema and the loader's
// rules cannot drift apart unnoticed. (go run ./tools/schemacheck schemas/models
// checks the examples against the schema alone.)
func TestSchemaExamplesAgreeWithLoader(t *testing.T) {
	root := filepath.Join("..", "..", "schemas", "models", "examples", "registry-entry")
	for _, kind := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(root, kind, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no %s examples found in %s (%v)", kind, root, err)
		}
		for _, f := range files {
			t.Run(kind+"/"+filepath.Base(f), func(t *testing.T) {
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				_, vs := ParseEntry(filepath.Base(f), data)
				switch {
				case kind == "valid" && len(vs) > 0:
					t.Errorf("a valid example was rejected: %v", vs)
				case kind == "invalid" && len(vs) == 0:
					t.Error("an invalid example was accepted")
				}
			})
		}
	}
}
