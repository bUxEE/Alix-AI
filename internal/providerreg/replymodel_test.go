package providerreg

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"

	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestReplyModelMatchesIsExactAndFailsClosed(t *testing.T) {
	cases := []struct {
		id, version, reported string
		want                  bool
	}{
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini", true},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini-2025-04-14", true},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini-20250414", true},
		{"claude-sonnet-4-5", "20250929", "claude-sonnet-4-5-20250929", true},
		{"gemini-2.5-flash", "001", "gemini-2.5-flash-001", true},
		// another model, a prefix, a longer id, another snapshot, another case, stray characters
		{"gpt-4.1-mini", "2025-04-14", "gpt-5-pro", false},
		{"gpt-4.1", "2025-04-14", "gpt-4.1-mini", false},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1", false},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-min", false},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini-2025-06-01", false},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini-evil", false},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini-", false},
		{"gpt-4.1-mini", "2025-04-14", "GPT-4.1-MINI", false},
		{"gpt-4.1-mini", "2025-04-14", " gpt-4.1-mini", false},
		{"gpt-4.1-mini", "2025-04-14", "gpt-4.1-mini\n", false},
		{"gpt-4.1-mini", "2025-04-14", "openai/gpt-4.1-mini", false},
		// the snapshot suffix is the PINNED version, never a free one
		{"gpt-4.1-mini", "", "gpt-4.1-mini-2025-04-14", false},
		{"gpt-4.1-mini", "", "gpt-4.1-mini", true},
		// nothing reported is never a match
		{"gpt-4.1-mini", "2025-04-14", "", false},
		{"", "", "", false},
		{"", "v1", "-v1", false},
	}
	for _, c := range cases {
		if got := ReplyModelMatches(providers.ModelID(c.id), providers.ModelVersion(c.version), c.reported); got != c.want {
			t.Errorf("ReplyModelMatches(%q, %q, %q) = %v, want %v", c.id, c.version, c.reported, got, c.want)
		}
	}
}

func TestObserveQuarantinesAReplyOfAnotherModel(t *testing.T) {
	reported := func(s string) Observation { return Observation{ReplyModel: &s} }
	t.Run("a reply of another model quarantines a qualified profile as model_version_changed", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		got := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, reported(string(p.ModelID)+"-other")))
		if got.Lifecycle != providers.ProfileLifecycleQuarantined || got.QuarantineReason == nil || *got.QuarantineReason != providers.ProviderProfileQuarantineReasonModelVersionChanged {
			t.Fatalf("lifecycle %s reason %v", got.Lifecycle, got.QuarantineReason)
		}
		calls, err := g.route(g.scope, p.ProfileID, req())
		wantReason(t, err, contracts.ErrQuarantined, Reason(providers.ProviderProfileQuarantineReasonModelVersionChanged))
		if calls != 0 {
			t.Fatal("a quarantined profile was used")
		}
	})
	t.Run("the pinned model, with or without its snapshot, changes nothing", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		for _, m := range []string{string(p.ModelID), string(p.ModelID) + "-" + string(p.ModelVersion)} {
			if got := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, reported(m))); got.Lifecycle != providers.ProfileLifecycleQualified {
				t.Fatalf("%q: lifecycle %s", m, got.Lifecycle)
			}
		}
	})
}
