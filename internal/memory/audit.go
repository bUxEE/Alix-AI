package memory

import "github.com/buxee/alix-ai/internal/audit"

// Audit event types for memory writes. Payloads hold ids, status and counts — never item text.
var (
	eventCreated = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.created", Description: "a long-term memory item was created", Levels: audit.LevelTenant,
	})
	eventUpdated = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.updated", Description: "a long-term memory item was patched", Levels: audit.LevelTenant,
	})
	eventDeleted = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.deleted", Description: "a long-term memory item was deleted", Levels: audit.LevelTenant,
	})
	eventConfirmed = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.confirmed", Description: "an assistant memory proposal was confirmed", Levels: audit.LevelTenant,
	})
	eventRejected = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.rejected", Description: "an assistant memory proposal was rejected", Levels: audit.LevelTenant,
	})
	eventProposed = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.proposed", Description: "the assistant proposed a long-term memory item", Levels: audit.LevelTenant,
	})
	eventExported = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "memory.exported", Description: "long-term memory was exported by its owner", Levels: audit.LevelTenant,
	})
	eventErased = audit.MustRegister[map[string]any](audit.TypeSpec{
		Name: "deletion.memory_cleared", Description: "long-term memory was erased on request", Levels: audit.LevelTenant,
	})
)

// Event names for tests and retention reports.
const (
	EventCreated   = audit.EventType("memory.created")
	EventUpdated   = audit.EventType("memory.updated")
	EventDeleted   = audit.EventType("memory.deleted")
	EventConfirmed = audit.EventType("memory.confirmed")
	EventRejected  = audit.EventType("memory.rejected")
	EventProposed  = audit.EventType("memory.proposed")
	EventExported  = audit.EventType("memory.exported")
	EventErased    = audit.EventType("deletion.memory_cleared")
)
