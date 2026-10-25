package decisionsec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ---- artifacts and manifests ----

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// artifacts are the files of one revision: name -> bytes.
type artifacts map[string][]byte

func revFiles(rev string) artifacts {
	return artifacts{
		"head.bin":    []byte("head weights of " + rev),
		"encoder.bin": []byte("encoder weights of " + rev),
		"tok.json":    []byte(`{"revision":"` + rev + `"}`),
	}
}

// manifestOf builds the manifest of one revision over its files. The encoder hash is the hash of encoder.bin.
func manifestOf(rev string, files artifacts) gen.ScorerManifest {
	m := fakescorer.DefaultManifest()
	m.ModelRevision = actions.OpaqueVersion(rev)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	m.Files = nil
	for _, n := range names {
		m.Files = append(m.Files, gen.ScorerManifestFileHash{Name: n, SHA256: gen.SHA256(sha(files[n])), SizeBytes: int64(len(files[n]))})
	}
	if enc, ok := files["encoder.bin"]; ok {
		h := gen.SHA256(sha(enc))
		m.Encoder.SHA256 = &h
	}
	return m
}

// memSource serves the bytes of a model directory.
type memSource struct {
	files   artifacts
	openErr error
	opened  []string
}

func (s *memSource) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	s.opened = append(s.opened, name)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.openErr != nil {
		return nil, s.openErr
	}
	b, ok := s.files[name]
	if !ok {
		return nil, fmt.Errorf("no such file %q", name)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// endless is a reader that never ends: a hostile file far bigger than its manifest entry says.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// ---- the runtime: a loader, instances and a resource gate ----

type instance struct {
	*fakescorer.Scorer
	mu        sync.Mutex
	healthErr error
	closed    int
}

func (i *instance) Health(context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.healthErr
}

func (i *instance) setHealth(err error) { i.mu.Lock(); i.healthErr = err; i.mu.Unlock() }

func (i *instance) Close(context.Context) error { i.mu.Lock(); i.closed++; i.mu.Unlock(); return nil }

func (i *instance) closes() int { i.mu.Lock(); defer i.mu.Unlock(); return i.closed }

type loader struct {
	mu        sync.Mutex
	clk       *clock.Fake
	loadErr   map[string]error // per revision
	healthErr map[string]error // initial health error per revision
	identity  map[string]gen.ScorerManifest
	calls     map[string]int
	insts     map[string][]*instance
}

func newLoader(clk *clock.Fake) *loader {
	return &loader{clk: clk, loadErr: map[string]error{}, healthErr: map[string]error{}, identity: map[string]gen.ScorerManifest{}, calls: map[string]int{}, insts: map[string][]*instance{}}
}

func (l *loader) Load(_ context.Context, rev lifecycle.Revision) (lifecycle.Instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := string(rev.Manifest.ModelRevision)
	l.calls[key]++
	if err := l.loadErr[key]; err != nil {
		return nil, err
	}
	m := rev.Manifest
	if other, ok := l.identity[key]; ok {
		m = other
	}
	in := &instance{Scorer: fakescorer.New(m, l.clk), healthErr: l.healthErr[key]}
	l.insts[key] = append(l.insts[key], in)
	return in, nil
}

func (l *loader) loads(rev string) int { l.mu.Lock(); defer l.mu.Unlock(); return l.calls[rev] }

func (l *loader) last(rev string) *instance {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.insts[rev]
	if len(s) == 0 {
		return nil
	}
	return s[len(s)-1]
}

// gate is a byte budget. The controller, retrieval and speech workers are resident and protected: nothing in the
// lifecycle.ResourceGate interface can reach them, and evict is a trap that must stay at zero.
type gate struct {
	mu        sync.Mutex
	budget    uint64
	held      uint64
	protected map[string]bool
	evicted   int
}

func newGate(budget uint64) *gate {
	return &gate{budget: budget, protected: map[string]bool{"controller": true, "retrieval": true, "speech": true}}
}

type reservation struct {
	g    *gate
	n    uint64
	once sync.Once
}

func (r *reservation) Release() {
	r.once.Do(func() { r.g.mu.Lock(); r.g.held -= r.n; r.g.mu.Unlock() })
}

func (g *gate) Admit(_ context.Context, n uint64, _ string) (lifecycle.Reservation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held+n > g.budget {
		return nil, contracts.New(contracts.ErrResourceExhausted, "decisionsec: the budget is spent")
	}
	g.held += n
	return &reservation{g: g, n: n}, nil
}

func (g *gate) resident() uint64 { g.mu.Lock(); defer g.mu.Unlock(); return g.held }

// ---- the supervisor rig ----

const gib = 1 << 30

type lcRig struct {
	t    *testing.T
	clk  *clock.Fake
	ld   *loader
	gate *gate
	met  *lifecycle.MemoryRegistry
	sup  *lifecycle.Supervisor
	cur  lifecycle.Revision
	keys *scorerSecKeys
}

func newLCRig(t *testing.T, mod func(*lifecycle.Config)) *lcRig {
	t.Helper()
	r := &lcRig{t: t, clk: clock.NewFake(t0), gate: newGate(20 * gib), met: lifecycle.NewMemoryRegistry()}
	r.keys = newScorerSecKeys(t)
	r.ld = newLoader(r.clk)
	r.cur = lifecycle.Revision{Manifest: manifestOf("rev1", revFiles("rev1")), PeakBytes: 8 * gib}
	cfg := lifecycle.DefaultConfig()
	cfg.Probation = 10 * time.Minute
	cfg.Profile = "sec"
	if mod != nil {
		mod(&cfg)
	}
	sup, err := lifecycle.New(lifecycle.Deps{Clock: r.clk, Loader: r.ld, Gate: r.gate, Registry: r.met, Revision: r.cur, Config: cfg, Verifier: r.keys.verifier(t, r.clk)})
	if err != nil {
		t.Fatal(err)
	}
	r.sup = sup
	return r
}

// next is a candidate revision with its files.
func (r *lcRig) next(rev string) (lifecycle.Revision, *memSource) {
	files := revFiles(rev)
	return r.keys.sign(r.t, lifecycle.Revision{Manifest: manifestOf(rev, files), PeakBytes: 8 * gib}, r.clk.Now()), &memSource{files: files}
}

// serve scores one request through the supervisor (which demand-loads the serving revision) and returns the
// revision that answered.
func (r *lcRig) serve() string {
	r.t.Helper()
	res, err := r.sup.ScoreDecision(bg, scopeOf("ten1"), lcRequest(r.clk))
	if err != nil {
		r.t.Fatalf("ScoreDecision: %v", err)
	}
	if res.ModelRevision == nil {
		r.t.Fatal("the answer names no revision")
	}
	return string(*res.ModelRevision)
}

func lcRequest(clk *clock.Fake) gen.DecisionRequest {
	return gen.DecisionRequest{
		TenantID: "ten1", ApplicationID: "app-sec", SessionID: "ses1", ContractVersion: "cv1",
		DecisionType: gen.DecisionTypeItemSelection, SchemaVersion: 1,
		BoundedState:       map[string]gen.BoundedScalar{"step": "pick"},
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet: gen.CandidateSet{
			Candidates:    []gen.CandidateSetCandidate{{CandidateID: common.Identifier("c1"), Label: "one"}, {CandidateID: common.Identifier("c2"), Label: "two"}},
			MaxSelections: 1, BuiltAt: ts(clk.Now()),
		},
		Locale: "en", Deadline: ts(clk.Now().Add(time.Second)),
	}
}

var _ decision.DecisionModelProvider = (*lifecycle.Supervisor)(nil)
