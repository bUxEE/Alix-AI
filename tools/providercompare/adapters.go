package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/adapters/external/anthropic"
	"github.com/buxee/alix-ai/adapters/external/gemini"
	"github.com/buxee/alix-ai/adapters/external/meta"
	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// providerNames is the fixed reporting order.
var providerNames = []string{"openai", "anthropic", "gemini", "meta"}

// placeholderKey is deliberately not shaped like any real key. The stub never checks it.
const placeholderKey = "fixture-credential-value-0001"

// fixedKeys is the key source of the harness: one placeholder, never a real credential.
type fixedKeys struct{}

func (fixedKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(placeholderKey), nil
}

// profileFacts is the part of a provider profile the report uses. The profile files are the
// ones each adapter ships (anthropic, gemini, meta: ProfileJSON) and, for OpenAI, the fixture
// profile of tools/providerconf; their terms, region and compliance members are "unknown".
type profileFacts struct {
	ProviderID       string `json:"provider_id"`
	ModelID          string `json:"model_id"`
	ModelVersion     string `json:"model_version"`
	Streaming        bool   `json:"streaming"`
	StructuredOutput string `json:"structured_output"`
	Modalities       struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	ToolCalling struct {
		Supported       bool `json:"supported"`
		StrictArguments bool `json:"strict_arguments"`
		ParallelCalls   bool `json:"parallel_calls"`
		MaxTools        int  `json:"max_tools"`
	} `json:"tool_calling"`
	RemoteMCP struct {
		Supported bool `json:"supported"`
	} `json:"remote_mcp"`
	NativeFeatures struct {
		Available []string `json:"available"`
	} `json:"native_features"`
	Limits struct {
		MaxContextToks int `json:"max_context_toks"`
		MaxOutputToks  int `json:"max_output_toks"`
	} `json:"limits"`
	ImageGeneration struct {
		Supported bool `json:"supported"`
	} `json:"image_generation"`
	Voice struct {
		STT      bool `json:"stt"`
		TTS      bool `json:"tts"`
		Realtime bool `json:"realtime"`
	} `json:"voice"`
	Terms struct {
		Retention   string `json:"retention"`
		TrainingUse string `json:"training_use"`
	} `json:"terms"`
	Region struct {
		Residency string `json:"residency"`
	} `json:"region"`
	RateLimits struct {
		RequestsPerMin int `json:"requests_per_min"`
		Retry          struct {
			MaxAttempts   int  `json:"max_attempts"`
			HonorsRetryAf bool `json:"honors_retry_after"`
		} `json:"retry"`
	} `json:"rate_limits"`
	Pricing struct {
		Currency    string `json:"currency"`
		EffectiveOn string `json:"effective_on"`
		SourceURL   string `json:"source_url"`
		Prices      struct {
			In struct {
				PerUnits   int64 `json:"per_units"`
				MicroUnits int64 `json:"micro_units"`
			} `json:"toks_in"`
			Out struct {
				PerUnits   int64 `json:"per_units"`
				MicroUnits int64 `json:"micro_units"`
			} `json:"toks_out"`
		} `json:"prices"`
	} `json:"pricing"`
}

// profileJSON returns the shipped profile of a provider.
func profileJSON(provider string) ([]byte, error) {
	switch provider {
	case "openai":
		s, err := fixture("openai", "profile.json")
		return []byte(s), err
	case "anthropic":
		return anthropic.ProfileJSON(), nil
	case "gemini":
		return gemini.ProfileJSON(), nil
	case "meta":
		return meta.ProfileJSON(), nil
	}
	return nil, fmt.Errorf("unknown provider %q", provider)
}

func loadProfile(provider string) (profileFacts, error) {
	raw, err := profileJSON(provider)
	if err != nil {
		return profileFacts{}, err
	}
	var p profileFacts
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("the %s profile does not decode: %w", provider, err)
	}
	if p.ProviderID != provider {
		return p, fmt.Errorf("the %s profile names provider %q", provider, p.ProviderID)
	}
	return p, nil
}

// subject is one real adapter wired to a stub through the egress dialer, as the composition
// root wires it (the destination must be on the allowlist, loopback plain HTTP included).
type subject struct {
	name    string
	profile profileFacts
	gen     providers.MultimodalGenerator
	stub    *stub
	sleeps  *sleepLog
}

// sleepLog records the back-off the adapter asked for instead of waiting for it, so a retry
// check does not spend wall-clock seconds. The injected latency of the stub is real.
type sleepLog struct{ d []time.Duration }

func (s *sleepLog) sleep(_ context.Context, d time.Duration) error { s.d = append(s.d, d); return nil }

// newSubject builds the adapter for provider against a fresh stub with latency lat.
func newSubject(provider string, lat latencyModel) (*subject, func(), error) {
	prof, err := loadProfile(provider)
	if err != nil {
		return nil, nil, err
	}
	st, err := newStub(provider, lat)
	if err != nil {
		return nil, nil, err
	}
	closeFn := st.Close
	dest, err := egress.DestinationFromURL(st.URL)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(provider, al); err != nil {
		closeFn()
		return nil, nil, err
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	sl := &sleepLog{}
	const ref = "cred_01COMPAREREFERENCE" // opaque reference id, not a key
	var gen providers.MultimodalGenerator
	switch provider {
	case "openai":
		c, e := openai.New(openai.Config{
			BaseURL: st.URL, CredentialRef: ref, Model: prof.ModelID,
			RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
		}, reg.Dialer(provider), fixedKeys{}, openai.WithLogger(log), openai.WithSleeper(sl.sleep))
		gen, err = c, e
	case "anthropic":
		c, e := anthropic.New(anthropic.Config{
			BaseURL: st.URL, CredentialRef: ref, Model: prof.ModelID,
			MaxOutputToks: prof.Limits.MaxOutputToks, MaxContextToks: prof.Limits.MaxContextToks,
			RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
		}, reg.Dialer(provider), fixedKeys{}, anthropic.WithLogger(log), anthropic.WithSleeper(sl.sleep))
		gen, err = c, e
	case "gemini":
		c, e := gemini.New(gemini.Config{
			BaseURL: st.URL, CredentialRef: ref, Model: prof.ModelID,
			MaxOutputToks: prof.Limits.MaxOutputToks, MaxContextToks: prof.Limits.MaxContextToks,
			RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
		}, reg.Dialer(provider), fixedKeys{}, gemini.WithLogger(log), gemini.WithSleeper(sl.sleep))
		gen, err = c, e
	case "meta":
		c, e := meta.New(meta.Config{
			BaseURL: st.URL, CredentialRef: ref, Model: prof.ModelID,
			MaxOutputToks: prof.Limits.MaxOutputToks, MaxContextToks: prof.Limits.MaxContextToks,
			RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
		}, reg.Dialer(provider), fixedKeys{}, meta.WithLogger(log), meta.WithSleeper(sl.sleep))
		gen, err = c, e
	}
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("building the %s adapter: %w", provider, err)
	}
	return &subject{name: provider, profile: prof, gen: gen, stub: st, sleeps: sl}, closeFn, nil
}

// compareScope is the scope every harness call carries.
func compareScope(session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_compare", ApplicationID: "app_compare", TenantID: "ten_compare", WorkspaceID: "wsp_compare",
		PrincipalID: "usr_compare", Roles: []string{"member"}, CapabilityScopes: []string{"cap.read"}, SessionID: session,
	}
}
