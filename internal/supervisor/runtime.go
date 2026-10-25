package supervisor

import (
	"context"
	"sync"
)

// Runtime brings a LocalAI instance into being: by starting a process (Exec) or
// by attaching to a unit the service manager keeps (Unit).
type Runtime interface {
	// Start returns the running instance. It must not tie the instance's
	// lifetime to ctx: cancelling ctx stops the supervisor, which then calls
	// Stop on the instance itself.
	Start(ctx context.Context) (Instance, error)
}

// Instance is one run of the runtime.
type Instance interface {
	// PID is the process id the supervisor started, or 0 for an attached unit.
	PID() int
	// Done is closed when the instance is gone. An attached unit's Done is nil
	// (never ready): its death is seen only through health probes.
	Done() <-chan struct{}
	// Stop ends an instance the supervisor started and returns only once it is
	// gone, or an error when that cannot be confirmed. It is a no-op for an
	// attached unit, which the service manager owns.
	Stop(ctx context.Context) error
}

// exitDetailer is implemented by an Instance that can say, in a fixed format
// that never carries text from the child, how it ended ("exit status 3").
type exitDetailer interface {
	ExitDetail() string
}

// Unit is the Runtime for LocalAI as a separate unit or service under its own
// identity (the hardened native install). The first Start attaches to what the
// service manager already runs and touches nothing. Later Starts call Restart,
// when one is configured, to ask the service manager for a restart; with no
// controller the supervisor just waits, with backoff, for the unit to come back.
// Unit starts no process and executes no command.
type Unit struct {
	// Restart asks the service manager to restart the unit. It is a
	// composition-root decision (a polkit-authorised helper, a service API).
	Restart func(ctx context.Context) error

	mu       sync.Mutex
	attached bool
}

// Start implements Runtime.
func (u *Unit) Start(ctx context.Context) (Instance, error) {
	u.mu.Lock()
	first := !u.attached
	u.attached = true
	u.mu.Unlock()
	if !first && u.Restart != nil {
		if err := u.Restart(ctx); err != nil {
			return nil, err
		}
	}
	return attachedUnit{}, nil
}

// attachedUnit is an instance the supervisor does not own.
type attachedUnit struct{}

func (attachedUnit) PID() int                   { return 0 }
func (attachedUnit) Done() <-chan struct{}      { return nil }
func (attachedUnit) Stop(context.Context) error { return nil }
