package vector_test

import (
	"context"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/rag/vector/vectortest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// memProvider is a second, independent implementation of the contract: an
// exact float64 scan over a slice, no database. It exists to prove two things
// about the contract suite: it is implementation-neutral (a provider that has
// nothing in common with the flat one passes it), and it has teeth (the same
// provider with one deliberate bug fails exactly the subtest that names it).
//
// bug is one of "", "postfilter", "reuse-any-scope", "delete-noop".
type memProvider struct {
	bug string

	mu   sync.Mutex
	gens map[genKey]*memGen
	rows map[rowKey]memRow
}

type genKey struct{ app, tenant, ws, gen string }
type rowKey struct {
	genKey
	chunk string
}
type memGen struct {
	source, model string
	dim           int
}
type memRow struct {
	vec []float64 // unit length
	key vector.InputKey
}

func newMem(bug string) *memProvider {
	return &memProvider{bug: bug, gens: map[genKey]*memGen{}, rows: map[rowKey]memRow{}}
}

var _ vector.VectorIndexProvider = (*memProvider)(nil)

func gk(s contracts.Scope, gen string) genKey {
	return genKey{s.ApplicationID, s.TenantID, s.WorkspaceID, gen}
}

func unit64(v []float32) []float64 {
	out := make([]float64, len(v))
	var sq float64
	for i, x := range v {
		out[i] = float64(x)
		sq += float64(x) * float64(x)
	}
	for i := range out {
		out[i] /= math.Sqrt(sq)
	}
	return out
}

func (m *memProvider) Info() vector.Info {
	return vector.Info{Name: "memory", Exact: true, Metric: "cosine", Storage: "float64", Reuses: true, StableTies: true}
}

func (m *memProvider) Upsert(_ context.Context, scope contracts.Scope, b vector.UpsertBatch) (vector.UpsertResult, error) {
	if err := scope.Validate(); err != nil {
		return vector.UpsertResult{}, err
	}
	if err := b.Validate(); err != nil {
		return vector.UpsertResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := gk(scope, b.Generation)
	dim := len(b.Items[0].Vector)
	if have, ok := m.gens[g]; ok {
		if have.source != b.SourceID || have.model != b.Model || have.dim != dim {
			return vector.UpsertResult{}, contracts.New(contracts.ErrConflict, "generation is fixed to its source, model and dimension")
		}
	} else {
		m.gens[g] = &memGen{b.SourceID, b.Model, dim}
	}
	var res vector.UpsertResult
	for _, it := range b.Items {
		rk := rowKey{g, it.ChunkID}
		if _, ok := m.rows[rk]; ok {
			res.Replaced++
		} else {
			res.Inserted++
		}
		m.rows[rk] = memRow{unit64(it.Vector), it.Key}
	}
	return res, nil
}

func (m *memProvider) Reuse(_ context.Context, scope contracts.Scope, r vector.ReuseRequest) (vector.ReuseResult, error) {
	if err := scope.Validate(); err != nil {
		return vector.ReuseResult{}, err
	}
	if err := r.Validate(); err != nil {
		return vector.ReuseResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out vector.ReuseResult
	target := gk(scope, r.Generation)
	for _, ref := range r.Refs {
		var found *memRow
		var foundGen genKey
		for rk, row := range m.rows {
			sameScope := rk.app == scope.ApplicationID && rk.tenant == scope.TenantID && rk.ws == scope.WorkspaceID
			if (sameScope || m.bug == "reuse-any-scope") && row.key == ref.Key && m.gens[rk.genKey].model == r.Model {
				row := row
				found, foundGen = &row, rk.genKey
				break
			}
		}
		if found == nil {
			continue
		}
		if _, ok := m.gens[target]; !ok {
			m.gens[target] = &memGen{r.SourceID, r.Model, m.gens[foundGen].dim}
		}
		m.rows[rowKey{target, ref.ChunkID}] = *found
		out.Reused = append(out.Reused, ref.ChunkID)
	}
	return out, nil
}

func (m *memProvider) Search(_ context.Context, scope contracts.Scope, q vector.Query) (vector.SearchResult, error) {
	if err := scope.Validate(); err != nil {
		return vector.SearchResult{}, err
	}
	if err := q.Validate(); err != nil {
		return vector.SearchResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	uq := unit64(q.Vector)
	var res vector.SearchResult
	type scored struct {
		hit vector.Hit
		own bool
	}
	var all []scored
	seen := map[string]bool{}
	for _, gen := range q.Generations {
		if seen[gen] {
			continue
		}
		seen[gen] = true
		g := gk(scope, gen)
		info, ok := m.gens[g]
		switch {
		case !ok && m.bug != "postfilter":
			res.Skipped = append(res.Skipped, vector.Skipped{Generation: gen, Reason: vector.SkipUnknownGeneration})
			continue
		case ok && info.model != q.Model:
			res.Skipped = append(res.Skipped, vector.Skipped{Generation: gen, Reason: vector.SkipModelMismatch})
			continue
		case ok && info.dim != len(uq):
			res.Skipped = append(res.Skipped, vector.Skipped{Generation: gen, Reason: vector.SkipDimMismatch})
			continue
		}
		for rk, row := range m.rows {
			if rk.gen != gen {
				continue
			}
			own := rk.app == scope.ApplicationID && rk.tenant == scope.TenantID && rk.ws == scope.WorkspaceID
			if !own && m.bug != "postfilter" {
				continue // the scope is applied BEFORE similarity
			}
			var dot float64
			for i := range uq {
				dot += uq[i] * row.vec[i]
			}
			src := ""
			if gi := m.gens[rk.genKey]; gi != nil {
				src = gi.source
			}
			all = append(all, scored{vector.Hit{ChunkID: rk.chunk, GenerationID: gen, SourceID: src, Score: float32(dot)}, own})
			res.Scanned++
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].hit.Score != all[j].hit.Score {
			return all[i].hit.Score > all[j].hit.Score
		}
		return all[i].hit.ChunkID < all[j].hit.ChunkID
	})
	if m.bug == "postfilter" { // rank globally, cut to K, THEN drop what is not the caller's: SEC-021
		if len(all) > q.K {
			all = all[:q.K]
		}
		own := all[:0]
		for _, s := range all {
			if s.own {
				own = append(own, s)
			}
		}
		all = own
	}
	for _, s := range all {
		if len(res.Hits) == q.K {
			break
		}
		res.Hits = append(res.Hits, s.hit)
	}
	return res, nil
}

func (m *memProvider) del(scope contracts.Scope, pick func(genKey, *memGen) bool, chunks map[string]bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bug == "delete-noop" {
		return 0
	}
	n := 0
	for rk := range m.rows {
		g := m.gens[rk.genKey]
		if rk.app != scope.ApplicationID || rk.tenant != scope.TenantID || rk.ws != scope.WorkspaceID || !pick(rk.genKey, g) {
			continue
		}
		if chunks != nil && !chunks[rk.chunk] {
			continue
		}
		delete(m.rows, rk)
		n++
	}
	for k, g := range m.gens {
		if k.app == scope.ApplicationID && k.tenant == scope.TenantID && k.ws == scope.WorkspaceID && pick(k, g) && chunks == nil {
			delete(m.gens, k)
		}
	}
	return n
}

func (m *memProvider) DeleteChunks(_ context.Context, scope contracts.Scope, generation string, ids []string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if len(ids) == 0 {
		return 0, contracts.New(contracts.ErrValidationFailed, "no ids")
	}
	return m.del(scope, func(k genKey, _ *memGen) bool { return k.gen == generation }, set), nil
}

func (m *memProvider) DeleteGeneration(_ context.Context, scope contracts.Scope, generation string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	return m.del(scope, func(k genKey, _ *memGen) bool { return k.gen == generation }, nil), nil
}

func (m *memProvider) DeleteSource(_ context.Context, scope contracts.Scope, source string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	return m.del(scope, func(_ genKey, g *memGen) bool { return g != nil && g.source == source }, nil), nil
}

func (m *memProvider) DeleteScope(_ context.Context, scope contracts.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	return m.del(scope, func(genKey, *memGen) bool { return true }, nil), nil
}

func (m *memProvider) Stats(_ context.Context, scope contracts.Scope) (vector.Stats, error) {
	if err := scope.Validate(); err != nil {
		return vector.Stats{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := vector.Stats{Info: m.Info()}
	counts := map[string]int{}
	for rk := range m.rows {
		if rk.app == scope.ApplicationID && rk.tenant == scope.TenantID && rk.ws == scope.WorkspaceID {
			counts[rk.gen]++
		}
	}
	for k, g := range m.gens {
		if k.app == scope.ApplicationID && k.tenant == scope.TenantID && k.ws == scope.WorkspaceID {
			st.Generations = append(st.Generations, vector.GenerationStats{Generation: k.gen, SourceID: g.source, Model: g.model, Dim: g.dim, Vectors: counts[k.gen]})
			st.Vectors += counts[k.gen]
		}
	}
	sort.Slice(st.Generations, func(i, j int) bool { return st.Generations[i].Generation < st.Generations[j].Generation })
	return st, nil
}

// TestMemoryProviderPassesTheContract: a provider with nothing in common with
// the flat one is a valid implementation, so the suite is about behaviour, not
// about this repository's SQL.
func TestMemoryProviderPassesTheContract(t *testing.T) {
	vectortest.RunProviderSuite(t, func(*testing.T) vector.VectorIndexProvider { return newMem("") })
}

// TestLeakyProviderSuite is the body of the mutation checks below; it only runs
// in the child process TestContractSuiteHasTeeth starts.
func TestLeakyProviderSuite(t *testing.T) {
	bug, ok := os.LookupEnv("ALIXAI_VECTOR_LEAKY")
	if !ok {
		t.Skip("runs only as a child of TestContractSuiteHasTeeth")
	}
	vectortest.RunProviderSuite(t, func(*testing.T) vector.VectorIndexProvider { return newMem(bug) })
}

// The contract suite must FAIL a provider that has the bug it is written to
// catch. Each mutant is run in a child process (a failing subtest cannot be
// asserted from inside the same test run) and must fail the named subtest.
func TestContractSuiteHasTeeth(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for bug, subtest := range map[string]string{
		"postfilter":      "ScopePrefilterBeforeSimilarity", // SEC-021: global top-k, then filter by scope
		"reuse-any-scope": "ReuseNeverCrossesScopes",        // SEC-023: reuse found by key across scopes
		"delete-noop":     "DeleteRemovesVectors",           // a deleted vector is still served
	} {
		t.Run(bug, func(t *testing.T) {
			cmd := exec.Command(exe, "-test.run", "^TestLeakyProviderSuite$", "-test.v")
			cmd.Env = append(os.Environ(), "ALIXAI_VECTOR_LEAKY="+bug)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("a provider with the %q bug passed the contract suite:\n%s", bug, out)
			}
			if !strings.Contains(string(out), "--- FAIL: TestLeakyProviderSuite/"+subtest) {
				t.Fatalf("the %q bug did not fail the %s subtest:\n%s", bug, subtest, out)
			}
		})
	}
}
