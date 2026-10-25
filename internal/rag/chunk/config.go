package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const (
	// ComponentName is the name the chunker records in an index generation
	// (rag.ComponentVersion.Name).
	ComponentName = "chunker"

	// AlgorithmVersion is the version of the chunking algorithm and of the ID
	// scheme. Bump it when a change alters the chunks or IDs produced for the
	// same input and config; a changed component creates a new generation.
	AlgorithmVersion = "1.0.0"

	// MaxChunkChars is the longest chunk text, in code points. It is the
	// maxLength of EvidenceItem.text in schemas/rag: a parent-expanded
	// passage must fit an evidence item whatever the token bounds say.
	MaxChunkChars = 32768

	// MaxDocumentBytes bounds the extracted text accepted in one call, so a
	// hostile or runaway parse cannot make the chunker allocate without limit.
	MaxDocumentBytes = 64 << 20

	// maxParentToks keeps ParentMaxToks inside MaxChunkChars: the estimate is
	// at least one token per three bytes, so a text of at most N estimated
	// tokens has at most 3N code points, and 3*10000 is under 32768.
	maxParentToks = 10000

	minChildMaxToks = 16

	// maxTitleRunes is the maxLength of a locator section.
	maxTitleRunes = 256
)

// Config holds the size bounds. Sizes are estimated tokens (EstimateTokens),
// not characters.
type Config struct {
	// ChildMinToks: a paragraph smaller than this is merged with the next
	// paragraph of the same section, page and visibility while the result
	// stays within ChildMaxToks. Table rows and code blocks are never merged.
	// 0 disables merging.
	ChildMinToks int `json:"child_min_toks" yaml:"child_min_toks"`
	// ChildMaxToks: no child is larger. A longer paragraph (or table row) is
	// split at a sentence or line boundary, else at whitespace, else between
	// code points.
	ChildMaxToks int `json:"child_max_toks" yaml:"child_max_toks"`
	// ParentMaxToks: no parent is larger (a parent is whole children, the
	// heading line included when it fits). At least ChildMaxToks, at most
	// 10000 so a parent always fits an evidence item.
	ParentMaxToks int `json:"parent_max_toks" yaml:"parent_max_toks"`
}

// DefaultConfig is the Lite-profile default. The estimate errs high, so a child
// of 256 estimated tokens is roughly 150 to 200 real ones: small enough for
// precise retrieval, large enough to hold a paragraph.
func DefaultConfig() Config {
	return Config{ChildMinToks: 48, ChildMaxToks: 256, ParentMaxToks: 1024}
}

// Validate refuses bounds the chunker cannot honour. It fails closed with
// contracts.ErrValidationFailed; there is no silent clamping.
func (c Config) Validate() error {
	bad := func(format string, args ...any) error {
		return contracts.Newf(contracts.ErrValidationFailed, "chunk config: "+format, args...)
	}
	switch {
	case c.ChildMinToks < 0:
		return bad("child_min_toks %d is negative", c.ChildMinToks)
	case c.ChildMaxToks < minChildMaxToks:
		return bad("child_max_toks %d is below the minimum of %d", c.ChildMaxToks, minChildMaxToks)
	case c.ChildMinToks > c.ChildMaxToks:
		return bad("child_min_toks %d exceeds child_max_toks %d", c.ChildMinToks, c.ChildMaxToks)
	case c.ParentMaxToks < c.ChildMaxToks:
		return bad("parent_max_toks %d is below child_max_toks %d", c.ParentMaxToks, c.ChildMaxToks)
	case c.ParentMaxToks > maxParentToks:
		return bad("parent_max_toks %d exceeds the maximum of %d", c.ParentMaxToks, maxParentToks)
	}
	return nil
}

// Fingerprint is a short, stable digest of the bounds. It is part of the
// chunker version.
func (c Config) Fingerprint() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("chunker-config/v1 min=%d max=%d pmax=%d",
		c.ChildMinToks, c.ChildMaxToks, c.ParentMaxToks)))
	return hex.EncodeToString(sum[:4])
}

// Component is the chunker identity and version recorded with an index
// generation (12.3). The version is the algorithm version plus the config
// fingerprint, so a change of bounds is a change of component.
func (c Config) Component() rag.ComponentVersion {
	return rag.ComponentVersion{
		Name:    ComponentName,
		Version: actions.OpaqueVersion(AlgorithmVersion + "+" + c.Fingerprint()),
	}
}
