package integrationaudit

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Check is one deterministic check of the audit. It never asks a model
// anything: a check reads the package, the adapter and the application's own
// answers, and says what it found.
//
// A check is registered once, from an init function or at construction, and
// then runs in every audit. Later milestones add checks without editing this
// package: M1-18 registers the runtime probes (sections 2, 6, 8, 9 and 10),
// M2-12 the RAG probes (sections 3, 6 and 10), M5-10 the re-audit triggers.
type Check interface {
	// ID is the stable dotted lower-case identifier of the check, such as
	// "package.authenticity". It keys the report and the Extension a check
	// adds; renaming one is an API change.
	ID() string
	// Title is a short human-readable name.
	Title() string
	// Section is the master plan 4.5 check this implements, 1 to 10, or 0 for
	// a supplementary check that belongs to none of them.
	Section() int
	// Stage is the step of master plan 9.4 the check belongs to. Checks run in
	// stage order.
	Stage() Stage
	// Target is the audit target the check examines.
	Target() Target
	// Run performs the check. An error means the check could not run at all
	// (it is recorded as a critical finding: an audit that did not run is not a
	// pass); everything the check finds out about the integration is returned
	// in the Outcome, never as an error.
	Run(ctx context.Context, in *Input) (*Outcome, error)
}

// PackageOptional is implemented by a check that can run without a loaded
// package (the authenticity check explains why a package could not be loaded).
// Every other check is not run when there is no package: it is recorded as
// unverified instead, so a package that failed its signature is never audited
// for anything else on the strength of unverified content.
type PackageOptional interface {
	RunsWithoutPackage() bool
}

var checkIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Spec describes a check built with NewCheck.
type Spec struct {
	ID      string
	Title   string
	Section int
	Stage   Stage
	Target  Target
	// WithoutPackage lets the check run when no package could be loaded.
	WithoutPackage bool
}

type funcCheck struct {
	spec Spec
	run  func(ctx context.Context, in *Input) (*Outcome, error)
}

// NewCheck builds a Check from a Spec and a function. It is how an extension
// registers a check without declaring a type.
func NewCheck(spec Spec, run func(ctx context.Context, in *Input) (*Outcome, error)) Check {
	return &funcCheck{spec: spec, run: run}
}

func (c *funcCheck) ID() string               { return c.spec.ID }
func (c *funcCheck) Title() string            { return c.spec.Title }
func (c *funcCheck) Section() int             { return c.spec.Section }
func (c *funcCheck) Stage() Stage             { return c.spec.Stage }
func (c *funcCheck) Target() Target           { return c.spec.Target }
func (c *funcCheck) RunsWithoutPackage() bool { return c.spec.WithoutPackage }
func (c *funcCheck) Run(ctx context.Context, in *Input) (*Outcome, error) {
	return c.run(ctx, in)
}

// validateCheck refuses a check that cannot be placed in a report.
func validateCheck(c Check) error {
	if c == nil {
		return fmt.Errorf("integrationaudit: nil check")
	}
	if !checkIDRE.MatchString(c.ID()) || len(c.ID()) > 64 {
		return fmt.Errorf("integrationaudit: check id %q is not a dotted lower-case identifier", c.ID())
	}
	if c.Title() == "" {
		return fmt.Errorf("integrationaudit: check %s has no title", c.ID())
	}
	if n := c.Section(); n < 0 || n > sectionCount {
		return fmt.Errorf("integrationaudit: check %s names section %d, want 0 to %d", c.ID(), n, sectionCount)
	}
	if !c.Stage().Valid() {
		return fmt.Errorf("integrationaudit: check %s has no valid stage", c.ID())
	}
	if !c.Target().Valid() {
		return fmt.Errorf("integrationaudit: check %s has no valid target", c.ID())
	}
	return nil
}

// Registry holds the checks of an audit. It is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	checks map[string]Check
}

// NewRegistry returns an empty registry. A Runner given one runs exactly the
// checks registered in it; Builtins lists the checks of this package.
func NewRegistry() *Registry { return &Registry{checks: map[string]Check{}} }

// Register adds a check. It refuses an invalid check and a second check with
// the same ID: a check cannot be replaced by registering over it, or an
// extension could silence a built-in one.
func (r *Registry) Register(c Check) error {
	if err := validateCheck(c); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.checks[c.ID()]; dup {
		return fmt.Errorf("integrationaudit: check %s is registered twice", c.ID())
	}
	r.checks[c.ID()] = c
	return nil
}

// Checks returns the registered checks in the order they run: by stage, then
// section, then ID.
func (r *Registry) Checks() []Check {
	r.mu.RLock()
	out := make([]Check, 0, len(r.checks))
	for _, c := range r.checks {
		out = append(out, c)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Stage() != b.Stage() {
			return a.Stage() < b.Stage()
		}
		if a.Section() != b.Section() {
			return a.Section() < b.Section()
		}
		return a.ID() < b.ID()
	})
	return out
}

// Default is the registry audits use unless a Runner is given another. It holds
// the built-in checks and whatever other packages register from their init
// functions.
var Default = func() *Registry {
	r := NewRegistry()
	for _, c := range Builtins() {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
	return r
}()

// Register adds a check to the Default registry. It is meant for init
// functions and panics on a programming error (an invalid or duplicate check),
// which is caught the first time the binary starts.
func Register(c Check) {
	if err := Default.Register(c); err != nil {
		panic(err)
	}
}

// CheckInfo describes a registered check; Input.Registered lists them so a
// check can tell what else is part of this audit.
type CheckInfo struct {
	ID      string
	Section int
	Stage   Stage
	Target  Target
}

// Input is everything a check may look at. The framework gathers it once per
// audit: the package is loaded and verified from the stored bytes, and the
// adapter is asked to describe and inspect itself once, so no two checks
// disagree about what the application said.
type Input struct {
	// Package is the typed package, or nil when it could not be loaded (the
	// loader found a critical problem or the stored bytes no longer verify).
	// Treat it as read-only.
	Package *apppkg.Package
	// Record is the installed package as the database holds it. Its state is
	// not part of the report.
	Record apppkg.Record
	// LoadFindings are the loader's findings for the stored bytes: every
	// finding of the checks of docs/integration/application-package.md
	// section 14, critical ones included when the package was refused.
	LoadFindings []contracts.Finding
	// Authenticity says how far the signature was checked.
	Authenticity apppkg.Authenticity
	// Signer is the signature that authenticated the package.
	Signer *trust.Accepted
	// CoreVersion is the AlixAI core version the package was checked against.
	CoreVersion string

	// Adapter reaches the application, or nil when none is available: then the
	// runtime and application targets cannot be inspected, and a check that
	// needs the application reports unverified instead of guessing. Probes
	// that call the application (M1-18) use it directly, with scopes derived
	// from Scope; it is the adapters.Wrap'd production adapter.
	Adapter adapters.AppAdapter
	// Description is what Describe returned, nil when there is no adapter or
	// Describe failed (DescribeErr says which).
	Description *adapters.Description
	// DescribeErr is the typed code of a failed Describe, or "".
	DescribeErr contracts.ErrCode
	// Inspection is what Inspect returned for every aspect, nil when there is
	// no adapter or Inspect failed.
	Inspection *adapters.Inspection
	// InspectErr is the typed code of a failed Inspect, or "".
	InspectErr contracts.ErrCode
	// Health is what Health returned, nil when there is no adapter or Health
	// failed.
	Health *adapters.HealthReport
	// HealthErr is the typed code of a failed Health, or "".
	HealthErr contracts.ErrCode

	// Scope is the identity the audit uses for its own calls to the adapter: a
	// valid scope bound to the audited application. It is not a tenant of the
	// application; probes mint their own disposable-tenant scopes from it.
	Scope contracts.Scope
	// Profile is the configured model and runtime profile, or "".
	Profile string
	// Clock is the audit's injected clock.
	Clock clock.Clock
	// Registered lists every check of this audit, so a check can tell whether
	// another covers what it cannot (the static RAG check records the live
	// verification as unverified only when no other section-3 check exists).
	Registered []CheckInfo
}

// HasOtherCheck reports whether a check other than self is registered for the
// section.
func (in *Input) HasOtherCheck(section int, self string) bool {
	for _, c := range in.Registered {
		if c.Section == section && c.ID != self {
			return true
		}
	}
	return false
}

// Outcome is what a check found. The framework derives the check's status from
// it: a critical finding makes the check fail; an Unverified item makes it
// unverified; a check that has any Unverified item can never be reported as
// pass, whatever else it says.
type Outcome struct {
	// Findings are the problems found. Each must satisfy
	// contracts.Finding.Validate; one that does not is replaced by a critical
	// finding, because a check that cannot say what is wrong cannot say it is
	// right.
	Findings []contracts.Finding
	// Covered lists the capabilities the check examined. Each gets the verdict
	// passed unless a finding or Unverified item that names it says otherwise.
	Covered []string
	// Verdicts are explicit capability verdicts (quarantined, unverified,
	// skipped, failed) with a reason, for what findings cannot express.
	Verdicts []CapabilityVerdict
	// Unverified lists what the check could not inspect.
	Unverified []Unverified
	// Sources are RAG source results.
	Sources []SourceResult
	// Coverage lists coverage counts.
	Coverage []CoverageItem
	// Probes are probe results.
	Probes []ProbeResult
	// Measurements are resource and latency measurements.
	Measurements []Measurement
	// Permissions are missing, excessive or contradictory permissions the
	// check found; they are appended to the report's.
	Permissions Permissions
	// External is what could leave the deployment, for the informational
	// external-mode inventory. Reports merge it: lists append, flags combine.
	External *External
	// Extension is check-specific data for the report. It is signed with the
	// report, so it admits only strings, integers, booleans, null, arrays and
	// objects; a value that cannot be canonicalised fails the check.
	Extension map[string]any
}

// CapabilityVerdict is an explicit verdict on one capability.
type CapabilityVerdict struct {
	CapabilityID string
	Status       CapabilityStatus
	Reason       string
}

// Add appends findings to the outcome and returns it, for chaining.
func (o *Outcome) Add(fs ...contracts.Finding) *Outcome {
	o.Findings = append(o.Findings, fs...)
	return o
}

// Unverify records something the check could not inspect.
func (o *Outcome) Unverify(u Unverified) *Outcome {
	o.Unverified = append(o.Unverified, u)
	return o
}
