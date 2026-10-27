package settings

import "github.com/buxee/alix-ai/internal/audit"

// TenantSettingsChanged is the audit payload for a tenant analyst settings write.
// It holds ids and digests — never ability secret material (there is none) and never
// host lists as free text beyond counts.
type TenantSettingsChanged struct {
	TenantID  string `json:"tenant_id"`
	UpdatedBy string `json:"updated_by"`
	SHA256    string `json:"sha256"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

var eventTenantWritten = audit.MustRegister[TenantSettingsChanged](audit.TypeSpec{
	Name:        "analyst.tenant_settings_written",
	Description: "tenant analyst ability ceilings were created or replaced (ADR 0047)",
	Levels:      audit.LevelTenant,
	Required:    []string{"tenant_id", "updated_by", "sha256"},
})

// EventTenantWritten is the registered event type name for tests.
const EventTenantWritten = audit.EventType("analyst.tenant_settings_written")
