package answer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func newResolverEnv(t *testing.T, cfg Config) (*Resolver, *fakeCatalog, *clock.Fake) {
	t.Helper()
	cat := newCatalog()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	r, err := NewResolver(cat, clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r, cat, clk
}

func TestResolverIssueNeedsAScopeAndASource(t *testing.T) {
	r, _, _ := newResolverEnv(t, DefaultConfig())
	good := RefSpec{SourceID: "src_x", Version: "v1"}
	if _, _, err := r.Issue(contracts.Scope{}, good); !contracts.HasCode(err, contracts.ErrScopeMissing) {
		t.Errorf("no scope: err = %v", err)
	}
	for name, spec := range map[string]RefSpec{
		"no source":  {Version: "v1"},
		"no version": {SourceID: "src_x"},
	} {
		if _, _, err := r.Issue(ta, spec); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	ref, exp, err := r.Issue(ta, good)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := ref.Value.(conversation.CitationOpenRefFile)
	if !ok || f.Kind != "file" || !ids.Valid(string(f.FileID), "fil") {
		t.Errorf("ref = %#v: want a fil_ ULID file reference", ref.Value)
	}
	if want := time.Date(2026, 10, 3, 9, 15, 0, 0, time.UTC); !exp.Equal(want) {
		t.Errorf("expires %s, want %s", exp, want)
	}
}

// Two references to the same source are distinct and unguessable from one another, and
// the preview is bounded.
func TestResolverReferencesAreDistinctAndBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PreviewRunes = 20
	r, cat, _ := newResolverEnv(t, cfg)
	cat.mu.Lock()
	cat.states[key(ta.TenantID, "src_x")] = queryState{Authorised: true, Availability: conversation.CitationAvailabilityAvailable}
	cat.mu.Unlock()
	seen := map[actions.FileID]bool{}
	for i := 0; i < 200; i++ {
		ref, _, err := r.Issue(ta, RefSpec{SourceID: "src_x", Version: "v1", Preview: "0123456789 0123456789 0123456789"})
		if err != nil {
			t.Fatal(err)
		}
		id := ref.Value.(conversation.CitationOpenRefFile).FileID
		if seen[id] {
			t.Fatalf("reference %s issued twice", id)
		}
		seen[id] = true
	}
	for id := range seen {
		got, err := r.Resolve(context.Background(), ta, id)
		if err != nil {
			t.Fatal(err)
		}
		if len([]rune(got.Preview)) > 23 { // the bound and the ellipsis
			t.Fatalf("preview of %d runes: %q", len([]rune(got.Preview)), got.Preview)
		}
		if got.DisplayName != "src_x" {
			t.Fatalf("display name %q: a source without a name falls back to its ID", got.DisplayName)
		}
		break
	}
	// With previews off the passage is not even kept.
	cfg.PreviewRunes = 0
	r2, cat2, _ := newResolverEnv(t, cfg)
	cat2.mu.Lock()
	cat2.states[key(ta.TenantID, "src_x")] = queryState{Authorised: true, Availability: conversation.CitationAvailabilityAvailable}
	cat2.mu.Unlock()
	ref, _, _ := r2.Issue(ta, RefSpec{SourceID: "src_x", Version: "v1", Preview: "secret passage"})
	got, err := r2.Resolve(context.Background(), ta, ref.Value.(conversation.CitationOpenRefFile).FileID)
	if err != nil || got.Preview != "" {
		t.Errorf("preview with previews off: %q, %v", got.Preview, err)
	}
}

// The resolver is safe for concurrent use: issuing, resolving and evicting at once
// neither loses a live reference nor exceeds the bound.
func TestResolverConcurrentUse(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRefs = 64
	r, cat, _ := newResolverEnv(t, cfg)
	cat.mu.Lock()
	cat.states[key(ta.TenantID, "src_x")] = queryState{Authorised: true, Availability: conversation.CitationAvailabilityAvailable}
	cat.mu.Unlock()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ref, _, err := r.Issue(ta, RefSpec{SourceID: "src_x", Version: "v1"})
				if err != nil {
					t.Error(err)
					return
				}
				// The newest reference of this goroutine may already be evicted by the
				// others; what it may never be is wrong.
				_, err = r.Resolve(context.Background(), ta, ref.Value.(conversation.CitationOpenRefFile).FileID)
				if err != nil && !contracts.HasCode(err, contracts.ErrPermissionDenied) {
					t.Errorf("Resolve: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if r.Len() > cfg.MaxRefs {
		t.Errorf("%d references remembered, bound is %d", r.Len(), cfg.MaxRefs)
	}
}

// The catalog's own open reference is what a reference resolves to.
func TestResolverOpensWithTheCatalogsReference(t *testing.T) {
	r, cat, _ := newResolverEnv(t, DefaultConfig())
	rec := conversation.CitationOpenRef{Value: conversation.CitationOpenRefRecord{Kind: "record", EntityType: "policy", EntityID: "p-7"}}
	cat.mu.Lock()
	cat.states[key(ta.TenantID, "src_x")] = queryState{DisplayName: "Policy 7", Authorised: true, Availability: conversation.CitationAvailabilityAvailable, OpenRef: &rec}
	cat.mu.Unlock()
	ref, _, err := r.Issue(ta, RefSpec{SourceID: "src_x", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(context.Background(), ta, ref.Value.(conversation.CitationOpenRefFile).FileID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Policy 7" {
		t.Errorf("display name = %q", got.DisplayName)
	}
	if o, ok := got.Open.Value.(conversation.CitationOpenRefRecord); !ok || o.EntityID != "p-7" {
		t.Errorf("open = %#v", got.Open.Value)
	}
	// A citation's own reference stays opaque and is not the host's record reference.
	if _, ok := ref.Value.(conversation.CitationOpenRefRecord); ok {
		t.Error("the citation's reference exposes the host's record")
	}
}
