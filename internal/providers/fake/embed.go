package fake

import (
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"
)

// DefaultEmbedDim is the dimensionality of the default fake embeddings.
const DefaultEmbedDim = 32

func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// EmbedVector is the fake's deterministic embedding: a hashed bag of words,
// L2-normalised. The same text always gives the same vector, and texts that
// share words have a higher cosine similarity, which is enough for retrieval
// tests that need "related" to beat "unrelated".
func EmbedVector(text string, dim int) []float32 {
	if dim <= 0 {
		dim = DefaultEmbedDim
	}
	toks := tokenize(text)
	if len(toks) == 0 {
		toks = []string{text}
	}
	acc := make([]float64, dim)
	for _, t := range toks {
		h := fnv.New64a()
		_, _ = h.Write([]byte(t))
		sum := h.Sum64()
		sign := 1.0
		if sum>>63 == 1 {
			sign = -1
		}
		acc[int(sum%uint64(dim))] += sign
	}
	var norm float64
	for _, x := range acc {
		norm += x * x
	}
	if norm == 0 { // opposing signs cancelled out; pick a fixed direction
		acc[0], norm = 1, 1
	}
	norm = math.Sqrt(norm)
	out := make([]float32, dim)
	for i, x := range acc {
		out[i] = float32(x / norm)
	}
	return out
}

// RerankScore is the fake's deterministic relevance: the fraction of the
// query's distinct words that appear in the document, in [0, 1].
func RerankScore(query, doc string) float64 {
	q := distinct(tokenize(query))
	if len(q) == 0 {
		return 0
	}
	d := make(map[string]bool)
	for _, t := range tokenize(doc) {
		d[t] = true
	}
	hit := 0
	for _, t := range q {
		if d[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(q))
}

func distinct(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
