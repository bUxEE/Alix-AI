package main

import "context"

// The seams of the harness. The real implementations are the docker and host
// memory sources, the readiness / operations / metrics probes and the SDK
// driver; the tests (and `--fake`) put scripted ones behind the same
// interfaces, so a run of the whole harness takes seconds.

// MemSample is one reading of the memory the stack uses, in MiB, summed over
// everything the source covers (every container, every process).
type MemSample struct {
	UsedMiB float64 `json:"used_mib"`
	// SwapMiB is the swap in use. It is meaningful only when SwapKnown is true:
	// docker stats has no swap column, so a source that cannot tell says so
	// instead of reporting a zero that nobody measured.
	SwapMiB   float64 `json:"swap_mib"`
	SwapKnown bool    `json:"swap_known"`
}

// MemoryDescriptor says where the memory figures come from (docker-stats,
// host-pid or fake) and from which containers or processes.
type MemoryDescriptor struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// Memory sources.
const (
	MemSourceDocker = "docker-stats"
	MemSourceHost   = "host-pid"
	MemSourceFake   = "fake"
)

// MemorySource takes one reading.
type MemorySource interface {
	Sample(ctx context.Context) (MemSample, error)
	Describe() MemoryDescriptor
}

// RoleState is one model role as the stack reports it.
type RoleState struct {
	Configured bool   `json:"configured"`
	Status     string `json:"status"` // ok, degraded or unavailable
}

// StackStats is what a probe reads from the running stack. A field the probe
// cannot know is left at its zero value with its Known flag false.
type StackStats struct {
	// Roles is nil when the probe cannot tell.
	Roles map[string]RoleState `json:"roles,omitempty"`

	QueueKnown         bool `json:"queue_known"`
	QueuePending       int  `json:"queue_pending"`
	QueueRunning       int  `json:"queue_running"`
	QueueIngestPending int  `json:"queue_ingest_pending"`

	// ReloadsKnown is true only for the operator metrics, where the governor
	// counts unloads and the loads that followed them.
	ReloadsKnown bool   `json:"reloads_known"`
	Reloads      uint64 `json:"reloads"`
	Unloads      uint64 `json:"unloads"`
}

// StackProbe reads the stack's own view of itself.
type StackProbe interface {
	Stats(ctx context.Context) (StackStats, error)
	// Kind names the probe for the result: readyz+ops, metrics or fake.
	Kind() string
}

// Step is one interactive turn of the mix.
type Step struct {
	ID       string `json:"id"`
	Class    string `json:"class"` // booking or rag
	Language string `json:"language"`
	Text     string `json:"text"`
}

// Driver puts load on the stack. Every error it returns is a failure of that
// one operation: the harness counts it and carries on, because a soak that
// stops at the first error cannot say how often it happens.
type Driver interface {
	// Preflight runs before the clock starts: readiness, one session, the
	// ingestion source. A stack that fails it is not soaked.
	Preflight(ctx context.Context) error
	// Turn sends one interactive turn and returns when it has ended. It never
	// confirms a prepared action: it rejects it (a 7-day soak must not use up
	// the clinic's slots).
	Turn(ctx context.Context, step Step) error
	// Ingest re-ingests the source once and returns when the new generation
	// serves, or fails.
	Ingest(ctx context.Context) error
	// Close ends the session. It is best effort.
	Close(ctx context.Context)
}

// ---------------------------------------------------------------- records

// Sample is one tick of the sampler: memory and swap, and what the stack says.
type Sample struct {
	AtMS    int64      `json:"at_ms"` // since the start of the run
	MemOK   bool       `json:"mem_ok"`
	Mem     MemSample  `json:"mem"`
	StackOK bool       `json:"stack_ok"`
	Stack   StackStats `json:"stack"`
}

// TurnRecord is one interactive turn. Err is the typed error code, empty on
// success. Latency is the whole turn, a rejection included.
type TurnRecord struct {
	AtMS  int64   `json:"at_ms"`
	Step  string  `json:"step"`
	Class string  `json:"class"`
	MS    float64 `json:"ms"`
	Err   string  `json:"err,omitempty"`
}

// IngestRecord is one re-ingestion, from the request to the serving generation.
type IngestRecord struct {
	AtMS int64   `json:"at_ms"`
	MS   float64 `json:"ms"`
	Err  string  `json:"err,omitempty"`
}

// Observations is everything the verdict is a function of.
type Observations struct {
	// Completed is false when the run was cut short (an interrupt).
	Completed bool           `json:"completed"`
	Samples   []Sample       `json:"samples"`
	Turns     []TurnRecord   `json:"turns"`
	Ingests   []IngestRecord `json:"ingests"`
}
