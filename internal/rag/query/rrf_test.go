package query

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

func fusedKeys(f []Fused) []string {
	out := make([]string, len(f))
	for i, x := range f {
		out[i] = x.Key
	}
	return out
}

// TestRRFCalculation checks Reciprocal Rank Fusion against values worked out by
// hand: score = sum over the lists holding the document of 1 / (k + rank), the
// rank counted from 1. The expected scores below are literal decimals, not the
// formula run again.
func TestRRFCalculation(t *testing.T) {
	t.Run("k=60, two lists", func(t *testing.T) {
		// A = [a b c], B = [b c d]
		//   a: 1/61                = 0.016393442622950820
		//   b: 1/62 + 1/61         = 0.016129032258064516 + 0.016393442622950820 = 0.032522474881015336
		//   c: 1/63 + 1/62         = 0.015873015873015872 + 0.016129032258064516 = 0.032002048131080388
		//   d: 1/63                = 0.015873015873015872
		got := Fuse(60, []string{"a", "b", "c"}, []string{"b", "c", "d"})
		want := []struct {
			key   string
			score float64
			ranks []int
		}{
			{"b", 0.032522474881015336, []int{2, 1}},
			{"c", 0.032002048131080388, []int{3, 2}},
			{"a", 0.016393442622950820, []int{1, 0}},
			{"d", 0.015873015873015872, []int{0, 3}},
		}
		if len(got) != len(want) {
			t.Fatalf("fused %d documents, want %d: %v", len(got), len(want), fusedKeys(got))
		}
		for i, w := range want {
			if got[i].Key != w.key {
				t.Fatalf("position %d is %q, want %q (order %v)", i, got[i].Key, w.key, fusedKeys(got))
			}
			if math.Abs(got[i].Score-w.score) > 1e-15 {
				t.Errorf("%s: score %.18f, want %.18f", w.key, got[i].Score, w.score)
			}
			if !reflect.DeepEqual(got[i].Ranks, w.ranks) {
				t.Errorf("%s: ranks %v, want %v", w.key, got[i].Ranks, w.ranks)
			}
		}
	})

	t.Run("k=1, one list holds the document twice over", func(t *testing.T) {
		// A = [a b c], B = [c]
		//   c: 1/(1+3) + 1/(1+1) = 0.25 + 0.5 = 0.75
		//   a: 1/2                = 0.5
		//   b: 1/3                = 0.3333333333333333
		got := Fuse(1, []string{"a", "b", "c"}, []string{"c"})
		if want := []string{"c", "a", "b"}; !reflect.DeepEqual(fusedKeys(got), want) {
			t.Fatalf("order %v, want %v", fusedKeys(got), want)
		}
		for i, want := range []float64{0.75, 0.5, 1.0 / 3.0} {
			if math.Abs(got[i].Score-want) > 1e-15 {
				t.Errorf("%s: score %.18f, want %.18f", got[i].Key, got[i].Score, want)
			}
		}
	})

	t.Run("k=10 is configurable and moves the score", func(t *testing.T) {
		// [a] and [a] at k=10: 2/11 = 0.18181818181818182
		got := Fuse(10, []string{"a"}, []string{"a"})
		if len(got) != 1 || math.Abs(got[0].Score-0.18181818181818182) > 1e-15 {
			t.Fatalf("got %+v, want one document scoring 2/11", got)
		}
	})

	t.Run("a key repeated inside one list counts once, at its best rank", func(t *testing.T) {
		// [a a b]: a is rank 1, b is rank 2 (not 3): 1/61 and 1/62.
		got := Fuse(60, []string{"a", "a", "b"})
		if want := []string{"a", "b"}; !reflect.DeepEqual(fusedKeys(got), want) {
			t.Fatalf("order %v, want %v", fusedKeys(got), want)
		}
		if math.Abs(got[0].Score-0.016393442622950820) > 1e-15 || math.Abs(got[1].Score-0.016129032258064516) > 1e-15 {
			t.Errorf("scores %.18f %.18f, want 1/61 and 1/62", got[0].Score, got[1].Score)
		}
	})

	t.Run("a document in two lists beats one in a single list at the same depth", func(t *testing.T) {
		// Both x and y are rank 1 in one list; x is also rank 5 in the other.
		got := Fuse(60, []string{"y", "q1", "q2", "q3", "x"}, []string{"x"})
		if got[0].Key != "x" {
			t.Fatalf("order %v: the document found by both lists must lead", fusedKeys(got))
		}
	})

	t.Run("a non-positive k falls back to 60", func(t *testing.T) {
		a := Fuse(0, []string{"a", "b"})
		b := Fuse(DefaultRRFK, []string{"a", "b"})
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("k=0 gave %+v, want the k=60 result %+v", a, b)
		}
	})

	t.Run("nothing in, nothing out", func(t *testing.T) {
		if got := Fuse(60); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
		if got := Fuse(60, nil, []string{}); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}

// A tie is broken the same way every time: it is a rule, not an accident of map
// order or of how floating-point terms were added.
func TestRRFTiesAreDeterministic(t *testing.T) {
	// u and v each hold ranks {1, 2}, from opposite lists: bit-identical scores.
	// The tie goes to the document seen first when the lists are read in order.
	got := Fuse(60, []string{"u", "v"}, []string{"v", "u"})
	if got[0].Score != got[1].Score {
		t.Fatalf("scores differ: %.20f vs %.20f; the sum must not depend on which list a rank came from", got[0].Score, got[1].Score)
	}
	if want := []string{"u", "v"}; !reflect.DeepEqual(fusedKeys(got), want) {
		t.Fatalf("order %v, want %v (first appearance)", fusedKeys(got), want)
	}

	// Two documents, each first in a different list: the better best rank is a
	// tie too, so first appearance decides; swapping the lists swaps the winner.
	if got := Fuse(60, []string{"x"}, []string{"y"}); !reflect.DeepEqual(fusedKeys(got), []string{"x", "y"}) {
		t.Fatalf("order %v, want [x y]", fusedKeys(got))
	}
	if got := Fuse(60, []string{"y"}, []string{"x"}); !reflect.DeepEqual(fusedKeys(got), []string{"y", "x"}) {
		t.Fatalf("order %v, want [y x]", fusedKeys(got))
	}

	// Three lists: m and n hold the same multiset of ranks {1, 2}, o has rank 1 once.
	got = Fuse(60, []string{"m", "n"}, []string{"o", "m"}, []string{"n"})
	// m: ranks 1, 2 -> 1/61 + 1/62 ; n: ranks 2, 1 -> the same multiset ; o: rank 1 in B.
	if got[0].Score != got[1].Score {
		t.Fatalf("m and n should tie: %.20f vs %.20f", got[0].Score, got[1].Score)
	}
	if got[0].Key != "m" || got[1].Key != "n" || got[2].Key != "o" {
		t.Fatalf("order %v, want [m n o]", fusedKeys(got))
	}

	// The same input, many times, in a shuffled map-iteration world, one answer.
	rng := rand.New(rand.NewPCG(7, 9))
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"}
	mk := func() [][]string {
		var lists [][]string
		for l := 0; l < 3; l++ {
			p := rng.Perm(len(keys))
			var list []string
			for _, i := range p[:5] {
				list = append(list, keys[i])
			}
			lists = append(lists, list)
		}
		return lists
	}
	for i := 0; i < 50; i++ {
		lists := mk()
		first := fusedKeys(Fuse(60, lists...))
		for j := 0; j < 20; j++ {
			if again := fusedKeys(Fuse(60, lists...)); !reflect.DeepEqual(first, again) {
				t.Fatalf("lists %v: %v then %v", lists, first, again)
			}
		}
	}
}

// Fusion never invents or loses a document and never lets one list's length
// change another's contribution.
func TestRRFNeverLosesADocument(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	for i := 0; i < 200; i++ {
		var lists [][]string
		want := map[string]bool{}
		for l := 0; l < 1+rng.IntN(4); l++ {
			var list []string
			for j := 0; j < rng.IntN(12); j++ {
				k := "d" + itoa(rng.IntN(15))
				list = append(list, k)
				want[k] = true
			}
			lists = append(lists, list)
		}
		got := Fuse(1+rng.IntN(100), lists...)
		if len(got) != len(want) {
			t.Fatalf("lists %v fused %d documents, want %d", lists, len(got), len(want))
		}
		for j := 1; j < len(got); j++ {
			if got[j].Score > got[j-1].Score {
				t.Fatalf("lists %v: not ordered by score at %d: %v", lists, j, got)
			}
		}
	}
}
