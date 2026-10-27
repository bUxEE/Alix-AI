package gemini

import (
	"context"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SearchWeb is not offered for Gemini. Grounding chunks use Google redirector URIs
// (vertexaisearch.cloud.google.com/…) that fail the host allowlist (or would make
// AlixAI fetch through Google's redirector), and Grounding with Google Search terms
// require displaying Search Suggestions (searchEntryPoint) to the end user — which
// an embedded analyst search step does not do. Keep native_web_search false; see ADR 0050.
func (c *Client) SearchWeb(context.Context, contracts.Scope, external.NativeSearchRequest) (external.NativeSearchResult, error) {
	return external.NativeSearchResult{}, contracts.New(contracts.ErrWebSearchUnavailable,
		"gemini: native web search is not offered (grounding redirect URIs and searchEntryPoint display terms)")
}
