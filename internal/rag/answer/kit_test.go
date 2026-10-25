package answer

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// scopeOf is a valid scope for a tenant and principal of the test application.
func scopeOf(tenant, principal string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_test",
		ApplicationID:    "app_test",
		TenantID:         tenant,
		WorkspaceID:      "w1",
		PrincipalID:      principal,
		Roles:            []string{"member"},
		CapabilityScopes: []string{"rag.answer"},
		SessionID:        "ses_test",
	}
}

// ta is the scope most tests answer under.
var ta = scopeOf("ta", "usr_alice")

// spec describes one evidence item of a test package.
type spec struct {
	source  string // source key, becomes the display name; the same key is the same source
	text    string
	version string
	avail   conversation.CitationAvailability
	stale   conversation.CitationStaleness
	section string
}

// srcIDs maps a source key to its src_ ID, stable inside one test binary run.
var (
	srcMu  sync.Mutex
	srcIDs = map[string]string{}
)

func srcID(key string) string {
	srcMu.Lock()
	defer srcMu.Unlock()
	if id, ok := srcIDs[key]; ok {
		return id
	}
	id := ids.New("src")
	srcIDs[key] = id
	return id
}

// mkPackage builds the package the query pipeline would build for the scope.
func mkPackage(scope contracts.Scope, question string, specs ...spec) rag.EvidencePackage {
	items := make([]rag.EvidenceItem, len(specs))
	toks := 0
	for i, s := range specs {
		if s.version == "" {
			s.version = "v1"
		}
		if s.avail == "" {
			s.avail = conversation.CitationAvailabilityAvailable
		}
		if s.stale == "" {
			s.stale = conversation.CitationStalenessCurrent
		}
		section := s.section
		if section == "" {
			section = "Section " + strconv.Itoa(i+1)
		}
		loc := conversation.CitationLocator{
			Section: &section,
			Span:    &conversation.CitationSpan{Start: int64(i * 1000), End: int64(i*1000 + len(s.text))},
		}
		egress, class := rag.SourceEgressLocalOnly, rag.SourceClassificationConfidential
		est := (len(s.text) + 2) / 3
		toks += est
		items[i] = rag.EvidenceItem{
			EvidenceID:           rag.EvidenceID("e" + strconv.Itoa(i+1)),
			Rank:                 int64(i + 1),
			ChunkID:              common.Identifier("chk_" + strconv.Itoa(i+1)),
			GenerationID:         rag.GenerationID(ids.New("gen")),
			SourceID:             actions.SourceID(srcID(s.source)),
			DisplayName:          s.source,
			Version:              actions.OpaqueVersion(s.version),
			Locator:              &loc,
			Availability:         s.avail,
			Staleness:            s.stale,
			OpenRef:              conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
			Text:                 s.text,
			EstToks:              rag.EstToks(est),
			SourceEgress:         &egress,
			SourceClassification: &class,
		}
	}
	return rag.EvidencePackage{
		PackageID: rag.EvidencePackageID(ids.New("evp")),
		Scope: rag.EvidencePackageScope{
			ApplicationID: common.Identifier(scope.ApplicationID),
			TenantID:      common.Identifier(scope.TenantID),
			WorkspaceID:   common.Identifier(scope.WorkspaceID),
			PrincipalID:   common.Identifier(scope.PrincipalID),
		},
		Query:     rag.SearchRequestQuery(question),
		CreatedAt: common.Timestamp("2026-10-03T09:00:00.000Z"),
		Items:     items,
		Budget:    rag.EvidencePackageBudget{MaxItems: 8, MaxToks: 3000, EstToks: rag.EstToks(toks)},
		Retrieval: rag.EvidencePackageRetrieval{Methods: []rag.RetrievalMethod{rag.RetrievalMethodFts}, Notices: rag.RetrievalNotices{}},
	}
}

// fakeCatalog is the source catalog of the tests: it applies the scope it is handed,
// reads sources by tenant, and can be changed between calls.
type fakeCatalog struct {
	mu     sync.Mutex
	states map[string]query.SourceState // tenant + "/" + source ID
	err    error
	calls  int
	asked  [][]string
}

func newCatalog() *fakeCatalog { return &fakeCatalog{states: map[string]query.SourceState{}} }

func key(tenant, source string) string { return tenant + "/" + source }

// grant makes the sources of the package readable to the scope's tenant, in the state
// each item states.
func (c *fakeCatalog) grant(scope contracts.Scope, pkg rag.EvidencePackage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range pkg.Items {
		c.states[key(scope.TenantID, string(it.SourceID))] = query.SourceState{
			DisplayName: it.DisplayName, Authorised: true, Availability: it.Availability, Staleness: it.Staleness,
		}
	}
}

func (c *fakeCatalog) set(scope contracts.Scope, source string, f func(*query.SourceState)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key(scope.TenantID, srcID(source))
	st := c.states[k]
	f(&st)
	c.states[k] = st
}

func (c *fakeCatalog) revoke(scope contracts.Scope, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.states, key(scope.TenantID, srcID(source)))
}

func (c *fakeCatalog) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *fakeCatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.asked = append(c.asked, append([]string(nil), sourceIDs...))
	if c.err != nil {
		return nil, c.err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		if st, ok := c.states[key(scope.TenantID, id)]; ok {
			out[id] = st
		}
	}
	return out, nil
}

// env bundles an Answerer with its fakes.
type env struct {
	t   *testing.T
	gen *fake.Provider
	cat *fakeCatalog
	clk *clock.Fake
	a   *Answerer
}

func newEnv(t *testing.T) *env { return newEnvCfg(t, DefaultConfig()) }

func newEnvCfg(t *testing.T, cfg Config) *env {
	t.Helper()
	e := &env{t: t, gen: fake.New(), cat: newCatalog(), clk: clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))}
	a, err := New(Deps{Generator: e.gen, Catalog: e.cat, Clock: e.clk}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.a = a
	return e
}

// claim is one claim as the scripted model writes it.
type claim struct {
	Text string   `json:"text"`
	IDs  []string `json:"evidence_ids"`
}

// reply scripts the model's output.
func (e *env) reply(claims ...claim) {
	e.t.Helper()
	if claims == nil {
		claims = []claim{}
	}
	e.gen.Reset()
	e.gen.On(fake.Any()).ReplyJSON(map[string]any{"claims": claims})
}

// answer grants the package's sources and answers it.
func (e *env) answer(scope contracts.Scope, pkg rag.EvidencePackage) Result {
	e.t.Helper()
	e.cat.grant(scope, pkg)
	res, err := e.a.Answer(context.Background(), scope, pkg)
	if err != nil {
		e.t.Fatalf("Answer: %v", err)
	}
	return res
}

func grounded(t *testing.T, r Result) rag.GroundedAnswer {
	t.Helper()
	ga, ok := r.Answer.Value.(rag.GroundedAnswer)
	if !ok {
		t.Fatalf("outcome is %T %+v, want a grounded answer", r.Answer.Value, r.Answer.Value)
	}
	return ga
}

func noEvidence(t *testing.T, r Result, reason rag.NoSupportedEvidenceReason) rag.NoSupportedEvidence {
	t.Helper()
	ns, ok := r.Answer.Value.(rag.NoSupportedEvidence)
	if !ok {
		t.Fatalf("outcome is %T %+v, want no_supported_evidence", r.Answer.Value, r.Answer.Value)
	}
	if ns.Reason != reason {
		t.Fatalf("reason = %q, want %q", ns.Reason, reason)
	}
	return ns
}

func claimTexts(ga rag.GroundedAnswer) []string {
	out := make([]string, len(ga.Claims))
	for i, c := range ga.Claims {
		out[i] = c.Text
	}
	return out
}

// checkInvariants asserts what provenance promises about an answer: every claim cites
// at least one existing citation and appears among that citation's claims, every
// citation supports at least one claim, and the answer encodes.
func checkInvariants(t *testing.T, ga rag.GroundedAnswer) {
	t.Helper()
	if len(ga.Claims) == 0 || len(ga.Citations) == 0 {
		t.Fatalf("answer with %d claims and %d citations", len(ga.Claims), len(ga.Citations))
	}
	used := map[int64]bool{}
	for _, cl := range ga.Claims {
		if len(cl.CitationIndexes) == 0 {
			t.Errorf("claim %q has no citation", cl.Text)
		}
		for _, ix := range cl.CitationIndexes {
			if ix < 0 || int(ix) >= len(ga.Citations) {
				t.Fatalf("claim %q cites index %d of %d citations", cl.Text, ix, len(ga.Citations))
			}
			used[ix] = true
			listed := false
			for _, ct := range ga.Citations[ix].Claims {
				listed = listed || ct == cl.Text
			}
			if !listed {
				t.Errorf("claim %q is not listed by its citation %d (%v)", cl.Text, ix, ga.Citations[ix].Claims)
			}
		}
	}
	for i, c := range ga.Citations {
		if !used[int64(i)] {
			t.Errorf("citation %d supports no claim", i)
		}
		if len(c.Claims) == 0 {
			t.Errorf("citation %d lists no claim", i)
		}
	}
	if _, err := json.Marshal(ga); err != nil {
		t.Fatalf("the answer does not encode: %v", err)
	}
}

// ----- the schema ------------------------------------------------------------

// compileSchemas compiles every published schema so a $ref between areas resolves;
// it reads the repository's schemas directory, the contract itself.
func compileSchemas(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	root := filepath.Join("..", "..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		obj, _ := doc.(map[string]any)
		id, _ := obj["$id"].(string)
		if id == "" {
			return nil
		}
		return c.AddResource(id, doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// validateAnswer checks an answer result against the published schema.
func validateAnswer(t *testing.T, sch *jsonschema.Schema, r rag.AnswerResult) {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("the answer does not match schemas/rag/answer-result: %v\n%s", err, raw)
	}
}

// ----- the evidence of the suite ---------------------------------------------

const (
	warrantyText = "The warranty lasts 2 years from the date of purchase.\n" +
		"It covers manufacturing defects but not water damage.\n" +
		"To claim, contact support within 30 days of discovering the defect."
	returnsText = "Customers may return unopened products within 14 days of delivery.\n" +
		"Refunds are processed within 5 business days after the product is received."
	pricingText = "The Pro plan costs 49.90 EUR per month.\n" +
		"The Team plan costs 120 EUR per month and includes 10 seats."
)

// queryState is the source state the fake catalog holds.
type queryState = query.SourceState

// schemaOf compiles one published schema.
func schemaOf(t *testing.T, id string) *jsonschema.Schema {
	t.Helper()
	sch, err := compileSchemas(t).Compile(id)
	if err != nil {
		t.Fatalf("compile %s: %v", id, err)
	}
	return sch
}

func itoa(n int) string { return strconv.Itoa(n) }

func fileIDString(s string) actions.FileID { return actions.FileID(s) }

// fileIDOf returns the file ID of a citation's open reference.
func fileIDOf(t *testing.T, c conversation.Citation) actions.FileID {
	t.Helper()
	f, ok := c.OpenRef.Value.(conversation.CitationOpenRefFile)
	if !ok {
		t.Fatalf("open_ref is %T, want a file reference", c.OpenRef.Value)
	}
	return f.FileID
}

// flipLast changes the last character of an ID.
func flipLast(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[len(b)-1] == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	return string(b)
}
