package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// providerOpenAI is the only provider the tool knows today. A second adapter
// adds a case in subjectFor and its own upstream and fixtures.
const providerOpenAI = "openai"

// openaiFixtureProfile is the profile the fixture run is made for. Its terms,
// region and compliance members are "unknown" and its terms hash is a
// placeholder: terms are recorded by a human (task X-H01) and a report is for
// the exact profile, model id and model version, so a report for the operator's
// real profile is made with --profile <file>.
func openaiFixtureProfile() (pv.ProviderProfile, error) {
	var p pv.ProviderProfile
	if err := json.Unmarshal([]byte(fixture("profile.json")), &p); err != nil {
		return p, fmt.Errorf("the embedded OpenAI profile does not decode: %w", err)
	}
	return p, nil
}

// ringKeys adapts the suite's key ring to the adapter's key source.
type ringKeys struct{ ring *suite.KeyRing }

func (k ringKeys) Key(_ context.Context, scope contracts.Scope, _ string) (credentials.Secret, error) {
	v, err := k.ring.Resolve(scope)
	if err != nil {
		return "", err
	}
	return credentials.Secret(v), nil
}

// lockedBuffer is a log sink the adapter and the suite can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newOpenAIClient builds the production adapter against base (a bare http
// origin on loopback in fixture mode), dialling through the egress allowlist
// exactly as the composition root does.
func newOpenAIClient(base string, profile pv.ProviderProfile, keys openai.KeySource, logs *lockedBuffer) (*openai.Client, error) {
	dest, err := egress.DestinationFromURL(base)
	if err != nil {
		return nil, err
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		return nil, err
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(openai.ProviderName, al); err != nil {
		return nil, err
	}
	cfg := openai.Config{
		BaseURL: base, CredentialRef: "cred_01CONFORMANCEREFERENCE", Model: string(profile.ModelID), // TEST-ONLY opaque reference id, not a key
		RequestTimeout: 20 * time.Second, StreamTimeout: 20 * time.Second,
	}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return openai.New(cfg, reg.Dialer(openai.ProviderName), keys,
		openai.WithLogger(log),
		openai.WithSleeper(func(context.Context, time.Duration) error { return nil }))
}

// openaiFixtureSubject returns a subject over the recorded fixtures: the real
// adapter, a real loopback HTTP server, a placeholder credential.
func openaiFixtureSubject(profile pv.ProviderProfile) (suite.Subject, func(), error) {
	up, err := newOpenAIUpstream()
	if err != nil {
		return suite.Subject{}, nil, err
	}
	ring := suite.NewKeyRing()
	logs := &lockedBuffer{}
	c, err := newOpenAIClient(up.URL, profile, ringKeys{ring}, logs)
	if err != nil {
		up.Close()
		return suite.Subject{}, nil, err
	}
	return suite.Subject{
		Name: "openai (fixtures)", Generator: c, Caps: c, Runtime: c, Profile: profile, Upstream: up, Keys: ring,
		Logs: logs.String,
	}, up.Close, nil
}
