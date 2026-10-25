package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/metrics"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The capabilities of the host-facing operations. The match is exact: no
// wildcard and no role stands in for one (the audit export has its own, in
// internal/audit).
const (
	// CapabilityQueueRead lets a token read the queue counts of its own scope.
	CapabilityQueueRead = "ops.queue.read"
	// CapabilityProvidersRead lets a token read the status of the model roles.
	CapabilityProvidersRead = "ops.providers.read"
)

// BundleFormat marks the diagnostics bundle document.
const BundleFormat = "alixai.diagnostics/v1"

// QueueSource is the job queue as diag reads it: *jobs.Queue implements it.
type QueueSource interface {
	// Status is the deployment-wide count per class and state (operator view).
	Status(ctx context.Context) (jobs.Status, error)
	// ScopedStatus is the same for one application, tenant and workspace.
	ScopedStatus(ctx context.Context, scope contracts.Scope) (jobs.Status, error)
}

// ProviderSource is the provider registry as diag reads it: *providers.Registry
// implements it.
type ProviderSource interface {
	Snapshot() []providers.RoleStatus
	Overall() providers.Health
}

// AuditExporter writes a scope's audit events: *audit.Log implements it. It
// authorises the export itself (audit.CapabilityExport) and records it.
type AuditExporter interface {
	Export(ctx context.Context, scope contracts.Scope, w io.Writer, opts audit.ExportOptions) (audit.ExportResult, error)
}

// ResourceUsage is what the resource governor reports, as plain numbers. The
// composition root fills it from resources.Governor.Stats; diag does not import
// that package (docs/architecture/module-map.md).
type ResourceUsage struct {
	MemoryBudgetBytes uint64 `json:"memory_budget_bytes"`
	RSSBytes          uint64 `json:"rss_bytes"`
	RuntimeBytes      uint64 `json:"runtime_bytes"`
	ReservedBytes     uint64 `json:"reserved_bytes"`
	InteractiveActive int    `json:"interactive_active"`
	// BackgroundHeld is true while background work yields; HoldReason is the
	// governor's stable reason code.
	BackgroundHeld bool   `json:"background_held"`
	HoldReason     string `json:"hold_reason,omitempty"`
	Admitted       uint64 `json:"admitted"`
	Held           uint64 `json:"held"`
	Rejected       uint64 `json:"rejected"`
	ModelUnloads   uint64 `json:"model_unloads"`
	ModelReloads   uint64 `json:"model_reloads"`
}

// Check is one health condition of the bundle, like gateway.Check: the error
// text goes into the bundle (redacted), so it must say what failed and nothing
// private.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Sources is everything a Service reads. Every member is optional: a missing
// source is reported as not configured, never as a healthy empty value.
type Sources struct {
	// Clock times the bundle and the uptime. Nil is the wall clock.
	Clock clock.Clock
	// Queue, Providers and Audit are the services diag reads.
	Queue     QueueSource
	Providers ProviderSource
	Audit     AuditExporter
	// Resources reads the governor's figures.
	Resources func() ResourceUsage
	// Config returns a snapshot of the configuration sections by name. The
	// bundle redacts it; it is never served as given.
	Config func() map[string]any
	// Checks are the health conditions beyond the model roles.
	Checks []Check
	// Errors is the ring of recent warnings and errors; nil keeps none.
	Errors *ErrorRing
	// RedactKeys are configuration or log keys, besides the built-in sensitive
	// ones, whose values never appear in a bundle (compared ignoring case, "_"
	// and "-").
	RedactKeys []string
	// Authorize is an extra, optional rule of the deployment, asked after the
	// capability is found in the token (for example "only the operator role").
	// An error refuses the call; nil adds no rule.
	Authorize func(ctx context.Context, scope contracts.Scope, capability string) error
}

// Service is the monitoring and diagnostics service. It is safe for concurrent
// use.
type Service struct {
	src Sources
	clk clock.Clock
	red *alog.Redactor
	reg *metrics.Registry
}

// New builds the Service and its metrics registry.
func New(src Sources) (*Service, error) {
	s := &Service{src: src, clk: src.Clock, red: alog.NewRedactor(src.RedactKeys)}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	s.reg = metrics.NewRegistry()
	cs := []metrics.Collector{metrics.RuntimeCollector(s.clk)}
	if src.Queue != nil {
		cs = append(cs, queueCollector(src.Queue))
	}
	if src.Providers != nil {
		cs = append(cs, providerCollector(src.Providers))
	}
	if src.Resources != nil {
		cs = append(cs, resourceCollector(src.Resources))
	}
	for _, c := range cs {
		if err := s.reg.Register(c); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Registry is the metrics registry: queue, provider and resource figures plus
// the process's own. Serve it with metrics.Handler or OperatorHandler.
func (s *Service) Registry() *metrics.Registry { return s.reg }

// authorize is the one place a host-facing operation is allowed: the scope must
// be complete, the token must carry the exact capability, and the optional rule
// of the deployment must agree.
func (s *Service) authorize(ctx context.Context, scope contracts.Scope, capability string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.Permits(capability) {
		return contracts.Newf(contracts.ErrPermissionDenied, "diag: the context does not permit %s", capability)
	}
	if s.src.Authorize != nil {
		if err := s.src.Authorize(ctx, scope, capability); err != nil {
			if contracts.CodeOf(err) == contracts.ErrPermissionDenied {
				return err
			}
			return contracts.Wrap(contracts.ErrPermissionDenied, err, "diag: the operation was not authorised")
		}
	}
	return nil
}

// QueueClass is the queue of one priority class.
type QueueClass struct {
	// Class is the stable class name (auth_commit, turn, rag_query, app_read,
	// ingest, maintenance) and Priority its rank, 1 the most urgent.
	Class    string `json:"class"`
	Priority int    `json:"priority"`
	Workers  int    `json:"workers"`
	MaxDepth int    `json:"max_depth"`
	Pending  int    `json:"pending"`
	Running  int    `json:"running"`
	// Succeeded, Failed and Cancelled count the finished jobs still retained.
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// OldestReadyAgeMs is how long the longest-waiting ready job has waited;
	// only the deployment-wide view knows it.
	OldestReadyAgeMs int64 `json:"oldest_ready_age_ms,omitempty"`
}

// QueueReport is the queue status.
type QueueReport struct {
	At time.Time `json:"at"`
	// Classes lists all six classes, most urgent first.
	Classes []QueueClass `json:"classes"`
}

func queueReport(st jobs.Status) QueueReport {
	r := QueueReport{At: st.At.UTC(), Classes: make([]QueueClass, 0, len(st.Classes))}
	for _, c := range st.Classes {
		r.Classes = append(r.Classes, QueueClass{
			Class: c.Name, Priority: int(c.Class), Workers: c.Workers, MaxDepth: c.MaxDepth,
			Pending: c.Pending, Running: c.Running, Succeeded: c.Succeeded, Failed: c.Failed, Cancelled: c.Cancelled,
			OldestReadyAgeMs: c.OldestReadyAgeMs,
		})
	}
	return r
}

// Queue is the queue status of the caller's own application, tenant and
// workspace. It needs CapabilityQueueRead. The deployment-wide queue is the
// operator's (Bundle, /metrics).
func (s *Service) Queue(ctx context.Context, scope contracts.Scope) (QueueReport, error) {
	if err := s.authorize(ctx, scope, CapabilityQueueRead); err != nil {
		return QueueReport{}, err
	}
	if s.src.Queue == nil {
		return QueueReport{}, contracts.New(contracts.ErrUnavailable, "diag: the queue is not available on this server")
	}
	st, err := s.src.Queue.ScopedStatus(ctx, scope)
	if err != nil {
		return QueueReport{}, contracts.Wrap(contracts.ErrUnavailable, err, "diag: the queue status could not be read")
	}
	return queueReport(st), nil
}

// ProviderRole is the status of one model role.
type ProviderRole struct {
	Role       string     `json:"role"`
	Configured bool       `json:"configured"`
	Provider   string     `json:"provider,omitempty"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	Version    string     `json:"version,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	// Effect and Behavior are set when the role is not ok: the stable
	// degradation code and what the service does instead (master plan 18).
	Effect   string `json:"effect,omitempty"`
	Behavior string `json:"behavior,omitempty"`
}

// ProviderReport is the provider and model status.
type ProviderReport struct {
	// Status is the summary: ok, degraded or unavailable.
	Status string         `json:"status"`
	Roles  []ProviderRole `json:"roles"`
}

func (s *Service) providerReport() ProviderReport {
	snap := s.src.Providers.Snapshot()
	r := ProviderReport{Status: string(s.src.Providers.Overall()), Roles: make([]ProviderRole, 0, len(snap))}
	for _, st := range snap {
		pr := ProviderRole{
			Role: string(st.Role), Configured: st.Configured, Provider: st.Provider, Status: string(st.Status),
			Reason: st.Reason, Version: st.Version, Effect: st.Effect, Behavior: st.Behavior,
		}
		if !st.Since.IsZero() {
			since := st.Since.UTC()
			pr.Since = &since
		}
		r.Roles = append(r.Roles, pr)
	}
	return r
}

// Providers is the status of the model roles: which provider serves each, its
// health and, when it is not ok, what the service does instead. It needs
// CapabilityProvidersRead. Reasons pass through the redactor.
func (s *Service) Providers(ctx context.Context, scope contracts.Scope) (ProviderReport, error) {
	if err := s.authorize(ctx, scope, CapabilityProvidersRead); err != nil {
		return ProviderReport{}, err
	}
	if s.src.Providers == nil {
		return ProviderReport{}, contracts.New(contracts.ErrUnavailable, "diag: no provider registry is available on this server")
	}
	r := s.providerReport()
	for i := range r.Roles {
		r.Roles[i].Reason = s.red.String(r.Roles[i].Reason)
	}
	return r, nil
}

// AuditExport writes the audit events of the caller's own application, tenant
// and workspace to w as JSON lines. internal/audit decides: it needs
// audit.CapabilityExport and the deployment's own export rule, records the
// export before any event leaves, and refuses it if it cannot.
func (s *Service) AuditExport(ctx context.Context, scope contracts.Scope, w io.Writer, opts audit.ExportOptions) (audit.ExportResult, error) {
	if s.src.Audit == nil {
		return audit.ExportResult{}, contracts.New(contracts.ErrUnavailable, "diag: the audit log is not available on this server")
	}
	return s.src.Audit.Export(ctx, scope, w, opts)
}

// CheckResult is one health condition in the bundle.
type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// BundleHealth is the health section of a bundle.
type BundleHealth struct {
	// Status is ok, degraded or unavailable (the model roles' summary), or
	// unknown when no provider registry is configured.
	Status string         `json:"status"`
	Roles  []ProviderRole `json:"roles"`
	Checks []CheckResult  `json:"checks"`
}

// Versions is the versions section of a bundle.
type Versions struct {
	Build version.Info `json:"build"`
}

// Bundle is the diagnostics bundle: what an operator attaches to a report.
// Everything in it has been through the redactor.
type Bundle struct {
	Format      string    `json:"format"`
	GeneratedAt time.Time `json:"generated_at"`
	Versions    Versions  `json:"versions"`
	// Config is the configuration by section, with sensitive values replaced.
	Config map[string]any `json:"config"`
	Health BundleHealth   `json:"health"`
	// Queue is the deployment-wide queue.
	Queue        *QueueReport   `json:"queue,omitempty"`
	Resources    *ResourceUsage `json:"resources,omitempty"`
	RecentErrors []ErrorEntry   `json:"recent_errors"`
	// Problems names the sources that could not be read; the rest of the bundle
	// is still reported.
	Problems []string `json:"problems"`
	// Redactions counts the values the redactor replaced.
	Redactions int `json:"redactions"`
}

// Bundle collects the diagnostics bundle of the whole deployment. It is the
// operator's view (configuration and the global queue included) and takes no
// scope: reach it through OperatorHandler on loopback, never through a
// host-facing route. A source that fails is listed in Problems and the bundle is
// still returned.
func (s *Service) Bundle(ctx context.Context) (Bundle, error) {
	b := Bundle{
		Format:       BundleFormat,
		GeneratedAt:  s.clk.Now().UTC(),
		Versions:     Versions{Build: version.Get()},
		Config:       map[string]any{},
		Health:       BundleHealth{Status: "unknown", Roles: []ProviderRole{}, Checks: []CheckResult{}},
		RecentErrors: []ErrorEntry{},
		Problems:     []string{},
	}
	problem := func(what string, err error) {
		b.Problems = append(b.Problems, what+": "+err.Error())
	}

	if s.src.Config != nil {
		b.Config = s.src.Config()
	} else {
		b.Problems = append(b.Problems, "config: no configuration source")
	}
	if s.src.Providers != nil {
		pr := s.providerReport()
		b.Health.Status, b.Health.Roles = pr.Status, pr.Roles
	} else {
		b.Problems = append(b.Problems, "providers: no provider registry")
	}
	checks := append([]Check(nil), s.src.Checks...)
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	for _, c := range checks {
		res := CheckResult{Name: c.Name, OK: true}
		if err := runCheck(ctx, c); err != nil {
			res.OK, res.Reason = false, err.Error()
		}
		b.Health.Checks = append(b.Health.Checks, res)
	}
	if s.src.Queue != nil {
		if st, err := s.src.Queue.Status(ctx); err != nil {
			problem("queue", err)
		} else {
			qr := queueReport(st)
			b.Queue = &qr
		}
	} else {
		b.Problems = append(b.Problems, "queue: no queue")
	}
	if s.src.Resources != nil {
		ru := s.src.Resources()
		b.Resources = &ru
	} else {
		b.Problems = append(b.Problems, "resources: no resource governor")
	}
	if s.src.Errors != nil {
		b.RecentErrors = s.src.Errors.Recent()
	}
	return s.sanitize(b)
}

// runCheck runs one check under a deadline, turning a panic into an error.
func runCheck(ctx context.Context, c Check) (err error) {
	if c.Fn == nil {
		return errors.New("no check function")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = errors.New("the check panicked")
		}
	}()
	return c.Fn(ctx)
}

// sanitize is the choke point of the bundle: the whole document is taken to its
// JSON form, every key and string is redacted, and the result is decoded back.
// Nothing that is not on the other side of this call leaves the package, so a
// section added later is covered without remembering to redact it.
func (s *Service) sanitize(b Bundle) (Bundle, error) {
	g, err := generic(b)
	if err != nil {
		return Bundle{}, contracts.Wrap(contracts.ErrInternal, err, "diag: the bundle could not be encoded")
	}
	w := &walker{red: s.red}
	clean := w.walk(g)
	raw, err := json.Marshal(clean)
	if err != nil {
		return Bundle{}, contracts.Wrap(contracts.ErrInternal, err, "diag: the bundle could not be encoded")
	}
	var out Bundle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // configuration numbers keep their exact digits
	if err := dec.Decode(&out); err != nil {
		return Bundle{}, contracts.Wrap(contracts.ErrInternal, err, "diag: the bundle could not be decoded")
	}
	out.Redactions += w.count
	return out, nil
}

// MarshalBundle is Bundle as indented JSON, the form an operator saves to a file.
func (s *Service) MarshalBundle(ctx context.Context) ([]byte, error) {
	b, err := s.Bundle(ctx)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("diag: %w", err)
	}
	return append(out, '\n'), nil
}
