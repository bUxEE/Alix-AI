package dictation_test

import (
	"testing"

	"github.com/buxee/alix-ai/internal/voice/dictation"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestConfigAudioClass(t *testing.T) {
	if got := (dictation.Config{}).EffectiveAudioClass(); got != dictation.AudioClassInternal {
		t.Fatalf("default = %q", got)
	}
	for _, c := range []string{"public", "internal", "confidential"} {
		cfg := dictation.Config{AudioClass: c}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if cfg.EffectiveAudioClass() != c {
			t.Fatalf("EffectiveAudioClass(%s) = %q", c, cfg.EffectiveAudioClass())
		}
	}
	if err := (dictation.Config{AudioClass: "restricted"}).Validate(); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("restricted: %v", err)
	}
}
