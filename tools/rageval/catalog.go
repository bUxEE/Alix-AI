package main

import (
	"context"
	"sync"

	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// corpusCatalog is the deterministic authority on sources for a run: it
// answers, for a scope, which sources its principal may read and in what state.
// It stands in for the source registry and the policy gateway, which the
// composition owns; what it implements is the rule they apply, and the rule is
// the one the corpus labels assume:
//
//   - a source of another tenant or workspace is not the caller's, so it is not
//     returned (and so, in the pipeline's words, treated exactly like one that
//     was never granted);
//   - a source with readers is returned only for those principals;
//   - a deleted source is not returned;
//   - an unavailable source is returned as such, and a stale one as stale.
//
// It never says more than the corpus declares: nothing is granted because it
// was not denied.
type corpusCatalog struct {
	mu    sync.RWMutex
	byID  map[string]catalogEntry
	calls int
}

type catalogEntry struct {
	key       string
	name      string
	tenantID  string
	workspace string
	readers   []string
	state     string
	version   string
}

func newCorpusCatalog() *corpusCatalog { return &corpusCatalog{byID: map[string]catalogEntry{}} }

// register adds a source under its src_ id.
func (c *corpusCatalog) register(id string, e catalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[id] = e
}

// Calls is the number of Sources calls served.
func (c *corpusCatalog) Calls() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.calls
}

// Sources implements query.SourceCatalog.
func (c *corpusCatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]query.SourceState, len(sourceIDs))
	for _, id := range sourceIDs {
		e, ok := c.byID[id]
		if !ok || e.tenantID != scope.TenantID || e.workspace != scope.WorkspaceID || e.state == StateDeleted {
			continue
		}
		if len(e.readers) > 0 && !contains(e.readers, scope.PrincipalID) {
			continue
		}
		st := query.SourceState{
			DisplayName:  e.name,
			Authorised:   true,
			Availability: conversation.CitationAvailabilityAvailable,
			Staleness:    conversation.CitationStalenessCurrent,
		}
		switch e.state {
		case StateUnavailable:
			st.Availability = conversation.CitationAvailabilityUnavailable
		case StateStale:
			st.Staleness = conversation.CitationStalenessStale
		}
		out[id] = st
	}
	return out, nil
}

// openCatalog is the catalog of the synthetic scale corpus: every source is
// the caller's own tenant's, readable, available and current. The scale run
// measures retrieval cost, not authorisation rules.
type openCatalog struct{}

func (openCatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	out := make(map[string]query.SourceState, len(sourceIDs))
	for _, id := range sourceIDs {
		out[id] = query.SourceState{
			Authorised:   true,
			Availability: conversation.CitationAvailabilityAvailable,
			Staleness:    conversation.CitationStalenessCurrent,
		}
	}
	return out, nil
}
