package vector

import (
	"math"
	"sort"
)

// Quantise turns a float vector into the stored form: one signed byte per
// dimension, scaled so the largest component maps to +-127 (a per-vector
// scale, clamped, so no component can wrap), plus the reciprocal of the
// quantised vector's own length. Cosine similarity is scale invariant, so no
// scale is stored: Score(q, code) is the cosine of the query and the
// quantised vector, normalised by the quantised vector's real length.
//
// It refuses an empty, non-finite or all-zero vector.
func Quantise(v []float32) (code []byte, invNorm float32, err error) {
	if err := checkVector("vector", v); err != nil {
		return nil, 0, err
	}
	var max float64
	for _, x := range v {
		if a := math.Abs(float64(x)); a > max {
			max = a
		}
	}
	scale := 127 / max
	code = make([]byte, len(v))
	var sq float64
	for i, x := range v {
		q := math.Round(float64(x) * scale)
		if q > 127 {
			q = 127
		} else if q < -127 {
			q = -127
		}
		code[i] = byte(int8(q))
		sq += q * q
	}
	if sq == 0 { // every component rounded to zero: cannot happen when max > 0, kept as a guard
		return nil, 0, invalid("vector quantises to zero")
	}
	return code, float32(1 / math.Sqrt(sq)), nil
}

// UnitQuery returns the query scaled to unit length, in float32, ready for
// Score. The query stays float: only the stored side is quantised, which keeps
// the error to one side.
func UnitQuery(v []float32) ([]float32, error) {
	if err := checkVector("query vector", v); err != nil {
		return nil, err
	}
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	if sq == 0 || math.IsInf(sq, 0) {
		return nil, invalid("query vector has no usable length")
	}
	inv := 1 / math.Sqrt(sq)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) * inv)
	}
	return out, nil
}

// Score is the cosine similarity of the unit query q and a stored vector.
func Score(q []float32, code []byte, invNorm float32) float32 {
	return dot(q, code) * invNorm
}

// dot is the float query against a signed-byte vector, four accumulators so
// the multiplications do not wait on one another. len(q) must equal len(c).
func dot(q []float32, c []byte) float32 {
	n := len(c)
	q = q[:n]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += q[i] * float32(int8(c[i]))
		s1 += q[i+1] * float32(int8(c[i+1]))
		s2 += q[i+2] * float32(int8(c[i+2]))
		s3 += q[i+3] * float32(int8(c[i+3]))
	}
	for ; i < n; i++ {
		s0 += q[i] * float32(int8(c[i]))
	}
	return (s0 + s1) + (s2 + s3)
}

// topK keeps the K best candidates seen: higher score first, and among equal
// scores the smaller chunk id first, so results are deterministic whatever
// order the rows arrive in.
type topK struct {
	k     int
	items []cand // min-heap by "worse"
}

type cand struct {
	score float32
	id    string
	gen   int // index into the searched generations
}

func newTopK(k int) *topK { return &topK{k: k, items: make([]cand, 0, k)} }

// worse reports whether a ranks below b.
func worse(a, b cand) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return a.id > b.id
}

// offer considers one candidate; id is copied only when it is kept.
func (t *topK) offer(score float32, id []byte, gen int) {
	if len(t.items) == t.k {
		root := t.items[0]
		if score < root.score || (score == root.score && string(id) >= root.id) {
			return
		}
		t.items[0] = cand{score, string(id), gen}
		t.down(0)
		return
	}
	t.items = append(t.items, cand{score, string(id), gen})
	t.up(len(t.items) - 1)
}

func (t *topK) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !worse(t.items[i], t.items[p]) {
			return
		}
		t.items[i], t.items[p] = t.items[p], t.items[i]
		i = p
	}
}

func (t *topK) down(i int) {
	n := len(t.items)
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < n && worse(t.items[l], t.items[m]) {
			m = l
		}
		if r < n && worse(t.items[r], t.items[m]) {
			m = r
		}
		if m == i {
			return
		}
		t.items[i], t.items[m] = t.items[m], t.items[i]
		i = m
	}
}

// sorted returns the kept candidates best first.
func (t *topK) sorted() []cand {
	out := append([]cand(nil), t.items...)
	sort.Slice(out, func(i, j int) bool { return worse(out[j], out[i]) })
	return out
}
