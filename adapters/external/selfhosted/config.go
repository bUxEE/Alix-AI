package selfhosted

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/adapters/external/chatcompat"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const ProviderName = "self_hosted"

// openLoopbackKeyRef is used when no credential_ref is configured (open mock).
const openLoopbackKeyRef = "cred_OPEN_LOOPBACK_NOAUTH00000"

type Config struct {
	BaseURL       string
	CredentialRef string
	Model         string
	ReplyModel    string

	RequestTimeout   time.Duration
	StreamTimeout    time.Duration
	MaxResponseBytes int64
	Retry            external.RetryPolicy
}

func (c Config) compat() chatcompat.Config {
	ref := c.CredentialRef
	if ref == "" {
		ref = openLoopbackKeyRef
	}
	return chatcompat.Config{
		BaseURL: refURL(c.BaseURL), CredentialRef: ref, WireModel: c.Model, ReplyModel: c.ReplyModel,
		RequestTimeout: c.RequestTimeout, StreamTimeout: c.StreamTimeout, MaxResponseBytes: c.MaxResponseBytes, Retry: c.Retry,
		// Shared self-hosted endpoints isolate vLLM prefix cache per tenant (hardening.md).
		CacheSaltFromScope: tenantCacheSalt,
	}
}

// tenantCacheSalt is a per-tenant salt for vLLM cache_salt (not a credential; separates KV blocks).
func tenantCacheSalt(scope contracts.Scope) string {
	if scope.TenantID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("alixai/self_hosted/cache_salt|" + scope.DeploymentID + "|" + scope.TenantID))
	return hex.EncodeToString(sum[:])
}

func refURL(u string) string { return u }

func (c Config) String() string {
	return fmt.Sprintf("selfhosted.Config{BaseURL:%s Model:%s}", c.BaseURL, c.Model)
}

func (c Config) Validate() error {
	if c.BaseURL == "" {
		return fmt.Errorf("a base URL is required")
	}
	cc := c.compat()
	if c.CredentialRef != "" {
		return cc.Validate()
	}
	if cc.BaseURL == "" || cc.WireModel == "" {
		return fmt.Errorf("a base URL and model are required")
	}
	return nil
}
