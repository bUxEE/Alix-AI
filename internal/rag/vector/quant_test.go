package vector

import (
	"bytes"
	"math"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func rnd(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed+1)) }

func unitVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var sq float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		sq += x * x
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(sq))
	}
	return v
}

func cosine(a, b []float32) float64 {
	var d, na, nb float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return d / math.Sqrt(na*nb)
}

func TestQuantiseClampsAndKeepsDirection(t *testing.T) {
	r := rnd(1)
	var worst float64
	for i := 0; i < 500; i++ {
		a, b := unitVec(r, 384), unitVec(r, 384)
		// Make b share direction with a so the cosines span a useful range.
		for j := range b {
			b[j] = 0.6*a[j] + 0.4*b[j]
		}
		code, inv, err := Quantise(b)
		if err != nil {
			t.Fatal(err)
		}
		uq, err := UnitQuery(a)
		if err != nil {
			t.Fatal(err)
		}
		got := float64(Score(uq, code, inv))
		if d := math.Abs(got - cosine(a, b)); d > worst {
			worst = d
		}
	}
	if worst > 0.01 {
		t.Fatalf("worst cosine error from int8 quantisation is %.4f, want under 0.01", worst)
	}
	t.Logf("worst |score - cosine| over 500 pairs of 384-d vectors: %.5f", worst)
}

func TestQuantiseNeverWraps(t *testing.T) {
	// The largest component is exactly +-127 and nothing exceeds it, whatever
	// the magnitude: an int8 cast of an unclamped value would wrap.
	for name, v := range map[string][]float32{
		"huge":          {3e38, -3e38, 1e38, 0},
		"tiny":          {1e-30, -1e-30, 5e-31, 0},
		"one hot":       {0, 0, 7, 0},
		"negative peak": {-5, 1, 2, 3},
		"mixed":         {0.001, -1000, 500, 0.5},
	} {
		code, inv, err := Quantise(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var peak int
		for _, c := range code {
			x := int(int8(c))
			if x < -127 || x > 127 {
				t.Fatalf("%s: code %d out of range", name, x)
			}
			if a := abs(x); a > peak {
				peak = a
			}
		}
		if peak != 127 {
			t.Fatalf("%s: peak %d, want the largest component to map to 127", name, peak)
		}
		if !(inv > 0) || math.IsInf(float64(inv), 0) {
			t.Fatalf("%s: inverse norm %v", name, inv)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestQuantiseIsScaleInvariant(t *testing.T) {
	r := rnd(2)
	v := unitVec(r, 96)
	big := make([]float32, len(v))
	for i := range v {
		big[i] = v[i] * 1e6
	}
	c1, i1, _ := Quantise(v)
	c2, i2, _ := Quantise(big)
	if !bytes.Equal(c1, c2) || math.Abs(float64(i1-i2)) > 1e-6 {
		t.Fatal("scaling a vector changed its stored form: cosine similarity must not depend on length")
	}
}

func TestQuantiseRefusesUnusableVectors(t *testing.T) {
	for name, v := range map[string][]float32{
		"nil":   nil,
		"zeros": {0, 0, 0},
		"nan":   {1, float32(math.NaN())},
		"inf":   {float32(math.Inf(-1)), 1},
	} {
		if _, _, err := Quantise(v); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: err = %v, want validation_failed", name, err)
		}
		if _, err := UnitQuery(v); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s (query): err = %v, want validation_failed", name, err)
		}
	}
}

func TestDotMatchesNaive(t *testing.T) {
	r := rnd(3)
	for _, dim := range []int{1, 3, 4, 5, 31, 32, 384, 1023} {
		q := make([]float32, dim)
		c := make([]byte, dim)
		var want float64
		for i := range q {
			q[i] = float32(r.NormFloat64())
			c[i] = byte(int8(r.IntN(255) - 127))
			want += float64(q[i]) * float64(int8(c[i]))
		}
		if got := float64(dot(q, c)); math.Abs(got-want) > 1e-3*math.Max(1, math.Abs(want)) {
			t.Fatalf("dim %d: dot = %v, want %v", dim, got, want)
		}
	}
}

func TestTopKMatchesFullSort(t *testing.T) {
	r := rnd(4)
	for trial := 0; trial < 200; trial++ {
		n, k := 1+r.IntN(300), 1+r.IntN(25)
		type row struct {
			id    string
			score float32
		}
		rows := make([]row, n)
		tk := newTopK(k)
		for i := range rows {
			// Few distinct scores, so ties are common; ids are unique.
			id := string(rune('a'+r.IntN(26))) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
			rows[i] = row{id: id, score: float32(r.IntN(8)) / 8}
			tk.offer(rows[i].score, []byte(rows[i].id), 0)
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].score != rows[j].score {
				return rows[i].score > rows[j].score
			}
			return rows[i].id < rows[j].id
		})
		want := rows[:min(k, n)]
		got := tk.sorted()
		if len(got) != len(want) {
			t.Fatalf("trial %d: %d kept, want %d", trial, len(got), len(want))
		}
		for i := range got {
			if got[i].id != want[i].id || got[i].score != want[i].score {
				t.Fatalf("trial %d: rank %d = %v/%v, want %v/%v", trial, i, got[i].id, got[i].score, want[i].id, want[i].score)
			}
		}
	}
}
