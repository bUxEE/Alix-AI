package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/adapters/external/anthropic"
	"github.com/buxee/alix-ai/internal/egress"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// providerAnthropic is the id of the Anthropic subject (the adapter of X-19).
const providerAnthropic = anthropic.ProviderName

// anthropicFixtureProfile is the profile the Anthropic fixture run is made for: the starting profile the adapter ships
// (anthropic.ProfileJSON). As for OpenAI, its terms are recorded by a human (X-H01) and a report is for the exact
// profile, model id and model version, so a report for the operator's real profile is made with --profile <file>.
func anthropicFixtureProfile() (pv.ProviderProfile, error) {
	var p pv.ProviderProfile
	if err := json.Unmarshal(anthropic.ProfileJSON(), &p); err != nil {
		return p, fmt.Errorf("the shipped Anthropic profile does not decode: %w", err)
	}
	return p, nil
}

// fixtureProfileFor is the embedded profile of a provider the tool knows.
func fixtureProfileFor(provider string) (pv.ProviderProfile, error) {
	switch provider {
	case providerOpenAI:
		return openaiFixtureProfile()
	case providerAnthropic:
		return anthropicFixtureProfile()
	}
	return pv.ProviderProfile{}, fmt.Errorf("unknown provider %q (known: %s, %s)", provider, providerOpenAI, providerAnthropic)
}

// newAnthropicClient builds the production adapter against base (a bare http origin on loopback in fixture mode),
// dialling through the egress allowlist exactly as the composition root does.
func newAnthropicClient(base string, profile pv.ProviderProfile, keys anthropic.KeySource, logs *lockedBuffer) (*anthropic.Client, error) {
	dest, err := egress.DestinationFromURL(base)
	if err != nil {
		return nil, err
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		return nil, err
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(anthropic.ProviderName, al); err != nil {
		return nil, err
	}
	cfg := anthropic.Config{
		BaseURL: base, CredentialRef: "cred_01CONFORMANCEREFERENCE", Model: string(profile.ModelID), // TEST-ONLY opaque reference id, not a key
		MaxOutputToks: int(profile.Limits.MaxOutputToks), MaxContextToks: int(profile.Limits.MaxContextToks),
		RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
	}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return anthropic.New(cfg, reg.Dialer(anthropic.ProviderName), keys,
		anthropic.WithLogger(log),
		anthropic.WithSleeper(func(context.Context, time.Duration) error { return nil }))
}

// anthropicFixtureSubject returns a subject over the recorded fixtures: the real adapter, a real loopback HTTP server, a
// placeholder credential. Live mode is OpenAI only until the Anthropic live run (X-H02) is wired.
func anthropicFixtureSubject(profile pv.ProviderProfile) (suite.Subject, func(), error) {
	up, err := newAnthropicUpstream()
	if err != nil {
		return suite.Subject{}, nil, err
	}
	ring := suite.NewKeyRing()
	logs := &lockedBuffer{}
	c, err := newAnthropicClient(up.URL, profile, ringKeys{ring}, logs)
	if err != nil {
		up.Close()
		return suite.Subject{}, nil, err
	}
	return suite.Subject{
		Name: "anthropic (fixtures)", Generator: c, Caps: c, Runtime: c, Profile: profile, Upstream: up, Keys: ring,
		Logs: logs.String,
	}, up.Close, nil
}
