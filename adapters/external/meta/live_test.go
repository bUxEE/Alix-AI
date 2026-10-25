//go:build live

package meta

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
//	ALIXAI_LIVE_META_KEY=... go test -tags live -run TestLive ./adapters/external/meta/
//
// Without further variables it targets the Meta Model API (https://api.meta.ai, model muse-spark-1.3).
// ALIXAI_LIVE_META_DIALECT=openai-compatible with ALIXAI_LIVE_META_BASE_URL (the host origin, required),
// ALIXAI_LIVE_META_API_PREFIX and ALIXAI_LIVE_META_MODEL (the host's Llama model id) target a hosted-Llama endpoint.
func TestLiveGenerate(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_META_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_META_KEY is not set")
	}
	model := os.Getenv("ALIXAI_LIVE_META_MODEL")
	if model == "" {
		model = "muse-spark-1.3"
	}
	dialect := Dialect(os.Getenv("ALIXAI_LIVE_META_DIALECT"))
	base := os.Getenv("ALIXAI_LIVE_META_BASE_URL")
	if base == "" {
		if dialect.resolved() != DialectModelAPI {
			t.Skip("ALIXAI_LIVE_META_BASE_URL is not set (a hosted-Llama endpoint has no default host)")
		}
		base = DefaultBaseURL
	}
	dest, err := egress.DestinationFromURL(base)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	c, err := New(Config{Dialect: dialect, BaseURL: base, APIPrefix: os.Getenv("ALIXAI_LIVE_META_API_PREFIX"), CredentialRef: "cred_live", Model: model}, reg.Dialer(ProviderName), liveKeys{key}) // TEST-ONLY opaque reference id, not a key
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
