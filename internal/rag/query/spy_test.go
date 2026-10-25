package query

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// spyLex wraps a Lexical to count its calls and to let a test replace or hold
// one of them: the way a test proves a cap is a cap on work done, not only on
// what was returned.
type spyLex struct {
	Lexical

	mu       sync.Mutex
	searches []index.Query

	parents  atomic.Int32
	rechecks atomic.Int32

	// searchFn, when set, replaces Search.
	searchFn func(ctx context.Context, scope contracts.Scope, q index.Query) (index.Result, error)
	// recheckFn, when set, replaces Recheck.
	recheckFn func(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Verdict, error)
}

func (s *spyLex) Search(ctx context.Context, scope contracts.Scope, q index.Query) (index.Result, error) {
	s.mu.Lock()
	s.searches = append(s.searches, q)
	fn := s.searchFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, scope, q)
	}
	return s.Lexical.Search(ctx, scope, q)
}

func (s *spyLex) ParentOf(ctx context.Context, scope contracts.Scope, h index.Hit, includeHidden bool) (index.Parent, bool, error) {
	s.parents.Add(1)
	return s.Lexical.ParentOf(ctx, scope, h, includeHidden)
}

func (s *spyLex) Recheck(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Verdict, error) {
	s.rechecks.Add(1)
	if s.recheckFn != nil {
		return s.recheckFn(ctx, scope, refs, includeHidden)
	}
	return s.Lexical.Recheck(ctx, scope, refs, includeHidden)
}

func (s *spyLex) searchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.searches)
}

func (s *spyLex) searchTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.searches))
	for i, q := range s.searches {
		out[i] = q.Text
	}
	return out
}
