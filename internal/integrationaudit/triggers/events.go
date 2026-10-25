package triggers

import (
	"context"
	"log/slog"

	"github.com/buxee/alix-ai/internal/audit"
)

// eventData is the data of the four ops events. Only fields that name a package,
// a run, a kind of change or a typed code are carried: never text an application
// or a package chose, a URL or a credential.
type eventData struct {
	// ManifestHash is the package the event is about, or "" when the application
	// had no active package.
	ManifestHash string   `json:"manifest_hash"`
	Critical     bool     `json:"critical"`
	Changes      []Change `json:"changes"`
	// RunID and Outcome are set by reaudit.completed.
	RunID   string `json:"run_id,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	// Reason says why traffic was stopped or why the watcher could not finish.
	Reason string `json:"reason,omitempty"`
	// Code is the typed error code of a failure.
	Code string `json:"code,omitempty"`
}

// The ops events of the re-audit triggers, on the application chain: the audit
// log is the operator's record of what the watcher saw and did. A Notifier,
// when one is wired, receives the same facts.
var (
	// EventTriggered: a change was detected and a re-audit is starting. Critical
	// is true when the change also stopped traffic.
	EventTriggered = audit.MustRegister[eventData](audit.TypeSpec{
		Name: "reaudit.triggered", Description: "a change after activation was detected and a re-audit started",
		Levels: audit.LevelApplication, Required: []string{"manifest_hash", "changes"},
	})
	// EventCompleted: a re-audit ran and stored its report.
	EventCompleted = audit.MustRegister[eventData](audit.TypeSpec{
		Name: "reaudit.completed", Description: "a re-audit triggered by a change stored its report",
		Levels: audit.LevelApplication, Required: []string{"manifest_hash", "run_id", "outcome"},
	})
	// EventQuarantined: a critical change took the active package out of service
	// and the capability registry confirmed it serves nothing.
	EventQuarantined = audit.MustRegister[eventData](audit.TypeSpec{
		Name: "reaudit.quarantined", Description: "a critical change stopped live traffic: the package is quarantined and the registry no longer serves it",
		Levels: audit.LevelApplication, Required: []string{"manifest_hash", "changes", "reason"},
	})
	// EventFailed: the watcher could not do what it set out to: a quarantine the
	// registry did not confirm (critical: the application may still be served) or a
	// re-audit that errored. The next pass tries again; a quarantine is confirmed
	// again before anything else.
	EventFailed = audit.MustRegister[eventData](audit.TypeSpec{
		Name: "reaudit.failed", Description: "the re-audit triggers could not complete a re-audit or confirm a quarantine",
		Levels: audit.LevelApplication, Required: []string{"manifest_hash", "reason"},
	})
)

// Severities of a Notice.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Notice is what the operator is told. Type is the audit event type of the same
// fact, so a feed built from Notices and one built from the audit log agree.
type Notice struct {
	Type          audit.EventType
	Severity      string
	ApplicationID string
	ManifestHash  string
	Changes       []Change
	RunID         string
	Outcome       string
	Reason        string
	Code          string
}

// Notifier forwards ops events to wherever the operator looks (the ops surface,
// a log line, a webhook the composition root owns). It must not block and must
// not call back into the Watcher. A Notifier that panics is contained: it can
// delay nothing the watcher does to stop traffic.
type Notifier interface {
	Notify(ctx context.Context, n Notice)
}

// NotifierFunc is a Notifier made of a function.
type NotifierFunc func(ctx context.Context, n Notice)

// Notify implements Notifier.
func (f NotifierFunc) Notify(ctx context.Context, n Notice) { f(ctx, n) }

// emit appends the ops event to the application chain and tells the Notifier.
// The audit append is returned: an event that cannot be recorded is an error the
// caller reports, but never a reason to leave a package in service (callers stop
// traffic first and emit after). The Notifier is told either way.
func (w *Watcher) emit(ctx context.Context, t audit.Type[eventData], sev string, n Notice, d eventData) error {
	var err error
	if loc, lerr := audit.ForApplication(n.ApplicationID, w.cfg.Principal); lerr != nil {
		err = lerr
	} else if _, aerr := t.Append(ctx, w.cfg.Log, loc, d); aerr != nil {
		err = aerr
	}
	n.Type, n.Severity = t.Name(), sev
	w.notify(ctx, n)
	return err
}

func (w *Watcher) notify(ctx context.Context, n Notice) {
	if w.cfg.Notifier == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			w.cfg.Logger.Error("a re-audit notifier panicked", slog.String("application_id", n.ApplicationID), slog.Any("panic", r))
		}
	}()
	w.cfg.Notifier.Notify(ctx, n)
}
