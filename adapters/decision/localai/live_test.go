//go:build live

package localai

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Live tests run only with `-tags live` and the environment below; without
// ALIXAI_LIVE_DECISION_LOCALAI_URL and ALIXAI_LIVE_DECISION_LOCALAI_MODEL they skip.
//
//	ALIXAI_LIVE_DECISION_LOCALAI_URL        loopback or https endpoint of a LocalAI
//	ALIXAI_LIVE_DECISION_LOCALAI_MODEL      a model that declares known_usecases: [decisions]
//	ALIXAI_LIVE_DECISION_LOCALAI_REVISION   optional revision pin
//	ALIXAI_LIVE_DECISION_LOCALAI_KEY_ENV    optional name of the variable holding the API key
func liveProvider(t *testing.T) *Provider {
	t.Helper()
	url, model := os.Getenv("ALIXAI_LIVE_DECISION_LOCALAI_URL"), os.Getenv("ALIXAI_LIVE_DECISION_LOCALAI_MODEL")
	if url == "" || model == "" {
		t.Skip("ALIXAI_LIVE_DECISION_LOCALAI_URL and ALIXAI_LIVE_DECISION_LOCALAI_MODEL are not set")
	}
	cfg := Config{Endpoint: url, Model: model, ModelRevision: os.Getenv("ALIXAI_LIVE_DECISION_LOCALAI_REVISION")}
	if env := os.Getenv("ALIXAI_LIVE_DECISION_LOCALAI_KEY_ENV"); env != "" {
		cfg.APIKeyRef = "env:" + env
	}
	return newProvider(t, cfg)
}

func TestLiveHealthAndProbe(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	// Whether this runtime hosts the Decisions API is exactly what is being found out:
	// an unsupported runtime is a result to record, not a failure of the adapter.
	if err := p.Probe(ctx); errors.Is(err, ErrUnsupportedRuntime) {
		t.Skipf("this runtime does not support the Decisions API: %v", err)
	} else if err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestLiveScoreDecision(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := p.ScoreDecision(ctx, testScope(), testRequest(3))
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if res.Disposition == gen.DecisionScoreResultDispositionFallback && res.FallbackReason != nil && *res.FallbackReason == gen.FallbackReasonUnsupportedRuntime {
		t.Skip("this runtime does not support the Decisions API")
	}
	t.Logf("disposition=%s selections=%d", res.Disposition, len(res.Selections))
}
