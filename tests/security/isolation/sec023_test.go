package isolationsec

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ---------------------------------------------------------------- files

type filesEnv struct {
	t   *testing.T
	svc *files.Service
	dir string
	clk *clock.Fake
}

func newFilesEnv(t *testing.T) *filesEnv {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	cfg := files.DefaultConfig()
	dir := t.TempDir()
	svc, err := files.New(files.Options{DB: storetest.Open(t, nil), DataDir: dir, Clock: clk, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return &filesEnv{t: t, svc: svc, dir: dir, clk: clk}
}

func (e *filesEnv) put(scope contracts.Scope, name string, content []byte) conversation.FileRef {
	e.t.Helper()
	ref, err := e.svc.Put(bg, scope, files.PutInput{Name: name, DeclaredMIME: "text/plain", Content: bytes.NewReader(content)})
	if err != nil {
		e.t.Fatalf("Put %s: %v", name, err)
	}
	return ref
}

// blobs lists the blobs on disk, as paths relative to the files directory.
func (e *filesEnv) blobs() []string {
	e.t.Helper()
	root := filepath.Join(e.dir, "files")
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() || strings.HasPrefix(rel, ".") {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// what a tenant can observe of its own upload: the reference without its id, what it is charged, and
// where its bytes are.
type uploadView struct {
	Ref   conversation.FileRef
	Usage files.Usage
	Blobs []string
}

// ---------------------------------------------------------------- vectors

// memSource is the chunk source of the embedding pipeline, per scope.
type memSource struct {
	mu     sync.Mutex
	chunks map[string]map[string]vector.ChunkInput
}

func scopeKey(s contracts.Scope) string {
	return s.ApplicationID + "/" + s.TenantID + "/" + s.WorkspaceID
}

func (m *memSource) put(scope contracts.Scope, cs ...vector.ChunkInput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chunks == nil {
		m.chunks = map[string]map[string]vector.ChunkInput{}
	}
	k := scopeKey(scope)
	if m.chunks[k] == nil {
		m.chunks[k] = map[string]vector.ChunkInput{}
	}
	for _, c := range cs {
		m.chunks[k][c.ID] = c
	}
}

func (m *memSource) LoadChunks(_ context.Context, scope contracts.Scope, _ string, ids []string) ([]vector.ChunkInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []vector.ChunkInput
	for _, id := range ids {
		if c, ok := m.chunks[scopeKey(scope)][id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

const (
	embedModelID = "fake-model"
	embedDim     = 64
)

type vecRig struct {
	t    *testing.T
	db   *store.DB
	flat *vector.Flat
	emb  *fake.Provider
	src  *memSource
	pipe *vector.Pipeline
}

func newVecRig(t *testing.T) *vecRig {
	t.Helper()
	r := &vecRig{t: t, emb: fake.New(fake.WithEmbedDim(embedDim)), src: &memSource{}}
	r.db = storetest.Open(t, nil)
	cfg := vector.DefaultConfig()
	cfg.CacheMiB = 8
	var err error
	if r.flat, err = vector.NewFlat(r.db, vector.FlatOptions{Config: cfg, Clock: clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))}); err != nil {
		t.Fatal(err)
	}
	if r.pipe, err = vector.NewPipeline(vector.PipelineOptions{Index: r.flat, Embedder: r.emb, Source: r.src, Model: embedModelID, BatchSize: 8}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *vecRig) embed(scope contracts.Scope, gen string, ids ...string) vector.EmbedStats {
	r.t.Helper()
	st, err := r.pipe.Embed(bg, scope, vector.EmbedRequest{Generation: gen, SourceID: "src_1", ChunkIDs: ids})
	if err != nil {
		r.t.Fatalf("Embed: %v", err)
	}
	return st
}

// embedCallsFor counts the embedder calls made under a tenant's scope, and the texts they carried.
func (r *vecRig) embedCallsFor(tenant string) (calls, inputs int) {
	for _, p := range r.emb.Prompts() {
		if p.Op == fake.OpEmbed && p.Scope.TenantID == tenant {
			calls++
			inputs += len(p.Embed.Inputs)
		}
	}
	return calls, inputs
}

func chunksOf(prefix string, n int) []vector.ChunkInput {
	out := make([]vector.ChunkInput, n)
	for i := range out {
		out[i] = vector.ChunkInput{
			ID:   fmt.Sprintf("chk_%s%03d", prefix, i),
			Text: fmt.Sprintf("para%s%d alpha%d bravo%d charlie%d", prefix, i, i*7919, i*104729, i*1299709),
			Path: []string{"Guide", "Section " + prefix},
		}
	}
	return out
}

func idsOf(cs []vector.ChunkInput) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// embedView is everything tenant A can observe of embedding its own chunks.
type embedView struct {
	First, Second     vector.EmbedStats
	EmbedderCalls     int
	EmbedderInputs    int
	EmbedderCallsGen2 int
	Stats             vector.Stats
	Hits              []string
}

// SEC-023
//
// Content-hash keyed reuse (embedding reuse, content-addressed file storage, deduplication) does not
// let one tenant learn that another holds identical content. Reuse keys include the scope, so an
// identical document in tenant B causes no observable difference (ids, sizes, counters, embedder
// calls, rows, bytes on disk) for tenant A. Inside one tenant the same content is reused and
// deduplicated, which is what proves the test can see reuse.
func TestSecContentHashReuseDoesNotCrossTenants(t *testing.T) {
	a := callerScope("app_clinic", "ten_a", "wks_1", "usr_1")
	b := callerScope("app_clinic", "ten_b", "wks_1", "usr_1")

	t.Run("files: identical bytes in another tenant change nothing tenant A can see", func(t *testing.T) {
		same := []byte("the exact same document, uploaded by two tenants")
		extra := []byte("a second document only tenant A holds")

		// Tenant A alone, then tenant A after tenant B stored the same bytes (and the same name).
		upload := func(holder *contracts.Scope) (view uploadView, e *filesEnv, foreign conversation.FileRef) {
			e = newFilesEnv(t)
			if holder != nil {
				foreign = e.put(*holder, "doc.txt", same)
			}
			before := e.blobs()
			ref := e.put(a, "doc.txt", same)
			e.put(a, "other.txt", extra)
			usage, err := e.svc.Usage(bg, a)
			if err != nil {
				t.Fatal(err)
			}
			var mine []string
			for _, p := range e.blobs() {
				if !slices.Contains(before, p) {
					mine = append(mine, p)
				}
			}
			ref.FileID = "" // the id is random per upload; its shape is checked below
			return uploadView{Ref: ref, Usage: usage, Blobs: mine}, e, foreign
		}
		alone, envAlone, _ := upload(nil)
		with, envWith, foreign := upload(&b)

		if diff := cmp.Diff(alone, with); diff != "" {
			t.Errorf("tenant A sees a difference when tenant B holds the same bytes (-alone +with):\n%s", diff)
		}

		// Control of the comparison: when the one that already holds the bytes is tenant A itself, the same
		// comparison does see it (the new blob is not new, the bytes are shared), so a cross-tenant reuse
		// would not have gone unseen.
		self, _, _ := upload(&a)
		if cmp.Diff(alone, self) == "" {
			t.Error("the comparison sees no difference when the same tenant already held the bytes")
		}
		if len(alone.Blobs) != 2 || alone.Usage.Files != 2 {
			t.Fatalf("tenant A holds %d new blobs and %d files, want 2 and 2: %+v", len(alone.Blobs), alone.Usage.Files, alone)
		}
		// The bytes of the two tenants are in different files, in different directories, under names that
		// are neither tenant's identifier nor the content hash.
		bBlobs := difference(envWith.blobs(), alone.Blobs)
		if len(bBlobs) != 1 {
			t.Fatalf("tenant B's one upload left %d blobs on disk: %v", len(bBlobs), bBlobs)
		}
		for _, p := range alone.Blobs {
			if dirOf(p) == dirOf(bBlobs[0]) {
				t.Errorf("the two tenants' blobs share the directory %s", dirOf(p))
			}
			if filepath.Base(p) == filepath.Base(bBlobs[0]) {
				t.Errorf("the two tenants' blobs share the name %s: the address does not include the scope", filepath.Base(p))
			}
		}
		for _, p := range append(slices.Clone(alone.Blobs), bBlobs...) {
			for _, bad := range []string{a.TenantID, b.TenantID, a.ApplicationID, strings.TrimPrefix(*with.Ref.ContentHash, "sha256:")} {
				if strings.Contains(p, bad) {
					t.Errorf("the path %q contains %q", p, bad)
				}
			}
		}
		// A's file in a world where B exists is the same file, name for name, as in the world where it does
		// not: the directory and the blob name are a function of the scope and the content alone.
		if !slices.Equal(alone.Blobs, with.Blobs) {
			t.Errorf("tenant A's blobs are at %v alone and %v beside tenant B", alone.Blobs, with.Blobs)
		}
		// B's reference resolves nowhere in A, with the very refusal of a reference that never existed.
		_, errForeign := envWith.svc.Stat(bg, a, string(foreign.FileID))
		_, errNone := envWith.svc.Stat(bg, a, "fil_01HZZZZZZZZZZZZZZZZZZZZZZZ")
		if contracts.CodeOf(errForeign) != contracts.ErrScopeViolation || errForeign.Error() != errNone.Error() {
			t.Errorf("a foreign reference: %v; a reference that does not exist: %v", errForeign, errNone)
		}
		_ = envAlone

		// Control: inside one tenant the same bytes are one blob under two references, so the layout can
		// show reuse when there is any; different bytes are another blob.
		e := newFilesEnv(t)
		r1 := e.put(a, "one.txt", same)
		r2 := e.put(a, "two.txt", same)
		if r1.FileID == r2.FileID {
			t.Fatal("two uploads share an id")
		}
		if n := len(e.blobs()); n != 1 {
			t.Fatalf("%d blobs for identical content in one tenant, want 1 (deduplicated)", n)
		}
		e.put(a, "three.txt", extra)
		if n := len(e.blobs()); n != 2 {
			t.Fatalf("%d blobs after different content, want 2", n)
		}
		// And the same bytes in another workspace of the same tenant are not shared either.
		a2 := callerScope(a.ApplicationID, a.TenantID, "wks_2", a.PrincipalID)
		e.put(a2, "four.txt", same)
		if n := len(e.blobs()); n != 3 {
			t.Fatalf("%d blobs after the same bytes in another workspace, want 3 (reuse is per workspace)", n)
		}
	})

	t.Run("embeddings: identical chunks in another tenant change nothing tenant A can see", func(t *testing.T) {
		cs := chunksOf("same", 6)
		scenario := func(holder *contracts.Scope) (embedView, *vecRig) {
			r := newVecRig(t)
			if holder != nil {
				r.src.put(*holder, cs...)
				first := r.embed(*holder, "gen_1", idsOf(cs)...)
				if first.Embedded != len(cs) || first.Reused != 0 {
					t.Fatalf("tenant B's first embedding = %v", first)
				}
			}
			r.src.put(a, cs...)
			var v embedView
			v.First = r.embed(a, "gen_1", idsOf(cs)...)
			v.EmbedderCalls, v.EmbedderInputs = r.embedCallsFor(a.TenantID)
			// A new generation of the same chunks in the same tenant: reuse inside the tenant.
			v.Second = r.embed(a, "gen_2", idsOf(cs)...)
			calls2, _ := r.embedCallsFor(a.TenantID)
			v.EmbedderCallsGen2 = calls2 - v.EmbedderCalls
			st, err := r.flat.Stats(bg, a)
			if err != nil {
				t.Fatal(err)
			}
			v.Stats = st
			q := fake.EmbedVector(vector.EmbeddingText(cs[3]), embedDim)
			res, err := r.flat.Search(bg, a, vector.Query{Vector: q, Model: embedModelID, K: 3, Generations: []string{"gen_1", "gen_2"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range res.Hits {
				v.Hits = append(v.Hits, fmt.Sprintf("%s/%s/%.4f", h.GenerationID, h.ChunkID, h.Score))
			}
			return v, r
		}
		alone, _ := scenario(nil)
		with, rig := scenario(&b)

		if diff := cmp.Diff(alone, with); diff != "" {
			t.Errorf("tenant A sees a difference when tenant B holds the same chunks (-alone +with):\n%s", diff)
		}

		// Control of the comparison: when the one that already holds the chunks is tenant A itself, the
		// same comparison sees the reuse.
		self, _ := scenario(&a)
		if cmp.Diff(alone, self) == "" || self.First.Reused != len(cs) {
			t.Errorf("the comparison sees no reuse when the same tenant already held the chunks: %+v", self.First)
		}
		// A's embedding was a real one: every chunk went to the embedder, none was found already held.
		if alone.First.Embedded != len(cs) || alone.First.Reused != 0 || alone.EmbedderInputs != len(cs) {
			t.Errorf("tenant A's first embedding = %v, with %d inputs sent to the embedder; want all %d embedded afresh", alone.First, alone.EmbedderInputs, len(cs))
		}
		// Control: inside the tenant the second generation reuses every vector and costs no embedder call, so
		// the counters would have shown a reuse across tenants.
		if alone.Second.Reused != len(cs) || alone.Second.Embedded != 0 || alone.EmbedderCallsGen2 != 0 {
			t.Errorf("tenant A's second generation = %v with %d embedder calls, want everything reused inside the tenant", alone.Second, alone.EmbedderCallsGen2)
		}
		if len(alone.Hits) == 0 || alone.Stats.Vectors != 2*len(cs) {
			t.Errorf("tenant A holds %d vectors and found hits %v", alone.Stats.Vectors, alone.Hits)
		}

		// The rows: no two scopes hold a row under the same input key, and the key of one text differs by
		// application, tenant, workspace and model.
		rows, err := rig.db.QueryContext(bg,
			`SELECT input_key FROM vector_rows GROUP BY input_key HAVING count(DISTINCT application_id || '/' || tenant_id || '/' || workspace_id) > 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if rows.Next() {
			t.Error("two scopes hold rows under the same input key: the reuse key does not include the scope")
		}
		text := vector.EmbeddingText(cs[0])
		keys := map[string]string{}
		for name, c := range map[string]struct {
			s     contracts.Scope
			model string
		}{
			"base": {a, embedModelID}, "tenant": {b, embedModelID},
			"workspace":   {callerScope(a.ApplicationID, a.TenantID, "wks_2", a.PrincipalID), embedModelID},
			"application": {callerScope("app_other", a.TenantID, a.WorkspaceID, a.PrincipalID), embedModelID},
			"model":       {a, "another-model"},
		} {
			k, err := vector.NewInputKey(c.s, c.model, text)
			if err != nil {
				t.Fatal(err)
			}
			if prev, dup := keys[k.String()]; dup {
				t.Errorf("the reuse key of %s equals that of %s", name, prev)
			}
			keys[k.String()] = name
		}
		// And the same key for the same scope, model and text: the control of the comparison above.
		k1, _ := vector.NewInputKey(a, embedModelID, text)
		k2, _ := vector.NewInputKey(a, embedModelID, text)
		if k1 != k2 || k1.IsZero() {
			t.Error("the reuse key of one scope, model and text is not stable")
		}
	})
}

// difference returns the elements of all that are not in some.
func difference(all, some []string) []string {
	var out []string
	for _, s := range all {
		if !slices.Contains(some, s) {
			out = append(out, s)
		}
	}
	return out
}

func dirOf(p string) string { return filepath.ToSlash(filepath.Dir(p)) }
