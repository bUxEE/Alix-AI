package web

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

type stubNative struct {
	err error
	n   int
}

func (s *stubNative) SearchWeb(context.Context, contracts.Scope, string, []string) (NativeCallResult, error) {
	s.n++
	return NativeCallResult{}, s.err
}

type stubFallback struct {
	n int
}

func (s *stubFallback) Name() string { return "searxng" }

func (s *stubFallback) Search(context.Context, string) ([]SearchHit, error) {
	s.n++
	return []SearchHit{{URL: "https://gov.it/x", Title: "t", Snippet: "s"}}, nil
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "wsp",
		PrincipalID: "usr", Roles: []string{"user"}, SessionID: "ses",
	}
}

func TestChainSearcherHardErrorNoFallback(t *testing.T) {
	native := &stubNative{err: contracts.New(contracts.ErrPermissionDenied, "egress denied")}
	fb := &stubFallback{}
	turn := NewTurn(HostPolicy{Allow: []string{"gov.it"}}, Budget{})
	ch := &ChainSearcher{
		Native: native, NativeOK: func(context.Context, contracts.Scope) bool { return true },
		Fallback: fb, FallbackOK: func(context.Context, contracts.Scope) bool { return true },
	}
	_, err := ch.Search(context.Background(), testScope(), turn, "public query", nil, false)
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("got %v", err)
	}
	if fb.n != 0 {
		t.Fatalf("fallback must not run on egress denial; n=%d", fb.n)
	}
}

func TestChainSearcherUnavailableFallsBack(t *testing.T) {
	native := &stubNative{err: contracts.New(contracts.ErrWebSearchUnavailable, "no native")}
	fb := &stubFallback{}
	turn := NewTurn(HostPolicy{Allow: []string{"gov.it"}}, Budget{})
	ch := &ChainSearcher{
		Native: native, NativeOK: func(context.Context, contracts.Scope) bool { return true },
		Fallback: fb, FallbackOK: func(context.Context, contracts.Scope) bool { return true },
	}
	res, err := ch.Search(context.Background(), testScope(), turn, "public query", nil, false)
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if fb.n != 1 {
		t.Fatalf("fallback n=%d", fb.n)
	}
}
