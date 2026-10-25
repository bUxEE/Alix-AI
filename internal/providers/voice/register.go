package voice

import (
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Roles returns the three voice roles in canonical order.
func Roles() []providers.Role {
	return []providers.Role{providers.RoleSTT, providers.RoleTTS, providers.RoleVAD}
}

// Register binds p to a voice role after checking that it implements the
// role's interface. The registry itself accepts any non-nil value for a
// reserved role (it cannot import this package), so voice code registers
// through here to keep a mistyped provider from being discovered at the first
// call. It fails with validation_failed for a non-voice role or a provider that
// does not implement the role, and with whatever providers.Registry.Register
// returns otherwise (conflict when the role is already registered).
func Register(r *providers.Registry, role providers.Role, p any) error {
	var ok bool
	switch role {
	case providers.RoleSTT:
		_, ok = p.(STT)
	case providers.RoleTTS:
		_, ok = p.(TTS)
	case providers.RoleVAD:
		_, ok = p.(VAD)
	default:
		return contracts.Newf(contracts.ErrValidationFailed, "voice: %q is not a voice role", string(role))
	}
	if !ok {
		return contracts.Newf(contracts.ErrValidationFailed, "voice: %T does not implement the %s role", p, role)
	}
	return r.Register(role, p)
}

// LookupSTT returns the provider registered for the STT role. It fails with
// unavailable (carrying the text_input_only effect) when none is registered.
func LookupSTT(r *providers.Registry) (STT, error) {
	return providers.Lookup[STT](r, providers.RoleSTT)
}

// LookupTTS returns the provider registered for the TTS role. It fails with
// unavailable (carrying the verified_text_only effect) when none is registered.
func LookupTTS(r *providers.Registry) (TTS, error) {
	return providers.Lookup[TTS](r, providers.RoleTTS)
}

// LookupVAD returns the provider registered for the VAD role. It fails with
// unavailable (carrying the manual_turn_taking effect) when none is registered.
func LookupVAD(r *providers.Registry) (VAD, error) {
	return providers.Lookup[VAD](r, providers.RoleVAD)
}
