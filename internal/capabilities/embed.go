package capabilities

import (
	"context"
	"math"
	"sort"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The optional embedding stage. It runs only when an embedder exists, only when
// the rules did not already find one clear candidate, and it can only add
// evidence about capabilities the policy gateway has already made visible:
//
//   - the descriptors it embeds are the visible capabilities' own text (title,
//     purpose, terminology labels), so a capability the caller may not see is
//     never sent to the embedder, which may be an external provider;
//   - descriptors are embedded once per package, language and model and cached,
//     so a turn costs one embedding call (the utterance);
//   - any failure (error, timeout, malformed vector, a model that changed between
//     the two calls) leaves the rules-only result untouched and is reported as
//     EmbeddingFailed. An embedder cannot make the router refuse, and cannot make
//     it expose anything.

// embedStage returns what the stage did. It may add embedding signals to
// candidates through get.
func (rt *Router) embedStage(ctx context.Context, scope contracts.Scope, ix *index, utt string, langs []string, visible map[string]policy.Decision, get func(string) *cand, ranked []*cand) EmbeddingStatus {
	emb, st := rt.embedder()
	if emb == nil {
		return st
	}
	if decisive(ranked, rt.cfg.ConfidentMargin) {
		return EmbeddingSkipped
	}
	lang := ""
	if len(langs) > 0 {
		lang = langs[0]
	}
	descs := ix.descriptors[lang]
	if len(descs) == 0 {
		return EmbeddingSkipped
	}

	ctx, cancel := context.WithTimeout(ctx, rt.cfg.EmbedTimeout)
	defer cancel()

	qreq := providers.EmbedRequest{Inputs: []string{utt}, Purpose: providers.EmbedQuery}
	qres, err := emb.Embed(ctx, scope, qreq)
	if err != nil || qres.CheckResult(qreq) != nil {
		return EmbeddingFailed
	}
	q := qres.Vectors[0]

	ids := make([]string, 0, len(visible))
	for id := range visible {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	docs := ix.cachedVectors(lang, qres.Model, len(q))
	var missing []string
	for _, id := range ids {
		if _, ok := docs[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		inputs := make([]string, len(missing))
		for i, id := range missing {
			inputs[i] = descs[id]
			if inputs[i] == "" {
				inputs[i] = id
			}
		}
		dreq := providers.EmbedRequest{Inputs: inputs, Purpose: providers.EmbedDocument}
		dres, err := emb.Embed(ctx, scope, dreq)
		if err != nil || dres.CheckResult(dreq) != nil || dres.Model != qres.Model || len(dres.Vectors[0]) != len(q) {
			return EmbeddingFailed
		}
		fresh := make(map[string][]float32, len(missing))
		for i, id := range missing {
			fresh[id] = dres.Vectors[i]
			docs[id] = dres.Vectors[i]
		}
		ix.storeVectors(lang, qres.Model, len(q), fresh)
	}

	for _, id := range ids {
		sim := cosine(q, docs[id])
		if sim < rt.cfg.MinSimilarity {
			continue
		}
		w := embedWeight * (sim - rt.cfg.MinSimilarity) / (1 - rt.cfg.MinSimilarity)
		if w <= 0 {
			continue
		}
		get(id).add(Signal{Kind: KindEmbedding, Weight: w, Similarity: sim})
	}
	return EmbeddingUsed
}

// cachedVectors returns a copy of the cached descriptor vectors of a language,
// or an empty map if there are none for this model and dimension: vectors of
// another model are never compared with a query of this one.
func (ix *index) cachedVectors(lang, model string, dim int) map[string][]float32 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := map[string][]float32{}
	vs := ix.vecs[lang]
	if vs == nil || vs.model != model || vs.dim != dim {
		return out
	}
	for id, v := range vs.vecs {
		out[id] = v
	}
	return out
}

// storeVectors adds descriptor vectors to the cache. If the model or dimension
// differs from what is cached, the old vectors are discarded first.
func (ix *index) storeVectors(lang, model string, dim int, add map[string][]float32) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	vs := ix.vecs[lang]
	if vs == nil || vs.model != model || vs.dim != dim {
		vs = &vecSet{model: model, dim: dim, vecs: map[string][]float32{}}
		ix.vecs[lang] = vs
	}
	for id, v := range add {
		vs.vecs[id] = v
	}
}

// cosine is the cosine similarity of two vectors, 0 for vectors of different
// length or with no length.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	c := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// Rounding can push a perfect match a hair past 1.
	return math.Max(-1, math.Min(1, c))
}
