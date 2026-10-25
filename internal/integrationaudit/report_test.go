package integrationaudit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// SEC-088: an integration-audit report cannot be forged or altered, and
// activation cannot rely on an unsigned one.
func TestAuditReportSignedAndVerifiable(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	// a blocked report, so that altering it into a clean one is worth trying
	run := h.audit(newStub(t, func(s *stub) {
		s.desc.Capabilities = s.desc.Capabilities[:1] // reservations.create is gone
	}))
	if run.Disposition() != DispositionBlocked {
		t.Fatalf("fixture: want a blocked report, got %s", run.Disposition())
	}
	verifier := h.cfg.Verifier

	t.Run("a stored report verifies", func(t *testing.T) {
		stored, ok, err := h.runner.LatestRun(ctx, appID, run.Record.ManifestHash)
		if err != nil || !ok {
			t.Fatalf("LatestRun: %v %v", ok, err)
		}
		v, err := VerifyReport(ctx, verifier, stored.Signed)
		if err != nil {
			t.Fatalf("VerifyReport: %v", err)
		}
		if v.Signer.KeyID != platformKeyID || !v.IssuedAt.Equal(t0) {
			t.Errorf("verified = %+v, want signer %s issued %s", v, platformKeyID, t0)
		}
		sha, _ := Digest(stored.Signed.Report)
		if sha != v.ReportSHA256 || sha != run.Signed.Attestation.ReportSHA256 {
			t.Errorf("the digest the attestation names is the digest of the report")
		}
		if stored.RunID != run.RunID {
			t.Errorf("run id %s != %s", stored.RunID, run.RunID)
		}
		// the attestation binds the application and package
		if a := stored.Signed.Attestation; a.ApplicationID != appID || a.ManifestHash != run.Record.ManifestHash || a.Format != AttestationFormat {
			t.Errorf("attestation = %+v", a)
		}
		// the stored form is canonical and round-trips to the same bytes
		doc, _ := stored.Signed.Marshal()
		again, err := ParseSignedReport(doc)
		if err != nil {
			t.Fatal(err)
		}
		doc2, _ := again.Marshal()
		if !bytes.Equal(doc, doc2) {
			t.Errorf("a signed report must survive storage byte for byte")
		}
		if _, err := VerifyReport(ctx, verifier, again); err != nil {
			t.Errorf("the reparsed report must verify: %v", err)
		}
	})

	mustFail := func(t *testing.T, s SignedReport, why string) {
		t.Helper()
		_, err := VerifyReport(ctx, verifier, s)
		if err == nil {
			t.Fatalf("%s: the report verified", why)
		}
		if c := contracts.CodeOf(err); c != contracts.ErrUnauthenticated && c != contracts.ErrExpired && c != contracts.ErrSchemaInvalid {
			t.Errorf("%s: code %s, want a refusal of the document", why, c)
		}
	}
	cloneReport := func() SignedReport {
		b, err := run.Signed.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		s, err := ParseSignedReport(b)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("an edited body fails", func(t *testing.T) {
		s := cloneReport()
		s.Report.Summary.Outcome = DispositionClear
		s.Report.Summary.Critical = 0
		mustFail(t, s, "outcome rewritten to clear")

		s = cloneReport()
		kept := s.Report.Findings[:0]
		for _, f := range s.Report.Findings {
			if f.Severity != contracts.SeverityCritical {
				kept = append(kept, f)
			}
		}
		s.Report.Findings = kept
		mustFail(t, s, "critical findings removed")

		s = cloneReport()
		s.Report.Subject.PackageVersion = "9.9.9"
		mustFail(t, s, "package version rewritten")
	})

	t.Run("an attestation about another package fails", func(t *testing.T) {
		s := cloneReport()
		s.Attestation.ManifestHash = strings.Repeat("a", 64)
		mustFail(t, s, "attestation moved to another package")
		s = cloneReport()
		s.Attestation.ApplicationID = "other-app"
		mustFail(t, s, "attestation moved to another application")
	})

	t.Run("no signature, or a malformed one, fails", func(t *testing.T) {
		s := cloneReport()
		s.Attestation.Signatures = nil
		mustFail(t, s, "unsigned")
		s = cloneReport()
		s.Attestation.Signatures[0].Signature = "AAAA"
		mustFail(t, s, "garbled signature")
		s = cloneReport()
		s.Attestation.Format = "alixai.audit-checkpoint/v1"
		mustFail(t, s, "another document format")
	})

	t.Run("a key registered for the host application does not sign reports", func(t *testing.T) {
		hostPub, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
		if _, err := h.trust.RegisterKey(ctx, appID, "host-ctx-key", hostPub, "a host application key"); err != nil {
			t.Fatal(err)
		}
		s := cloneReport()
		sig, err := trust.SignPayload(s.Attestation.payload(), "host-ctx-key", hostPriv, t0, t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		s.Attestation.Signatures = []trust.Signature{sig}
		mustFail(t, s, "signed by a key registered for the host application")

		// not even the publisher key the package itself is signed with
		sig, err = trust.SignPayload(s.Attestation.payload(), devkeys.KeyID, devkeys.PrivateKey(), t0, t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		s.Attestation.Signatures = []trust.Signature{sig}
		mustFail(t, s, "signed by the package's own publisher key")
	})

	t.Run("a platform signature over another kind of document does not verify as an attestation", func(t *testing.T) {
		s := cloneReport()
		// The same platform key, over a payload with the same members but the
		// format of another document the platform signs.
		other := attestationPayload{
			Format: "alixai.audit-checkpoint/v1", ReportSHA256: s.Attestation.ReportSHA256,
			ApplicationID: s.Attestation.ApplicationID, ManifestHash: s.Attestation.ManifestHash, IssuedAt: s.Attestation.IssuedAt,
		}
		sig, err := trust.SignPayload(other, platformKeyID, h.priv, t0, t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		s.Attestation.Signatures = []trust.Signature{sig}
		mustFail(t, s, "signature over a checkpoint-format payload")

		// and a platform signature over a payload that names another report
		s = cloneReport()
		wrong := s.Attestation.payload()
		wrong.ReportSHA256 = strings.Repeat("b", 64)
		sig, _ = trust.SignPayload(wrong, platformKeyID, h.priv, t0, t0.Add(time.Hour))
		s.Attestation.Signatures = []trust.Signature{sig}
		mustFail(t, s, "signature over the digest of another report")
	})

	t.Run("the issue time is bound to the signature", func(t *testing.T) {
		s := cloneReport()
		s.Attestation.IssuedAt = t0.Add(10 * time.Hour).Format(timeLayout)
		mustFail(t, s, "issued_at changed after signing")
	})

	t.Run("the signature is dated when the audit was made", func(t *testing.T) {
		// signed_at is not covered by the signature: it must be the issue time that is
		s := cloneReport()
		s.Attestation.Signatures[0].SignedAt = t0.Add(-time.Hour).Format(timeLayout)
		mustFail(t, s, "signed_at moved back after signing")

		// and a holder of the key cannot date a signature away from the audit either,
		// as one whose key was retired since would
		s = cloneReport()
		sig, err := trust.SignPayload(s.Attestation.payload(), platformKeyID, h.priv, t0.Add(-24*time.Hour), t0.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		s.Attestation.Signatures = []trust.Signature{sig}
		mustFail(t, s, "signed a day before the audit it attests")
	})

	t.Run("an expired signature fails", func(t *testing.T) {
		h.clk.Advance(DefaultReportValidity + time.Hour)
		defer h.clk.Set(t0)
		_, err := VerifyReport(ctx, verifier, cloneReport())
		if !contracts.HasCode(err, contracts.ErrExpired) {
			t.Errorf("an expired report signature: %v", err)
		}
	})

	t.Run("a revoked platform key fails", func(t *testing.T) {
		if err := h.trust.RevokeKey(ctx, trust.PlatformApplicationID, platformKeyID, "test"); err != nil {
			t.Fatal(err)
		}
		mustFail(t, cloneReport(), "signed by a revoked platform key")
	})
}

// A runner whose signing key the trust store does not accept refuses to produce
// a report at all: a report that would not verify is never stored.
func TestRunnerRefusesUnregisteredSigningKey(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := h.cfg
	cfg.Signing = &trust.SigningKey{KeyID: "unregistered-signer", Key: priv}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Audit(ctx, RunRequest{ApplicationID: appID, Adapter: newStub(t)}); err == nil {
		t.Fatal("an audit signed by a key the trust store does not know must fail")
	}
	if _, ok, _ := h.runner.LatestRun(ctx, appID, h.onlyHash()); ok {
		t.Errorf("nothing may be stored for a report that does not verify")
	}
	cfg.Signing = nil
	r, _ = NewRunner(cfg)
	if _, err := r.Audit(ctx, RunRequest{ApplicationID: appID}); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("an audit with no signing key is unavailable, got %v", err)
	}
}

// SEC-088: two audits of the same inputs are identical apart from the signature
// timestamp. Two audits of the same inputs produce byte-identical reports; only the
// attestation (when it was made, and the signature over it) differs.
func TestAuditIsDeterministic(t *testing.T) {
	build := func(order string) (*harness, *Run) {
		h := newHarness(t, func(o *hopts) { o.probes = false })
		// register the checks in a different order: the report must not depend on it
		reg := NewRegistry()
		cs := append(Builtins(), stubProbes()...)
		if order == "reverse" {
			for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
				cs[i], cs[j] = cs[j], cs[i]
			}
		}
		for _, c := range cs {
			if err := reg.Register(c); err != nil {
				t.Fatal(err)
			}
		}
		cfg := h.cfg
		cfg.Checks = reg
		var err error
		if h.runner, err = NewRunner(cfg); err != nil {
			t.Fatal(err)
		}
		h.install(validMinimalDir)
		return h, nil
	}
	ad := func() *stub {
		return newStub(t, func(s *stub) { s.desc.Unmapped = []string{"b.op", "a.op"} })
	}

	h, _ := build("forward")
	first := h.audit(ad())
	h.clk.Advance(37 * time.Second)
	second := h.audit(ad())
	if first.RunID == second.RunID {
		t.Fatal("two runs must have two identifiers")
	}

	body := func(r *Run) []byte {
		b, err := canonical(r.Signed.Report)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(body(first), body(second)) {
		t.Fatalf("two audits of the same inputs differ:\n%s\n---\n%s", body(first), body(second))
	}
	a, b := first.Signed.Attestation, second.Signed.Attestation
	if a.ReportSHA256 != b.ReportSHA256 {
		t.Errorf("the digest of identical reports must be identical")
	}
	if a.IssuedAt == b.IssuedAt {
		t.Errorf("the attestation carries the time and must differ")
	}
	// everything but the attestation's time and signature is byte-identical
	a.IssuedAt, b.IssuedAt = "", ""
	a.Signatures, b.Signatures = nil, nil
	first.Signed.Attestation, second.Signed.Attestation = a, b
	x, _ := first.Signed.Marshal()
	y, _ := second.Signed.Marshal()
	if !bytes.Equal(x, y) {
		t.Errorf("signed reports differ in more than the signature and its timestamp")
	}

	// The report is stable across many runs (map iteration order is random in Go)
	// and across the order the checks were registered in.
	want := first.Signed.Attestation.ReportSHA256
	h2, _ := build("reverse")
	for i := 0; i < 12; i++ {
		h2.clk.Advance(time.Second)
		if got := h2.audit(ad()).Signed.Attestation.ReportSHA256; got != want {
			t.Fatalf("run %d: digest %s, want %s", i, got, want)
		}
	}

	// And across activation: the report holds no mutable state of the package,
	// so a re-audit of the active package is the same report.
	h3, _ := build("forward")
	run := h3.audit(ad())
	acceptAll(t, h3, run)
	if _, err := h3.runner.Activate(ctx, ActivateRequest{ApplicationID: appID}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	h3.clk.Advance(time.Minute)
	again, err := h3.runner.Audit(ctx, RunRequest{ApplicationID: appID, ManifestHash: run.Record.ManifestHash, Adapter: ad()})
	if err != nil {
		t.Fatalf("re-audit of the active package: %v", err)
	}
	if again.Signed.Attestation.ReportSHA256 != run.Signed.Attestation.ReportSHA256 {
		t.Errorf("a re-audit after activation must be the same report:\n%s", reportJSON(again.Signed.Report))
	}
}

// acceptAll accepts every warning of a run with an operator the harness
// authorises.
func acceptAll(t *testing.T, h *harness, run *Run) {
	t.Helper()
	for _, f := range run.Signed.Report.Findings {
		if f.Severity != contracts.SeverityWarning {
			continue
		}
		if _, err := h.runner.AcceptWarning(ctx, AcceptRequest{
			ApplicationID: appID, ManifestHash: run.Record.ManifestHash, FindingKey: f.Key,
			OperatorID: "alice", Reason: "reviewed in the test",
		}); err != nil {
			t.Fatalf("AcceptWarning(%s): %v", f.CheckID, err)
		}
	}
}

// Anything the audit could not inspect is unverified in the report and is never
// shown as passed.
func TestUnverifiedWhenNoInspectionAdapter(t *testing.T) {
	allAspects := func() []string {
		var out []string
		for _, a := range adapters.AllAspects() {
			out = append(out, "aspect:"+string(a))
		}
		return out
	}
	target := func(r Report, t Target) TargetResult {
		for _, x := range r.Targets {
			if x.Target == t {
				return x
			}
		}
		return TargetResult{}
	}
	contains := func(list []string, want ...string) bool {
		have := map[string]bool{}
		for _, s := range list {
			have[s] = true
		}
		for _, w := range want {
			if !have[w] {
				return false
			}
		}
		return true
	}

	t.Run("no adapter at all", func(t *testing.T) {
		h := newHarness(t)
		h.install(validMinimalDir)
		rep := h.audit(nil).Signed.Report

		if rep.Surface.Inspected || len(rep.Surface.Discovered) != 0 {
			t.Errorf("nothing was inspected, so nothing may be reported as discovered: %+v", rep.Surface)
		}
		if got := section(rep, SectionControlSurface).Status; got != StatusUnverified {
			t.Errorf("section 7 = %s, want unverified", got)
		}
		rt := target(rep, TargetRuntime)
		if rt.Status != StatusUnverified || !contains(rt.Unverified, append(allAspects(), "health")...) {
			t.Errorf("the runtime target must list all nine aspects and health as unverified: %+v", rt)
		}
		if got := target(rep, TargetApplication).Status; got == StatusPass {
			t.Errorf("the application target cannot pass without an adapter")
		}
		for _, c := range rep.Capabilities {
			if c.Status != CapabilityUnverified {
				t.Errorf("capability %s = %s, want unverified (nothing checked it against the application)", c.ID, c.Status)
			}
		}
		if !hasFinding(rep, "surface.not_inspected", contracts.SeverityWarning) {
			t.Errorf("an uninspected application must raise surface.not_inspected:\n%s", reportJSON(rep))
		}
		if rep.Summary.Outcome == DispositionClear {
			t.Errorf("a report that inspected nothing is not clear")
		}
		// the package half is still audited and still passes where it can
		if got := section(rep, SectionAuthenticity).Status; got != StatusPass {
			t.Errorf("section 1 = %s, want pass: the package half does not need an adapter", got)
		}
	})

	t.Run("an adapter that exposes some aspects", func(t *testing.T) {
		h := newHarness(t)
		h.install(validMinimalDir)
		rep := h.audit(newStub(t)).Signed.Report // reports versions, health and limits only

		rt := target(rep, TargetRuntime)
		want := []string{"aspect:credentials", "aspect:dependencies", "aspect:modules", "aspect:network", "aspect:scopes", "aspect:storage"}
		for _, w := range want {
			if !contains(rt.Unverified, w) {
				t.Errorf("%s is not exposed and must be unverified: %v", w, rt.Unverified)
			}
		}
		for _, reported := range []string{"aspect:versions", "aspect:health", "aspect:limits"} {
			if contains(rt.Unverified, reported) {
				t.Errorf("%s was exposed and must not be listed as unverified", reported)
			}
		}
		if rt.Status != StatusUnverified {
			t.Errorf("a target with unverified aspects is unverified, never pass: %s", rt.Status)
		}
		// the facts that were exposed are recorded
		var found bool
		for _, e := range rep.Extensions {
			if e.Check == "runtime.inspection" {
				facts, _ := e.Data["facts"].(map[string]any)
				found = facts["versions.application_version"] != nil
			}
		}
		if !found {
			t.Errorf("the exposed facts must be in the report")
		}
	})

	t.Run("an inspection call that fails", func(t *testing.T) {
		h := newHarness(t)
		h.install(validMinimalDir)
		rep := h.audit(newStub(t, func(s *stub) {
			s.inspErr = contracts.New(contracts.ErrUnavailable, "inspection is down")
			s.healthErr = errors.New("health is down")
		})).Signed.Report
		rt := target(rep, TargetRuntime)
		if !contains(rt.Unverified, append(allAspects(), "health")...) {
			t.Errorf("a failed inspection leaves every aspect unverified: %v", rt.Unverified)
		}
		if !hasFinding(rep, "system.inspect_failed", contracts.SeverityWarning) || !hasFinding(rep, "system.health_unverified", contracts.SeverityWarning) {
			t.Errorf("failed inspection and health calls are warnings:\n%s", reportJSON(rep))
		}
	})

	t.Run("an application that reports itself down is critical", func(t *testing.T) {
		h := newHarness(t)
		h.install(validMinimalDir)
		rep := h.audit(newStub(t, func(s *stub) {
			s.health = adapters.HealthReport{Status: adapters.HealthUnavailable, Reason: "database down"}
		})).Signed.Report
		if !hasFinding(rep, "system.health_unavailable", contracts.SeverityCritical) {
			t.Errorf("an unavailable application blocks:\n%s", reportJSON(rep))
		}
	})
}

// The profile the audit ran on is recorded, and one below the package's
// minimum is critical.
func TestProfileAgainstMinimum(t *testing.T) {
	h := newHarness(t, func(o *hopts) { o.profile = "experimental-4g" })
	h.install(validMinimalDir) // requires lite
	rep := h.audit(newStub(t)).Signed.Report
	if rep.Profile.Name != "experimental-4g" || rep.Profile.Minimum != "lite" {
		t.Errorf("profile = %+v", rep.Profile)
	}
	if !hasFinding(rep, "profile.below_minimum", contracts.SeverityCritical) {
		t.Errorf("a profile below the package's minimum blocks:\n%s", reportJSON(rep))
	}
	h = newHarness(t, func(o *hopts) { o.profile = "balanced" })
	h.install(validMinimalDir)
	rep = h.audit(newStub(t)).Signed.Report
	if hasFinding(rep, "profile.below_minimum", contracts.SeverityCritical) {
		t.Errorf("balanced meets lite")
	}
	for _, u := range rep.Unverified {
		if u.Subject == "profile" {
			t.Errorf("a configured profile is not unverified")
		}
	}
}

// Report JSON contains no float and no timestamp: the signed document admits
// neither.
func TestReportHoldsNoTimeOrFloat(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	rep := h.audit(newStub(t)).Signed.Report
	b, _ := json.Marshal(rep)
	if bytes.Contains(b, []byte("T09:00")) || bytes.Contains(b, []byte(t0.Format("2006-01-02T"))) {
		t.Errorf("the report body must not hold the time it was made")
	}
	if _, err := canonical(rep); err != nil {
		t.Errorf("the report must be canonical JSON: %v", err)
	}
}

// onlyHash is the manifest hash of the single package installed.
func (h *harness) onlyHash() string {
	h.t.Helper()
	recs, err := h.inst.List(ctx, appID)
	if err != nil || len(recs) != 1 {
		h.t.Fatalf("want exactly one installed package: %v %v", len(recs), err)
	}
	return recs[0].ManifestHash
}
