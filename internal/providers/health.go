package providers

import "context"

// HealthReport is what a runtime says about itself.
type HealthReport struct {
	Status Health `json:"status"`
	// Reason is a short, non-sensitive explanation when Status is not ok.
	Reason string `json:"reason,omitempty"`
	// Version is the runtime version the adapter observed ("" if unknown), so
	// callers can refuse an unqualified runtime.
	Version string `json:"version,omitempty"`
}

// AIRuntimeProvider is what a runtime adapter exposes beyond the role
// interfaces: a name and a health probe. The registry probes every registered
// provider that implements it. One value may serve several roles.
type AIRuntimeProvider interface {
	// Name identifies the runtime ("localai", "fake").
	Name() string
	// Health probes the runtime. A returned error means the probe itself
	// failed and counts as unavailable.
	Health(ctx context.Context) (HealthReport, error)
}
