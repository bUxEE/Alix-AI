package index

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The 100k-row measurement behind the design (see doc.go). It is skipped unless
// ALIXAI_INDEX_BENCH_ROWS is set, because building 100k chunks takes about ten
// seconds and the queries a minute:
//
//	ALIXAI_INDEX_BENCH_ROWS=100000 ALIXAI_INDEX_BENCH_DIST=even5,even100,skew \
//	    go test -run TestBenchScopedSearch -v ./internal/rag/index/
//
// The corpus is the one M0-05 used: 30 words per chunk drawn from a 3000-word
// vocabulary with a Zipf distribution, so a few terms occur in most chunks. It
// is the worst case for BM25, which scores every row that matches.

// pseudoWord is a letters-only word that no identifier rule would pick up.
func pseudoWord(i int) string {
	const cons, vow = "bdfgklmnprstvz", "aeiou"
	var b strings.Builder
	for k := 0; k < 3; k++ {
		s := i % 70
		i /= 70
		b.WriteByte(cons[s%14])
		b.WriteByte(vow[s/14])
	}
	return b.String()
}

// benchChunks builds n synthetic children; every 500th also holds a part number
// "PN-<doc>-<index>".
func benchChunks(zipf *rand.Zipf, vocab []string, doc string, n int) chunk.Result {
	words := func(k int) string {
		var b strings.Builder
		for i := 0; i < k; i++ {
			b.WriteString(vocab[zipf.Uint64()])
			b.WriteByte(' ')
		}
		return strings.TrimSpace(b.String())
	}
	parent := chunk.Chunk{ID: "chp_" + doc, Kind: chunk.KindSection, Text: "section", Section: "Section", Path: []string{"Section"}, Span: chunk.Span{Start: 0, End: 7}}
	res := chunk.Result{Parents: []chunk.Chunk{parent}}
	for i := 0; i < n; i++ {
		text := words(30)
		if i%500 == 0 {
			text += fmt.Sprintf(" PN-%s-%d", doc, i)
		}
		res.Children = append(res.Children, chunk.Chunk{
			ID: fmt.Sprintf("chk_%s_%05d", doc, i), ParentID: parent.ID, Kind: chunk.KindParagraph, Ordinal: i,
			Text: text, Section: "Section " + vocab[zipf.Uint64()], Path: []string{"Section"},
			Span: chunk.Span{Start: i * 200, End: i*200 + len([]rune(text))}, ContentHash: fmt.Sprintf("%064d", i), EstToks: 60,
		})
	}
	return res
}

func TestBenchScopedSearch(t *testing.T) {
	rows, _ := strconv.Atoi(os.Getenv("ALIXAI_INDEX_BENCH_ROWS"))
	if rows == 0 {
		t.Skip("set ALIXAI_INDEX_BENCH_ROWS to run the 100k-row measurement")
	}
	dists := strings.Split(os.Getenv("ALIXAI_INDEX_BENCH_DIST"), ",")
	if dists[0] == "" {
		dists = []string{"even5", "even100", "skew"}
	}
	for _, d := range dists {
		t.Run(d, func(t *testing.T) { benchScoped(t, d, rows) })
	}
}

func benchScoped(t *testing.T, dist string, rows int) {
	var sizes []int
	switch dist {
	case "even5":
		for i := 0; i < 5; i++ {
			sizes = append(sizes, rows/5)
		}
	case "even100":
		for i := 0; i < 100; i++ {
			sizes = append(sizes, rows/100)
		}
	case "skew": // one scope holds half of everything, fifty share the rest
		sizes = append(sizes, rows/2)
		for i := 0; i < 50; i++ {
			sizes = append(sizes, (rows-rows/2)/50)
		}
	case "single": // the whole corpus in one scope
		sizes = []int{rows}
	default:
		t.Fatalf("unknown distribution %q", dist)
	}
	f := newFixture(t)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 2))
	vocab := make([]string, 3000)
	for i := range vocab {
		vocab[i] = pseudoWord(i)
	}
	zipf := rand.NewZipf(rng, 1.2, 4, uint64(len(vocab)-1))

	scopes := make([]contracts.Scope, len(sizes))
	start := time.Now()
	for s, n := range sizes {
		scopes[s] = f.scope("app", fmt.Sprintf("t%03d", s), "w1")
		g, err := f.ix.BeginGeneration(ctx, scopes[s], f.source(), f.comps())
		if err != nil {
			t.Fatal(err)
		}
		for d := 0; n > 0; d++ {
			k := min(n, 1000)
			if _, err := f.ix.PutDocument(ctx, scopes[s], string(g.GenerationID),
				Document{ID: fmt.Sprintf("doc-%03d-%d", s, d), Version: "v1", Chunks: benchChunks(zipf, vocab, fmt.Sprintf("%03d%d", s, d), k)}); err != nil {
				t.Fatal(err)
			}
			n -= k
		}
		if _, err := f.ix.ValidateGeneration(ctx, scopes[s], string(g.GenerationID), Expectation{}); err != nil {
			t.Fatal(err)
		}
		if err := f.ix.ActivateGeneration(ctx, scopes[s], string(g.GenerationID)); err != nil {
			t.Fatal(err)
		}
	}
	build := time.Since(start)

	type job struct {
		scope int
		q     Query
	}
	const perKind = 100
	kinds := []string{"AND2 head terms", "OR16 zipf terms", "OR8 + part number"}
	jobs := make([][]job, len(kinds))
	pick := func() int {
		if dist == "skew" && rng.IntN(2) == 0 {
			return 0 // half of the traffic goes to the big scope
		}
		return rng.IntN(len(scopes))
	}
	term := func() string { return vocab[zipf.Uint64()] }
	for i := 0; i < perKind; i++ {
		jobs[0] = append(jobs[0], job{pick(), Query{Text: term() + " " + term(), MatchAll: true, Limit: 10}})
		var parts []string
		for j := 0; j < 16; j++ {
			parts = append(parts, term())
		}
		jobs[1] = append(jobs[1], job{pick(), Query{Text: strings.Join(parts, " "), Limit: 10}})
		s := pick()
		pn := fmt.Sprintf("PN-%03d0-%d", s, 500*rng.IntN(max(sizes[s]/500, 1)))
		jobs[2] = append(jobs[2], job{s, Query{Text: strings.Join(parts[:8], " ") + " " + pn, Limit: 10}})
	}
	run := func(js []job, readers int) (p50, p95 time.Duration) {
		lat := make([]time.Duration, 0, len(js))
		var mu sync.Mutex
		var wg sync.WaitGroup
		ch := make(chan job)
		for w := 0; w < readers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					t0 := time.Now()
					if _, err := f.ix.Search(ctx, scopes[j.scope], j.q); err != nil {
						t.Error(err)
						continue
					}
					d := time.Since(t0)
					mu.Lock()
					lat = append(lat, d)
					mu.Unlock()
				}
			}()
		}
		for _, j := range js {
			ch <- j
		}
		close(ch)
		wg.Wait()
		sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
		return lat[len(lat)/2], lat[len(lat)*95/100]
	}
	var dbBytes int64
	if st, err := os.Stat(f.db.Path()); err == nil {
		dbBytes = st.Size()
	}
	t.Logf("%s: %d rows in %d scopes, built in %.1fs (%.0f rows/s), db %.0f MiB", dist, rows, len(sizes), build.Seconds(), float64(rows)/build.Seconds(), float64(dbBytes)/(1<<20))
	for k, name := range kinds {
		a50, a95 := run(jobs[k], 1)
		b50, b95 := run(jobs[k], 4)
		t.Logf("%-8s %-20s 1 reader p50 %7.1f ms p95 %7.1f ms | 4 readers p50 %7.1f ms p95 %7.1f ms", dist, name,
			ms(a50), ms(a95), ms(b50), ms(b95))
	}

	// The control: the same FTS5 table, queried the way M0-05 did, filtering on
	// the UNINDEXED scope column after the match instead of on the rowid range.
	// It is not a code path of the package; it exists to show what the design
	// saves, on this corpus and this machine.
	control := func(js []job, readers int) (p50, p95 time.Duration) {
		lat := make([]time.Duration, 0, len(js))
		var mu sync.Mutex
		var wg sync.WaitGroup
		ch := make(chan job)
		for w := 0; w < readers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					pq, err := parseQuery(j.q.Text, j.q.MatchAll)
					if err != nil {
						t.Error(err)
						continue
					}
					sc := scopes[j.scope]
					t0 := time.Now()
					err = f.db.View(ctx, func(tx *store.Tx) error {
						rows, err := tx.QueryContext(ctx,
							`SELECT rowid, bm25(ragindex_fts, `+bm25Weights+`) AS score FROM ragindex_fts
WHERE ragindex_fts MATCH ? AND application_id = ? AND tenant_id = ? AND workspace_id = ?
ORDER BY score LIMIT 10`, pq.match, sc.ApplicationID, sc.TenantID, sc.WorkspaceID)
						if err != nil {
							return err
						}
						defer rows.Close()
						for rows.Next() {
							var rid int64
							var score float64
							if err := rows.Scan(&rid, &score); err != nil {
								return err
							}
						}
						return rows.Err()
					})
					if err != nil {
						t.Error(err)
						continue
					}
					d := time.Since(t0)
					mu.Lock()
					lat = append(lat, d)
					mu.Unlock()
				}
			}()
		}
		for _, j := range js {
			ch <- j
		}
		close(ch)
		wg.Wait()
		sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
		return lat[len(lat)/2], lat[len(lat)*95/100]
	}
	for k := 0; k < 2; k++ {
		c50, c95 := control(jobs[k], 4)
		t.Logf("%-8s %-20s CONTROL post-filter on UNINDEXED column, 4 readers p50 %7.1f ms p95 %7.1f ms", dist, kinds[k], ms(c50), ms(c95))
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
