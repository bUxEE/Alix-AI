package xai

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestSearchWebNotOffered(t *testing.T) {
	c := &Client{}
	_, err := c.SearchWeb(context.Background(), contracts.Scope{}, external.NativeSearchRequest{Query: "x"})
	if contracts.CodeOf(err) != contracts.ErrWebSearchUnavailable {
		t.Fatalf("got %v, want web_search_unavailable", err)
	}
}
