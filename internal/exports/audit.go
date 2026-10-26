package exports

import "github.com/buxee/alix-ai/internal/audit"

// Audit event for a successful export. Payload holds ids, format and size — never block content.
var eventCreated = audit.MustRegister[map[string]any](audit.TypeSpec{
	Name: "exports.created", Description: "a downloadable file was generated from verified blocks", Levels: audit.LevelTenant,
})

// EventCreated is the audit event type name for tests.
const EventCreated = audit.EventType("exports.created")
