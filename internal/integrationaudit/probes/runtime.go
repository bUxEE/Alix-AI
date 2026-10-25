package probes

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Options configures the checks Checks builds.
type Options struct {
	// Source returns the Provisioner of an audit. Nil means the sources
	// registered with SetProvisionerSource and then the adapter itself.
	Source ProvisionerSource
	// ScratchDir is the directory in which the checks that run the platform's own
	// coordinator keep their throwaway store. Empty means a fresh directory under
	// the system's temporary directory; either way it is removed afterwards.
	ScratchDir string
	// MaxSweep bounds the derived cross-tenant probes of the scope check. Zero
	// means DefaultMaxSweep.
	MaxSweep int
}

// DefaultMaxSweep is how many derived cross-scope probes the scope check makes
// at most, on top of the probes the package declares.
const DefaultMaxSweep = 400

func (o Options) source() ProvisionerSource {
	if o.Source != nil {
		return o.Source
	}
	return defaultSource
}

func (o Options) maxSweep() int {
	if o.MaxSweep > 0 {
		return o.MaxSweep
	}
	return DefaultMaxSweep
}

// host wraps the adapter the audit was given. It is the only way the probes
// call the application, and it makes an outage sticky: once the application has
// timed out or refused to answer, the rest of the check is not attempted (each
// further call would wait out its own timeout) and what is left is recorded as
// unverified.
type host struct {
	ad   adapters.AppAdapter
	down error
}

var errHostDown = contracts.New(contracts.ErrUnavailable, "the application stopped answering during the audit")

// outage reports whether err says the application could not be reached or did
// not answer in time, or that the audit itself was cancelled.
func outage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch contracts.CodeOf(err) {
	case contracts.ErrUnavailable, contracts.ErrTimeout, contracts.ErrResourceExhausted:
		return true
	}
	return false
}

func (h *host) note(err error) error {
	if outage(err) && h.down == nil {
		h.down = err
	}
	return err
}

func (h *host) read(ctx context.Context, sc contracts.Scope, capID string, fields map[string]any) (act.DeterministicResult, error) {
	if h.down != nil {
		return act.DeterministicResult{}, errHostDown
	}
	res, err := h.ad.Read(ctx, sc, adapters.ReadRequest{CapabilityID: capID, Fields: fields})
	return res, h.note(err)
}

func (h *host) validate(ctx context.Context, sc contracts.Scope, capID string, fields map[string]any) (act.DeterministicResult, error) {
	if h.down != nil {
		return act.DeterministicResult{}, errHostDown
	}
	res, err := h.ad.Validate(ctx, sc, adapters.ValidateRequest{CapabilityID: capID, Fields: fields})
	return res, h.note(err)
}

func (h *host) prepare(ctx context.Context, sc contracts.Scope, capID string, fields map[string]any) (act.DeterministicResult, error) {
	if h.down != nil {
		return act.DeterministicResult{}, errHostDown
	}
	res, err := h.ad.Prepare(ctx, sc, adapters.PrepareRequest{CapabilityID: capID, Fields: fields})
	return res, h.note(err)
}

func (h *host) commit(ctx context.Context, sc contracts.Scope, p act.PreparedAction) (act.DeterministicResult, error) {
	if h.down != nil {
		return act.DeterministicResult{}, errHostDown
	}
	res, err := h.ad.Commit(ctx, sc, adapters.CommitRequest{CapabilityID: string(p.CapabilityID), IdempotencyKey: p.IdempotencyKey, Prepared: p})
	return res, h.note(err)
}

func (h *host) status(ctx context.Context, sc contracts.Scope, capID string, key act.IdempotencyKey) (act.DeterministicResult, error) {
	if h.down != nil {
		return act.DeterministicResult{}, errHostDown
	}
	res, err := h.ad.Status(ctx, sc, adapters.StatusRequest{CapabilityID: capID, IdempotencyKey: key})
	return res, h.note(err)
}

func (h *host) cancel(ctx context.Context, sc contracts.Scope, capID string, key act.IdempotencyKey) (act.DeterministicResult, error) {
	if h.down != nil {
		return act.DeterministicResult{}, errHostDown
	}
	res, err := h.ad.Cancel(ctx, sc, adapters.CancelRequest{CapabilityID: capID, IdempotencyKey: key})
	return res, h.note(err)
}

// rt is the state of one check run: the outcome being built, what could not be
// probed and why, and what the check is allowed to touch.
type rt struct {
	opts     Options
	in       *integrationaudit.Input
	pkg      *apppkg.Package
	out      *integrationaudit.Outcome
	section  int
	h        *host
	prov     Provisioner
	changer  RecordChanger   // nil when the application cannot change a record on request
	injector FaultInjector   // nil when it cannot be made to fail on request
	stepUp   StepUpCompleter // nil when it cannot complete a step-up on request

	// caps lists the capabilities the check probes, in order: those the model is
	// ever offered (audience user).
	caps    []string
	covered map[string]bool
	skipped map[string]string // capability -> first reason it could not be probed
	cov     map[string]*[2]int
	ran     map[string]int // probes that ran, by capability (the scope check)
	extra   map[string]any

	rig    *rig
	rigErr error
}

// capMode says which capabilities a check is about.
type capMode int

const (
	// modeAll: every capability the model is offered.
	modeAll capMode = iota
	// modeConsequential: only what changes data. A read-only capability has nothing
	// to commit, and the check says so.
	modeConsequential
	// modeSmoke: only the capabilities the smoke paths of the suites run through.
	modeSmoke
)

// newRT makes the state of a check run.
func newRT(o Options, in *integrationaudit.Input, section int, mode capMode) *rt {
	r := &rt{
		opts: o, in: in, pkg: in.Package, out: &integrationaudit.Outcome{}, section: section,
		covered: map[string]bool{}, skipped: map[string]string{}, cov: map[string]*[2]int{}, ran: map[string]int{}, extra: map[string]any{},
	}
	if in.Adapter != nil {
		r.h = &host{ad: in.Adapter}
	}
	for _, id := range in.Package.SortedCapabilityIDs() {
		c := in.Package.Capabilities[id]
		switch {
		case c.Audience != ap.CapabilityAudienceUser:
			r.out.Verdicts = append(r.out.Verdicts, integrationaudit.CapabilityVerdict{
				CapabilityID: id, Status: integrationaudit.CapabilitySkipped,
				Reason: "an admin capability is never offered in a user conversation, so the runtime probes do not exercise it",
			})
		case mode == modeConsequential && !consequential(c):
			r.out.Verdicts = append(r.out.Verdicts, integrationaudit.CapabilityVerdict{
				CapabilityID: id, Status: integrationaudit.CapabilitySkipped,
				Reason: "a read-only capability changes nothing, so it has no commit to make safe",
			})
		case mode == modeSmoke && !onSmokePath(in.Package, id):
		default:
			r.caps = append(r.caps, id)
		}
	}
	return r
}

func (r *rt) spec(id string) ap.Capability { return r.pkg.Capabilities[id] }

// notProbed records that a capability could not be probed and why. Only the
// first reason is kept; the findings are raised once, by finish. A capability
// that another suite did probe completely is not affected.
func (r *rt) notProbed(capID, reason string) {
	if r.covered[capID] {
		return
	}
	if _, ok := r.skipped[capID]; !ok {
		r.skipped[capID] = reason
	}
}

// hasCriticalFor reports whether the check has already found a critical problem
// with a capability.
func (r *rt) hasCriticalFor(capID string) bool {
	for _, f := range r.out.Findings {
		if f.Capability == capID && f.Severity == contracts.SeverityCritical {
			return true
		}
	}
	return false
}

// probed records that a capability was exercised completely: whatever an earlier
// suite could not do for it no longer matters.
func (r *rt) probed(capID string) {
	r.covered[capID] = true
	delete(r.skipped, capID)
}

// count adds to a coverage figure of the report.
func (r *rt) count(name string, covered, total int) {
	c := r.cov[name]
	if c == nil {
		c = &[2]int{}
		r.cov[name] = c
	}
	c[0] += covered
	c[1] += total
}

// rigOf returns the platform pipeline of this check run, building it the first
// time. A failure to build it is remembered, so it is not retried for every
// capability.
func (r *rt) rigOf(ctx context.Context) (*rig, error) {
	if r.rig != nil {
		return r.rig, nil
	}
	if r.rigErr != nil {
		return nil, r.rigErr
	}
	g, err := newRig(ctx, r.opts, r.pkg, r.in.Adapter, r.in.Clock.Now())
	if err != nil {
		r.rigErr = fmt.Errorf("the platform's own coordinator could not be started for the audit (%s)", err.Error())
		return nil, r.rigErr
	}
	r.rig = g
	return g, nil
}

func (r *rt) closeRig() {
	r.rig.close()
	r.rig = nil
}

// gate decides whether the check can reach a disposable tenant at all. A reason
// of "" means it can.
func (r *rt) gate(ctx context.Context) string {
	switch {
	case r.in.Adapter == nil:
		return "no adapter was available for the application"
	case len(r.pkg.Tests) == 0:
		return "the package declares no test suite, so it names no disposable tenant"
	}
	prov, err := r.opts.source()(ctx, r.in)
	switch {
	case err != nil:
		return fmt.Sprintf("the provisioner for the application's disposable tenant could not be built (%s)", errKind(err))
	case prov == nil:
		return "the application's adapter offers no operation that creates a disposable tenant"
	}
	r.prov = timed{p: prov, d: provisionTimeout}
	if c, ok := prov.(RecordChanger); ok {
		r.changer = timedChanger{c: c, d: provisionTimeout}
	}
	if f, ok := prov.(FaultInjector); ok {
		r.injector = timedInjector{f: f, d: provisionTimeout}
	}
	if s, ok := prov.(StepUpCompleter); ok {
		r.stepUp = timedCompleter{c: s, d: provisionTimeout}
	}
	return ""
}

// blockAll records every capability as not probed for one reason: the check
// could not start.
func (r *rt) blockAll(reason string) {
	for _, id := range r.caps {
		r.notProbed(id, reason)
	}
	if len(r.caps) == 0 {
		r.out.Unverify(integrationaudit.Unverified{
			Subject: fmt.Sprintf("runtime_probes:section_%d", r.section), Section: r.section, Target: integrationaudit.TargetApplication, Reason: reason,
		})
	}
	r.finish()
}

// finish turns what could not be probed into verdicts, unverified items and
// findings, and the capabilities that were probed into covered ones. A
// consequential capability that could not be probed is a critical finding; a
// read-only one is a warning.
func (r *rt) finish() {
	// Fail closed: a capability the check neither completed nor gave a reason for
	// was not probed, unless a critical finding already says why it fails.
	for _, id := range r.caps {
		if r.covered[id] || r.skipped[id] != "" || r.hasCriticalFor(id) {
			continue
		}
		r.skipped[id] = "the check did not complete for it"
	}
	var skippedIDs []string
	for id := range r.skipped {
		skippedIDs = append(skippedIDs, id)
	}
	sort.Strings(skippedIDs)
	for _, id := range skippedIDs {
		reason := r.skipped[id]
		c := r.spec(id)
		r.out.Verdicts = append(r.out.Verdicts, integrationaudit.CapabilityVerdict{
			CapabilityID: id, Status: integrationaudit.CapabilityUnverified, Reason: reason,
		})
		r.out.Unverify(integrationaudit.Unverified{
			Subject: fmt.Sprintf("runtime_probes:section_%d", r.section), Capability: id, Section: r.section,
			Target: integrationaudit.TargetApplication, Reason: reason,
		})
		d := fdNotProbedMinor
		if consequential(c) {
			d = fdNotProbed
		}
		// The message does not name the check: the same reason found by several checks is
		// one finding, which is what the operator has to act on.
		r.out.Add(newFinding(d, id, "capability %s was not probed against a disposable tenant: %s", quote(id), reason))
	}
	for _, id := range r.caps {
		if r.covered[id] && r.skipped[id] == "" {
			r.out.Covered = append(r.out.Covered, id)
		}
	}
	for _, name := range sortedKeys(r.cov) {
		r.out.Coverage = append(r.out.Coverage, integrationaudit.CoverageItem{Name: name, Covered: r.cov[name][0], Total: r.cov[name][1]})
	}
	if len(r.extra) > 0 {
		r.out.Extension = r.extra
	}
}

// suiteCtx is one test suite of the package being run: its pool of disposable
// worlds and the outcome shared with the other suites.
type suiteCtx struct {
	r     *rt
	suite ap.PackageTests
	pool  *pool
}

// drive runs a check body once for every test suite of the package, with a pool of
// disposable worlds that is always cleaned up, whatever the body does.
func drive(ctx context.Context, o Options, in *integrationaudit.Input, section int, body func(sc *suiteCtx, ctx context.Context)) (*integrationaudit.Outcome, error) {
	return driveWith(ctx, o, in, section, modeAll, body)
}

// onSmokePath reports whether a smoke path of the package runs through the capability.
func onSmokePath(pkg *apppkg.Package, capID string) bool {
	for _, suite := range pkg.Tests {
		for _, p := range suite.Smoke.Paths {
			if p.CapabilityID != nil && string(*p.CapabilityID) == capID {
				return true
			}
		}
	}
	return false
}

// driveWith is drive for a check that is about some of the capabilities only.
func driveWith(ctx context.Context, o Options, in *integrationaudit.Input, section int, mode capMode, body func(sc *suiteCtx, ctx context.Context)) (*integrationaudit.Outcome, error) {
	r := newRT(o, in, section, mode)
	if reason := r.gate(ctx); reason != "" {
		r.blockAll(reason)
		return r.out, nil
	}
	defer r.closeRig()
	for _, suite := range in.Package.Tests {
		r.runSuite(ctx, suite, body)
	}
	r.finish()
	return r.out, nil
}

func (r *rt) runSuite(ctx context.Context, suite ap.PackageTests, body func(sc *suiteCtx, ctx context.Context)) {
	sc := &suiteCtx{r: r, suite: suite, pool: newPool(r.prov, suite, r.in.Scope, r.pkgCapabilityIDs(), r.in.Clock)}
	defer func() {
		// Cleanup runs even when the body panics: a disposable tenant is never
		// left behind by a failing probe.
		left := sc.pool.closeAll(ctx)
		r.reportLeftovers(string(suite.Suite), left)
	}()
	body(sc, ctx)
	sc.down() // what an outage cut short is not probed
}

// pkgCapabilityIDs lists every capability of the package, the capability scopes
// of the probe's context token.
func (r *rt) pkgCapabilityIDs() []string { return r.pkg.SortedCapabilityIDs() }

func (r *rt) reportLeftovers(suite string, left []leftover) {
	for _, l := range left {
		switch {
		case l.err != nil:
			r.out.Add(newFinding(fdLeftBehind, "", "the cleanup of fixture tenant %s of suite %s failed (%s), so the disposable tenant may still exist",
				quote(l.tenantRef), quote(suite), errKind(l.err)))
		case l.remaining > 0:
			r.out.Add(newFinding(fdLeftBehind, "", "after the cleanup of fixture tenant %s of suite %s the application still reports %d record(s) of it",
				quote(l.tenantRef), quote(suite), l.remaining))
		default:
			r.out.Add(newFinding(fdLeftBehind, "", "after the cleanup of fixture tenant %s of suite %s a second cleanup found the tenant still there",
				quote(l.tenantRef), quote(suite)))
		}
	}
}

// world returns a clean world of the suite, or records why none could be made:
// every capability is then not probed, because the fixtures could not be
// provisioned.
func (sc *suiteCtx) world(ctx context.Context) (*world, bool) {
	w, err := sc.pool.get(ctx)
	if err != nil {
		for _, id := range sc.r.caps {
			sc.r.notProbed(id, "the disposable tenant of suite "+quote(string(sc.suite.Suite))+" could not be provisioned: "+err.Error())
		}
		return nil, false
	}
	return w, true
}

// freshWorld returns a world no probe has touched.
func (sc *suiteCtx) freshWorld(ctx context.Context) (*world, bool) {
	sc.pool.markDirty()
	return sc.world(ctx)
}

// down reports whether the application stopped answering; when it did, the
// capabilities the check has not finished with are recorded as not probed.
func (sc *suiteCtx) down() bool {
	if sc.r.h.down == nil {
		return false
	}
	for _, id := range sc.r.caps {
		if !sc.r.covered[id] {
			sc.r.notProbed(id, "the application stopped answering (or the audit was cancelled), so the rest of the check was not attempted")
		}
	}
	return true
}

// untyped reports whether err is the answer of an application that did not send
// a typed DeterministicResult.
func untyped(err error) bool {
	return err != nil && contracts.HasCode(err, contracts.ErrSchemaInvalid)
}

// probe builds a probe result.
func probe(kind integrationaudit.ProbeKind, capID, subject, expected, observed string, passed bool) integrationaudit.ProbeResult {
	return integrationaudit.ProbeResult{Kind: kind, Capability: capID, Subject: subject, Expected: expected, Observed: observed, Passed: passed}
}

// codeOf renders the error code of a failed call, or "none".
func codeOf(err error) string {
	if c := contracts.CodeOf(err); c != "" {
		return string(c)
	}
	return "untyped_error"
}
