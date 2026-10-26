package profile_test

import (
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestPackDictationOKWithoutVAD(t *testing.T) {
	sttOnly := profile.Pack{ID: "en-stt", Language: "en", Roles: []providers.Role{providers.RoleSTT}}
	if !sttOnly.DictationOK() {
		t.Fatal("STT-only pack should be ok for dictation")
	}
	if err := sttOnly.ConversationOK(); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("ConversationOK = %v, want unavailable", err)
	}
}

func TestPackConversationRequiresVAD(t *testing.T) {
	full := profile.Pack{ID: "en", Language: "en", Roles: []providers.Role{providers.RoleSTT, providers.RoleTTS, providers.RoleVAD}}
	if err := full.ConversationOK(); err != nil {
		t.Fatal(err)
	}
	if !full.DictationOK() {
		t.Fatal("full pack should also serve dictation")
	}
}
