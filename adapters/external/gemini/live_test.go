//go:build live

package gemini

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// liveKeys serves the key from an environment variable. It exists for this
// human-gated test only (real key, real money, task X-H02): the adapter itself
// has no such source, and the test is skipped when the variable is unset.
type liveKeys struct{ key string }

func (l liveKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(l.key), nil
}

// TestLiveGenerate makes one small real call. Run with
//
//	ALIXAI_LIVE_GEMINI_KEY=... go test -tags live -run TestLive ./adapters/external/gemini/
//
// ALIXAI_LIVE_GEMINI_MODEL overrides the model (default gemini-3.5-flash-lite).
// The OAuth path needs a browser consent and is exercised by X-H02 by hand.
func TestLiveGenerate(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_GEMINI_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_GEMINI_KEY is not set")
	}
	model := os.Getenv("ALIXAI_LIVE_GEMINI_MODEL")
	if model == "" {
		model = "gemini-3.5-flash-lite"
	}
	dest, err := egress.DestinationFromURL(DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	c, err := New(Config{CredentialRef: "cred_live", Model: model}, reg.Dialer(ProviderName), liveKeys{key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := c.CheckModel(ctx, testScope()); err != nil {
		t.Fatalf("model check: %v", err)
	}
	req := userReq("Reply with the single word: ready")
	req.MaxToks = 400
	res, err := c.Generate(ctx, testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text() == "" || res.Usage.ToksIn == 0 || res.Usage.ToksOut == 0 {
		t.Fatalf("result %+v", res)
	}
	t.Logf("tok_in=%d tok_out=%d cost_micros=%d model=%s", res.Usage.ToksIn, res.Usage.ToksOut, res.Usage.CostMicros, res.Model)
}
