package vectortest

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Model is the embedding model id the suite uses.
const Model = "suite-model-v1"

// Rand is a deterministic random source for fixtures.
func Rand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }

// RandomUnit is a uniformly random unit vector.
func RandomUnit(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var sq float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		sq += x * x
	}
	return scale(v, 1/math.Sqrt(sq))
}

// Perturb returns v plus gaussian noise of the given standard deviation per
// component (relative to a unit vector), renormalised.
func Perturb(r *rand.Rand, v []float32, sigma float64) []float32 {
	out := make([]float32, len(v))
	var sq float64
	for i, x := range v {
		y := float64(x) + r.NormFloat64()*sigma/math.Sqrt(float64(len(v)))
		out[i] = float32(y)
		sq += y * y
	}
	return scale(out, 1/math.Sqrt(sq))
}

func scale(v []float32, f float64) []float32 {
	for i := range v {
		v[i] = float32(float64(v[i]) * f)
	}
	return v
}

// Clustered returns n vectors drawn around `centres` random centres, the shape
// real embeddings have (topics), unlike uniform random vectors which have no
// neighbour structure at all.
func Clustered(r *rand.Rand, n, dim, centres int, sigma float64) [][]float32 {
	cs := make([][]float32, centres)
	for i := range cs {
		cs[i] = RandomUnit(r, dim)
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = Perturb(r, cs[r.IntN(centres)], sigma)
	}
	return out
}

// Cosine is the float64 cosine similarity.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// BruteForce returns the chunk ids of the k vectors most similar to q, by an
// exhaustive float64 cosine scan that shares no code with any provider; ties
// break by chunk id like the providers do. It is the reference recall is
// measured against.
func BruteForce(ids []string, vecs [][]float32, q []float32, k int) []string {
	type scored struct {
		id string
		s  float64
	}
	all := make([]scored, len(ids))
	for i := range ids {
		all[i] = scored{ids[i], Cosine(q, vecs[i])}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].s != all[j].s {
			return all[i].s > all[j].s
		}
		return all[i].id < all[j].id
	})
	if k > len(all) {
		k = len(all)
	}
	out := make([]string, k)
	for i := range out {
		out[i] = all[i].id
	}
	return out
}

// Recall is the fraction of want that got contains.
func Recall(want, got []string) float64 {
	if len(want) == 0 {
		return 1
	}
	in := make(map[string]struct{}, len(got))
	for _, g := range got {
		in[g] = struct{}{}
	}
	hit := 0
	for _, w := range want {
		if _, ok := in[w]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

// Scope builds a valid scope for the suite.
func Scope(app, tenant, workspace string) contracts.Scope {
	return storetest.Scope(app, tenant, workspace)
}

// Items builds upsert items for scope: chunk ids prefix0, prefix1, ..., keys
// derived from the scope, the suite model and the chunk id as the "input".
func Items(scope contracts.Scope, prefix string, vecs [][]float32) []vector.Item {
	items := make([]vector.Item, len(vecs))
	for i, v := range vecs {
		id := fmt.Sprintf("%s%04d", prefix, i)
		key, err := vector.NewInputKey(scope, Model, "input of "+id)
		if err != nil {
			panic(err)
		}
		items[i] = vector.Item{ChunkID: id, Key: key, Vector: v}
	}
	return items
}

// IDs lists the chunk ids of items.
func IDs(items []vector.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ChunkID
	}
	return out
}

// Vectors lists the vectors of items.
func Vectors(items []vector.Item) [][]float32 {
	out := make([][]float32, len(items))
	for i, it := range items {
		out[i] = it.Vector
	}
	return out
}
