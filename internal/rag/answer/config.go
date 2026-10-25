package answer

import (
	"fmt"
	"math"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// SectionName is the name of the configuration section.
const SectionName = "rag_answer"

// Hard ceilings, taken from the schemas an answer is written into
// (schemas/rag/grounded-answer, schemas/conversation/citation).
const (
	// MaxClaimsCeiling is the schema's maximum for GroundedAnswer.claims.
	MaxClaimsCeiling = 64
	// MaxCitationsCeiling is the schema's maximum for GroundedAnswer.citations.
	MaxCitationsCeiling = 32
	// MaxClaimRunes is the longest claim: a Citation lists the claims it
	// supports with a 512 character limit, and every claim of an answer must
	// appear in the citations that name it.
	MaxClaimRunes = 512
	// MaxCitationClaims is the schema's maximum for Citation.claims.
	MaxCitationClaims = 32
	// MaxClaimCitations is the schema's maximum for a claim's citation_indexes.
	MaxClaimCitations = 8
	// maxEvidencePerClaim bounds the handles the model may cite per claim.
	maxEvidencePerClaim = 4
	// maxOutputBytes bounds the model output the core is willing to parse.
	maxOutputBytes = 256 << 10
	// maxRefsCeiling and the duration caps bound the resolver's memory and the
	// life of a reference.
	maxRefsCeiling = 1_000_000
	maxRefTTL      = 24 * time.Hour
	maxGenerate    = 5 * time.Minute
)

// Config is the "rag_answer" configuration section: the caps of one grounded
// answer and the life of the citation references it hands out. Every cap is
// positive and nothing is clamped: start from DefaultConfig (the config loader
// does) and Validate before use.
type Config struct {
	// MaxClaims caps the claims of an answer, and the claims the model may
	// write (the output constraint carries it). Claims beyond it are dropped.
	MaxClaims int `yaml:"max_claims"`
	// MaxCitations caps the citations of an answer. A claim that would need one
	// more is dropped.
	MaxCitations int `yaml:"max_citations"`
	// MaxOutputToks caps the tokens the model may generate for the claims.
	MaxOutputToks int `yaml:"max_output_toks"`
	// MinCoverage is the share of a claim's content terms that one cited
	// passage window must contain for the claim to count as supported. Numbers,
	// dates, times and identifiers are not part of the share: every one of them
	// must be present, exactly.
	MinCoverage float64 `yaml:"min_coverage"`
	// WindowSentences is the number of consecutive sentences of a passage that
	// together may support one claim. 1 demands a single sentence.
	WindowSentences int `yaml:"window_sentences"`
	// MinClaimTerms is the fewest content terms (words, numbers, identifiers) a
	// claim needs to be checked at all: a claim of one word is not a statement.
	MinClaimTerms int `yaml:"min_claim_terms"`
	// GenerateTimeout bounds the model call.
	GenerateTimeout time.Duration `yaml:"generate_timeout"`

	// RefTTL is how long a citation reference resolves after it is issued.
	RefTTL time.Duration `yaml:"ref_ttl"`
	// MaxRefs caps the references the resolver remembers; the oldest go first.
	MaxRefs int `yaml:"max_refs"`
	// PreviewRunes caps the passage a reference previews. 0 disables previews:
	// the reference then only resolves to the source.
	PreviewRunes int `yaml:"preview_runes"`
}

// Defaults: the Lite profile's. A short, strictly bounded answer, a reference that
// outlives a reading session but not a working day.
const (
	DefaultMaxClaims       = 12
	DefaultMaxCitations    = 16
	DefaultMaxOutputToks   = 768
	DefaultMinCoverage     = 0.85
	DefaultWindowSentences = 2
	DefaultMinClaimTerms   = 2
	DefaultGenerateTimeout = 30 * time.Second
	DefaultRefTTL          = 15 * time.Minute
	DefaultMaxRefs         = 4096
	DefaultPreviewRunes    = 600
)

// DefaultConfig returns the Lite-profile defaults.
func DefaultConfig() Config {
	return Config{
		MaxClaims:       DefaultMaxClaims,
		MaxCitations:    DefaultMaxCitations,
		MaxOutputToks:   DefaultMaxOutputToks,
		MinCoverage:     DefaultMinCoverage,
		WindowSentences: DefaultWindowSentences,
		MinClaimTerms:   DefaultMinClaimTerms,
		GenerateTimeout: DefaultGenerateTimeout,
		RefTTL:          DefaultRefTTL,
		MaxRefs:         DefaultMaxRefs,
		PreviewRunes:    DefaultPreviewRunes,
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
	dur := func(name string, v, lo, hi time.Duration) error {
		if v < lo || v > hi {
			return fmt.Errorf("%s %s out of range (%s-%s)", name, v, lo, hi)
		}
		return nil
	}
	if math.IsNaN(c.MinCoverage) || c.MinCoverage < 0.5 || c.MinCoverage > 1 {
		return fmt.Errorf("min_coverage %v out of range (0.5-1)", c.MinCoverage)
	}
	for _, err := range []error{
		in("max_claims", c.MaxClaims, 1, MaxClaimsCeiling),
		in("max_citations", c.MaxCitations, 1, MaxCitationsCeiling),
		in("max_output_toks", c.MaxOutputToks, 16, 8192),
		in("window_sentences", c.WindowSentences, 1, 8),
		in("min_claim_terms", c.MinClaimTerms, 1, 8),
		dur("generate_timeout", c.GenerateTimeout, time.Second, maxGenerate),
		dur("ref_ttl", c.RefTTL, time.Second, maxRefTTL),
		in("max_refs", c.MaxRefs, 16, maxRefsCeiling),
		in("preview_runes", c.PreviewRunes, 0, 4000),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

// section is the registered "rag_answer" config section. Owners read it through
// this pointer (see config.Register).
var section = DefaultConfig()

func init() { config.Register(SectionName, &section) }

// FromConfig returns the process configuration as loaded.
func FromConfig() Config { return section }
