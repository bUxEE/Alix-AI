package query

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
)

func words(s string) []string { return strings.Fields(s) }

func TestReduceQuery(t *testing.T) {
	t.Run("short questions are untouched", func(t *testing.T) {
		for _, q := range []string{"warranty", "does the warranty cover water damage", "  spaced   out  "} {
			got := reduceQuery(q, 8)
			if want := strings.Join(words(q), " "); got != want {
				t.Errorf("%q reduced to %q", q, got)
			}
		}
	})

	t.Run("long questions are cut to the cap, longest words first, in the order written", func(t *testing.T) {
		q := "a b c the quick brown fox jumps over the lazy dog while extraordinary bureaucratic paperwork accumulates endlessly"
		got := words(reduceQuery(q, 4))
		want := []string{"extraordinary", "bureaucratic", "paperwork", "accumulates"}
		if len(got) != 4 {
			t.Fatalf("kept %d words %q, cap is 4", len(got), got)
		}
		// All four longest words are kept, and in question order.
		if !reflect.DeepEqual(got, want) {
			t.Errorf("kept %q, want %q", got, want)
		}
	})

	t.Run("stop words lose to content words", func(t *testing.T) {
		got := reduceQuery("what is the best way to reset the pump housing", 3)
		for _, stop := range []string{"what", "is", "the", "to", "way"} {
			for _, w := range words(got) {
				if w == stop {
					t.Errorf("kept the short word %q in %q", stop, got)
				}
			}
		}
		if !strings.Contains(got, "housing") {
			t.Errorf("%q lost a content word", got)
		}
	})

	t.Run("identifiers are kept whole, even past the cap", func(t *testing.T) {
		q := "please find the status of order ABC-123 and part PN-4471-B shipped via warehouse nine before december"
		got := words(reduceQuery(q, 2))
		for _, id := range []string{"ABC-123", "PN-4471-B"} {
			if !hasString(got, id) {
				t.Errorf("identifier %s lost: %q", id, got)
			}
		}
		if len(got) > 2+2 {
			t.Errorf("kept %d words %q: two identifiers and at most two plain words", len(got), got)
		}
	})

	t.Run("a distinct-word cap, not a token cap", func(t *testing.T) {
		got := words(reduceQuery("alpha alpha alpha beta beta gamma", 8))
		if len(got) != 6 {
			t.Errorf("%q: a question under the cap is returned as written", got)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		q := strings.Repeat("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima ", 5)
		first := reduceQuery(q, 5)
		for i := 0; i < 50; i++ {
			if again := reduceQuery(q, 5); again != first {
				t.Fatalf("%q then %q", first, again)
			}
		}
	})

	t.Run("nothing to search for", func(t *testing.T) {
		for _, q := range []string{"", "   ", "?!? ... ---", "\"\" ''"} {
			if got := reduceQuery(q, 8); got != "" {
				t.Errorf("%q reduced to %q, want nothing", q, got)
			}
		}
	})

	t.Run("it is text, never an expression", func(t *testing.T) {
		got := reduceQuery(`foo" OR bar NEAR(a b) col:x *`, 8)
		if strings.ContainsAny(got, `"()*`) && !strings.Contains(got, "col:x") {
			t.Errorf("%q", got)
		}
	})
}

// Pathological questions hit the term cap before the index does any work.
//
// SEC-024
func TestQueryTermsAreCapped(t *testing.T) {
	e := newEnv(t)
	sc := scopeOf("ta")
	e.addSource(sc, "Warranty", docSpec{id: "warranty", md: warrantyMD()})
	spy := &spyLex{Lexical: e.ix}
	d := e.deps()
	d.Lexical = spy
	cfg := DefaultConfig()
	cfg.MaxQueryTerms = 6
	p := e.pipeline(d, cfg)

	var long []string
	for i := 0; i < 300; i++ {
		long = append(long, fmt.Sprintf("term%cword%c", 'a'+rune(i%26), 'a'+rune(i/26)))
	}
	question := strings.Join(long, " ")
	if utf8.RuneCountInString(question) > MaxQuestionChars {
		question = question[:MaxQuestionChars]
	}
	res := mustQuery(t, p, sc, Request{Text: question})
	if len(spy.searches) != 1 {
		t.Fatalf("%d searches", len(spy.searches))
	}
	if n := len(words(spy.searches[0].Text)); n > 6 {
		t.Errorf("the index was handed %d terms, the cap is 6", n)
	}
	if spy.searches[0].Limit != cfg.FTSCandidates {
		t.Errorf("limit %d, want the candidate cap %d", spy.searches[0].Limit, cfg.FTSCandidates)
	}
	if string(res.Package.Query) != question {
		t.Errorf("the package must carry the question as asked, not as searched")
	}

	t.Run("query syntax is inert", func(t *testing.T) {
		for _, q := range []string{
			`warranty" OR tenant_id:tb`,
			`"unbalanced warranty`,
			`warranty NEAR(defects water, 2) *`,
			`text:warranty -defects +water ^boost`,
			`warranty AND OR NOT`,
			"warranty\u0000", // refused before it reaches anything
		} {
			res, err := p.Query(context.Background(), sc, Request{Text: q})
			if strings.Contains(q, "\x00") {
				if err == nil {
					t.Errorf("a NUL in the question was accepted")
				}
				continue
			}
			if err != nil {
				t.Errorf("%q: %v", q, err)
				continue
			}
			for _, it := range res.Package.Items {
				if string(it.SourceID) == "" {
					t.Errorf("%q: empty item", q)
				}
			}
		}
	})

	t.Run("a question within the character limit fits the index's byte limit", func(t *testing.T) {
		// CJK text has no spaces: 2000 characters are one 6000-byte word, under
		// the term cap and over index.MaxQueryBytes.
		spy.mu.Lock()
		spy.searches = nil
		spy.mu.Unlock()
		q := strings.Repeat("保修", MaxQuestionChars/2)
		if _, err := p.Query(context.Background(), sc, Request{Text: q}); err != nil {
			t.Fatalf("a %d-character question was refused: %v", utf8.RuneCountInString(q), err)
		}
		if spy.searchCount() == 0 {
			t.Fatal("the index was not searched")
		}
		for _, s := range spy.searches {
			if len(s.Text) > index.MaxQueryBytes || !utf8.ValidString(s.Text) {
				t.Errorf("the index was handed %d bytes (valid UTF-8 %v), its limit is %d", len(s.Text), utf8.ValidString(s.Text), index.MaxQueryBytes)
			}
		}
	})

	t.Run("a question with nothing to search for does not touch the index", func(t *testing.T) {
		spy.mu.Lock()
		spy.searches = nil
		spy.mu.Unlock()
		res := mustQuery(t, p, sc, Request{Text: "?!? ..."})
		if spy.searchCount() != 0 || res.Diagnostics.FTSCandidates != 0 {
			t.Errorf("%d searches for a question of punctuation", spy.searchCount())
		}
	})
}

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults are invalid: %v", err)
	}
	d := DefaultConfig()
	if d.RRFK != 60 {
		t.Errorf("default RRF k is %d, want 60", d.RRFK)
	}
	if d.Rerank || d.Rewrite {
		t.Errorf("the Lite defaults must have the reranker and the rewrite off")
	}
	if FromConfig() != DefaultConfig() {
		t.Errorf("the registered section does not start from the defaults")
	}

	bad := map[string]func(*Config){
		"rrf_k 0":                  func(c *Config) { c.RRFK = 0 },
		"max_query_terms 0":        func(c *Config) { c.MaxQueryTerms = 0 },
		"max_query_terms over 16":  func(c *Config) { c.MaxQueryTerms = index.MaxQueryTerms + 1 },
		"fts_candidates 0":         func(c *Config) { c.FTSCandidates = 0 },
		"fts_candidates over max":  func(c *Config) { c.FTSCandidates = index.DefaultMaxLimit + 1 },
		"vector_candidates over":   func(c *Config) { c.VectorCandidates = vector.MaxK + 1 },
		"vector_min_score 2":       func(c *Config) { c.VectorMinScore = 2 },
		"shortlist over ceiling":   func(c *Config) { c.Shortlist = MaxShortlistCeiling + 1 },
		"max_items over schema":    func(c *Config) { c.MaxItems = MaxItemsCeiling + 1; c.Shortlist = MaxShortlistCeiling },
		"max_items over shortlist": func(c *Config) { c.MaxItems = c.Shortlist + 1 },
		"max_toks 0":               func(c *Config) { c.MaxToks = 0 },
		"lookups over ceiling":     func(c *Config) { c.MaxParentLookups = MaxParentLookupsCeiling + 1 },
		"negative lookups":         func(c *Config) { c.MaxParentLookups = -1 },
		"rerank_candidates 1":      func(c *Config) { c.RerankCandidates = 1 },
		"rerank over shortlist":    func(c *Config) { c.RerankCandidates = c.Shortlist + 1 },
		"max_rewrites 0":           func(c *Config) { c.MaxRewrites = 0 },
		"rewrites over ceiling":    func(c *Config) { c.MaxRewrites = MaxRewritesCeiling + 1 },
		"timeout 0":                func(c *Config) { c.FTSTimeout = 0 },
		"timeout over a minute":    func(c *Config) { c.TotalTimeout = 2 * time.Minute },
	}
	for name, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted; nothing is clamped, a bad value is an error", name)
		}
	}
}
