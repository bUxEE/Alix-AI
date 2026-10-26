//go:build live

package openrouter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

type liveKeys struct{ key string }

func (l liveKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(l.key), nil
}

func liveScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_live", ApplicationID: "app_live", TenantID: "ten_live", WorkspaceID: "wsp_live",
		PrincipalID: "usr_live", SessionID: "ses_live",
	}
}

func TestLiveGenerate(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_OPENROUTER_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_OPENROUTER_KEY is not set")
	}
	model := os.Getenv("ALIXAI_LIVE_OPENROUTER_MODEL")
	if model == "" {
		model = "openai/gpt-4o-mini"
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
	if err := c.CheckModel(ctx, liveScope()); err != nil {
		t.Fatalf("model check: %v", err)
	}
	req := providers.RichRequest{Messages: []providers.RichMessage{{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("Reply with the single word: ready")}}}}
	req.MaxToks = 400
	res, err := c.Generate(ctx, liveScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text() == "" || res.Usage.ToksIn == 0 {
		t.Fatalf("result %+v", res)
	}
}
