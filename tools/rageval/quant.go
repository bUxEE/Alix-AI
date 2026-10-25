package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/vector"
)

// The quantisation check: ADR 0011 stores one signed byte per dimension and
// measured, on synthetic vectors, that the quantised scan keeps recall@10 against
// the exact (float32) one at 0.976 to 0.990, over a bar of 0.95. It left the same
// question open for real vectors: "recall@10 on a real corpus embedded with
// Granite Embedding 97M... if real vectors miss it, the first remedy is a
// higher-precision stored format". This measures it with the model's own vectors,
// with the code the index uses (vector.Quantise, UnitQuery and Score), without the
// index: for each question, the ten nearest vectors by exact cosine against the
// ten nearest by the quantised score.

const (
	quantK   = 10   // recall@10, as in ADR 0011
	quantBar = 0.95 // the bar of ADR 0011
	// quantBatch is how many texts one embedding call carries.
	quantBatch = 32
)

// Quantization is the result of the check.
type Quantization struct {
	Embedder string  `json:"embedder"` // fake or localai: the embedder of the run
	Scheme   string  `json:"scheme"`
	K        int     `json:"k"`
	Bar      float64 `json:"bar"`
	// Sets are the vector sets the check ran on, each with its own queries.
	Sets []QuantSet `json:"sets"`
}

// QuantSet is one set of real vectors and the questions asked of it.
type QuantSet struct {
	Name string `json:"name"`
	// Text says what the vectors are of (natural language, or the synthetic text
	// of the scale sweep).
	Text    string `json:"text"`
	Dim     int    `json:"dim"`
	Vectors int    `json:"vectors"`
	Queries int    `json:"queries"`
	// K is the cut-off actually used: the smaller of quantK and the vectors.
	K int `json:"k"`
	// RecallAtK has one entry per question: the share of its exact top K that the
	// quantised scan also returns.
	RecallAtK []float64 `json:"recall_at_k"`
	Mean      *float64  `json:"mean"`
	Min       *float64  `json:"min"`
	// Perfect counts the questions whose recall is 1.
	Perfect int `json:"perfect"`
	// MeanAbsScoreErr and MaxAbsScoreErr are |quantised cosine - exact cosine|
	// over every pair of a question and a vector.
	MeanAbsScoreErr float64 `json:"mean_abs_score_err"`
	MaxAbsScoreErr  float64 `json:"max_abs_score_err"`
	// MeetsBar is mean recall at or over the bar.
	MeetsBar bool `json:"meets_bar"`
}

// quantSummary derives a set's aggregates from its per-question recalls, so the
// writer and the checker share one definition.
func quantSummary(s QuantSet) QuantSet {
	s.Mean, s.Min, s.Perfect, s.MeetsBar = nil, nil, 0, false
	if len(s.RecallAtK) == 0 {
		return s
	}
	sum, lo := 0.0, math.Inf(1)
	for _, r := range s.RecallAtK {
		sum += r
		lo = math.Min(lo, r)
		if r == 1 {
			s.Perfect++
		}
	}
	s.Mean, s.Min = round6(sum/float64(len(s.RecallAtK))), round6(lo)
	s.MeetsBar = *s.Mean >= quantBar
	return s
}

// embedAll embeds texts in batches and checks every vector has one dimension.
func embedAll(ctx context.Context, emb providers.Embedder, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += quantBatch {
		batch := texts[start:min(start+quantBatch, len(texts))]
		res, err := emb.Embed(ctx, scaleScopeOf(0), providers.EmbedRequest{Inputs: batch})
		if err != nil {
			return nil, fmt.Errorf("embedding texts %d to %d: %w", start, start+len(batch), err)
		}
		if len(res.Vectors) != len(batch) {
			return nil, fmt.Errorf("the embedder returned %d vectors for %d texts", len(res.Vectors), len(batch))
		}
		out = append(out, res.Vectors...)
	}
	if len(out) > 0 {
		for i, v := range out {
			if len(v) != len(out[0]) {
				return nil, fmt.Errorf("vector %d has %d dimensions, the first has %d", i, len(v), len(out[0]))
			}
		}
	}
	return out, nil
}

// measureQuant embeds the texts and the queries and compares the quantised scan
// with the exact one.
func measureQuant(ctx context.Context, emb providers.Embedder, name, text string, texts, queries []string) (QuantSet, error) {
	if len(texts) == 0 || len(queries) == 0 {
		return QuantSet{}, errors.New("the quantisation check needs texts and queries")
	}
	docs, err := embedAll(ctx, emb, texts)
	if err != nil {
		return QuantSet{}, err
	}
	qs, err := embedAll(ctx, emb, queries)
	if err != nil {
		return QuantSet{}, err
	}
	return quantCompare(name, text, docs, qs)
}

// quantCompare is the arithmetic of the check, on vectors already embedded.
func quantCompare(name, text string, docs, queries [][]float32) (QuantSet, error) {
	if len(docs) == 0 || len(queries) == 0 || len(docs[0]) != len(queries[0]) {
		return QuantSet{}, errors.New("the quantisation check needs vectors of one dimension")
	}
	n := len(docs)
	unit := make([][]float32, n)
	codes := make([][]byte, n)
	inv := make([]float32, n)
	for i, d := range docs {
		u, err := vector.UnitQuery(d)
		if err != nil {
			return QuantSet{}, fmt.Errorf("vector %d: %w", i, err)
		}
		unit[i] = u
		if codes[i], inv[i], err = vector.Quantise(d); err != nil {
			return QuantSet{}, fmt.Errorf("vector %d: %w", i, err)
		}
	}
	set := QuantSet{Name: name, Text: text, Dim: len(docs[0]), Vectors: n, Queries: len(queries), K: min(quantK, n)}
	var errSum, errMax float64
	pairs := 0
	exact, quant := make([]float64, n), make([]float64, n)
	for qi, q := range queries {
		uq, err := vector.UnitQuery(q)
		if err != nil {
			return QuantSet{}, fmt.Errorf("query %d: %w", qi, err)
		}
		for i := range docs {
			var dot float64
			for j, x := range uq {
				dot += float64(x) * float64(unit[i][j])
			}
			exact[i], quant[i] = dot, float64(vector.Score(uq, codes[i], inv[i]))
			d := math.Abs(quant[i] - exact[i])
			errSum += d
			errMax = math.Max(errMax, d)
			pairs++
		}
		set.RecallAtK = append(set.RecallAtK, *round6(topKOverlap(exact, quant, set.K)))
	}
	set.MeanAbsScoreErr, set.MaxAbsScoreErr = *round6(errSum / float64(pairs)), *round6(errMax)
	return quantSummary(set), nil
}

// topKOverlap is the share of the k best by a (the reference) that the k best
// by b also hold. Ties go to the lower index, so it is deterministic.
func topKOverlap(a, b []float64, k int) float64 {
	top := func(s []float64) map[int]bool {
		idx := make([]int, len(s))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(x, y int) bool { return s[idx[x]] > s[idx[y]] })
		set := make(map[int]bool, k)
		for _, i := range idx[:k] {
			set[i] = true
		}
		return set
	}
	ref, got := top(a), top(b)
	hit := 0
	for i := range ref {
		if got[i] {
			hit++
		}
	}
	return float64(hit) / float64(k)
}

// docsQuantInputs reads the Markdown files of dir (not recursively, by name),
// chunks them and returns up to maxChunks chunk texts as the vectors' texts and up
// to nQueries of their section headings, evenly spread, as the questions: natural
// language at a size where "the ten nearest" is a small part of the set.
func docsQuantInputs(ctx context.Context, dir string, maxChunks, nQueries int) (texts, queries []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		return nil, nil, err
	}
	var sections []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || len(texts) >= maxChunks {
			continue
		}
		b, err := readCappedFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		res, err := chk.Chunk(ctx, chunk.FromMarkdown(e.Name(), string(b)))
		if err != nil {
			return nil, nil, fmt.Errorf("chunking %s: %w", e.Name(), err)
		}
		for _, c := range res.Children {
			if len(texts) >= maxChunks {
				break
			}
			texts = append(texts, c.Text)
			if s := strings.TrimSpace(c.Section); s != "" && !seen[s] {
				seen[s] = true
				sections = append(sections, s)
			}
		}
	}
	if len(sections) < 10 || len(texts) < quantK {
		return nil, nil, fmt.Errorf("%s holds %d chunks and %d section headings: too few for the quantisation check", dir, len(texts), len(sections))
	}
	for i := 0; i < min(nQueries, len(sections)); i++ {
		queries = append(queries, sections[i*len(sections)/min(nQueries, len(sections))])
	}
	return texts, queries, nil
}

// runQuantCheck measures the sets: the labelled corpus (natural language, every
// chunk of every document, with the labelled questions); when docsDir is set, a
// thousand chunks of the Markdown there against section headings (natural
// language, 10 times the size); and the synthetic text of the scale sweep (1,000
// chunks, 50 short and 50 long questions), which is the hard case: pseudo-words
// embed close together.
func runQuantCheck(ctx context.Context, emb providers.Embedder, kind string, corpus *Corpus, seed uint64, docsDir string) (*Quantization, error) {
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		return nil, err
	}
	var texts []string
	for i := range corpus.Manifest.Sources {
		for _, v := range corpus.Manifest.Sources[i].Versions {
			for _, d := range v.Documents {
				res, err := chk.Chunk(ctx, chunk.FromMarkdown(d.ID, corpus.Doc(d.File)))
				if err != nil {
					return nil, fmt.Errorf("chunking %s: %w", d.ID, err)
				}
				for _, c := range res.Children {
					texts = append(texts, c.Text)
				}
			}
		}
	}
	var questions []string
	for _, q := range corpus.Questions {
		questions = append(questions, q.Text)
	}
	a, err := measureQuant(ctx, emb, "labelled-corpus", "every chunk of the labelled corpus (English and Italian) against its 50 questions", texts, questions)
	if err != nil {
		return nil, err
	}

	sets := []QuantSet{a}
	if docsDir != "" {
		dt, dq, err := docsQuantInputs(ctx, docsDir, 1000, 100)
		if err != nil {
			return nil, err
		}
		d, err := measureQuant(ctx, emb, "markdown-docs", fmt.Sprintf("%d chunks of the English Markdown of %s against %d of its section headings", len(dt), filepath.ToSlash(docsDir), len(dq)), dt, dq)
		if err != nil {
			return nil, err
		}
		sets = append(sets, d)
	}

	vocab := scaleVocabulary()
	zipf := newZipf(seed+1, 1)
	var synth []string
	for _, c := range scaleDocument(zipf, vocab, 0, "d0000", 0, 1000).Children {
		synth = append(synth, c.Text)
	}
	sq := append(scaleQuestions(scaleKindShort, 50, seed), scaleQuestions(scaleKindLong, 50, seed)...)
	b, err := measureQuant(ctx, emb, "synthetic-scale-text", "1,000 chunks of the scale sweep's seeded pseudo-word text against 50 short and 50 long questions", synth, sq)
	if err != nil {
		return nil, err
	}
	return &Quantization{
		Embedder: kind, Scheme: "int8, one scale per vector, cosine of the float query and the quantised vector (internal/rag/vector Quantise, UnitQuery, Score)",
		K: quantK, Bar: quantBar, Sets: append(sets, b),
	}, nil
}
