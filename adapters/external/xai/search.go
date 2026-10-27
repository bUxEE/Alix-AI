package xai

import (
	"context"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SearchWeb is not offered on the Chat Completions client AlixAI uses for xAI.
// Official web_search lives on the Responses API (https://docs.x.ai/developers/tools/web-search);
// until that route is allowlisted here, native search stays unavailable.
func (c *Client) SearchWeb(context.Context, contracts.Scope, external.NativeSearchRequest) (external.NativeSearchResult, error) {
	return external.NativeSearchResult{}, contracts.New(contracts.ErrUnavailable,
		"xai: native web search requires the Responses API; this client uses Chat Completions only")
}
