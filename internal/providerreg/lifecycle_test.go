package providerreg

import (
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func eventKinds(t *testing.T, g *rig, id providers.ProfileID) []Event {
	t.Helper()
	evs, err := g.reg.Events(bg, g.scope, id)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return evs
}

func hasEvent(evs []Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestProfileStoresCredentialReferenceNeverSecret(t *testing.T) {
	g := newRig(t)
	p := g.register()

	// A pasted key is not a reference: refused, never echoed, never stored.
	for _, bad := range []providers.CredentialRefID{providers.CredentialRefID(tSecret), "cred_short", "", providers.CredentialRefID(strings.ToLower(string(credRef(1))))} {
		_, err := g.reg.BindCredential(bg, g.scope, p.ProfileID, bad)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonInvalidProfile)
		if strings.Contains(err.Error(), tSecret) {
			t.Fatalf("the error echoes the secret: %v", err)
		}
	}
	// A draft cannot carry a credential at all: the credential is bound, not supplied.
	d := g.draft()
	ref := credRef(2)
	d.CredRef = &ref
	if _, err := g.reg.Register(bg, g.scope, d); err == nil {
		t.Fatal("Register accepted a draft that carries a credential reference")
	}
	// An endpoint is not a place for a secret either.
	for _, ep := range []string{
		"https://user:" + tSecret + "@api.example.com/v1",
		"https://api.example.com/v1?key=" + tSecret,
		"https://api.example.com/v1#" + tSecret,
		"http://api.example.com/v1", // plain http only on loopback
	} {
		d := g.draft()
		d.Endpoint = ep
		_, err := g.reg.Register(bg, g.scope, d)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonInvalidProfile)
		if strings.Contains(err.Error(), tSecret) {
			t.Fatalf("the error echoes the secret: %v", err)
		}
	}

	// The reference binds; it is stored as the opaque id and nothing else.
	bound := g.prof(g.reg.BindCredential(bg, g.scope, p.ProfileID, credRef(1)))
	if bound.CredRef == nil || *bound.CredRef != credRef(1) {
		t.Fatalf("CredRef = %v", bound.CredRef)
	}
	stored := g.prof(g.reg.Get(bg, g.scope, p.ProfileID))
	if stored.CredRef == nil || *stored.CredRef != credRef(1) {
		t.Fatalf("stored CredRef = %v", stored.CredRef)
	}

	// Every stored byte of the module: no canary anywhere.
	for _, q := range []string{
		`SELECT group_concat(document, char(10)) FROM providerreg_profiles`,
		`SELECT group_concat(kind || reason || related_id || actor, char(10)) FROM providerreg_events`,
	} {
		var all string
		if err := g.db.QueryRowContext(bg, q).Scan(&all); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(all, tSecret) {
			t.Fatalf("a secret-like value reached the store: %q", q)
		}
		if !strings.Contains(all, string(credRef(1))) && strings.Contains(q, "providerreg_profiles") {
			t.Fatal("the credential reference is not in the stored profile")
		}
	}
	// The profile has no member that can hold a secret: its keys are the schema's.
	var doc string
	if err := g.db.QueryRowContext(bg, `SELECT document FROM providerreg_profiles WHERE profile_id = ?`, string(p.ProfileID)).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"api_key", "apikey", "secret", "password", "bearer", "authorization"} {
		if strings.Contains(strings.ToLower(doc), `"`+banned) {
			t.Fatalf("the stored profile has a %q member", banned)
		}
	}
}

func TestStaleTermsQuarantineProfile(t *testing.T) {
	t.Run("a review goes stale on the day its interval ends and Check quarantines it before any sweep", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1)) // reviewed 2026-10-02, 30 days: stale from 2026-11-01 00:00 UTC

		g.clk.Set(time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC))
		if calls, err := g.route(g.scope, p.ProfileID, req()); err != nil || calls != 1 {
			t.Fatalf("one second before the boundary: calls=%d err=%v", calls, err)
		}
		g.clk.Set(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
		calls, err := g.route(g.scope, p.ProfileID, req())
		wantReason(t, err, contracts.ErrQuarantined, ReasonStaleReview)
		if calls != 0 {
			t.Fatal("a stale profile was used")
		}
		got := g.prof(g.reg.Get(bg, g.scope, p.ProfileID))
		if got.Lifecycle != providers.ProfileLifecycleQuarantined || got.QuarantineReason == nil ||
			*got.QuarantineReason != providers.ProviderProfileQuarantineReasonStaleReview {
			t.Fatalf("not persisted as quarantined/stale_review: %v %v", got.Lifecycle, got.QuarantineReason)
		}
		evs := eventKinds(t, g, p.ProfileID)
		last := evs[len(evs)-1]
		if last.Kind != EventQuarantined || last.Reason != "stale_review" || last.Actor != systemActor || last.ToLifecycle != providers.ProfileLifecycleQuarantined {
			t.Fatalf("last event = %+v", last)
		}
		// Quarantined stays quarantined with its reason; it is not requalified in place.
		_, err = g.reg.Qualify(bg, g.scope, p.ProfileID)
		wantReason(t, err, contracts.ErrQuarantined, ReasonStaleReview)
	})

	t.Run("a changed terms hash quarantines; a review releases to candidate and revalidation is needed", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		same := p.Terms.TermsHash
		if got := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{TermsHash: &same})); got.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("an unchanged hash changed the lifecycle: %v", got.Lifecycle)
		}
		changed := providers.SHA256("sha256:" + strings.Repeat("b", 64))
		got := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{TermsHash: &changed}))
		if got.Lifecycle != providers.ProfileLifecycleQuarantined || *got.QuarantineReason != providers.ProviderProfileQuarantineReasonTermsChanged {
			t.Fatalf("after a terms change: %v %v", got.Lifecycle, got.QuarantineReason)
		}
		_, err := g.route(g.scope, p.ProfileID, req())
		wantReason(t, err, contracts.ErrQuarantined, ReasonTermsChanged)

		g.clk.Advance(48 * time.Hour)
		rev := g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: changed, ReviewedOn: "2026-10-04"}))
		if rev.Lifecycle != providers.ProfileLifecycleCandidate || rev.Terms.TermsHash != changed || rev.LastPolicyReview != "2026-10-04" {
			t.Fatalf("after review: %v %v %v", rev.Lifecycle, rev.Terms.TermsHash, rev.LastPolicyReview)
		}
		_, err = g.route(g.scope, p.ProfileID, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonNotQualified)
		// The report was produced under the old terms and is gone: a new one is needed.
		_, err = g.reg.Qualify(bg, g.scope, p.ProfileID)
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonConformanceMissing)
		g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(rev, g.clk.Now().Add(-time.Minute), "")))
		if q, err := g.reg.Qualify(bg, g.scope, p.ProfileID); err != nil || q.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("requalify: %v %v", q.Lifecycle, err)
		}
	})

	t.Run("a stale conformance report cannot qualify and the sweep quarantines what it finds", func(t *testing.T) {
		g := newRig(t)
		old := g.qualified(credRef(1))
		fresh := g.register()
		g.clk.Set(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
		held, err := g.reg.Revalidate(bg, g.scope)
		if err != nil || len(held) != 2 {
			t.Fatalf("Revalidate = %v, %v; want both stale profiles", held, err)
		}
		for _, id := range []providers.ProfileID{old.ProfileID, fresh.ProfileID} {
			if p := g.prof(g.reg.Get(bg, g.scope, id)); p.Lifecycle != providers.ProfileLifecycleQuarantined {
				t.Fatalf("%s not quarantined: %v", id, p.Lifecycle)
			}
		}
		// Review and re-record: the 30 day old report is too old to qualify again.
		g.prof(g.reg.Review(bg, g.scope, old.ProfileID, ReviewInput{TermsHash: old.Terms.TermsHash, ReviewedOn: "2026-11-01"}))
		_, err = g.reg.Qualify(bg, g.scope, old.ProfileID)
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonConformanceStale)
		cur := g.prof(g.reg.Get(bg, g.scope, old.ProfileID))
		g.prof(g.reg.RecordConformance(bg, g.scope, old.ProfileID, g.reportFor(cur, g.clk.Now().Add(-time.Minute), "")))
		if q, err := g.reg.Qualify(bg, g.scope, old.ProfileID); err != nil || q.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("after a fresh report: %v %v", q.Lifecycle, err)
		}
	})

	t.Run("the interval is capped at 30 days and a stale or future review is refused at registration", func(t *testing.T) {
		g := newRig(t)
		for name, mutate := range map[string]func(*providers.ProviderProfile){
			"interval over 30": func(d *providers.ProviderProfile) { d.ReviewIntervalDays = 31 },
			"interval zero":    func(d *providers.ProviderProfile) { d.ReviewIntervalDays = 0 },
			"review too old":   func(d *providers.ProviderProfile) { d.LastPolicyReview = "2026-08-01" },
			"review in future": func(d *providers.ProviderProfile) { d.LastPolicyReview = "2026-10-03" },
			"terms in future":  func(d *providers.ProviderProfile) { d.Terms.RecordedOn = "2027-01-01" },
		} {
			d := g.draft()
			mutate(&d)
			if _, err := g.reg.Register(bg, g.scope, d); ReasonOf(err) != ReasonInvalidProfile {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
}

func TestModelVersionPinned(t *testing.T) {
	g := newRig(t)
	p := g.qualified(credRef(1))
	if p.ModelVersion != "v1" || p.LastConformance == nil {
		t.Fatalf("fixture: %v %v", p.ModelVersion, p.LastConformance)
	}

	// A report for the pinned version cannot be recorded against another one.
	other := p
	other.ModelVersion = "v2"
	_, err := g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(other, g.clk.Now(), ""))
	wantReason(t, err, contracts.ErrValidationFailed, ReasonConformanceMismatch)
	// ... nor a report for another model id or provider.
	otherModel := p
	otherModel.ModelID = "scripted-pro"
	_, err = g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(otherModel, g.clk.Now(), ""))
	wantReason(t, err, contracts.ErrValidationFailed, ReasonConformanceMismatch)

	// The provider serves a different snapshot than the one pinned: quarantine, not silent use.
	drift := providers.ModelVersion("v1-patched")
	got := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{ModelVersion: &drift}))
	if got.Lifecycle != providers.ProfileLifecycleQuarantined || *got.QuarantineReason != providers.ProviderProfileQuarantineReasonModelVersionChanged {
		t.Fatalf("after drift: %v %v", got.Lifecycle, got.QuarantineReason)
	}
	_, err = g.route(g.scope, p.ProfileID, req())
	wantReason(t, err, contracts.ErrQuarantined, ReasonModelVersionChanged)

	// The operator pins the new version: qualification is reset, the old report is gone.
	pinned := g.prof(g.reg.PinModelVersion(bg, g.scope, p.ProfileID, "v2"))
	if pinned.ModelVersion != "v2" || pinned.Lifecycle != providers.ProfileLifecycleCandidate || pinned.LastConformance != nil || pinned.QuarantineReason != nil {
		t.Fatalf("after pin: %+v", pinned.Lifecycle)
	}
	_, err = g.route(g.scope, p.ProfileID, req())
	wantReason(t, err, contracts.ErrPermissionDenied, ReasonNotQualified)
	_, err = g.reg.Qualify(bg, g.scope, p.ProfileID)
	wantReason(t, err, contracts.ErrPermissionDenied, ReasonConformanceMissing)

	// The old version's report still cannot qualify the new version; the new one's can.
	_, err = g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(p, g.clk.Now().Add(-time.Minute), ""))
	wantReason(t, err, contracts.ErrValidationFailed, ReasonConformanceMismatch)
	g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(pinned, g.clk.Now().Add(-time.Minute), "")))
	if q, err := g.reg.Qualify(bg, g.scope, p.ProfileID); err != nil || q.Lifecycle != providers.ProfileLifecycleQualified || q.ModelVersion != "v2" {
		t.Fatalf("qualify v2: %v %v", q.Lifecycle, err)
	}
	if !hasEvent(eventKinds(t, g, p.ProfileID), EventModelVersionPinned) {
		t.Fatal("the pin is not recorded")
	}
	// Re-pinning the same version is a no-op and keeps the qualification.
	if same := g.prof(g.reg.PinModelVersion(bg, g.scope, p.ProfileID, "v2")); same.Lifecycle != providers.ProfileLifecycleQualified {
		t.Fatalf("a no-op pin reset the profile: %v", same.Lifecycle)
	}
	if _, err := g.reg.PinModelVersion(bg, g.scope, p.ProfileID, "not a version!"); err == nil {
		t.Fatal("a malformed version was accepted")
	}
}

func TestNotProductionQualifiedWithoutConformance(t *testing.T) {
	g := newRig(t)
	g.creds.set(credRef(1), providers.CredentialRefStatusActive)
	p := g.register()
	g.prof(g.reg.BindCredential(bg, g.scope, p.ProfileID, credRef(1)))
	p = g.ack(p)

	mustBlock := func(t *testing.T, id providers.ProfileID, reason Reason) {
		t.Helper()
		_, err := g.reg.Qualify(bg, g.scope, id)
		wantReason(t, err, contracts.ErrPermissionDenied, reason)
		if got := g.prof(g.reg.Get(bg, g.scope, id)); got.Lifecycle != providers.ProfileLifecycleCandidate {
			t.Fatalf("lifecycle = %v after a refused Qualify", got.Lifecycle)
		}
		if _, err := g.route(g.scope, id, req()); ReasonOf(err) != ReasonNotQualified {
			t.Fatalf("a profile that is not qualified was routed: %v", err)
		}
	}

	t.Run("no report", func(t *testing.T) { mustBlock(t, p.ProfileID, ReasonConformanceMissing) })

	t.Run("a failing report is recorded and never qualifies", func(t *testing.T) {
		rep := g.reportFor(p, g.clk.Now().Add(-time.Hour), "rag.no_path_or_cred_leakage")
		got := g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, rep))
		if got.LastConformance == nil || got.LastConformance.Outcome != providers.ConformanceReportOutcomeFailed {
			t.Fatal("the failed report was not recorded")
		}
		mustBlock(t, p.ProfileID, ReasonConformanceFailed)
	})

	t.Run("the outcome field is not trusted: one failed check blocks a report that says passed", func(t *testing.T) {
		rep := g.reportFor(p, g.clk.Now().Add(-time.Hour), "operations.audit_completeness")
		rep.Outcome = providers.ConformanceReportOutcomePassed // a forged or buggy summary
		g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, rep))
		mustBlock(t, p.ProfileID, ReasonConformanceFailed)
	})

	t.Run("a report written for another declaration of media does not qualify", func(t *testing.T) {
		rep := g.reportFor(p, g.clk.Now().Add(-time.Hour), "")
		rep.Declared.Image = true // the suite ran as if the profile took images; the profile does not
		g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, rep))
		mustBlock(t, p.ProfileID, ReasonConformanceMismatch)
	})

	t.Run("a declared capability cannot be not_applicable", func(t *testing.T) {
		img := g.register(func(d *providers.ProviderProfile) {
			d.Modalities.Input = append(d.Modalities.Input, providers.ModalityImage)
			d.Media = &providers.ProviderProfileMedia{Images: &providers.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: []string{"image/png"}, Retention: providers.RetentionModeZero}}
		})
		g.prof(g.reg.BindCredential(bg, g.scope, img.ProfileID, credRef(1)))
		g.ack(img)
		rep := g.reportFor(img, g.clk.Now().Add(-time.Hour), "")
		rep.Groups.Multimodal.ImageInputOutput.Result = providers.ConformanceReportConditionalCheckResultResultNotApplicable
		g.prof(g.reg.RecordConformance(bg, g.scope, img.ProfileID, rep))
		mustBlock(t, img.ProfileID, ReasonConformanceFailed)
	})

	t.Run("a report dated in the future does not qualify", func(t *testing.T) {
		f := g.register()
		g.prof(g.reg.BindCredential(bg, g.scope, f.ProfileID, credRef(1)))
		g.ack(f)
		g.prof(g.reg.RecordConformance(bg, g.scope, f.ProfileID, g.reportFor(f, g.clk.Now().Add(3*time.Hour), "")))
		mustBlock(t, f.ProfileID, ReasonConformanceMismatch)
	})

	t.Run("a report older than the recorded one is refused", func(t *testing.T) {
		newer := g.reportFor(p, g.clk.Now().Add(-time.Minute), "")
		g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, newer))
		_, err := g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(p, g.clk.Now().Add(-2*time.Hour), ""))
		wantReason(t, err, contracts.ErrValidationFailed, ReasonConformanceStale)
	})

	t.Run("an exact passing report, with the credential and the acknowledgement, qualifies", func(t *testing.T) {
		got := g.prof(g.reg.Qualify(bg, g.scope, p.ProfileID))
		if got.Lifecycle != providers.ProfileLifecycleQualified || got.LastConformance == nil || got.LastConformance.Outcome != providers.ConformanceReportOutcomePassed {
			t.Fatalf("lifecycle = %v", got.Lifecycle)
		}
		if _, err := g.route(g.scope, p.ProfileID, req()); err != nil {
			t.Fatalf("a qualified profile was not routed: %v", err)
		}
	})

	t.Run("a failing report on a qualified profile quarantines it; a passing one releases it", func(t *testing.T) {
		bad := g.reportFor(p, g.clk.Now().Add(-time.Second), "control.typed_task_proposal")
		got := g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, bad))
		if got.Lifecycle != providers.ProfileLifecycleQuarantined || *got.QuarantineReason != providers.ProviderProfileQuarantineReasonConformanceFailed {
			t.Fatalf("after a failing report: %v", got.Lifecycle)
		}
		g.clk.Advance(time.Minute)
		good := g.reportFor(p, g.clk.Now().Add(-time.Second), "")
		if got := g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, good)); got.Lifecycle != providers.ProfileLifecycleCandidate {
			t.Fatalf("after a passing report: %v", got.Lifecycle)
		}
	})

	t.Run("a credential the service does not call active blocks qualification", func(t *testing.T) {
		c := g.register()
		g.prof(g.reg.BindCredential(bg, g.scope, c.ProfileID, credRef(5))) // unknown to the service: invalid
		g.ack(c)
		g.prof(g.reg.RecordConformance(bg, g.scope, c.ProfileID, g.reportFor(c, g.clk.Now().Add(-time.Hour), "")))
		mustBlock(t, c.ProfileID, ReasonCredentialInvalid)
		d := g.register()
		g.ack(d)
		g.prof(g.reg.RecordConformance(bg, g.scope, d.ProfileID, g.reportFor(d, g.clk.Now().Add(-time.Hour), "")))
		mustBlock(t, d.ProfileID, ReasonCredentialMissing)
	})
}

func TestBillingNoticeAckRequiredBeforeActivation(t *testing.T) {
	g := newRig(t)
	g.creds.set(credRef(1), providers.CredentialRefStatusActive)
	p := g.register()
	g.prof(g.reg.BindCredential(bg, g.scope, p.ProfileID, credRef(1)))
	g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(p, g.clk.Now().Add(-time.Hour), "")))

	// Everything but the acknowledgement is in place: activation is still refused.
	_, err := g.reg.Qualify(bg, g.scope, p.ProfileID)
	wantReason(t, err, contracts.ErrPermissionDenied, ReasonBillingNoticeNotAcknowledged)
	if _, err := g.route(g.scope, p.ProfileID, req()); ReasonOf(err) != ReasonNotQualified {
		t.Fatalf("routed without an acknowledgement: %v", err)
	}

	// The acknowledgement needs both confirmations and the notice that is current.
	_, err = g.reg.AcknowledgeBillingNotice(bg, g.scope, p.ProfileID, Acknowledgement{NoticeVersion: p.BillingNotice.Version, ConsumerSubscriptionIsNotAPIAccess: true})
	wantReason(t, err, contracts.ErrValidationFailed, ReasonBillingConfirmationsMissing)
	_, err = g.reg.AcknowledgeBillingNotice(bg, g.scope, p.ProfileID, Acknowledgement{NoticeVersion: "1999-01-01.0", ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true})
	wantReason(t, err, contracts.ErrValidationFailed, ReasonBillingNoticeVersionMismatch)
	if got := g.prof(g.reg.Get(bg, g.scope, p.ProfileID)); got.BillingNoticeAck != nil {
		t.Fatal("a refused acknowledgement was recorded")
	}

	// Recorded with who, when, which version and which digest.
	g.clk.Advance(90 * time.Minute)
	acked := g.prof(g.reg.AcknowledgeBillingNotice(bg, g.scope, p.ProfileID, Acknowledgement{
		NoticeVersion: p.BillingNotice.Version, ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
	}))
	a := acked.BillingNoticeAck
	if a == nil || string(a.AcknowledgedBy) != tAdmin || string(a.AcknowledgedAt) != "2026-10-02T13:30:00Z" ||
		a.NoticeVersion != p.BillingNotice.Version || a.NoticeDigest != NoticeDigest(p.BillingNotice) ||
		a.ProfileID != p.ProfileID || a.ProviderID != p.ProviderID ||
		string(a.Scope.DeploymentID) != "dep_test" || a.Scope.ApplicationID == nil || string(*a.Scope.ApplicationID) != tApp ||
		!a.Confirmations.ConsumerSubscriptionIsNotAPIAccess || !a.Confirmations.SeparateAPIBillingUnderstood {
		t.Fatalf("acknowledgement = %+v", a)
	}
	if !hasEvent(eventKinds(t, g, p.ProfileID), EventBillingAcknowledged) {
		t.Fatal("the acknowledgement is not in the event record")
	}
	if got := g.prof(g.reg.Qualify(bg, g.scope, p.ProfileID)); got.Lifecycle != providers.ProfileLifecycleQualified {
		t.Fatalf("lifecycle after the acknowledgement = %v", got.Lifecycle)
	}

	// A notice that changes afterwards no longer matches what was acknowledged: the profile is
	// refused again until the operator acknowledges the new text.
	if _, err := g.db.ExecContext(bg,
		`UPDATE providerreg_profiles SET document = json_set(document, '$.billing_notice.text', 'The notice changed.') WHERE profile_id = ?`, string(p.ProfileID)); err != nil {
		t.Fatal(err)
	}
	_, err = g.route(g.scope, p.ProfileID, req())
	wantReason(t, err, contracts.ErrPermissionDenied, ReasonNotQualified)

	// A profile that claims a subscription is API access, or has no notice, cannot be registered.
	for name, mutate := range map[string]func(*providers.ProviderProfile){
		"claims subscription grants API": func(d *providers.ProviderProfile) { d.BillingNotice.SubscriptionGrantsAPIAccess = true },
		"no notice text":                 func(d *providers.ProviderProfile) { d.BillingNotice.Text = " " },
		"no notice version":              func(d *providers.ProviderProfile) { d.BillingNotice.Version = "" },
	} {
		d := g.draft()
		mutate(&d)
		if _, err := g.reg.Register(bg, g.scope, d); ReasonOf(err) != ReasonInvalidProfile {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
