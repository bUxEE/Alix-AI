package models

import (
	"encoding/json"
	"os"
	"testing"
)

// scorerEntry is a valid, fully pinned decision-scorer candidate with no
// dependency: the shape of the small sub-1B scorers.
func scorerEntry(id string) Entry {
	e := candidateEntry(id)
	e.Roles = []Role{RoleDecisionScorer}
	e.Capabilities = []string{"decision-scoring"}
	e.Artifact.Format = "safetensors"
	e.Artifact.Filename = strp("model.safetensors")
	e.Artifact.SHA256 = strp(testSHA)
	e.Artifact.SizeBytes = i64p(3)
	e.Quantization = Quant{Scheme: "F16", NominalBits: 16}
	e.Backend = Backend{Runtime: "localai", Name: "vllm-cpp", AllowedRuntimeVersions: []string{}}
	e.Scorer = &Scorer{Architecture: ScorerNonAutoregressiveEncoder, MaxOptions: intp(20)}
	return e
}

// encoderEntry is a valid, pinned frozen-encoder dependency.
func encoderEntry(id string) Entry {
	e := scorerEntry(id)
	e.Roles = []Role{RoleDecisionEncoder}
	e.Capabilities = []string{"encoding"}
	e.Scorer = nil
	return e
}

// biEncoderEntry is a candidate bi-encoder scorer that depends on encoderID.
func biEncoderEntry(id, encoderID string) Entry {
	e := scorerEntry(id)
	e.Scorer = &Scorer{Architecture: ScorerBiEncoder}
	e.Dependencies = []Dependency{{ID: encoderID, Relation: DependencyFrozenEncoder}}
	return e
}

// registryOf builds a registry from entries; JSON is valid YAML.
func registryOf(t *testing.T, es ...Entry) (*Registry, error) {
	t.Helper()
	files := map[string][]byte{}
	for _, e := range es {
		files[e.ID+".yaml"] = jsonOf(t, e)
	}
	return LoadFS(registryFS(files))
}

// measuredScorer is a qualified-emulated scorer measured on profile with a
// peak of peakMiB: the evidence a scorer default needs.
func measuredScorer(id string, profile Profile, peakMiB float64) Entry {
	e := scorerEntry(id)
	e.Status = StatusQualifiedEmulated
	e.Review = Review{Date: strp("2026-10-08"), EvidencePath: strp("docs/efficiency/D-08.md")}
	e.Measurement = Measure{Profile: &profile, Emulated: boolp(true), ResultsPath: strp("benchmarks/results/d08/"), MeasuredOn: strp("2026-10-08")}
	e.Memory = Memory{PeakRSSMB: &peakMiB, SteadyRSSMB: &peakMiB}
	return e
}

// TestScorerEntryRequiresEncoderDependencyToResolve: a scorer that depends on
// a frozen encoder loads only when the encoder is in the registry, has the
// decision-encoder role, is pinned and is not rejected or retired; a
// bi-encoder without any dependency is refused on its own.
func TestScorerEntryRequiresEncoderDependencyToResolve(t *testing.T) {
	t.Run("resolves", func(t *testing.T) {
		r, err := registryOf(t, biEncoderEntry("scorer-bi", "enc"), encoderEntry("enc"))
		if err != nil {
			t.Fatalf("a scorer with a pinned encoder dependency was refused: %v", err)
		}
		if got, _ := r.Get("scorer-bi"); len(got.Dependencies) != 1 || got.Dependencies[0].ID != "enc" {
			t.Fatalf("dependency not loaded: %+v", got.Dependencies)
		}
	})

	cases := []struct {
		name    string
		entries []Entry
		rule    Rule
		field   string
	}{
		{"encoder missing from the registry", []Entry{biEncoderEntry("scorer-bi", "enc")}, RuleDependency, "dependencies[0]"},
		{"dependency is not an encoder", []Entry{biEncoderEntry("scorer-bi", "other"), scorerEntry("other")}, RuleDependency, "dependencies[0]"},
		{"encoder is unpinned", []Entry{biEncoderEntry("scorer-bi", "enc"), func() Entry {
			e := encoderEntry("enc")
			e.Artifact.SHA256, e.Artifact.SizeBytes = nil, nil
			return e
		}()}, RuleDependency, "dependencies[0]"},
		{"encoder is rejected", []Entry{biEncoderEntry("scorer-bi", "enc"), func() Entry {
			e := encoderEntry("enc")
			e.Status = StatusRejected
			return e
		}()}, RuleDependency, "dependencies[0]"},
		{"entry depends on itself", []Entry{func() Entry {
			e := biEncoderEntry("scorer-bi", "scorer-bi")
			return e
		}()}, RuleDependency, "dependencies[0]"},
		{"bi-encoder declares no dependency", []Entry{func() Entry {
			e := biEncoderEntry("scorer-bi", "enc")
			e.Dependencies = nil
			return e
		}(), encoderEntry("enc")}, RuleSchema, "dependencies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := registryOf(t, tc.entries...)
			le := loadError(t, err)
			requireViolation(t, le, tc.rule, tc.field)
		})
	}

	t.Run("bi-encoder without a dependency is refused by the loader rule too", func(t *testing.T) {
		e := biEncoderEntry("scorer-bi", "enc")
		e.Dependencies = nil
		if vs := e.Validate(); !hasRuleField(vs, RuleScorer, "dependencies") {
			t.Fatalf("Validate accepted a bi-encoder with no dependency: %v", vs)
		}
	})
	t.Run("duplicate dependency", func(t *testing.T) {
		e := biEncoderEntry("scorer-bi", "enc")
		e.Dependencies = append(e.Dependencies, e.Dependencies[0])
		if vs := e.Validate(); !hasRuleField(vs, RuleDependency, "dependencies[1]") {
			t.Fatalf("a duplicated dependency was accepted: %v", vs)
		}
	})
	t.Run("unknown relation is refused by the schema", func(t *testing.T) {
		_, vs := ParseEntry("scorer-bi.yaml", mutated(t, biEncoderEntry("scorer-bi", "enc"), func(m map[string]any) {
			m["dependencies"] = []any{map[string]any{"id": "enc", "relation": "teacher"}}
		}))
		if len(vs) == 0 {
			t.Fatal("an unknown dependency relation was accepted")
		}
	})
	t.Run("scorer block is required for a scorer and only for a scorer", func(t *testing.T) {
		noBlock := scorerEntry("scr")
		noBlock.Scorer = nil
		if vs := noBlock.Validate(); !hasRuleField(vs, RuleScorer, "scorer") {
			t.Errorf("a decision-scorer without the scorer block was accepted: %v", vs)
		}
		if _, vs := ParseEntry("scr.yaml", mutated(t, scorerEntry("scr"), func(m map[string]any) { delete(m, "scorer") })); len(vs) == 0 {
			t.Error("the schema accepted a decision-scorer without the scorer block")
		}
		stray := candidateEntry("cnd")
		stray.Scorer = &Scorer{Architecture: ScorerBiEncoder}
		if vs := stray.Validate(); !hasRuleField(vs, RuleScorer, "scorer") {
			t.Errorf("a scorer block on a non-scorer was accepted: %v", vs)
		}
	})
}

// TestScorerCannotBeDefaultForLiteWithoutEvidence: a decision scorer is the
// default of a profile only with evidence (qualified, measured on that profile,
// a results path and a measured peak inside the profile's scorer row). The
// experimental-4g profile never has one and a frozen encoder never is a default.
func TestScorerCannotBeDefaultForLiteWithoutEvidence(t *testing.T) {
	def := func(p Profile) []Default { return []Default{{Role: RoleDecisionScorer, Profile: p}} }

	t.Run("candidate with a default", func(t *testing.T) {
		e := scorerEntry("scr")
		e.DefaultFor = def(ProfileLite)
		if vs := e.Validate(); !hasRuleField(vs, RuleScorer, "default_for[0]") {
			t.Fatalf("a candidate scorer was accepted as the lite default: %v", vs)
		}
		if _, err := registryOf(t, e); err == nil {
			t.Fatal("a registry with a candidate scorer as the lite default loaded")
		}
	})

	t.Run("qualified without a results path", func(t *testing.T) {
		e := measuredScorer("scr", ProfileLite, 250)
		e.Measurement.ResultsPath = nil
		e.DefaultFor = def(ProfileLite)
		if vs := e.Validate(); !hasRuleField(vs, RuleScorer, "default_for[0]") {
			t.Fatalf("a scorer default without evidence was accepted: %v", vs)
		}
	})

	t.Run("measured on another profile", func(t *testing.T) {
		e := measuredScorer("scr", ProfileBalanced, 250)
		e.DefaultFor = def(ProfileLite)
		if vs := e.Validate(); !hasRuleField(vs, RuleScorer, "default_for[0]") {
			t.Fatalf("a balanced measurement justified a lite default: %v", vs)
		}
	})

	t.Run("no measured peak", func(t *testing.T) {
		e := measuredScorer("scr", ProfileLite, 250)
		e.Memory.PeakRSSMB = nil
		e.DefaultFor = def(ProfileLite)
		if vs := e.Validate(); !hasRuleField(vs, RuleScorer, "default_for[0]") {
			t.Fatalf("a scorer default with no measured peak was accepted: %v", vs)
		}
	})

	t.Run("peak over the scorer row", func(t *testing.T) {
		for _, tc := range []struct {
			p    Profile
			peak float64
			ok   bool
		}{
			{ProfileLite, 300, true}, {ProfileLite, 301, false},
			{ProfileBalanced, 600, true}, {ProfileBalanced, 601, false},
		} {
			e := measuredScorer("scr", tc.p, tc.peak)
			e.DefaultFor = def(tc.p)
			if got := len(e.Validate()) == 0; got != tc.ok {
				t.Errorf("%s peak %.0f MiB: accepted = %v, want %v (%v)", tc.p, tc.peak, got, tc.ok, e.Validate())
			}
		}
	})

	t.Run("experimental-4g never has a scorer", func(t *testing.T) {
		e := measuredScorer("scr", ProfileExperimental4G, 100)
		e.DefaultFor = def(ProfileExperimental4G)
		if vs := e.Validate(); !hasRuleField(vs, RuleScorer, "default_for[0]") {
			t.Fatalf("a scorer was accepted as the experimental-4g default: %v", vs)
		}
	})

	t.Run("a scorer that needs an encoder is a default for high only", func(t *testing.T) {
		for _, tc := range []struct {
			p  Profile
			ok bool
		}{{ProfileLite, false}, {ProfileBalanced, false}, {ProfileHigh, true}} {
			e := biEncoderEntry("scr", "enc")
			peak := 100.0
			e.Status = StatusQualifiedEmulated
			e.Review = Review{Date: strp("2026-10-08"), EvidencePath: strp("docs/efficiency/D-08.md")}
			e.Measurement = Measure{Profile: &tc.p, Emulated: boolp(true), ResultsPath: strp("benchmarks/results/d08/")}
			e.Memory = Memory{PeakRSSMB: &peak}
			e.DefaultFor = def(tc.p)
			if got := len(e.Validate()) == 0; got != tc.ok {
				t.Errorf("%s: accepted = %v, want %v (%v)", tc.p, got, tc.ok, e.Validate())
			}
		}
	})

	t.Run("a frozen encoder is never a default", func(t *testing.T) {
		e := encoderEntry("enc")
		e.DefaultFor = []Default{{Role: RoleDecisionEncoder, Profile: ProfileHigh}}
		if vs := e.Validate(); !hasRuleField(vs, RuleDefault, "default_for[0]") {
			t.Fatalf("a decision-encoder default was accepted: %v", vs)
		}
		if _, vs := ParseEntry("enc.yaml", jsonOf(t, e)); len(vs) == 0 {
			t.Fatal("the schema accepted a decision-encoder default")
		}
	})

	t.Run("evidence makes the default valid", func(t *testing.T) {
		e := measuredScorer("scr", ProfileLite, 250)
		e.DefaultFor = def(ProfileLite)
		if vs := e.Validate(); len(vs) != 0 {
			t.Fatalf("a qualified, measured scorer inside the lite row was refused: %v", vs)
		}
		r, err := registryOf(t, e)
		if err != nil {
			t.Fatalf("registry with an evidenced scorer default: %v", err)
		}
		if got, ok := r.DefaultFor(RoleDecisionScorer, ProfileLite); !ok || got.ID != "scr" {
			t.Fatalf("DefaultFor = %v, %v", got.ID, ok)
		}
	})
}

// TestCLM8BEntryStartsAsCandidateAndUnpinned: the shipped CLM-v0.1-8B entry is a
// candidate that no profile defaults to and no runtime version has qualified:
// "unpinned" as a default and as a runtime, with nothing measured. Its frozen
// Qwen3-8B encoder resolves to a pinned entry that is itself a candidate and
// never a default. (The files of both are hash-pinned so the CLM sidecar and
// the release manifest can name them; pinned is not qualified.)
func TestCLM8BEntryStartsAsCandidateAndUnpinned(t *testing.T) {
	r, err := LoadDir(realRegistry, WithEvidenceFS(repoEvidenceFS(t)))
	if err != nil {
		t.Fatalf("the shipped registry is invalid: %v", err)
	}
	clm, ok := r.Get("clm-v0.1-8b")
	if !ok {
		t.Fatal("clm-v0.1-8b is missing from the registry")
	}
	if clm.Status != StatusCandidate {
		t.Errorf("status = %s, want candidate (qualified-emulated only through D-08 evidence)", clm.Status)
	}
	if !clm.HasRole(RoleDecisionScorer) || clm.Scorer == nil || clm.Scorer.Architecture != ScorerBiEncoder {
		t.Errorf("CLM is a bi-encoder decision scorer, got roles %v scorer %+v", clm.Roles, clm.Scorer)
	}
	if len(clm.DefaultFor) != 0 {
		t.Errorf("default_for = %v, want none: no profile defaults to CLM here", clm.DefaultFor)
	}
	for _, p := range []Profile{ProfileExperimental4G, ProfileLite, ProfileBalanced, ProfileHigh} {
		if got, ok := r.DefaultFor(RoleDecisionScorer, p); ok {
			t.Errorf("%s has the decision-scorer default %s; with no D-08 evidence the default is the deterministic baseline (no model)", p, got.ID)
		}
		if got, ok := r.DefaultFor(RoleDecisionEncoder, p); ok {
			t.Errorf("%s has the decision-encoder default %s; an encoder is a dependency only", p, got.ID)
		}
	}
	if len(clm.Backend.AllowedRuntimeVersions) != 0 {
		t.Errorf("allowed_runtime_versions = %v: no runtime version has qualified CLM", clm.Backend.AllowedRuntimeVersions)
	}
	m, s, l, p := clm.Memory, clm.Scores, clm.LoadTime, clm.Performance
	if m.PeakRSSMB != nil || m.SteadyRSSMB != nil || s.Task != nil || s.Tool != nil || s.RAG != nil || s.Safety != nil ||
		l.ColdMS != nil || l.WarmMS != nil || p.LatencyMSP50 != nil || p.TokensPerSecond != nil ||
		clm.Measurement.ResultsPath != nil || clm.Measurement.Profile != nil || len(clm.Languages.Qualified) != 0 {
		t.Errorf("CLM carries a measurement or qualification it has not earned: %+v %+v", m, s)
	}

	if len(clm.Dependencies) != 1 || clm.Dependencies[0].Relation != DependencyFrozenEncoder {
		t.Fatalf("dependencies = %+v, want one frozen-encoder", clm.Dependencies)
	}
	enc, ok := r.Get(clm.Dependencies[0].ID)
	if !ok {
		t.Fatalf("encoder %q does not resolve", clm.Dependencies[0].ID)
	}
	a := enc.Artifact
	if enc.ID != "qwen3-8b" || !enc.HasRole(RoleDecisionEncoder) || enc.Status != StatusCandidate || len(enc.DefaultFor) != 0 {
		t.Errorf("encoder = %s roles %v status %s defaults %v", enc.ID, enc.Roles, enc.Status, enc.DefaultFor)
	}
	if a.Filename == nil || a.Revision == nil || a.SHA256 == nil || a.SizeBytes == nil {
		t.Errorf("the Qwen3-8B encoder dependency must be pinned: %+v", a)
	}

	// Laya and every other small scorer is a benchmark candidate and fallback.
	if clm.Fallback.ModelID == nil {
		t.Error("CLM names no scorer fallback; Laya is the benchmark fallback")
	} else if fb, ok := r.Get(*clm.Fallback.ModelID); !ok || !fb.HasRole(RoleDecisionScorer) || fb.Status != StatusCandidate {
		t.Errorf("fallback %v is not a candidate decision scorer", clm.Fallback.ModelID)
	}
	if clm.Fallback.DegradedMode != "deterministic-routing" {
		t.Errorf("degraded mode = %s, want the deterministic baseline", clm.Fallback.DegradedMode)
	}

	// At least one sub-1B scorer is registered next to it, with its size recorded.
	small := 0
	for _, e := range r.Entries() {
		if e.HasRole(RoleDecisionScorer) && e.Scorer != nil && e.Scorer.Architecture != ScorerBiEncoder &&
			e.Artifact.SizeBytes != nil && *e.Artifact.SizeBytes < 2_000_000_000 && e.Status == StatusCandidate {
			small++
		}
	}
	if small == 0 {
		t.Error("no sub-2 GB candidate decision scorer is registered")
	}
}

// TestSchemaRevisionMatchesPublished: the Go constant and the published
// schema's x-schema-revision keyword move together.
func TestSchemaRevisionMatchesPublished(t *testing.T) {
	data, err := os.ReadFile(publishedSchema)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Rev int    `json:"x-schema-revision"`
		ID  string `json:"$id"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Rev != SchemaRevision {
		t.Errorf("x-schema-revision = %d, SchemaRevision = %d", doc.Rev, SchemaRevision)
	}
	if doc.ID != SchemaID {
		t.Errorf("$id = %q, SchemaID = %q", doc.ID, SchemaID)
	}
}
