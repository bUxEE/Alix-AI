package sdktest

import (
	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// What a host-application test says about one turn of the fake AlixAI server. A plan describes the outcome in
// terms of the API (a reply, a prepared action, a failure); the fake turns it into the events, headers and
// state a real service would produce, so a test cannot forge an invalid stream by hand.

// Turn is a turn the fake received: the session it belongs to and what the user sent.
type Turn struct {
	SessionID string
	Input     sdkgo.TurnInput
}

// CommitPlan says what committing a prepared action does when the user confirms it.
type CommitPlan struct {
	// Status is the receipt status: committed (the default), queued, rejected, partial or uncertain.
	Status string
	// ErrorCode and ErrorMessage are the typed reason of a rejected, partial or uncertain commit.
	ErrorCode    contracts.ErrCode
	ErrorMessage string
}

// PrepareInput describes an action the assistant prepares. The action is at risk level 2: it waits for the
// user's explicit confirmation, and the confirmation secret comes in the response header (event stream) or the
// `confirmation` member (JSON turn).
type PrepareInput struct {
	// CapabilityID must be one the token's capability scopes allow, else the turn fails with permission_denied.
	CapabilityID string
	// Summary is what the user confirms.
	Summary string
	// Text is the assistant's text for the turn.
	Text string
	// Commit is what confirming does. The zero value commits.
	Commit CommitPlan
}

type planKind int

const (
	planReply planKind = iota
	planPrepare
	planFail
	planReject
)

// TurnPlan is what the assistant does in one turn. Build one with Reply, PrepareAction, FailTurn or RejectTurn.
type TurnPlan struct {
	kind    planKind
	text    string
	prepare PrepareInput
	code    contracts.ErrCode
	message string
	status  int
}

// Reply is a plan in which the assistant answers with text.
func Reply(text string) TurnPlan { return TurnPlan{kind: planReply, text: text} }

// PrepareAction is a plan in which the assistant prepares an action that waits for the user's confirmation.
func PrepareAction(input PrepareInput) TurnPlan { return TurnPlan{kind: planPrepare, prepare: input} }

// FailTurn is a plan in which the turn ends in a typed failure: an `error` event, then `done` with
// finish_reason failed (still HTTP 200).
func FailTurn(code contracts.ErrCode, message string) TurnPlan {
	return TurnPlan{kind: planFail, code: code, message: message}
}

// RejectTurn is a plan in which the turn could not be run at all: an error response with this status, never a
// stream.
func RejectTurn(status int, code contracts.ErrCode, message string) TurnPlan {
	return TurnPlan{kind: planReject, status: status, code: code, message: message}
}
