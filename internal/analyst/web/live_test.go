//go:build live

package web

import (
	"context"
	"os"
	"testing"
)

// Optional live adapter smoke tests. Set ALIXAI_LIVE_BRAVE_KEY / ALIXAI_LIVE_TAVILY_KEY
// and build with -tags live. CI does not run these.

func TestLiveBrave(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_BRAVE_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_BRAVE_KEY not set")
	}
	b := &Brave{Secrets: staticSecret(key), CredRef: "cred_live"}
	hits, err := b.Search(context.Background(), "site:gov.it company incentive")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}
}

func TestLiveTavily(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_TAVILY_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_TAVILY_KEY not set")
	}
	tv := &Tavily{Secrets: staticSecret(key), CredRef: "cred_live"}
	hits, err := tv.Search(context.Background(), "Italian company incentive")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}
}
