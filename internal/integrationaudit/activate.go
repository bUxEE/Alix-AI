package integrationaudit

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The reasons an activation is refused, in Details["reason"] of the error.
const (
	ReasonNoReport         = "no_report"
	ReasonCriticalFindings = "critical_findings"
	ReasonWarningsPending  = "warnings_not_accepted"
	ReasonStaleReport      = "stale_report"
	ReasonNewerAudit       = "newer_audit"
	ReasonReportMismatch   = "report_mismatch"
)

var (
	operatorRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:-]{0,127}$`)
	findingKeyRE = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

const maxReasonLen = 500

// StoredRun is a run as the database holds it.
type StoredRun struct {
	// Seq orders the runs of a package: the newest has the highest.
	Seq    int64
	RunID  string
	Signed SignedReport
}

// LatestRun returns the newest stored run for a package, with ok false when
// the package has never been audited. The signature is not verified here;
// Activate and AcceptWarning verify what they rely on.
func (r *Runner) LatestRun(ctx context.Context, applicationID, manifestHash string) (run StoredRun, ok bool, err error) {
	err = r.cfg.DB.View(ctx, func(tx *store.Tx) error {
		run, ok, err = latestRunTx(ctx, tx, applicationID, manifestHash)
		return err
	})
	if err != nil {
		return StoredRun{}, false, wrapStore(err, "cannot read the audit report")
	}
	return run, ok, nil
}

func latestRunTx(ctx context.Context, tx *store.Tx, app, hash string) (StoredRun, bool, error) {
	var (
		run StoredRun
		doc string
		sha string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT seq, run_id, report_sha256, document FROM integrationaudit_runs
		  WHERE application_id = ? AND manifest_hash = ? ORDER BY seq DESC LIMIT 1`, app, hash).
		Scan(&run.Seq, &run.RunID, &sha, &doc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return StoredRun{}, false, nil
	case err != nil:
		return StoredRun{}, false, err
	}
	signed, perr := ParseSignedReport([]byte(doc))
	if perr != nil {
		return StoredRun{}, false, perr
	}
	// The row's own digest column must agree with the document; the signature
	// check at activation is what proves the document, this catches a swapped
	// row early.
	if signed.Attestation.ReportSHA256 != sha {
		return StoredRun{}, false, contracts.New(contracts.ErrInternal, "integrationaudit: a stored report does not match its recorded digest")
	}
	run.Signed = signed
	return run, true, nil
}

// Acceptance is a warning an authorised operator accepted for a package.
type Acceptance struct {
	ApplicationID string
	ManifestHash  string
	FindingKey    string
	CheckID       string
	Capability    string
	OperatorID    string
	Reason        string
	// RunID is the run whose report contained the finding when it was accepted.
	RunID      string
	AcceptedAt time.Time
	// Created is false when the warning had already been accepted: the stored
	// acceptance is returned unchanged.
	Created bool
}

// Acceptances returns the warnings accepted for a package, by finding key.
func (r *Runner) Acceptances(ctx context.Context, applicationID, manifestHash string) (map[string]Acceptance, error) {
	out := map[string]Acceptance{}
	err := r.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = acceptancesTx(ctx, tx, applicationID, manifestHash)
		return err
	})
	if err != nil {
		return nil, wrapStore(err, "cannot read the accepted warnings")
	}
	return out, nil
}

func acceptancesTx(ctx context.Context, tx *store.Tx, app, hash string) (map[string]Acceptance, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT finding_key, check_id, capability, operator_id, reason, run_id, accepted_at
		   FROM integrationaudit_acceptances WHERE application_id = ? AND manifest_hash = ?`, app, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Acceptance{}
	for rows.Next() {
		a := Acceptance{ApplicationID: app, ManifestHash: hash}
		var at int64
		if err := rows.Scan(&a.FindingKey, &a.CheckID, &a.Capability, &a.OperatorID, &a.Reason, &a.RunID, &at); err != nil {
			return nil, err
		}
		a.AcceptedAt = time.UnixMilli(at).UTC()
		out[a.FindingKey] = a
	}
	return out, rows.Err()
}

// AcceptRequest accepts one warning of a package's latest report.
type AcceptRequest struct {
	ApplicationID string
	// ManifestHash picks the package; empty means the application's newest
	// quarantined package.
	ManifestHash string
	// FindingKey is the key of the warning in the report (KeyedFinding.Key).
	FindingKey string
	// OperatorID is who accepts. It is checked by the Runner's Authorizer and
	// recorded with the time and the reason.
	OperatorID string
	// Reason says why the warning is acceptable. Required.
	Reason string
}

// AcceptWarning records that an authorised operator accepted a warning of the
// package's latest signed report.
//
// Acceptance only ever removes the need to have a warning accepted before
// activation. It never widens anything: the record has no member that any
// capability, policy or scope decision reads, a critical finding can never be
// accepted, and an acceptance is bound to this package and this exact finding,
// so another package, or the same warning with another message, needs its own.
//
// The operator must be authorised by the Runner's Authorizer (deny by default),
// must be named, and must give a reason. The acceptance and its audit event are
// written in one transaction.
func (r *Runner) AcceptWarning(ctx context.Context, req AcceptRequest) (*Acceptance, error) {
	op := req.OperatorID
	switch {
	case !operatorRE.MatchString(op):
		return nil, contracts.New(contracts.ErrValidationFailed, "integrationaudit: an acceptance needs the identity of an operator (1 to 128 letters, digits and ._@:-)")
	case !findingKeyRE.MatchString(req.FindingKey):
		return nil, contracts.New(contracts.ErrValidationFailed, "integrationaudit: the finding key is 16 lower-case hex characters")
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxReasonLen || !utf8.ValidString(reason) {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "integrationaudit: an acceptance needs a reason of 1 to %d characters", maxReasonLen)
	}
	if err := r.auth.AuthorizeAcceptance(ctx, op, req.ApplicationID); err != nil {
		return nil, err
	}
	rec, err := r.resolve(ctx, req.ApplicationID, req.ManifestHash)
	if err != nil {
		return nil, err
	}
	if rec.State != apppkg.StateQuarantined {
		return nil, contracts.Newf(contracts.ErrConflict, "integrationaudit: the package is %s: only a quarantined package has warnings to accept", rec.State)
	}
	run, ok, err := r.LatestRun(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refusal(ReasonNoReport, "the package has not been audited")
	}
	if _, err := VerifyReport(ctx, r.cfg.Verifier, run.Signed); err != nil {
		return nil, err
	}
	var found *KeyedFinding
	for i := range run.Signed.Report.Findings {
		if run.Signed.Report.Findings[i].Key == req.FindingKey {
			found = &run.Signed.Report.Findings[i]
			break
		}
	}
	switch {
	case found == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "integrationaudit: the latest report of the package has no finding with that key")
	case found.Severity != contracts.SeverityWarning:
		return nil, contracts.Newf(contracts.ErrValidationFailed,
			"integrationaudit: only a warning can be accepted; this finding is %s", string(found.Severity))
	}

	loc, err := audit.ForApplication(rec.ApplicationID, op)
	if err != nil {
		return nil, err
	}
	acc := Acceptance{
		ApplicationID: rec.ApplicationID, ManifestHash: rec.ManifestHash, FindingKey: req.FindingKey,
		CheckID: found.CheckID, Capability: found.Capability, OperatorID: op, Reason: reason, RunID: run.RunID,
		AcceptedAt: r.clk.Now().UTC().Truncate(time.Millisecond), Created: true,
	}
	err = r.cfg.DB.Tx(ctx, func(tx *store.Tx) error {
		existing, err := acceptancesTx(ctx, tx, rec.ApplicationID, rec.ManifestHash)
		if err != nil {
			return err
		}
		if e, dup := existing[req.FindingKey]; dup {
			e.Created = false
			acc = e
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO integrationaudit_acceptances
			   (application_id, manifest_hash, finding_key, check_id, capability, operator_id, reason, run_id, accepted_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			acc.ApplicationID, acc.ManifestHash, acc.FindingKey, acc.CheckID, acc.Capability, acc.OperatorID,
			acc.Reason, acc.RunID, acc.AcceptedAt.UnixMilli()); err != nil {
			return err
		}
		_, err = r.cfg.Log.AppendTx(ctx, tx, loc, audit.NewEvent{
			Type: audit.EventAuditRunWarningAccepted,
			Data: map[string]any{
				"run_id": acc.RunID, "manifest_hash": acc.ManifestHash, "finding_key": acc.FindingKey,
				"check_id": acc.CheckID, "capability": acc.Capability, "operator": acc.OperatorID, "reason": acc.Reason,
			},
		})
		return err
	})
	if err != nil {
		return nil, wrapStore(err, "cannot record the acceptance")
	}
	return &acc, nil
}

// refusal builds the error that says an activation was refused and why.
func refusal(reason, message string) *contracts.Error {
	return contracts.New(contracts.ErrQuarantined, "integrationaudit: the package stays in quarantine: "+message).
		WithDetails("reason", reason)
}

// ActivateRequest asks for a package to be activated.
type ActivateRequest struct {
	ApplicationID string
	// ManifestHash picks the package; empty means the application's newest
	// quarantined package.
	ManifestHash string
	// OperatorID is who activates, for the audit log. Empty means the system.
	OperatorID string
}

// Activation is the result of Activate.
type Activation struct {
	// Record is the package after the transition.
	Record apppkg.Record
	// RunID is the audit run whose report cleared it; it is also the
	// audit_report_id stored with the package.
	RunID string
	// Superseded is the manifest hash of the package this one replaced, or "".
	Superseded string
	// AcceptedWarnings are the warnings the activation relied on.
	AcceptedWarnings []Acceptance
}

// Activate moves a quarantined package to active. It is the only code that
// does: nothing in apppkg can, and the table refuses an activation without an
// audit report.
//
// A package is activated only when all of these hold, checked in this order and
// again inside the transaction that changes it:
//
//  1. it is quarantined, and its stored bytes still verify (a key revoked or a
//     file changed since the audit fails here, closed);
//  2. it has a stored report, and the report is the newest of the package: an
//     earlier clean report never outlives a later audit that found problems;
//  3. the report verifies: the platform signature, the digest of the body,
//     the binding to this application and package;
//  4. the report is not older than Config.MaxReportAge;
//  5. the report has no critical finding, and every warning in it was accepted
//     by an authorised operator for this package.
//
// The package it replaces, if any, becomes superseded in the same transaction,
// and the activation is recorded in the audit log in that transaction, so it
// happens with its record or not at all. After the commit the capabilities
// registry is refreshed, so the activation takes effect at once and not after
// its cache expires; if that fails the error says so and the registry serves
// nothing for the application (fail closed).
func (r *Runner) Activate(ctx context.Context, req ActivateRequest) (*Activation, error) {
	rec, err := r.resolve(ctx, req.ApplicationID, req.ManifestHash)
	if err != nil {
		return nil, err
	}
	if rec.State == apppkg.StateActive {
		return nil, contracts.New(contracts.ErrConflict, "integrationaudit: the package is already active")
	}
	if rec.State != apppkg.StateQuarantined {
		return nil, contracts.Newf(contracts.ErrConflict, "integrationaudit: the package is %s, which is final", rec.State)
	}
	// 1. The stored bytes verify now.
	if _, _, err := r.cfg.Installer.Open(ctx, rec.ApplicationID, rec.ManifestHash); err != nil {
		return nil, contracts.Wrap(contracts.ErrQuarantined, err, "integrationaudit: the package no longer verifies and stays in quarantine").
			WithDetails("reason", "package_unverified")
	}
	// 2 and 3. The newest report, verified.
	run, ok, err := r.LatestRun(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refusal(ReasonNoReport, "the package has not been audited")
	}
	verified, err := VerifyReport(ctx, r.cfg.Verifier, run.Signed)
	if err != nil {
		return nil, err
	}
	rep := run.Signed.Report
	if rep.Subject.ApplicationID != rec.ApplicationID || rep.Subject.ManifestHash != rec.ManifestHash {
		return nil, refusal(ReasonReportMismatch, "the report is about another package")
	}
	// 4. Fresh, and made since the package last entered quarantine: a report
	// that predates a quarantine is not evidence about the package as it is
	// now. (The comparison is at the one-second granularity of the attestation.)
	now := r.clk.Now().UTC()
	if age := now.Sub(verified.IssuedAt); age > r.cfg.MaxReportAge || age < -trust.ClockSkew {
		return nil, refusal(ReasonStaleReport, "the report is older than the maximum report age; audit the package again")
	}
	if verified.IssuedAt.Before(rec.StatusChangedAt.Truncate(time.Second)) {
		return nil, refusal(ReasonStaleReport, "the package was quarantined after the report was made; audit the package again")
	}
	// 5. No critical finding, every warning accepted.
	accepted, err := r.Acceptances(ctx, rec.ApplicationID, rec.ManifestHash)
	if err != nil {
		return nil, err
	}
	relied, err := gate(rep, accepted)
	if err != nil {
		return nil, err
	}

	loc, err := audit.ForApplication(rec.ApplicationID, req.OperatorID)
	if err != nil {
		return nil, err
	}
	var superseded string
	err = r.cfg.DB.Tx(ctx, func(tx *store.Tx) error {
		superseded = ""
		// The newest report may have changed since it was read.
		cur, ok, err := latestRunTx(ctx, tx, rec.ApplicationID, rec.ManifestHash)
		if err != nil {
			return err
		}
		if !ok || cur.Seq != run.Seq {
			return refusal(ReasonNewerAudit, "the package was audited again while it was being activated")
		}
		changed := r.clk.Now().UTC().UnixMilli()
		var current string
		err = tx.QueryRowContext(ctx,
			`SELECT manifest_hash FROM apppkg_packages WHERE application_id = ? AND status = 'active'`, rec.ApplicationID).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE apppkg_packages SET status = 'superseded', status_changed_at = ?, status_reason = ?
				  WHERE application_id = ? AND manifest_hash = ? AND status = 'active'`,
				changed, "superseded by "+rec.ManifestHash, rec.ApplicationID, current); err != nil {
				return err
			}
			superseded = current
			if _, err := r.cfg.Log.AppendTx(ctx, tx, loc, audit.NewEvent{
				Type: audit.EventPackageDeactivated,
				Data: map[string]any{"manifest_hash": current, "reason": "superseded", "superseded_by": rec.ManifestHash},
			}); err != nil {
				return err
			}
		}
		// Still the quarantine the report was checked against: one that came since
		// (an activation and a new quarantine in between) is newer than the report.
		res, err := tx.ExecContext(ctx,
			`UPDATE apppkg_packages SET status = 'active', audit_report_id = ?, status_changed_at = ?, status_reason = ?
			  WHERE application_id = ? AND manifest_hash = ? AND status = 'quarantined' AND status_changed_at = ?`,
			run.RunID, changed, "activated by integration audit "+run.RunID, rec.ApplicationID, rec.ManifestHash, rec.StatusChangedAt.UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return contracts.New(contracts.ErrConflict, "integrationaudit: the package is no longer in the quarantine its report was checked against")
		}
		keys := make([]string, 0, len(relied))
		for _, a := range relied {
			keys = append(keys, a.FindingKey)
		}
		_, err = r.cfg.Log.AppendTx(ctx, tx, loc, audit.NewEvent{
			Type: audit.EventPackageActivated,
			Data: map[string]any{
				"manifest_hash": rec.ManifestHash, "package_version": rec.PackageVersion, "run_id": run.RunID,
				"report_sha256": verified.ReportSHA256, "accepted_warnings": keys,
			},
		})
		return err
	})
	if err != nil {
		return nil, wrapStore(err, "cannot activate the package")
	}

	act := &Activation{RunID: run.RunID, Superseded: superseded, AcceptedWarnings: relied}
	if after, gerr := r.cfg.Installer.Get(ctx, rec.ApplicationID, rec.ManifestHash); gerr == nil {
		act.Record = after
	}
	if reg := r.cfg.Capabilities; reg != nil {
		if err := reg.Refresh(ctx, rec.ApplicationID); err != nil {
			// Refresh has already dropped the application from the registry.
			return act, contracts.Wrap(contracts.ErrUnavailable, err,
				"integrationaudit: the package is active but the capability registry could not load it; it serves nothing until it can")
		}
	}
	return act, nil
}

// gate decides whether a report permits activation given the accepted
// warnings, and returns the acceptances it relied on. The counts are recomputed
// from the findings, not read from the summary.
func gate(rep Report, accepted map[string]Acceptance) ([]Acceptance, error) {
	var crit int
	var pending []string
	var relied []Acceptance
	for _, f := range rep.Findings {
		switch f.Severity.Rank() {
		case 0:
			crit++
		case 1:
			if a, ok := accepted[f.Key]; ok {
				relied = append(relied, a)
			} else {
				pending = append(pending, f.Key)
			}
		}
	}
	if failed := failedCapabilities(rep); crit > 0 || len(failed) > 0 {
		return nil, refusal(ReasonCriticalFindings, "the audit found critical problems; fix them and audit again").
			WithDetails("critical", crit).WithDetails("failed_capabilities", nz(failed))
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		return nil, refusal(ReasonWarningsPending, "warnings are not accepted by an authorised operator").
			WithDetails("pending", pending)
	}
	sort.Slice(relied, func(i, j int) bool { return relied[i].FindingKey < relied[j].FindingKey })
	return relied, nil
}

// Quarantine takes an active package out of service: it becomes quarantined
// again, with its audit report cleared, and the capability registry stops
// serving it at once. A package that is not active is a conflict. It is how a
// re-audit that finds a critical problem stops live traffic (M5-10); the
// package can be activated again only by a new clean report.
func (r *Runner) Quarantine(ctx context.Context, applicationID, manifestHash, reason, operatorID string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxReasonLen {
		return contracts.Newf(contracts.ErrValidationFailed, "integrationaudit: quarantining needs a reason of 1 to %d characters", maxReasonLen)
	}
	rec, err := r.resolveExact(ctx, applicationID, manifestHash)
	if err != nil {
		return err
	}
	loc, err := audit.ForApplication(rec.ApplicationID, operatorID)
	if err != nil {
		return err
	}
	err = r.cfg.DB.Tx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE apppkg_packages SET status = 'quarantined', audit_report_id = NULL, status_changed_at = ?, status_reason = ?
			  WHERE application_id = ? AND manifest_hash = ? AND status = 'active'`,
			r.clk.Now().UTC().UnixMilli(), reason, rec.ApplicationID, rec.ManifestHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return contracts.New(contracts.ErrConflict, "integrationaudit: the package is not active")
		}
		_, err = r.cfg.Log.AppendTx(ctx, tx, loc, audit.NewEvent{
			Type: audit.EventPackageQuarantined,
			Data: map[string]any{"manifest_hash": rec.ManifestHash, "reason": reason},
		})
		return err
	})
	if err != nil {
		return wrapStore(err, "cannot quarantine the package")
	}
	if reg := r.cfg.Capabilities; reg != nil {
		reg.Deactivate(rec.ApplicationID)
	}
	return nil
}

// resolveExact is resolve for a call that must name the package.
func (r *Runner) resolveExact(ctx context.Context, app, hash string) (apppkg.Record, error) {
	if hash == "" {
		return apppkg.Record{}, contracts.New(contracts.ErrScopeMissing, "integrationaudit: no package given")
	}
	return r.resolve(ctx, app, hash)
}
