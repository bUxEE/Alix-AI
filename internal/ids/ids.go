// Package ids generates AlixAI identifiers: a lowercase type prefix, an
// underscore and a ULID, e.g. ses_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D.
//
// ULIDs sort by creation time. Within one millisecond the generator increments
// the random component, so identifiers from one Generator are strictly
// increasing even when many are created in the same instant.
package ids

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/buxee/alix-ai/internal/clock"
)

const maxPrefixLen = 32

// prefixRE: lowercase words joined by single underscores, e.g. "ses", "audit_evt".
var prefixRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

// ValidPrefix reports whether p is a legal ID prefix.
func ValidPrefix(p string) bool {
	return len(p) <= maxPrefixLen && prefixRE.MatchString(p)
}

// Generator produces prefixed ULIDs from an injected clock. It is safe for
// concurrent use.
type Generator struct {
	clk clock.Clock

	mu      sync.Mutex
	entropy io.Reader
}

// NewGenerator returns a Generator that stamps IDs with clk and draws entropy
// from crypto/rand. A nil clk means clock.Real.
func NewGenerator(clk clock.Clock) *Generator {
	return newGenerator(clk, rand.Reader)
}

func newGenerator(clk clock.Clock, entropy io.Reader) *Generator {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Generator{clk: clk, entropy: ulid.Monotonic(entropy, 0)}
}

// New returns "<prefix>_<ULID>". The prefix is a programmer-chosen constant
// (see ValidPrefix); an invalid one panics rather than minting a malformed ID.
func (g *Generator) New(prefix string) string {
	if !ValidPrefix(prefix) {
		panic(fmt.Sprintf("ids: invalid prefix %q", prefix))
	}
	g.mu.Lock()
	id, err := ulid.New(ulid.Timestamp(g.clk.Now()), g.entropy)
	g.mu.Unlock()
	if err != nil {
		// Entropy exhaustion within one millisecond (2^80 IDs) or a failing
		// system RNG: neither can be recovered from by returning a weaker ID.
		panic(fmt.Sprintf("ids: cannot generate ULID: %v", err))
	}
	return prefix + "_" + id.String()
}

var defaultGen = NewGenerator(clock.Real{})

// New returns "<prefix>_<ULID>" using the process-wide generator and the real
// clock. Code that has an injected clock should use its own Generator.
func New(prefix string) string { return defaultGen.New(prefix) }

// ErrInvalidID is returned by Parse for anything that is not a well-formed
// "<prefix>_<ULID>" identifier.
var ErrInvalidID = errors.New("ids: invalid identifier")

// Parse splits an ID into its prefix and ULID, validating both. It is strict:
// the ULID must be the canonical 26-character uppercase form.
func Parse(id string) (prefix string, u ulid.ULID, err error) {
	i := strings.LastIndexByte(id, '_')
	if i < 0 || !ValidPrefix(id[:i]) {
		return "", ulid.ULID{}, fmt.Errorf("%w: bad prefix", ErrInvalidID)
	}
	body := id[i+1:]
	u, perr := ulid.ParseStrict(body)
	if perr != nil || u.String() != body {
		return "", ulid.ULID{}, fmt.Errorf("%w: bad ULID", ErrInvalidID)
	}
	return id[:i], u, nil
}

// Valid reports whether id is a well-formed ID carrying the given prefix.
func Valid(id, prefix string) bool {
	p, _, err := Parse(id)
	return err == nil && p == prefix
}
