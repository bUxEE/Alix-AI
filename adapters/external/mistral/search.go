package mistral

import (
	"context"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SearchWeb is not offered on the Chat Completions client AlixAI uses for Mistral.
// Official web_search lives on Conversations/Agents
// (https://docs.mistral.ai/agents/connectors/websearch); Chat Completions has no
// result references. Keep native_web_search false; see ADR 0050.
func (c *Client) SearchWeb(context.Context, contracts.Scope, external.NativeSearchRequest) (external.NativeSearchResult, error) {
	return external.NativeSearchResult{}, contracts.New(contracts.ErrWebSearchUnavailable,
		"mistral: native web search is not offered (Conversations/Agents only; this client uses Chat Completions)")
}
