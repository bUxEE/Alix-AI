package jobs

import "fmt"

// Class is a priority class (master plan section 17.1). Lower numbers are
// more urgent. A job's class is fixed by its registered type, never chosen by
// the caller, so a producer cannot promote its own work.
type Class uint8

// The six priority classes of section 17.1.
const (
	// ClassAuthCommit is authentication, authorization and commits.
	ClassAuthCommit Class = iota + 1
	// ClassTurn is an active text or voice turn.
	ClassTurn
	// ClassRAGQuery is the current RAG query.
	ClassRAGQuery
	// ClassRead is ordinary application reads.
	ClassRead
	// ClassIngest is ingestion and embeddings.
	ClassIngest
	// ClassMaintenance is OCR, reindexing and maintenance.
	ClassMaintenance
)

const (
	firstClass = ClassAuthCommit
	lastClass  = ClassMaintenance
	numClasses = int(lastClass)
)

// Classes returns the six classes in priority order.
func Classes() []Class {
	out := make([]Class, 0, numClasses)
	for c := firstClass; c <= lastClass; c++ {
		out = append(out, c)
	}
	return out
}

// Valid reports whether c is one of the six classes.
func (c Class) Valid() bool { return c >= firstClass && c <= lastClass }

// String is the class's stable name, used in logs and the ops status.
func (c Class) String() string {
	switch c {
	case ClassAuthCommit:
		return "auth_commit"
	case ClassTurn:
		return "turn"
	case ClassRAGQuery:
		return "rag_query"
	case ClassRead:
		return "app_read"
	case ClassIngest:
		return "ingest"
	case ClassMaintenance:
		return "maintenance"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// State is where a job is in its life.
type State string

// Job states. succeeded, failed and cancelled are terminal.
const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Terminal reports whether a job in this state will never run again.
func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}
