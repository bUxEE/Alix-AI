//go:build live

package moonshot

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

func TestLiveGenerate(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_MOONSHOT_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_MOONSHOT_KEY is not set")
	}
	model := os.Getenv("ALIXAI_LIVE_MOONSHOT_MODEL")
	if model == "" {
		model = "moonshot-v1-8k"
	}
	dest, err := egress.DestinationFromURL(DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	c, err := New(Config{CredentialRef: "cred_live", Model: model}, reg.Dialer(ProviderName), liveKeys{key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	scope := contracts.Scope{DeploymentID: "d", ApplicationID: "a", TenantID: "t", WorkspaceID: "w", PrincipalID: "p", SessionID: "s"}
	if err := c.CheckModel(ctx, scope); err != nil {
		t.Fatalf("model check: %v", err)
	}
	req := providers.RichRequest{Messages: []providers.RichMessage{{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("Reply ready")}}}}
	req.MaxToks = 400
	if _, err := c.Generate(ctx, scope, req); err != nil {
		t.Fatal(err)
	}
}
