package mistral

import (
	"encoding/json"
	"testing"

	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestShippedProfileDecodes(t *testing.T) {
	var p pv.ProviderProfile
	if err := json.Unmarshal(ProfileJSON(), &p); err != nil {
		t.Fatal(err)
	}
	if p.ProviderID != ProviderName {
		t.Fatalf("%+v", p)
	}
	if p.Region.Residency != pv.ResidencyPinned {
		t.Fatalf("want pinned EU residency, got %+v", p.Region)
	}
}
