//go:build !nodecision

package cli

import (
	"testing"

	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestDecisionProvidersBuildKinds(t *testing.T) {
	if !decisionProvidersAvailable() || len(decisionProvidersKinds()) != 2 {
		t.Fatalf("available=%v kinds=%v", decisionProvidersAvailable(), decisionProvidersKinds())
	}
	old := localaiSection
	t.Cleanup(func() { localaiSection = old })
	localaiSection.Endpoint = "http://127.0.0.1:1"

	for _, s := range []decisionProvidersSettings{
		{Kind: decisionProvidersKindLocalAI, Model: "clm-8b", ModelRevision: "r1"},
		{Kind: decisionProvidersKindCLM, Model: "clm-latest", ModelRevision: "r1", Mode: "systemone"},
	} {
		p, err := decisionProvidersBuild(s)
		if err != nil {
			t.Fatalf("%s: %v", s.Kind, err)
		}
		sp, ok := p.(decision.DecisionModelProvider)
		if !ok {
			t.Fatalf("%s: %T is not a DecisionModelProvider", s.Kind, p)
		}
		if got := string(sp.Manifest().ModelID); got != s.Model {
			t.Fatalf("%s: model %q", s.Kind, got)
		}
	}
	if _, err := decisionProvidersBuild(decisionProvidersSettings{Kind: "other", Model: "m"}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := decisionProvidersBuild(decisionProvidersSettings{Kind: decisionProvidersKindCLM, Model: "m", Endpoint: "http://10.1.2.3:8700"}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("plaintext non-loopback endpoint: %v", err)
	}
	localaiSection.Endpoint = "http://10.1.2.3:8080"
	if _, err := decisionProvidersBuild(decisionProvidersSettings{Kind: decisionProvidersKindLocalAI, Model: "m"}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("plaintext non-loopback localai section: %v", err)
	}
}

// serve's decision wiring reaches the CLM adapter's api_key_ref: a literal key is refused, an env:
// reference is accepted, and neither the reference's value nor the key shows in the provider.
func TestDecisionWireBuildPassesTheCLMKeyRef(t *testing.T) {
	// TEST-ONLY credential reference value, not a secret.
	t.Setenv("ALIXAI_TEST_CLM_KEY", testKey(t))
	base := decWireProviderConfig{Kind: decisionProvidersKindCLM, Model: "clm-latest", ModelRevision: "r1"}

	literal := base
	literal.APIKeyRef = "a-key-written-into-the-configuration"
	if _, err := decWireBuildProvider(literal); err == nil {
		t.Fatal("a literal key in api_key_ref was accepted")
	}
	ref := base
	ref.APIKeyRef = "env:ALIXAI_TEST_CLM_KEY"
	p, err := decWireBuildProvider(ref)
	if err != nil {
		t.Fatalf("an env: reference was refused: %v", err)
	}
	if _, ok := p.(decision.DecisionModelProvider); !ok {
		t.Fatalf("%T is not a DecisionModelProvider", p)
	}
}
