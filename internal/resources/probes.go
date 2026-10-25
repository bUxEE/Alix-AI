package resources

import (
	"context"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// MemoryProbe reads the resident memory of this process, in bytes. It is a
// cheap local call. An error means the reading is unknown.
type MemoryProbe interface {
	ProcessRSS() (uint64, error)
}

// MemoryProbeFunc adapts a function to MemoryProbe.
type MemoryProbeFunc func() (uint64, error)

// ProcessRSS calls f.
func (f MemoryProbeFunc) ProcessRSS() (uint64, error) { return f() }

// OSMemoryProbe reads this process's real resident set size from the
// operating system (working set on Windows, VmRSS on Linux, the Go runtime's
// own estimate elsewhere).
type OSMemoryProbe struct{}

// ProcessRSS implements MemoryProbe.
func (OSMemoryProbe) ProcessRSS() (uint64, error) { return osProcessRSS() }

// DiskProbe reports the free space available to this process on the volume
// that holds path, in bytes. An error means the reading is unknown.
type DiskProbe interface {
	FreeBytes(path string) (uint64, error)
}

// DiskProbeFunc adapts a function to DiskProbe.
type DiskProbeFunc func(path string) (uint64, error)

// FreeBytes calls f.
func (f DiskProbeFunc) FreeBytes(path string) (uint64, error) { return f(path) }

// OSDiskProbe asks the operating system (statfs, GetDiskFreeSpaceEx). It
// reports the space available to an unprivileged caller, which honours quotas.
type OSDiskProbe struct{}

// FreeBytes implements DiskProbe.
func (OSDiskProbe) FreeBytes(path string) (uint64, error) { return osFreeBytes(path) }

// LoadedModel is one model a runtime reports as resident in memory.
type LoadedModel struct {
	// ID is the runtime's name for the model.
	ID string
	// Role is the role it serves ("" when unknown).
	Role providers.Role
	// Bytes is the memory the runtime says the model holds.
	Bytes uint64
	// LoadedAt and LastUsed are the runtime's own timestamps; the zero time
	// means the runtime does not say. The governor keeps its own record too
	// and uses the later of the two.
	LoadedAt time.Time
	LastUsed time.Time
}

// RuntimeMemory is what a runtime reports about its own memory.
type RuntimeMemory struct {
	// Bytes is the runtime's total resident memory. When zero, the sum of the
	// loaded models' Bytes is used.
	Bytes  uint64
	Models []LoadedModel
}

// Total is Bytes, or the models' sum when the runtime reports no total.
func (m RuntimeMemory) Total() uint64 {
	if m.Bytes > 0 {
		return m.Bytes
	}
	var sum uint64
	for _, lm := range m.Models {
		sum += lm.Bytes
	}
	return sum
}

// MemoryReporter is implemented by a runtime adapter that can say how much
// memory it holds and which models are loaded. internal/providers has no such
// method, so the governor asks for it through this optional interface (reached
// by type assertion on the registered provider, see RuntimeFromRegistry).
// RuntimeMemory may call the runtime over the network: the governor never calls
// it from inside a queue transaction.
type MemoryReporter interface {
	RuntimeMemory(ctx context.Context) (RuntimeMemory, error)
}

// Unloader is implemented by a runtime adapter that can unload a model.
type Unloader interface {
	Unload(ctx context.Context, modelID string) error
}

// ModelRuntime is both halves: what the idle-unload policy needs.
type ModelRuntime interface {
	MemoryReporter
	Unloader
}

// RuntimeFromRegistry returns the model runtime behind the registered
// providers, or nil when none of them implements ModelRuntime (an old adapter,
// a fake): the governor then has no provider-reported memory and unloads
// nothing. A provider serving several roles is used once.
func RuntimeFromRegistry(reg *providers.Registry) ModelRuntime {
	if reg == nil {
		return nil
	}
	var found []ModelRuntime
	seen := map[string]bool{}
	for _, role := range providers.Roles() {
		p, ok := reg.Provider(role)
		if !ok {
			continue
		}
		rt, ok := p.(ModelRuntime)
		if !ok {
			continue
		}
		key := "role:" + string(role)
		if named, ok := p.(providers.AIRuntimeProvider); ok {
			key = "name:" + named.Name()
		}
		if !seen[key] {
			seen[key] = true
			found = append(found, rt)
		}
	}
	switch len(found) {
	case 0:
		return nil
	case 1:
		return found[0]
	}
	return &multiRuntime{rts: found}
}

// multiRuntime fans out over several runtimes (a rare shape: one adapter per
// role). Memory is summed; an Unload goes to the runtime that last reported the
// model, and to none when no runtime did.
type multiRuntime struct {
	rts []ModelRuntime

	mu    sync.Mutex
	owner map[string]ModelRuntime
}

func (m *multiRuntime) RuntimeMemory(ctx context.Context) (RuntimeMemory, error) {
	var out RuntimeMemory
	owner := map[string]ModelRuntime{}
	for _, rt := range m.rts {
		rm, err := rt.RuntimeMemory(ctx)
		if err != nil {
			return RuntimeMemory{}, err
		}
		out.Bytes += rm.Total()
		out.Models = append(out.Models, rm.Models...)
		for _, lm := range rm.Models {
			owner[lm.ID] = rt
		}
	}
	m.mu.Lock()
	m.owner = owner
	m.mu.Unlock()
	return out, nil
}

func (m *multiRuntime) Unload(ctx context.Context, modelID string) error {
	m.mu.Lock()
	rt := m.owner[modelID]
	m.mu.Unlock()
	if rt == nil {
		return contracts.Newf(contracts.ErrUnavailable, "resources: no runtime reported model %q loaded", modelID)
	}
	return rt.Unload(ctx, modelID)
}
