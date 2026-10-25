package settings

import "github.com/buxee/alix-ai/internal/audit"

// DocumentWritten is the data of the settings.document_written audit event: one
// settings document was created or replaced. It says what changed (which level,
// which revision, which top-level sections) and carries the digest of the new
// canonical document, never a value: the event is evidence that a change happened
// and who made it (the event's principal and session are the caller's), and the
// document itself is what the settings API returns.
type DocumentWritten struct {
	// Level is user, tenant or deployment.
	Level string `json:"level"`
	// Version is the new revision and PreviousVersion the one it replaced (0 for
	// the first write).
	Version         int64 `json:"version"`
	PreviousVersion int64 `json:"previous_version"`
	// Schema is the urn of the schema the document was validated against.
	Schema string `json:"schema"`
	// SHA256 is the lower-case hex digest of the new canonical document.
	SHA256 string `json:"sha256"`
	// ChangedSections names the top-level members whose value changed.
	ChangedSections []string `json:"changed_sections"`
}

// EventDocumentWritten is the registered audit event type of a settings change
// (the typed handle: the payload is a DocumentWritten, so a call site can not
// misspell the type or pass another event's data). It is appended in the same
// store transaction as the write, so a change that can not be recorded does not
// happen, and it may be appended to a tenant chain (user and tenant settings) or
// to the application chain (deployment settings).
//
// It is distinct from the core audit.EventSettingsChanged, which records one
// setting of another component; this one records a whole settings document of
// this module.
var EventDocumentWritten = audit.MustRegister[DocumentWritten](audit.TypeSpec{
	Name:        "settings.document_written",
	Description: "a settings document (user, tenant or deployment level) was created or replaced; carries the level, revisions, changed sections and the digest of the new document, never a value",
	Levels:      audit.LevelTenant | audit.LevelApplication,
	Required:    []string{"level", "version", "previous_version", "sha256"},
})
