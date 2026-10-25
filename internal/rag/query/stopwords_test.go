package query

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// *index.Index is a HitReader now (index.Hits, M2-16): the pipeline can read the
// chunks its vector stage found without a reader supplied from outside.
var _ HitReader = (*index.Index)(nil)

func TestStopWordsAreExported(t *testing.T) {
	all := StopWords()
	if len(all) < 100 {
		t.Fatalf("%d stop words", len(all))
	}
	if !sort.StringsAreSorted(all) {
		t.Error("StopWords is not sorted")
	}
	if len(slices.Compact(slices.Clone(all))) != len(all) {
		t.Error("StopWords has a duplicate")
	}
	for _, w := range all {
		if !IsStopWord(w) || !IsStopWord(strings.ToUpper(w)) {
			t.Errorf("IsStopWord(%q) is false for a word of StopWords", w)
		}
		if w != strings.ToLower(w) || strings.TrimSpace(w) != w || w == "" {
			t.Errorf("stop word %q is not a plain lower-case word", w)
		}
	}
	for _, w := range []string{"the", "of", "della", "più", "per"} {
		if !IsStopWord(w) {
			t.Errorf("%q is not a stop word", w)
		}
	}
	for _, w := range []string{"", "kiwi", "harbour", "dock", "ABC-123", "tariffa"} {
		if IsStopWord(w) {
			t.Errorf("%q is a stop word", w)
		}
	}
	// What a caller does to the slice is its own business.
	all[0] = "zzz-changed"
	if StopWords()[0] == "zzz-changed" || IsStopWord("zzz-changed") {
		t.Error("StopWords hands out the package's own storage")
	}
}

// reduceQuery keeps a stop word out of a content word's slot: the exported
// list is the one that decides it.
func TestStopWordsDecideTheTermBudget(t *testing.T) {
	got := reduceQuery("the harbour of the dock and the schedule for tide tables", 3)
	for _, w := range strings.Fields(got) {
		if IsStopWord(w) {
			t.Errorf("a stop word took a term slot: %q in %q", w, got)
		}
	}
}

// The index applies its own copy of the list inside Search (it cannot import
// this package). The two must agree, because the query package spends its term
// budget on the assumption that the index will drop what this list calls a stop
// word: a word of this list that the index kept would cost a slot and match
// every chunk. The test asks the index itself: a query "word kiwi" finds a chunk
// that has the word but not kiwi only when the index does NOT drop the word.
func TestStopWordsAreTheIndexs(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, nil)
	ix, err := index.New(db, clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)), index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	scope := storetest.Scope("app", "t1", "w1")
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// One document per word, each holding exactly that word, so that a hit says
	// which word the index kept.
	words := append(StopWords(), "harbour", "dock", "orchard")
	gen, err := ix.BeginGeneration(ctx, scope, ids.New("src"), index.Components{
		Parser: rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"}, Chunker: chk.Component(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range words {
		id := "doc-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		res, err := chk.Chunk(ctx, chunk.FromMarkdown(id, w+" "+w+" "+w))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ix.PutDocument(ctx, scope, string(gen.GenerationID), index.Document{ID: id, Version: "v1", Chunks: res}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.ValidateGeneration(ctx, scope, string(gen.GenerationID), index.Expectation{}); err != nil {
		t.Fatal(err)
	}
	if err := ix.ActivateGeneration(ctx, scope, string(gen.GenerationID)); err != nil {
		t.Fatal(err)
	}
	hitsFor := func(w string) int {
		t.Helper()
		res, err := ix.Search(ctx, scope, index.Query{Text: w + " kiwi", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Hits)
	}
	// Control: a content word is found by itself, so the probe can see a kept word.
	for _, w := range []string{"harbour", "dock", "orchard"} {
		if hitsFor(w) == 0 {
			t.Fatalf("control: the index does not find %q: the probe cannot tell a kept word from a dropped one", w)
		}
	}
	for _, w := range StopWords() {
		if n := hitsFor(w); n != 0 {
			t.Errorf("the query package calls %q a stop word but the index keeps it (%d hits): the two lists have drifted", w, n)
		}
	}
}
