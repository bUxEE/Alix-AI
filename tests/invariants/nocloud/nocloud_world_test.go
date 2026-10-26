package nocloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/parse"
	fsprov "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	clinic "github.com/buxee/alix-ai/tests/conversation"
)

// ---- the dialer that fails on any destination outside the host -------------------------------------

// netGuard is the outbound-network block of the in-process run: every connection the process opens
// through the default HTTP transport and every name lookup through the default resolver goes through it.
// A loopback destination is allowed (the host's own); any other destination, and every name that is not
// an IP address, is recorded and refused. SEC-071: the run is only clean when nothing was attempted.
type netGuard struct {
	mu       sync.Mutex
	attempts []string
	real     net.Dialer
}

func (g *netGuard) record(kind, what string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempts = append(g.attempts, kind+" "+what)
}

// dial is the http.Transport's DialContext.
func (g *netGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
			return g.real.DialContext(ctx, network, addr)
		}
	}
	g.record("dial", network+" "+addr)
	return nil, fmt.Errorf("m5 gate: outbound network is blocked: %s %s", network, addr)
}

// resolverDial is the default resolver's Dial: a pure-Go lookup of a name reaches it, so a lookup
// is recorded and refused before it can leave the machine.
func (g *netGuard) resolverDial(_ context.Context, network, addr string) (net.Conn, error) {
	g.record("resolve", network+" "+addr)
	return nil, fmt.Errorf("m5 gate: name resolution is blocked: %s %s", network, addr)
}

// install puts the guard in front of the default transport and resolver, and restores them.
func (g *netGuard) install(t *testing.T) {
	t.Helper()
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T", http.DefaultTransport)
	}
	oldDial, oldProxy, oldResolver := tr.DialContext, tr.Proxy, net.DefaultResolver
	tr.DialContext, tr.Proxy = g.dial, nil
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: g.resolverDial}
	tr.CloseIdleConnections()
	t.Cleanup(func() {
		tr.DialContext, tr.Proxy = oldDial, oldProxy
		net.DefaultResolver = oldResolver
		tr.CloseIdleConnections()
	})
}

func (g *netGuard) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.attempts...)
}

// ---- the stack ----------------------------------------------------------------------------------------

var ncPinned = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// ncEmbedModel is the embedding model of the world: the fake embedder reports it and the vector
// generations are built with it.
const ncEmbedModel = "test-embed-32"

const ncClinicSource = "clinic-policies"

type activePkg struct{ capabilities.Package }

func (activePkg) Status() policy.PackageStatus { return policy.StatusActive }

type allowScopes struct{}

func (allowScopes) CheckScope(context.Context, contracts.Scope) error { return nil }

// ncStack is the composition under test: the real dialogue engine, policy gateway, action coordinator and
// session store over the clinic-booking package and its stand-in host, and the real RAG pipeline (parser
// worker, chunker, FTS5 and vector indexes, durable job queue, ingestion, query with RRF, grounded
// answerer) over the package's own policies folder. Only the models are scripted.
type ncStack struct {
	t     *testing.T
	clk   *clock.Fake
	db    *store.DB
	eng   *dialogue.Engine
	coord *actions.Coordinator
	ctl   *pfake.Provider // the controller of the dialogue
	scope contracts.Scope

	q    *jobs.Queue
	pool *jobs.Pool
	svc  *ingest.Service
	pipe *query.Pipeline
	ans  *answer.Answerer
	fp   *pfake.Provider // embedding and grounded answering
	reg  *ncRegistry

	srcID string
	files map[string][]byte
}

func (s *ncStack) count(table string) int {
	s.t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func newNCStack(t *testing.T, parserWorker string) *ncStack {
	t.Helper()
	ctx := context.Background()
	root := moduleRoot(t)
	pkgDir := filepath.Join(root, "examples", "reference-apps", "clinic-booking", "package")
	res, err := apppkg.Load(ctx, apppkg.FromDir(pkgDir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil || !res.OK() {
		t.Fatalf("the clinic package does not load clean: %v %v", err, res.Findings)
	}
	pkg := activePkg{capabilities.FromApppkg(res.Package)}

	s := &ncStack{t: t, clk: clock.NewFake(ncPinned), scope: clinic.RoleScope(clinic.RoleReceptionist), files: map[string][]byte{}}
	s.db = storetest.Open(t, nil)
	alog, err := audit.New(s.db, audit.Options{Clock: s.clk})
	if err != nil {
		t.Fatal(err)
	}

	// ---- text and action: the engine over the clinic package ----
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		sub := r.Subject
		return audit.ForIDs(sub.ApplicationID, sub.TenantID, sub.WorkspaceID, sub.PrincipalID, sub.SessionID)
	})
	gw := policy.NewGateway(s.clk, policy.WithSink(sink))
	creg := capabilities.NewRegistry(nil, capabilities.WithClock(s.clk))
	if err := creg.Activate(pkg); err != nil {
		t.Fatal(err)
	}
	guard, err := adapters.Wrap(clinic.NewClinicHost(s.clk).Adapter(), adapters.Options{ApplicationID: string(res.Package.Manifest.Application.ID)})
	if err != nil {
		t.Fatal(err)
	}
	adapterFor := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
	s.coord, err = actions.New(actions.Deps{
		DB: s.db, Policy: gw, Audit: alog, Clock: s.clk, Adapters: adapterFor,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	ss, err := sessions.New(s.db, s.clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := dctx.ProfileByName(dctx.ProfileNames()[0])
	if err != nil {
		t.Fatal(err)
	}
	s.ctl = pfake.New()
	sel, err := dialogue.LocalSelector(s.ctl, profile.Budget())
	if err != nil {
		t.Fatal(err)
	}

	// ---- RAG: the real pipeline over the package's own policies folder ----
	pause := ingest.NewPause(ingest.DiskPressureFunc(func(context.Context) (bool, error) { return false, nil }), s.clk, 30*time.Second, nil)
	s.q, err = jobs.NewQueue(s.db, jobs.Options{Config: jobs.DefaultConfig(), Clock: s.clk, Policy: pause.Policy(nil), Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	ix, err := index.New(s.db, s.clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	pcfg := parse.DefaultConfig()
	pcfg.WorkerPath = parserWorker
	pcfg.StartupTimeout = 90 * time.Second
	pcfg.WallTimeout = 60 * time.Second
	runr, err := parse.NewRunner(pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runr.Close() })

	s.fp = pfake.New(pfake.WithModel(ncEmbedModel))
	ncInstallAnswerer(s.fp)
	s.reg = &ncRegistry{prov: fsprov.New(fsprov.Config{Clock: s.clk}), defs: map[string]source.Definition{}}

	vc := vector.DefaultConfig()
	vc.CacheMiB = 0
	flat, err := vector.NewFlat(s.db, vector.FlatOptions{Config: vc, Clock: s.clk})
	if err != nil {
		t.Fatal(err)
	}
	s.svc, err = ingest.New(ingest.Deps{
		DB: s.db, Clock: s.clk, Queue: s.q, Index: ix, Registry: s.reg,
		Parser: ingest.NewRunnerParser(runr), Chunker: chk, Audit: alog, Pause: pause, Config: ingest.DefaultConfig(),
		Vectors: flat, Embedding: &ingest.Embedding{Embedder: s.fp, Model: ncEmbedModel, Component: rag.ComponentVersion{Name: "fake-embed", Version: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.pool, err = jobs.NewPool(s.q, jobs.PoolOptions{ScopeChecker: allowScopes{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.svc.Handle(s.pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.pool.Shutdown(sctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})
	roles := providers.NewRegistry(s.clk)
	if err := roles.Register(providers.RoleEmbedder, s.fp); err != nil {
		t.Fatal(err)
	}
	cat := &ncCatalog{s: s}
	s.pipe, err = query.New(query.Deps{
		Lexical: ix, Hits: ix, Vectors: flat, Catalog: cat, Providers: roles, EmbeddingModel: ncEmbedModel, Clock: s.clk,
	}, query.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.ans, err = answer.New(answer.Deps{Generator: s.fp, Catalog: cat, Clock: s.clk}, answer.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	s.eng, err = dialogue.New(dialogue.Deps{
		Sessions: ss, Registry: creg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil }),
		Policy:   gw, Actions: s.coord, Adapters: adapterFor, Providers: sel, Audit: alog, Clock: s.clk,
		Config:   dialogue.Config{Profile: profile, TemplateOnly: ncAllActs()},
		Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{},
		Knowledge: ncKnowledge{pipe: s.pipe, ans: s.ans},
	})
	if err != nil {
		t.Fatal(err)
	}

	s.hostPolicies(res, pkgDir)
	return s
}

// ncAllActs is every act of the dialogue: a stack with all of them templated has no model-phrased
// response, so a reply is exactly what the deterministic layer verified.
func ncAllActs() []conversation.DialogueDirectiveAct {
	return []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActRequestConfirmation,
		conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure,
		conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActOfferHelp,
		conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
	}
}

// ncKnowledge is the dialogue's Knowledge: retrieval then the grounded answerer, in the caller's scope.
type ncKnowledge struct {
	pipe *query.Pipeline
	ans  *answer.Answerer
}

func (k ncKnowledge) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	res, err := k.pipe.Query(ctx, scope, query.Request{Text: string(req.Question)})
	if err != nil {
		return rag.AnswerResult{}, err
	}
	out, err := k.ans.Answer(ctx, scope, res.Package)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	return out.Answer, nil
}

// hostPolicies registers the clinic package's own policies folder as a source of the stack's scope: the
// package's declaration as it is, except that the root becomes a folder source over a copy of the real files
// (no provider that serves a package root is part of this build) and the scope bindings become request
// bound.
func (s *ncStack) hostPolicies(res *apppkg.Result, pkgDir string) {
	s.t.Helper()
	decl, ok := res.Package.Sources[ncClinicSource]
	if !ok {
		s.t.Fatalf("the clinic package no longer declares a %q source", ncClinicSource)
	}
	pr, ok := decl.Root.Value.(ap.RAGSourceRootPackage)
	if !ok {
		s.t.Fatalf("the %q source root is %T, want a package root", ncClinicSource, decl.Root.Value)
	}
	folder := filepath.Join(pkgDir, filepath.FromSlash(pr.Path))
	if _, err := os.Stat(folder); err != nil {
		folder = filepath.Join(pkgDir, "documentation", filepath.FromSlash(pr.Path))
	}
	base, err := filepath.EvalSymlinks(s.t.TempDir())
	if err != nil {
		s.t.Fatal(err)
	}
	root := filepath.Join(base, "policies")
	old := time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
	err = filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return err
		}
		rel, err := filepath.Rel(folder, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		s.files[filepath.ToSlash(rel)] = data
		return os.Chtimes(dst, old, old)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	if len(s.files) < 4 {
		s.t.Fatalf("the clinic policies folder %s holds %d markdown files, want at least 4", folder, len(s.files))
	}
	decl.Type = ap.RAGSourceTypeLocalFolder
	decl.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}}
	decl.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest}
	decl.Scope.Workspace = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest}
	s.srcID = ids.New(source.IDPrefix)
	def, err := source.FromDeclaration(decl, source.Binding{SourceID: s.srcID, TenantID: s.scope.TenantID, WorkspaceID: s.scope.WorkspaceID})
	if err != nil {
		s.t.Fatal(err)
	}
	s.reg.set(def)
	s.reg.title = def.Title
}

// syncPolicies queues a sync of the source and drains the queue until every document is parsed, indexed
// and embedded.
func (s *ncStack) syncPolicies() ingest.SourceInfo {
	s.t.Helper()
	ctx := context.Background()
	sc := s.scope
	sc.CapabilityScopes = append(append([]string(nil), sc.CapabilityScopes...), "rag.sync", "rag.query")
	res, err := s.svc.RequestSync(ctx, sc, s.srcID, ingest.SyncOptions{Key: "m5-gate:1"})
	if err != nil {
		s.t.Fatal(err)
	}
	s.drain()
	job, err := s.q.Get(ctx, sc, res.Job.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	if job.State != jobs.StateSucceeded {
		s.t.Fatalf("the sync ended %s: %s\n%s", job.State, job.ErrorMessage, s.dumpJobs())
	}
	st, err := s.svc.Status(ctx, sc, s.srcID)
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

func (s *ncStack) running() int {
	n := 0
	for _, c := range jobs.Classes() {
		n += s.pool.Running(c)
	}
	return n
}

func (s *ncStack) pending() int {
	var n int
	row := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM jobs WHERE state = 'running' OR (state = 'pending' AND (attempts > 0 OR run_at <= ?))`,
		s.clk.Now().UnixMilli())
	if err := row.Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func (s *ncStack) drain() {
	s.t.Helper()
	stuck := 0
	for i := 0; i < 5000; i++ {
		n, err := s.pool.Poll(context.Background())
		if err != nil {
			s.t.Fatal(err)
		}
		deadline := time.Now().Add(120 * time.Second)
		for s.running() > 0 {
			if time.Now().After(deadline) {
				s.t.Fatal("job handlers did not finish")
			}
			time.Sleep(time.Millisecond)
		}
		if s.pending() == 0 {
			return
		}
		if n == 0 {
			if stuck++; stuck > 40 {
				s.t.Fatalf("the queue did not drain: %d jobs left\n%s", s.pending(), s.dumpJobs())
			}
			s.clk.Advance(30 * time.Second)
			continue
		}
		stuck = 0
	}
	s.t.Fatal("drain did not finish")
}

func (s *ncStack) dumpJobs() string {
	rows, err := s.db.QueryContext(context.Background(), `SELECT type, state, attempts, coalesce(error_code, ''), coalesce(error_message, '') FROM jobs ORDER BY seq`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, state, code, msg string
		var attempts int
		if err := rows.Scan(&typ, &state, &attempts, &code, &msg); err != nil {
			return err.Error()
		}
		fmt.Fprintf(&b, "  %-24s %-9s attempts=%d %s %s\n", typ, state, attempts, code, msg)
	}
	return b.String()
}

// typed is a text turn, the way POST /v1/sessions/{id}/turns runs it. The clock moves by a user's typing
// time first so the engine's own rate limit stays in force.
func (s *ncStack) typed(in dialogue.TurnInput) dialogue.Result {
	s.t.Helper()
	s.clk.Advance(15 * time.Second)
	res, err := s.eng.Turn(context.Background(), s.scope, in)
	if err != nil {
		s.t.Fatalf("text turn %+v: %v", in, err)
	}
	return res
}

// propose scripts the controller's single proposal for the next model-bound turn.
func (s *ncStack) propose(capability string, fields map[string]any) {
	b, err := json.Marshal(map[string]any{
		"capability_id": capability, "fields": fields,
		"entity_refs": []any{}, "intent": "task", "uncertainty": map[string]any{"overall": 0.9},
	})
	if err != nil {
		panic(err)
	}
	s.ctl.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(string(b)).Times(1)
}

// ---- the registry and the catalog of sources --------------------------------------------------------

type ncRegistry struct {
	mu    sync.Mutex
	prov  source.Provider
	defs  map[string]source.Definition
	title string
}

func (r *ncRegistry) Resolve(_ context.Context, scope contracts.Scope, id string) (ingest.Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[id]
	if !ok || d.CheckScope(scope) != nil {
		return ingest.Resolved{}, ingest.ErrSourceGone
	}
	return ingest.Resolved{Def: d, Provider: r.prov}, nil
}

func (r *ncRegistry) set(d source.Definition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[d.SourceID] = d
}

// ncCatalog is the deterministic authority on sources: a source is readable only by the scope it was
// registered under, and its state is read from the ingestion status.
type ncCatalog struct{ s *ncStack }

func (c *ncCatalog) Sources(ctx context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		if id != c.s.srcID || scope.ApplicationID != c.s.scope.ApplicationID || scope.TenantID != c.s.scope.TenantID || scope.WorkspaceID != c.s.scope.WorkspaceID {
			continue // not readable by this scope: indistinguishable from not existing
		}
		info, err := c.s.svc.Status(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		st := query.SourceState{DisplayName: c.s.reg.title, Authorised: true}
		if info.Available {
			st.Availability = conversation.CitationAvailabilityAvailable
			st.Staleness = conversation.CitationStalenessUnknown
		} else {
			st.Availability = conversation.CitationAvailabilityUnavailable
		}
		out[id] = st
	}
	return out, nil
}

// ---- the scripted grounded-answering model ------------------------------------------------------------

var (
	ncQuestionRE = regexp.MustCompile(`(?s)<question>\n(.*?)\n</question>`)
	ncEvidenceRE = regexp.MustCompile(`(?s)<evidence id="(e\d+)">\n(.*?)\n</evidence>`)
	ncSentenceRE = regexp.MustCompile(`[^.!?]+[.!?]+`)
)

// ncInstallAnswerer registers a faithful extractor as the controller role of the answering provider: for
// each passage it is given it states the sentence that shares most terms with the question, citing that
// passage and no other. Embeddings use the fake's deterministic defaults.
func ncInstallAnswerer(fp *pfake.Provider) {
	fp.On(pfake.ForRole(providers.RoleController)).ReplyFunc(func(c pfake.Call) (string, error) {
		var user string
		for _, msg := range c.Request.Messages {
			if msg.Role == providers.MsgUser {
				user = msg.Content
			}
		}
		question := ""
		if qm := ncQuestionRE.FindStringSubmatch(user); qm != nil {
			question = qm[1]
		}
		qt := ncTerms(question)
		type claim struct {
			Text string   `json:"text"`
			IDs  []string `json:"evidence_ids"`
		}
		out := []claim{}
		for _, em := range ncEvidenceRE.FindAllStringSubmatch(user, -1) {
			best, bestScore := "", 0
			for _, sentence := range ncSentences(em[2]) {
				n := 0
				for t := range ncTerms(sentence) {
					if qt[t] {
						n++
					}
				}
				if n > bestScore {
					best, bestScore = sentence, n
				}
			}
			if best != "" {
				out = append(out, claim{Text: best, IDs: []string{em[1]}})
			}
		}
		b, err := json.Marshal(map[string]any{"claims": out})
		return string(b), err
	})
}

func ncSentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, s := range ncSentenceRE.FindAllString(line, -1) {
			if s = strings.TrimSpace(s); len(strings.Fields(s)) >= 4 {
				out = append(out, s)
			}
		}
	}
	return out
}

func ncTerms(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127)
	}) {
		if len([]rune(f)) >= 4 {
			out[f] = true
		}
	}
	return out
}

// proposeOutOfScope scripts the controller calling the next request out of scope: the engine then asks
// Knowledge before it would refuse.
func (s *ncStack) proposeOutOfScope() {
	b, err := json.Marshal(map[string]any{
		"capability_id": nil, "fields": map[string]any{},
		"entity_refs": []any{}, "intent": "out_of_scope", "uncertainty": map[string]any{"overall": 0.9},
	})
	if err != nil {
		panic(err)
	}
	s.ctl.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(string(b)).Times(1)
}
