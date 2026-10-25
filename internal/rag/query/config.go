package query

import (
	"fmt"
	"math"
	"time"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
)

// SectionName is the name of the configuration section.
const SectionName = "rag_query"

// Hard ceilings. They bound what any configuration (or request) can ask of one
// query: master plan 17.2, "capped RAG candidates and parent expansion".
const (
	// MaxItemsCeiling is the schema's maximum for EvidencePackage.items.
	MaxItemsCeiling = 64
	// MaxToksCeiling is the schema's maximum for the package token budget.
	MaxToksCeiling = 10_000_000
	// MaxShortlistCeiling bounds the fused shortlist, and so the work of
	// hydration, parent expansion, reranking and the final recheck.
	MaxShortlistCeiling = 100
	// MaxRewritesCeiling bounds the alternative queries one search may run.
	MaxRewritesCeiling = 4
	// MaxRerankCeiling bounds the documents sent to the reranker.
	MaxRerankCeiling = 50
	// MaxParentLookupsCeiling bounds the parent sections one query may read.
	MaxParentLookupsCeiling = 16
	// maxStageTimeout bounds every duration in the section.
	maxStageTimeout = time.Minute
)

// Config is the "rag_query" configuration section: the candidate, term, item,
// token, parent and time caps of one query. Every cap is positive; there is no
// "unlimited" and no silent default for a zero value: start from DefaultConfig
// (the config loader does) and Validate before use.
//
// The defaults are the Lite profile's: reranking off, query rewriting off,
// small candidate lists, a package that fits a short, strictly bounded prompt.
// docs/efficiency/M2-09.md has what each costs, measured.
type Config struct {
	// RRFK is the constant k of Reciprocal Rank Fusion, score = sum of
	// 1 / (k + rank). 60 is the value of the original paper.
	RRFK int `yaml:"rrf_k"`

	// MaxQueryTerms caps the plain terms of the question that reach the
	// full-text stage (identifiers are always kept, whole). BM25 scores every
	// row that matches any term, so this is the main lever on the cost of a
	// query inside a large scope. The index itself keeps at most 16.
	MaxQueryTerms int `yaml:"max_query_terms"`
	// FTSCandidates and VectorCandidates cap what each retrieval stage hands to
	// fusion.
	FTSCandidates    int `yaml:"fts_candidates"`
	VectorCandidates int `yaml:"vector_candidates"`
	// VectorMinScore drops vector hits whose cosine similarity is below it. A flat
	// scan always returns its top K however poor the match, so on a small
	// corpus the tail is noise; 0 drops anti-correlated chunks only. It is a
	// floor on a raw model score, so it is model-specific: calibrate it before
	// raising it (M2-14).
	VectorMinScore float64 `yaml:"vector_min_score"`
	// Shortlist is how many fused candidates go on to hydration, parent
	// expansion, reranking and the final recheck.
	Shortlist int `yaml:"shortlist"`

	// MaxItems and MaxToks bound the evidence package: at most MaxItems items
	// whose estimated model tokens add up to at most MaxToks. A request may ask
	// for less, never for more.
	MaxItems int `yaml:"max_items"`
	MaxToks  int `yaml:"max_toks"`

	// ParentExpansion turns on expansion of a hit to its section.
	// MaxParentLookups caps how many sections one query reads (a lookup that
	// finds nothing still counts), and MaxParentToks is the largest section,
	// in estimated tokens, that may replace a child; a larger one keeps the
	// child.
	ParentExpansion  bool `yaml:"parent_expansion"`
	MaxParentLookups int  `yaml:"max_parent_lookups"`
	MaxParentToks    int  `yaml:"max_parent_toks"`

	// Rerank turns on the optional reranker. It is off in the Lite profile
	// (master plan 7.1) even when a Reranker role is registered; RerankCandidates
	// is how many of the fused shortlist it sees.
	Rerank           bool `yaml:"rerank"`
	RerankCandidates int  `yaml:"rerank_candidates"`

	// Rewrite turns on query rewriting for low-confidence searches: a search
	// whose first full-text pass finds fewer than RewriteBelowHits candidates may ask the
	// Rewriter for up to MaxRewrites alternative queries, each run as one more
	// full-text pass, all inside RewriteTimeout and the total time budget.
	Rewrite          bool `yaml:"rewrite"`
	RewriteBelowHits int  `yaml:"rewrite_below_hits"`
	MaxRewrites      int  `yaml:"max_rewrites"`

	// Time budgets. Each stage runs under its own deadline, never longer than
	// what is left of TotalTimeout. A stage that outlives it is cut: the
	// optional stages (vectors, rerank, rewrite, parent expansion) degrade and
	// say so; the full-text stage fails the query with a typed timeout, because
	// an empty package means "nothing authorised matched" and a timed-out
	// search has not established that.
	TotalTimeout   time.Duration `yaml:"total_timeout"`
	FTSTimeout     time.Duration `yaml:"fts_timeout"`
	EmbedTimeout   time.Duration `yaml:"embed_timeout"`
	VectorTimeout  time.Duration `yaml:"vector_timeout"`
	RerankTimeout  time.Duration `yaml:"rerank_timeout"`
	RewriteTimeout time.Duration `yaml:"rewrite_timeout"`
}

// Defaults.
const (
	DefaultRRFK             = 60
	DefaultMaxQueryTerms    = 8
	DefaultFTSCandidates    = 40
	DefaultVectorCandidates = 40
	DefaultShortlist        = 24
	DefaultMaxItems         = 8
	DefaultMaxToks          = 3000
	DefaultMaxParentLookups = 3
	DefaultMaxParentToks    = 768
	DefaultRerankCandidates = 12
	DefaultRewriteBelowHits = 3
	DefaultMaxRewrites      = 2

	DefaultTotalTimeout   = 5 * time.Second
	DefaultFTSTimeout     = 1500 * time.Millisecond
	DefaultEmbedTimeout   = 1500 * time.Millisecond
	DefaultVectorTimeout  = 1000 * time.Millisecond
	DefaultRerankTimeout  = 2 * time.Second
	DefaultRewriteTimeout = 1 * time.Second
)

// DefaultConfig returns the Lite-profile defaults: expansion on, reranking and
// rewriting off.
func DefaultConfig() Config {
	return Config{
		RRFK:             DefaultRRFK,
		MaxQueryTerms:    DefaultMaxQueryTerms,
		FTSCandidates:    DefaultFTSCandidates,
		VectorCandidates: DefaultVectorCandidates,
		Shortlist:        DefaultShortlist,
		MaxItems:         DefaultMaxItems,
		MaxToks:          DefaultMaxToks,
		ParentExpansion:  true,
		MaxParentLookups: DefaultMaxParentLookups,
		MaxParentToks:    DefaultMaxParentToks,
		Rerank:           false,
		RerankCandidates: DefaultRerankCandidates,
		Rewrite:          false,
		RewriteBelowHits: DefaultRewriteBelowHits,
		MaxRewrites:      DefaultMaxRewrites,
		TotalTimeout:     DefaultTotalTimeout,
		FTSTimeout:       DefaultFTSTimeout,
		EmbedTimeout:     DefaultEmbedTimeout,
		VectorTimeout:    DefaultVectorTimeout,
		RerankTimeout:    DefaultRerankTimeout,
		RewriteTimeout:   DefaultRewriteTimeout,
	}
}

// Validate implements the config section validation hook. Every number must be
// inside its range; nothing is clamped.
func (c Config) Validate() error {
	in := func(name string, v, lo, hi int) error {
		if v < lo || v > hi {
			return fmt.Errorf("%s %d out of range (%d-%d)", name, v, lo, hi)
		}
		return nil
	}
	scoreIn := func(name string, v float64) error {
		if math.IsNaN(v) || v < -1 || v > 1 {
			return fmt.Errorf("%s %v out of range (-1 to 1)", name, v)
		}
		return nil
	}
	dur := func(name string, v time.Duration) error {
		if v < time.Millisecond || v > maxStageTimeout {
			return fmt.Errorf("%s %s out of range (1ms-%s)", name, v, maxStageTimeout)
		}
		return nil
	}
	for _, err := range []error{
		in("rrf_k", c.RRFK, 1, 10_000),
		in("max_query_terms", c.MaxQueryTerms, 1, index.MaxQueryTerms),
		in("fts_candidates", c.FTSCandidates, 1, index.DefaultMaxLimit),
		in("vector_candidates", c.VectorCandidates, 1, vector.MaxK),
		scoreIn("vector_min_score", c.VectorMinScore),
		in("shortlist", c.Shortlist, 1, MaxShortlistCeiling),
		in("max_items", c.MaxItems, 1, MaxItemsCeiling),
		in("max_toks", c.MaxToks, 1, MaxToksCeiling),
		in("max_parent_lookups", c.MaxParentLookups, 0, MaxParentLookupsCeiling),
		in("max_parent_toks", c.MaxParentToks, 1, 10_000),
		in("rerank_candidates", c.RerankCandidates, 2, MaxRerankCeiling),
		in("rewrite_below_hits", c.RewriteBelowHits, 1, MaxShortlistCeiling),
		in("max_rewrites", c.MaxRewrites, 1, MaxRewritesCeiling),
		dur("total_timeout", c.TotalTimeout),
		dur("fts_timeout", c.FTSTimeout),
		dur("embed_timeout", c.EmbedTimeout),
		dur("vector_timeout", c.VectorTimeout),
		dur("rerank_timeout", c.RerankTimeout),
		dur("rewrite_timeout", c.RewriteTimeout),
	} {
		if err != nil {
			return err
		}
	}
	if c.MaxItems > c.Shortlist {
		return fmt.Errorf("max_items %d exceeds shortlist %d: the package could never fill", c.MaxItems, c.Shortlist)
	}
	if c.RerankCandidates > c.Shortlist {
		return fmt.Errorf("rerank_candidates %d exceeds shortlist %d", c.RerankCandidates, c.Shortlist)
	}
	return nil
}

// section is the registered "rag_query" config section. Owners read it through
// this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register(SectionName, &section) }

// FromConfig returns the process configuration as loaded.
func FromConfig() Config { return section }
