package lifecycle

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ErrHashMismatch is wrapped by the error VerifyArtifacts (and so Update) returns when
// a file's size or SHA-256 differs from the manifest.
var ErrHashMismatch = errors.New("lifecycle: artifact does not match the manifest hash")

// ArtifactSource opens the files of a revision by manifest name (a model directory, a
// sidecar download cache). Verification only reads; nothing here writes a file.
type ArtifactSource interface {
	Open(ctx context.Context, name string) (io.ReadCloser, error)
}

// PromotionGate is the extra check a staged revision must pass before it serves, for
// example the smoke or calibration gate of ADR 0031. A non-nil error keeps the staged
// revision out.
type PromotionGate interface {
	Check(ctx context.Context, staged Instance) error
}

// PromotionGateFunc adapts a function to a PromotionGate.
type PromotionGateFunc func(ctx context.Context, staged Instance) error

// Check implements PromotionGate.
func (f PromotionGateFunc) Check(ctx context.Context, staged Instance) error { return f(ctx, staged) }

// VerifyArtifacts streams every file the manifest lists out of src and compares its
// size and SHA-256 with the manifest. It fails closed: no files, a malformed hash, an
// unsafe name, an unreadable file, a duplicate name, or an encoder hash that no listed
// file carries are all errors. A mismatch wraps ErrHashMismatch.
func VerifyArtifacts(ctx context.Context, m gen.ScorerManifest, src ArtifactSource) error {
	if src == nil {
		return contracts.New(contracts.ErrValidationFailed, "lifecycle: no artifact source to verify against")
	}
	if len(m.Files) == 0 {
		return contracts.New(contracts.ErrValidationFailed, "lifecycle: the manifest lists no files to verify")
	}
	listed := map[string]bool{}
	names := map[string]bool{}
	for _, f := range m.Files {
		if !safeName(f.Name) || names[f.Name] || f.SizeBytes < 0 || !lowerHex64(string(f.SHA256)) {
			return contracts.New(contracts.ErrValidationFailed, "lifecycle: the manifest has an invalid file entry").
				WithDetails("file", f.Name)
		}
		names[f.Name] = true
		listed[string(f.SHA256)] = true
		rc, err := src.Open(ctx, f.Name)
		if err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "lifecycle: cannot read an artifact to verify").WithDetails("file", f.Name)
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(rc, f.SizeBytes+1))
		_ = rc.Close()
		if err != nil {
			return contracts.Wrap(contracts.ErrUnavailable, err, "lifecycle: cannot read an artifact to verify").WithDetails("file", f.Name)
		}
		if n != f.SizeBytes {
			return contracts.Wrap(contracts.ErrValidationFailed, ErrHashMismatch, "lifecycle: artifact size differs from the manifest").
				WithDetails("file", f.Name)
		}
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h.Sum(nil))), []byte(f.SHA256)) != 1 {
			return contracts.Wrap(contracts.ErrValidationFailed, ErrHashMismatch, "lifecycle: artifact hash differs from the manifest").
				WithDetails("file", f.Name)
		}
	}
	if m.Encoder.SHA256 != nil && !listed[string(*m.Encoder.SHA256)] {
		return contracts.Wrap(contracts.ErrValidationFailed, ErrHashMismatch, "lifecycle: the encoder hash matches no verified file")
	}
	return nil
}

func safeName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, `/\:`) && !strings.Contains(n, "..")
}

func lowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// UpdateStatus is the outcome of Update.
type UpdateStatus string

// The update outcomes.
const (
	// UpdateRejected: nothing was loaded or switched (hash mismatch, resource gate).
	UpdateRejected UpdateStatus = "rejected"
	// UpdateRolledBack: the staged revision failed to load, to pass its health probe
	// or to pass the promotion gate, was discarded, and the previous revision still serves.
	UpdateRolledBack UpdateStatus = "rolled_back"
	// UpdateProbation: the new revision serves and the previous one is kept until
	// Promote, the end of Config.Probation, or a rollback.
	UpdateProbation UpdateStatus = "probation"
	// UpdatePromoted: the new revision serves and the previous one is gone.
	UpdatePromoted UpdateStatus = "promoted"
)

// UpdateReport says what Update did. Reason is a fixed word (signature, hash_mismatch,
// verification, resource_gate, load, health_probe, gate), never raw error text.
type UpdateReport struct {
	From, To string
	Status   UpdateStatus
	Reason   string
}

// Update stages next beside the serving revision and switches to it only when it is
// safe:
//
//  0. the manifest signature is verified through the trust store (Deps.Verifier) with its
//     validity window; an unsigned, forged, expired or unverifiable manifest is rejected
//     before any hash in it is believed (Reason "signature");
//  1. every file is verified against the manifest hashes (nothing is loaded on a mismatch);
//  2. the resource gate admits the second copy (a rejection leaves the serving revision
//     untouched and evicts nothing);
//  3. the staged revision loads and passes its health probe and, if given, pg;
//  4. the supervisor switches atomically. With Config.Probation > 0 the previous
//     revision stays resident until the probation ends, so a failed probe (Tick) or a
//     reported gate breach (Rollback) returns to it; otherwise the switch is final.
//
// A failure at any step keeps the previous revision serving and is reported with a
// typed error. A model revision change needs no cache purge: the Engine keys its cache
// by the manifest.
func (s *Supervisor) Update(ctx context.Context, next Revision, src ArtifactSource, pg PromotionGate) (UpdateReport, error) {
	s.mu.Lock()
	rep := UpdateReport{From: string(s.rev.Manifest.ModelRevision), To: string(next.Manifest.ModelRevision)}
	switch {
	case s.stopped:
		s.mu.Unlock()
		return rep, contracts.New(contracts.ErrUnavailable, "lifecycle: the scorer supervisor is stopped")
	case s.updating || s.prob != nil:
		s.mu.Unlock()
		return rep, contracts.New(contracts.ErrConflict, "lifecycle: an update is in progress or in probation; promote or roll it back first")
	}
	if err := next.valid(); err != nil {
		s.mu.Unlock()
		rep.Status, rep.Reason = UpdateRejected, "verification"
		return rep, err
	}
	if next.Manifest.ModelRevision == s.rev.Manifest.ModelRevision && next.Manifest.ModelID == s.rev.Manifest.ModelID {
		s.mu.Unlock()
		rep.Status, rep.Reason = UpdateRejected, "verification"
		return rep, contracts.New(contracts.ErrValidationFailed, "lifecycle: the revision is already the serving one")
	}
	s.updating = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.updating = false
		s.mu.Unlock()
	}()

	// The signature comes first: the file hashes below come from the same manifest, so they
	// are believed only once the manifest itself is.
	if err := s.verify.Verify(ctx, next.Manifest, next.SignatureWindow); err != nil {
		rep.Status, rep.Reason = UpdateRejected, "signature"
		s.met.event(MetricRollbacks, rep.Reason, next)
		return rep, err
	}
	if err := VerifyArtifacts(ctx, next.Manifest, src); err != nil {
		rep.Status, rep.Reason = UpdateRejected, "verification"
		if errors.Is(err, ErrHashMismatch) {
			rep.Reason = "hash_mismatch"
		}
		s.met.event(MetricRollbacks, rep.Reason, next)
		return rep, err
	}
	res, gerr := s.gate.Admit(ctx, next.PeakBytes, stagedGateLabel)
	if gerr != nil {
		rep.Status, rep.Reason = UpdateRejected, "resource_gate"
		s.met.event(MetricRollbacks, rep.Reason, next)
		return rep, gateRejection(gerr)
	}
	discard := func(inst Instance, reason string, err error) (UpdateReport, error) {
		s.closeInstance(inst)
		res.Release()
		rep.Status, rep.Reason = UpdateRolledBack, reason
		s.met.event(MetricRollbacks, reason, next)
		return rep, err
	}

	lctx, cancel := context.WithTimeout(ctx, s.cfg.LoadTimeout)
	defer cancel()
	inst, err := s.loader.Load(lctx, next)
	if err == nil {
		err = checkIdentity(next, inst)
	}
	if err != nil {
		return discard(inst, "load", contracts.Wrap(contracts.ErrUnavailable, err, "lifecycle: the staged revision failed to load; the previous revision keeps serving"))
	}
	if err := s.probe(lctx, inst); err != nil {
		return discard(inst, "health_probe", contracts.Wrap(contracts.ErrUnavailable, err, "lifecycle: the staged revision failed its health probe; the previous revision keeps serving"))
	}
	if pg != nil {
		if err := pg.Check(lctx, inst); err != nil {
			return discard(inst, "gate", contracts.Wrap(contracts.ErrValidationFailed, err, "lifecycle: the staged revision failed the promotion gate; the previous revision keeps serving"))
		}
	}

	// Switch. A load of the old revision may be in flight; wait for it to land so the
	// slot being replaced is the one that is resident.
	for {
		s.mu.Lock()
		if s.state != stateBusy {
			break
		}
		ch := s.busy
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return discard(inst, "load", contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "lifecycle: gave up waiting to switch revisions"))
		}
	}
	if s.stopped {
		s.mu.Unlock()
		return discard(inst, "load", contracts.New(contracts.ErrUnavailable, "lifecycle: the scorer supervisor is stopped"))
	}
	now := s.clk.Now()
	prevRev := s.rev
	var prev *slot
	if s.state == stateReady {
		prev = s.cur
	}
	s.cur = &slot{rev: next, inst: inst, res: res, loadedAt: now}
	s.rev = next
	s.state = stateReady
	s.lastUsed = now
	s.probeOK = true
	s.lastErr = ""
	s.failures = 0
	if s.cfg.Probation > 0 {
		s.prob = &probation{prevRev: prevRev, prev: prev, until: now.Add(s.cfg.Probation)}
		rep.Status = UpdateProbation
	} else {
		if prev != nil {
			s.retired = append(s.retired, prev)
		}
		rep.Status = UpdatePromoted
	}
	s.mu.Unlock()
	s.met.event(MetricLoads, "update", next)
	s.drain(ctx)
	return rep, nil
}

// Promote ends the probation early: the previous revision is dropped.
func (s *Supervisor) Promote(ctx context.Context) error {
	s.mu.Lock()
	if s.prob == nil {
		s.mu.Unlock()
		return contracts.New(contracts.ErrConflict, "lifecycle: no update is in probation")
	}
	s.promoteLocked()
	s.mu.Unlock()
	s.drain(ctx)
	return nil
}

// Rollback abandons a revision that is in probation because a gate was breached (the
// gate metrics of ADR 0031 regressed): the previous revision serves again.
func (s *Supervisor) Rollback(ctx context.Context) error {
	s.mu.Lock()
	if s.prob == nil {
		s.mu.Unlock()
		return contracts.New(contracts.ErrConflict, "lifecycle: no update is in probation to roll back")
	}
	if s.state == stateBusy {
		s.mu.Unlock()
		return contracts.New(contracts.ErrConflict, "lifecycle: busy loading; retry")
	}
	s.rollbackLocked("gate_breach")
	s.mu.Unlock()
	s.drain(ctx)
	return nil
}

func (s *Supervisor) promoteLocked() {
	if p := s.prob; p != nil && p.prev != nil {
		s.retired = append(s.retired, p.prev)
	}
	s.prob = nil
}

// rollbackLocked returns from the revision in probation to the previous one. The
// previous instance, kept resident, serves at once; when nothing was resident the
// previous revision is simply demand-loaded again.
func (s *Supervisor) rollbackLocked(reason string) {
	p := s.prob
	s.prob = nil
	if bad := s.cur; bad != nil {
		bad.force = true
		s.retired = append(s.retired, bad)
	}
	s.met.event(MetricRollbacks, reason, s.rev)
	s.rev = p.prevRev
	s.failures = 0
	s.nextRetry = s.clk.Now()
	if p.prev != nil {
		s.cur = p.prev
		s.state = stateReady
		s.probeOK = true
		s.lastUsed = s.clk.Now()
		return
	}
	s.cur = nil
	s.state = stateUnloaded
	s.probeOK = false
}
