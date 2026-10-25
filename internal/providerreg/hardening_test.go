package providerreg

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var changedHash = providers.SHA256("sha256:" + strings.Repeat("b", 64))

// SEC: a re-pin must not be a way around the human decision a quarantine asks for.
func TestPinModelVersionNeverLiftsAQuarantineWithoutAReview(t *testing.T) {
	const staleDay = "2026-11-01"
	cases := []struct {
		name   string
		reason providers.ProviderProfileQuarantineReason
		enter  func(g *rig, p providers.ProviderProfile)
		// end is the one call that ends this quarantine, run after the pin.
		end func(g *rig, p providers.ProviderProfile) providers.ProviderProfile
	}{
		{
			"terms_changed", providers.ProviderProfileQuarantineReasonTermsChanged,
			func(g *rig, p providers.ProviderProfile) {
				g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{TermsHash: &changedHash}))
			},
			func(g *rig, p providers.ProviderProfile) providers.ProviderProfile {
				return g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: changedHash, ReviewedOn: "2026-10-02"}))
			},
		},
		{
			"region_changed", providers.ProviderProfileQuarantineReasonRegionChanged,
			func(g *rig, p providers.ProviderProfile) {
				g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{Regions: []providers.RegionCode{"us-east"}}))
			},
			func(g *rig, p providers.ProviderProfile) providers.ProviderProfile {
				return g.prof(g.reg.Release(bg, g.scope, p.ProfileID, ReleaseInput{ReviewedOn: "2026-10-02"}))
			},
		},
		{
			"excessive_permissions", providers.ProviderProfileQuarantineReasonExcessivePermissions,
			func(g *rig, p providers.ProviderProfile) {
				g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{ExcessivePermissions: true}))
			},
			func(g *rig, p providers.ProviderProfile) providers.ProviderProfile {
				return g.prof(g.reg.Release(bg, g.scope, p.ProfileID, ReleaseInput{ReviewedOn: "2026-10-02"}))
			},
		},
		{
			"operator_hold", providers.ProviderProfileQuarantineReasonOperatorHold,
			func(g *rig, p providers.ProviderProfile) { g.prof(g.reg.Hold(bg, g.scope, p.ProfileID)) },
			func(g *rig, p providers.ProviderProfile) providers.ProviderProfile {
				return g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: p.Terms.TermsHash, ReviewedOn: "2026-10-02"}))
			},
		},
		{
			"stale_review", providers.ProviderProfileQuarantineReasonStaleReview,
			func(g *rig, p providers.ProviderProfile) {
				g.clk.Set(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
				if held, err := g.reg.Revalidate(bg, g.scope); err != nil || len(held) != 1 {
					t.Fatalf("Revalidate = %v, %v", held, err)
				}
			},
			func(g *rig, p providers.ProviderProfile) providers.ProviderProfile {
				return g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: p.Terms.TermsHash, ReviewedOn: staleDay}))
			},
		},
		{
			"conformance_failed", providers.ProviderProfileQuarantineReasonConformanceFailed,
			func(g *rig, p providers.ProviderProfile) {
				bad := g.reportFor(p, g.clk.Now().Add(-time.Minute), "inference.text_generation_and_streaming")
				g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, bad))
			},
			func(g *rig, p providers.ProviderProfile) providers.ProviderProfile {
				cur := g.prof(g.reg.Get(bg, g.scope, p.ProfileID))
				return g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(cur, g.clk.Now().Add(-time.Minute), "")))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			p := g.qualified(credRef(1))
			tc.enter(g, p)
			before := g.prof(g.reg.Get(bg, g.scope, p.ProfileID))
			if before.Lifecycle != providers.ProfileLifecycleQuarantined || before.QuarantineReason == nil || *before.QuarantineReason != tc.reason {
				t.Fatalf("setup: %v %v", before.Lifecycle, before.QuarantineReason)
			}

			pinned := g.prof(g.reg.PinModelVersion(bg, g.scope, p.ProfileID, "v2"))
			if pinned.Lifecycle != providers.ProfileLifecycleQuarantined || pinned.QuarantineReason == nil || *pinned.QuarantineReason != tc.reason {
				t.Fatalf("the pin lifted the %s quarantine: %v %v", tc.name, pinned.Lifecycle, pinned.QuarantineReason)
			}
			if pinned.ModelVersion != "v2" || pinned.LastConformance != nil {
				t.Fatalf("the pin did not take the version and clear the report: %v %v", pinned.ModelVersion, pinned.LastConformance != nil)
			}
			// Nothing is served and nothing qualifies while the reason stands.
			calls, err := g.route(g.scope, p.ProfileID, req())
			wantReason(t, err, contracts.ErrQuarantined, Reason(tc.reason))
			if calls != 0 {
				t.Fatal("a quarantined profile was used")
			}
			_, err = g.reg.Qualify(bg, g.scope, p.ProfileID)
			wantReason(t, err, contracts.ErrQuarantined, Reason(tc.reason))

			// The call that belongs to the reason ends it, and only it.
			ended := tc.end(g, pinned)
			if ended.Lifecycle != providers.ProfileLifecycleCandidate || ended.QuarantineReason != nil || ended.ModelVersion != "v2" {
				t.Fatalf("after the proper release: %v %v %v", ended.Lifecycle, ended.QuarantineReason, ended.ModelVersion)
			}
		})
	}

	t.Run("a model_version_changed quarantine still ends with the pin", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		drift := providers.ModelVersion("v1-patched")
		g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{ModelVersion: &drift}))
		if got := g.prof(g.reg.PinModelVersion(bg, g.scope, p.ProfileID, "v2")); got.Lifecycle != providers.ProfileLifecycleCandidate || got.QuarantineReason != nil {
			t.Fatalf("after the pin: %v %v", got.Lifecycle, got.QuarantineReason)
		}
	})
}

func TestRegionChangedAndExcessivePermissionsHaveAReleasePath(t *testing.T) {
	pinnedEU := func(d *providers.ProviderProfile) {
		d.Region.Residency = providers.ResidencyPinned
		d.Region.ProcessingRegions = []providers.RegionCode{"eu-de"}
	}
	eventReason := func(t *testing.T, g *rig, p providers.ProviderProfile, reason Reason) {
		t.Helper()
		evs := eventKinds(t, g, p.ProfileID)
		last := evs[len(evs)-1]
		if last.Kind != EventReleased || last.Reason != string(reason) || last.Actor != tAdmin {
			t.Fatalf("last event = %+v", last)
		}
	}

	t.Run("region_changed: Review does not release; Release records the accepted regions", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1), pinnedEU)
		moved := []providers.RegionCode{"us-east"}
		if same := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{Regions: []providers.RegionCode{"eu-de"}})); same.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("an unchanged region quarantined: %v", same.Lifecycle)
		}
		g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{Regions: moved}))
		if _, err := g.route(g.scope, p.ProfileID, req()); ReasonOf(err) != ReasonRegionChanged {
			t.Fatalf("route: %v", err)
		}

		// Review is the wrong call: the quarantine stays.
		got := g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: p.Terms.TermsHash, ReviewedOn: "2026-10-02"}))
		if got.Lifecycle != providers.ProfileLifecycleQuarantined || *got.QuarantineReason != providers.ProviderProfileQuarantineReasonRegionChanged {
			t.Fatalf("Review released a region quarantine: %v", got.Lifecycle)
		}
		// Release needs a valid date and the accepted regions, and does not change anything otherwise.
		for name, in := range map[string]ReleaseInput{
			"no regions":       {ReviewedOn: "2026-10-02"},
			"future date":      {ReviewedOn: "2026-10-03", Regions: moved},
			"malformed date":   {ReviewedOn: "yesterday", Regions: moved},
			"date before last": {ReviewedOn: "2026-10-01", Regions: moved},
			"malformed region": {ReviewedOn: "2026-10-02", Regions: []providers.RegionCode{"Not A Region"}},
		} {
			if _, err := g.reg.Release(bg, g.scope, p.ProfileID, in); ReasonOf(err) != ReasonInvalidProfile {
				t.Errorf("%s: err = %v", name, err)
			}
		}
		if cur := g.prof(g.reg.Get(bg, g.scope, p.ProfileID)); cur.Lifecycle != providers.ProfileLifecycleQuarantined || !slices.Equal(cur.Region.ProcessingRegions, []providers.RegionCode{"eu-de"}) {
			t.Fatalf("a refused Release changed the profile: %v %v", cur.Lifecycle, cur.Region.ProcessingRegions)
		}

		rel := g.prof(g.reg.Release(bg, g.scope, p.ProfileID, ReleaseInput{ReviewedOn: "2026-10-02", Regions: moved}))
		if rel.Lifecycle != providers.ProfileLifecycleCandidate || rel.QuarantineReason != nil || !slices.Equal(rel.Region.ProcessingRegions, moved) {
			t.Fatalf("after Release: %v %v %v", rel.Lifecycle, rel.QuarantineReason, rel.Region.ProcessingRegions)
		}
		eventReason(t, g, p, ReasonRegionChanged)
		// A release is not a qualification: the profile is not served until Qualify.
		_, err := g.route(g.scope, p.ProfileID, req())
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonNotQualified)
		if q, err := g.reg.Qualify(bg, g.scope, p.ProfileID); err != nil || q.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("requalify: %v %v", q.Lifecycle, err)
		}
		// What was accepted is the new baseline.
		if again := g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{Regions: moved})); again.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("the accepted region quarantined again: %v", again.Lifecycle)
		}
	})

	t.Run("excessive_permissions: released only while the credential is reported active", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(2))
		g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{ExcessivePermissions: true}))
		_, err := g.route(g.scope, p.ProfileID, req())
		wantReason(t, err, contracts.ErrQuarantined, ReasonExcessivePermissions)
		_, err = g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: p.Terms.TermsHash, ReviewedOn: "2026-10-02"})
		if err != nil {
			t.Fatal(err)
		}
		if cur := g.prof(g.reg.Get(bg, g.scope, p.ProfileID)); cur.Lifecycle != providers.ProfileLifecycleQuarantined {
			t.Fatalf("Review released an excessive_permissions quarantine: %v", cur.Lifecycle)
		}

		g.creds.set(credRef(2), providers.CredentialRefStatusInvalid)
		_, err = g.reg.Release(bg, g.scope, p.ProfileID, ReleaseInput{ReviewedOn: "2026-10-02"})
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
		g.creds.err = errors.New("credential service down")
		_, err = g.reg.Release(bg, g.scope, p.ProfileID, ReleaseInput{ReviewedOn: "2026-10-02"})
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonCredentialInvalid)
		g.creds.err = nil

		g.creds.set(credRef(2), providers.CredentialRefStatusActive)
		rel := g.prof(g.reg.Release(bg, g.scope, p.ProfileID, ReleaseInput{ReviewedOn: "2026-10-02"}))
		if rel.Lifecycle != providers.ProfileLifecycleCandidate || rel.QuarantineReason != nil {
			t.Fatalf("after Release: %v %v", rel.Lifecycle, rel.QuarantineReason)
		}
		eventReason(t, g, p, ReasonExcessivePermissions)
		if q, err := g.reg.Qualify(bg, g.scope, p.ProfileID); err != nil || q.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("requalify: %v %v", q.Lifecycle, err)
		}
	})

	t.Run("Release ends nothing else and stays inside the application", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(3))
		in := ReleaseInput{ReviewedOn: "2026-10-02"}
		_, err := g.reg.Release(bg, g.scope, p.ProfileID, in)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonNotReleasable) // not quarantined

		g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{TermsHash: &changedHash}))
		_, err = g.reg.Release(bg, g.scope, p.ProfileID, in)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonNotReleasable) // another reason
		if cur := g.prof(g.reg.Get(bg, g.scope, p.ProfileID)); cur.Lifecycle != providers.ProfileLifecycleQuarantined {
			t.Fatalf("Release ended a terms_changed quarantine: %v", cur.Lifecycle)
		}

		g.prof(g.reg.Hold(bg, g.scope, p.ProfileID))
		_, err = g.reg.Release(bg, g.scope, p.ProfileID, in)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonNotReleasable)

		_, err = g.reg.Release(bg, g.scopeOf(tOther, tTenant, tWsp), p.ProfileID, in)
		wantReason(t, err, contracts.ErrValidationFailed, ReasonProfileNotFound)
		partial := g.scope
		partial.WorkspaceID = ""
		if _, err = g.reg.Release(bg, partial, p.ProfileID, in); contracts.CodeOf(err) != contracts.ErrScopeMissing {
			t.Fatalf("incomplete scope: %v", err)
		}

		g.prof(g.reg.Retire(bg, g.scope, p.ProfileID))
		_, err = g.reg.Release(bg, g.scope, p.ProfileID, in)
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonRetired)
	})
}

func TestTermsHashChangeClearsTheConformanceReport(t *testing.T) {
	t.Run("observed change, then review", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		g.prof(g.reg.Observe(bg, g.scope, p.ProfileID, Observation{TermsHash: &changedHash}))
		rev := g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: changedHash, ReviewedOn: "2026-10-02"}))
		if rev.Lifecycle != providers.ProfileLifecycleCandidate || rev.LastConformance != nil {
			t.Fatalf("after review: %v report=%v", rev.Lifecycle, rev.LastConformance != nil)
		}
		// The stale report cannot qualify the profile, not even a fresh and exact one recorded
		// before the terms were reviewed: only a report recorded afterwards counts.
		_, err := g.reg.Qualify(bg, g.scope, p.ProfileID)
		wantReason(t, err, contracts.ErrPermissionDenied, ReasonConformanceMissing)
		g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(rev, g.clk.Now().Add(-time.Minute), "")))
		if q, err := g.reg.Qualify(bg, g.scope, p.ProfileID); err != nil || q.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("requalify on a new report: %v %v", q.Lifecycle, err)
		}
	})

	t.Run("a review that records a different hash resets a qualified profile and a candidate", func(t *testing.T) {
		g := newRig(t)
		q := g.qualified(credRef(1))
		got := g.prof(g.reg.Review(bg, g.scope, q.ProfileID, ReviewInput{TermsHash: changedHash, ReviewedOn: "2026-10-02"}))
		if got.Lifecycle != providers.ProfileLifecycleCandidate || got.LastConformance != nil {
			t.Fatalf("qualified profile: %v report=%v", got.Lifecycle, got.LastConformance != nil)
		}
		// A candidate holding a report loses it too.
		c := g.register()
		g.prof(g.reg.RecordConformance(bg, g.scope, c.ProfileID, g.reportFor(c, g.clk.Now().Add(-time.Minute), "")))
		got = g.prof(g.reg.Review(bg, g.scope, c.ProfileID, ReviewInput{TermsHash: changedHash, ReviewedOn: "2026-10-02"}))
		if got.LastConformance != nil {
			t.Fatal("a candidate kept its report across a terms change")
		}
	})

	t.Run("an unchanged hash keeps the report and the qualification", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		g.clk.Advance(48 * time.Hour)
		got := g.prof(g.reg.Review(bg, g.scope, p.ProfileID, ReviewInput{TermsHash: p.Terms.TermsHash, ReviewedOn: "2026-10-04"}))
		if got.Lifecycle != providers.ProfileLifecycleQualified || got.LastConformance == nil {
			t.Fatalf("an unchanged review reset the profile: %v report=%v", got.Lifecycle, got.LastConformance != nil)
		}
	})
}

// jobsRig is a registry and a job queue over one database, with a pool that accepts the
// revalidation service identity.
type jobsRig struct {
	*rig
	q    *jobs.Queue
	pool *jobs.Pool
}

func newJobsRig(t *testing.T) *jobsRig {
	t.Helper()
	g := newRig(t)
	db := storetest.Open(t, nil) // the default registry: jobs and providerreg
	reg, err := New(db, Config{Clock: g.clk, Credentials: g.creds})
	if err != nil {
		t.Fatal(err)
	}
	g.db, g.reg = db, reg
	q, err := jobs.NewQueue(db, jobs.Options{Clock: g.clk})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Register(JobSpec()); err != nil {
		t.Fatal(err)
	}
	pool, err := jobs.NewPool(q, jobs.PoolOptions{
		ScopeChecker: jobs.ScopeCheckerFunc(func(context.Context, contracts.Scope) error { return nil }),
		ServiceAuthorizer: jobs.ServiceAuthorizerFunc(func(_ context.Context, id string, _ jobs.Job) error {
			if id != ServiceIdentity {
				return contracts.New(contracts.ErrPermissionDenied, "unknown service identity")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pool.Shutdown(ctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})
	if err := reg.Handle(pool); err != nil {
		t.Fatal(err)
	}
	return &jobsRig{rig: g, q: q, pool: pool}
}

// drain runs the pool until no job is left to start and every job it started has settled.
func (j *jobsRig) drain() {
	j.t.Helper()
	limit := time.Now().Add(10 * time.Second)
	for {
		n, err := j.pool.Poll(bg)
		if err != nil {
			j.t.Fatalf("Poll: %v", err)
		}
		for j.pool.Running(jobs.ClassMaintenance) > 0 {
			if time.Now().After(limit) {
				j.t.Fatal("timed out waiting for the revalidation job")
			}
			time.Sleep(time.Millisecond)
		}
		if n == 0 {
			return
		}
	}
}

func TestRevalidateJobQuarantinesExpiredReviews(t *testing.T) {
	g := newJobsRig(t)
	if got := JobSpec(); got.Class != jobs.ClassMaintenance {
		t.Fatalf("the revalidation job is class %v, want 6 (maintenance)", got.Class)
	}
	live := g.qualified(credRef(1))
	cand := g.register()
	// A profile of another application is swept under that application's own job scope.
	otherGrant := g.draft()
	otherGrant.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tOther}}
	otherScope := g.scopeOf(tOther, tTenant, tWsp)
	other := g.prof(g.reg.Register(bg, otherScope, otherGrant))

	// Fresh reviews: the job runs for both applications and quarantines nothing.
	res, err := g.reg.ScheduleRevalidation(bg, g.q, "dep_test", time.Hour)
	if err != nil || res.Applications != 2 || res.Enqueued != 2 || res.Existing != 0 {
		t.Fatalf("first pass = %+v, %v", res, err)
	}
	if res, err = g.reg.ScheduleRevalidation(bg, g.q, "dep_test", time.Hour); err != nil || res.Enqueued != 0 || res.Existing != 2 {
		t.Fatalf("a second pass in the same period queued again: %+v, %v", res, err)
	}
	g.drain()
	for _, id := range []providers.ProfileID{live.ProfileID, cand.ProfileID} {
		if p := g.prof(g.reg.Get(bg, g.scope, id)); p.Lifecycle == providers.ProfileLifecycleQuarantined {
			t.Fatalf("%s quarantined while fresh", id)
		}
	}

	// The reviews expire at 00:00 UTC on 2026-11-01. No request names any profile: only the job runs.
	g.clk.Set(time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC))
	if res, err = g.reg.ScheduleRevalidation(bg, g.q, "dep_test", time.Hour); err != nil || res.Enqueued != 2 {
		t.Fatalf("pass after expiry = %+v, %v", res, err)
	}
	g.drain()
	for _, c := range []struct {
		scope contracts.Scope
		id    providers.ProfileID
	}{{g.scope, live.ProfileID}, {g.scope, cand.ProfileID}, {otherScope, other.ProfileID}} {
		p := g.prof(g.reg.Get(bg, c.scope, c.id))
		if p.Lifecycle != providers.ProfileLifecycleQuarantined || p.QuarantineReason == nil || *p.QuarantineReason != providers.ProviderProfileQuarantineReasonStaleReview {
			t.Fatalf("%s after the job: %v %v", c.id, p.Lifecycle, p.QuarantineReason)
		}
	}
	evs := eventKinds(t, g.rig, live.ProfileID)
	if last := evs[len(evs)-1]; last.Kind != EventQuarantined || last.Reason != "stale_review" || last.Actor != systemActor {
		t.Fatalf("last event = %+v", last)
	}
	// Quarantined profiles are no longer live: the next pass queues nothing.
	g.clk.Advance(2 * time.Hour)
	if res, err = g.reg.ScheduleRevalidation(bg, g.q, "dep_test", time.Hour); err != nil || res.Applications != 0 || res.Enqueued != 0 {
		t.Fatalf("pass with nothing live = %+v, %v", res, err)
	}

	t.Run("the handler refuses a job that does not run as the service", func(t *testing.T) {
		g := newJobsRig(t)
		p := g.qualified(credRef(1))
		g.clk.Set(time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC))
		// A user-scope job of the same type: the pool lets it through (its scope is valid), the
		// handler must refuse it.
		out, err := g.q.Enqueue(bg, g.scope, jobs.EnqueueRequest{Type: JobRevalidate})
		if err != nil {
			t.Fatal(err)
		}
		g.drain()
		waitJob(t, g.q, g.scope, out.Job.ID, jobs.StateFailed)
		if cur := g.prof(g.reg.Get(bg, g.scope, p.ProfileID)); cur.Lifecycle != providers.ProfileLifecycleQualified {
			t.Fatalf("a job without the service identity changed a profile: %v", cur.Lifecycle)
		}
	})

	t.Run("the schedule loop queues a pass and stops with its context", func(t *testing.T) {
		g := newJobsRig(t)
		g.qualified(credRef(1))
		ctx, cancel := context.WithTimeout(bg, 60*time.Millisecond)
		defer cancel()
		if err := g.reg.RunRevalidationSchedule(ctx, g.q, "dep_test", 10*time.Millisecond, func(err error) { t.Errorf("pass failed: %v", err) }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("loop ended with %v", err)
		}
		if res, err := g.reg.ScheduleRevalidation(bg, g.q, "dep_test", 10*time.Millisecond); err != nil || res.Existing != 1 || res.Enqueued != 0 {
			t.Fatalf("the loop queued nothing (fake clock, same period): %+v, %v", res, err)
		}
	})

	t.Run("a missing queue is unavailable, never a silent skip", func(t *testing.T) {
		if _, err := g.reg.ScheduleRevalidation(bg, nil, "dep_test", 0); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("err = %v", err)
		}
	})
}

func waitJob(t *testing.T, q *jobs.Queue, scope contracts.Scope, id string, want jobs.State) {
	t.Helper()
	limit := time.Now().Add(10 * time.Second)
	for {
		if j, err := q.Get(bg, scope, id); err == nil && j.State == want {
			return
		}
		if time.Now().After(limit) {
			t.Fatalf("timed out waiting for job %s to be %s", id, want)
		}
		time.Sleep(time.Millisecond)
	}
}
