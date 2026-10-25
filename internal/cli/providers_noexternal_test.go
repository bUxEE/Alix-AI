//go:build noexternal

package cli

import "testing"

func TestExternalProviderStubBuildsNothing(t *testing.T) {
	if ExternalProvidersBuilt() {
		t.Fatal("the noexternal build reports adapters")
	}
	p, err := NewExternalProvider(ExternalProviderSpec{Provider: ExternalProviderOpenAI})
	if err == nil || p != nil {
		t.Fatalf("%v %v", p, err)
	}
}
