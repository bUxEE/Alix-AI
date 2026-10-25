package triggers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The schedule queues one watch job per watched application per period, and only
// for applications the watcher has a reason to look at.
func TestSweepQueuesOneJobPerApplicationPerPeriod(t *testing.T) {
	h := newHarness(t, func(o *hopts) { o.queue = true })

	// a package that was installed and never activated is nobody's live application
	h.install(validMinimalDir)
	res, err := h.w.Sweep(ctx)
	if err != nil || res.Applications != 0 || len(h.enq.all()) != 0 {
		t.Fatalf("nothing is live: %+v %v", res, err)
	}

	live := h.goLive(validMinimalDir)
	res, err = h.w.Sweep(ctx)
	if err != nil || res.Applications != 1 || res.Enqueued != 1 || res.Existing != 0 {
		t.Fatalf("one live application: %+v %v", res, err)
	}
	jobs := h.enq.all()
	if len(jobs) != 1 || jobs[0].scope.ApplicationID != appID {
		t.Fatalf("jobs: %+v", jobs)
	}
	var p JobPayload
	if err := json.Unmarshal(jobs[0].payload, &p); err != nil || p.ApplicationID != appID {
		t.Errorf("the payload is a reference to the application: %s %v", jobs[0].payload, err)
	}
	if err := jobs[0].scope.Validate(); err != nil {
		t.Errorf("the job's scope: %v", err)
	}

	// the same period again queues nothing new; the next period queues again
	h.clk.Advance(time.Minute)
	if res, err = h.w.Sweep(ctx); err != nil || res.Enqueued != 0 || res.Existing != 1 {
		t.Errorf("same period: %+v %v", res, err)
	}
	h.clk.Advance(DefaultSweepPeriod)
	if res, err = h.w.Sweep(ctx); err != nil || res.Enqueued != 1 {
		t.Errorf("next period: %+v %v", res, err)
	}

	// an application the watcher quarantined is still watched: it needs a report
	// that postdates the quarantine
	if _, err := h.w.ReportDrift(ctx, appID, Drift{Endpoint: "e", Tool: "t", Kind: "schema_changed"}); err != nil {
		t.Fatal(err)
	}
	if h.state(live) != apppkg.StateQuarantined {
		t.Fatal("fixture")
	}
	h.clk.Advance(DefaultSweepPeriod)
	if res, err = h.w.Sweep(ctx); err != nil || res.Applications != 1 {
		t.Errorf("a package the watcher quarantined is still watched: %+v %v", res, err)
	}
}

func TestSweepNeedsAQueueAndStopsWhenItIsFull(t *testing.T) {
	h := newHarness(t) // no queue
	if _, err := h.w.Sweep(ctx); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("without a queue: %v", err)
	}

	h = newHarness(t, func(o *hopts) { o.queue = true })
	h.goLive(validMinimalDir)
	h.enq.err = contracts.New(contracts.ErrResourceExhausted, "the class is full")
	if _, err := h.w.Sweep(ctx); !contracts.HasCode(err, contracts.ErrResourceExhausted) {
		t.Errorf("a full class stops the pass, untouched: %v", err)
	}
}

// The job body is the check, under a service identity and nothing else: it is
// refused for a user's job and for a payload that is not the one reference.
func TestHandleJob(t *testing.T) {
	h := newHarness(t)
	live := h.goLive(validMinimalDir)
	good, _ := json.Marshal(JobPayload{ApplicationID: appID})

	for name, tc := range map[string]struct {
		identity string
		payload  string
		want     contracts.ErrCode
	}{
		"no service identity":        {"", string(good), contracts.ErrPermissionDenied},
		"another service identity":   {"history.retention", string(good), contracts.ErrPermissionDenied},
		"an empty payload":           {ServiceIdentity, "", contracts.ErrValidationFailed},
		"null":                       {ServiceIdentity, "null", contracts.ErrValidationFailed},
		"no application":             {ServiceIdentity, `{"application_id":""}`, contracts.ErrValidationFailed},
		"an unknown member":          {ServiceIdentity, `{"application_id":"demo-library","force":true}`, contracts.ErrValidationFailed},
		"something after the object": {ServiceIdentity, `{"application_id":"demo-library"} {}`, contracts.ErrValidationFailed},
	} {
		err := h.w.HandleJob(ctx, tc.identity, json.RawMessage(tc.payload))
		if !contracts.HasCode(err, tc.want) {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
		if retry := contracts.CodeOf(err).Retryable(); retry {
			t.Errorf("%s: a refusal must not be retried", name)
		}
	}
	h.mustServe(live.ManifestHash)

	// the job runs the check: a drift the adapters flagged stops traffic
	h.clk.Advance(2 * time.Second)
	h.drifts = []Drift{{Endpoint: "library-mcp", Tool: "reserve_title", Kind: "tool_missing"}}
	if err := h.w.HandleJob(ctx, ServiceIdentity, good); err != nil {
		t.Fatal(err)
	}
	h.mustNotServe("the job body is the check")
}

func TestJobScopeIsValidAndAboutOneApplication(t *testing.T) {
	sc := JobScope("dep_1", "demo-library")
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	if sc.ApplicationID != "demo-library" || sc.DeploymentID != "dep_1" || sc.PrincipalID != SystemPrincipal {
		t.Errorf("%+v", sc)
	}
	if JobClass != 6 {
		t.Errorf("the schedule is class 6 (maintenance)")
	}
}
