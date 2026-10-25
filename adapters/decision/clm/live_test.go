//go:build live

package clm

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Live tests run only with `-tags live` and the environment below; without
// ALIXAI_LIVE_DECISION_CLM_URL and ALIXAI_LIVE_DECISION_CLM_MODEL they skip.
//
//	ALIXAI_LIVE_DECISION_CLM_URL        loopback or https endpoint of the CLM service (default port 8700)
//	ALIXAI_LIVE_DECISION_CLM_MODEL      the model name /v1/models lists
//	ALIXAI_LIVE_DECISION_CLM_REVISION   optional revision pin the service must report
//	ALIXAI_LIVE_DECISION_CLM_MODE       optional "rank" or "systemone"
func liveProvider(t *testing.T) *Provider {
	t.Helper()
	url, model := os.Getenv("ALIXAI_LIVE_DECISION_CLM_URL"), os.Getenv("ALIXAI_LIVE_DECISION_CLM_MODEL")
	if url == "" || model == "" {
		t.Skip("ALIXAI_LIVE_DECISION_CLM_URL and ALIXAI_LIVE_DECISION_CLM_MODEL are not set")
	}
	return newProvider(t, Config{Endpoint: url, Model: model, ModelRevision: os.Getenv("ALIXAI_LIVE_DECISION_CLM_REVISION"), Mode: os.Getenv("ALIXAI_LIVE_DECISION_CLM_MODE")})
}

func TestLiveReadyAndProbe(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := p.Probe(ctx); errors.Is(err, ErrUnsupportedRuntime) {
		t.Skipf("this service does not provide the scoring route: %v", err)
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
	t.Logf("disposition=%s selections=%d", res.Disposition, len(res.Selections))
	_ = gen.DecisionScoreResultDispositionAccepted
}
