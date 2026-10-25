package main

import (
	"fmt"
	"sort"

	"github.com/buxee/alix-ai/internal/rag/query"
)

// RAG caps by profile: the evidence package and the candidate lists of
// docs/operations/resource-budgets.md 6.3. The pipeline's own defaults are those
// of a roomy profile (8 items, 3,000 estimated tokens); a Lite service carries
// four items and a thousand tokens, and a result measured with the defaults is
// not a result for Lite's package. --rag-caps says which one a run used.
const (
	CapsDefault  = "default"
	CapsLite     = "lite"
	CapsBalanced = "balanced"
	CapsMinimal  = "experimental-4g"
)

// ragCap is one profile's row of the table.
type ragCap struct {
	maxItems, maxToks int
	candidates        int // per retrieval leg, FTS and vector
	parentExpansion   bool
	parentLookups     int
	parentToks        int
}

var ragCapTable = map[string]ragCap{
	CapsMinimal:  {maxItems: 3, maxToks: 480, candidates: 12},
	CapsLite:     {maxItems: 4, maxToks: 1000, candidates: 24, parentExpansion: true, parentLookups: 1, parentToks: 300},
	CapsBalanced: {maxItems: 6, maxToks: 2000, candidates: 48, parentExpansion: true, parentLookups: 2, parentToks: 500},
}

func ragCapNames() []string {
	names := []string{CapsDefault}
	for n := range ragCapTable {
		names = append(names, n)
	}
	sort.Strings(names[1:])
	return names
}

// applyRAGCaps sets the caps of a profile on a query configuration and leaves
// everything else (the floor, the reranker, the time budgets) as it is.
func applyRAGCaps(c *query.Config, name string) error {
	if name == "" || name == CapsDefault {
		return nil
	}
	rc, ok := ragCapTable[name]
	if !ok {
		return fmt.Errorf("--rag-caps %q is not one of %v", name, ragCapNames())
	}
	c.MaxItems, c.MaxToks = rc.maxItems, rc.maxToks
	c.FTSCandidates, c.VectorCandidates = rc.candidates, rc.candidates
	c.ParentExpansion = rc.parentExpansion
	if rc.parentExpansion {
		c.MaxParentLookups, c.MaxParentToks = rc.parentLookups, rc.parentToks
	}
	return nil
}

// capsName is the name a result records: the empty flag value is the default.
func capsName(s string) string {
	if s == "" {
		return CapsDefault
	}
	return s
}

// capKs limits the cut-offs of recall@k to what the package can hold: the
// cut-offs at or under maxItems, and maxItems itself.
func capKs(ks []int, maxItems int) []int {
	var out []int
	for _, k := range ks {
		if k < maxItems {
			out = append(out, k)
		}
	}
	return append(out, maxItems)
}
