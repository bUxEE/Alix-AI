package contracts

import (
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The structured conversation and action objects (master plan 11.2, 14.1) are
// generated from schemas/conversation and schemas/actions by tools/schemagen.
// These aliases make them reachable as contracts.TaskProposal and so on, so
// callers import one package and the generated types stay the single source:
// there is no hand-written copy that could drift from the schema.
//
// The generated packages import nothing but the standard library and each
// other (never this package), so this file adds no import cycle and
// pkg/contracts stays stdlib-only transitively.
//
// Scope, Error, ErrCode and RiskLevel are NOT aliased: they stay hand-written
// here because they carry behaviour (Validate, Retryable, Escalate, strict
// JSON) that generated types do not. The generated packages hold shape-only
// twins (gen/common.Scope, gen/common.Error, gen/actions.RiskLevel) whose JSON
// is identical, so a value can be re-encoded from one to the other.
type (
	// TaskProposal is what the model may propose for a user turn (schemas/conversation/task-proposal).
	TaskProposal = conversation.TaskProposal
	// DialogueDirective is what the core tells the model to say (schemas/conversation/dialogue-directive).
	DialogueDirective = conversation.DialogueDirective
	// TurnEvent is one event of a streamed turn (schemas/conversation/turn-event).
	TurnEvent = conversation.TurnEvent
	// RichBlock is one block of a rich assistant message (schemas/conversation/rich-block).
	RichBlock = conversation.RichBlock
	// Citation is one authorised source supporting a RAG answer (schemas/conversation/citation).
	Citation = conversation.Citation
	// FileRef is a scoped, opaque reference to an attachment (schemas/conversation/file-ref).
	FileRef = conversation.FileRef

	// PreparedAction is a validated action waiting for confirmation (schemas/actions/prepared-action).
	PreparedAction = actions.PreparedAction
	// ActionReceipt is the durable outcome of an action (schemas/actions/action-receipt).
	ActionReceipt = actions.ActionReceipt
	// DeterministicResult is the normalised form of every adapter result (schemas/actions/deterministic-result).
	DeterministicResult = actions.DeterministicResult
)
