package external

// NativeSearchRequest is a minimal provider-native web search call: only a
// vetted query (plus a fixed instruction). No datasets, conversation, or turn
// system prompt.
type NativeSearchRequest struct {
	Query        string
	AllowDomains []string // bare domains for provider filters when supported
	MaxUses      int      // Anthropic max_uses; 0 → 1
}

// NativeSearchHit is one citation/result from a provider search tool.
type NativeSearchHit struct {
	URL     string
	Title   string
	Snippet string
}

// NativeSearchResult is the parsed outcome of one SearchWeb call.
type NativeSearchResult struct {
	Hits        []NativeSearchHit
	SearchCalls int // provider-reported search tool invocations
	ToksIn      int
	ToksOut     int
}
