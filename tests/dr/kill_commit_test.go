package dr

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const bookCommitTool = "appointments_book_commit"

// TestDRKillDuringCommit kills the service in the middle of a commit, at the two points that matter
// (master plan 18, "Commit response uncertain: query idempotency/status before retrying", and "in-flight
// action remains uncommitted unless receipt proves otherwise"):
//
//   - before the application has seen the commit: nothing was applied, and after the restart the
//     preparation is back where it was, with no receipt;
//   - after the application applied it and before the reply came back: the booking exists, and after the
//     restart the service finds it by asking the application's status, writes the receipt and does not
//     send the commit again.
//
// Either way the write-ahead record (the committing state and its audit event, SEC-087) was there before
// the application was called, the application holds at most one booking, and the audit chains verify.
func TestDRKillDuringCommit(t *testing.T) {
	for _, tc := range []struct {
		point       killPoint
		wantState   string
		wantCommits int    // commits the application ends up with for this booking
		wantReceipt string // "" when no receipt may exist
	}{
		{killBeforeApply, "confirmed", 0, ""},
		{killAfterApply, "committed", 1, "committed"},
	} {
		t.Run(string(tc.point), func(t *testing.T) {
			drKillDuringCommit(t, tc.point, tc.wantState, tc.wantCommits, tc.wantReceipt)
		})
	}
}

func drKillDuringCommit(t *testing.T, point killPoint, wantState string, wantCommits int, wantReceipt string) {
	e := sharedEnv(t)
	d := e.newDeployment(t)
	w := seedWorld(t, e.clinic, "kill", 2)
	scenario := "kill during commit, " + strings.ReplaceAll(string(point), "_", " ")

	// The preparation is made before the service starts (see the package comment).
	c := d.openCore()
	prep := c.prepareBooking(w, 0)
	c.close()
	commitsBefore := e.clinic.stats(t).commitsOf("appointments_book")
	proxyBefore := e.proxy.calls(bookCommitTool)

	srv := d.startServe()
	inj := e.proxy.arm(bookCommitTool, point)
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	res := make(chan outcome, 1)
	confirmToken := w.hostToken(t, prep.SessionID)
	go func() {
		status, body, err := srv.call(http.MethodPost, "/v1/actions/"+prep.ID+"/confirm", confirmToken, map[string]any{"token": prep.Secret})
		res <- outcome{status, body, err}
	}()

	if err := waitFor(inj.reached, "the commit to reach the application's door", time.Minute); err != nil {
		t.Fatalf("%v\n%s", err, srv.logs())
	}
	if point == killAfterApply {
		if err := waitFor(inj.applied, "the application to answer the commit", time.Minute); err != nil {
			t.Fatal(err)
		}
		if got := e.clinic.stats(t).commitsOf("appointments_book"); got != commitsBefore+1 {
			t.Fatalf("the application has %d commits, want %d: it should have applied the booking before the reply was lost", got, commitsBefore+1)
		}
	}
	// The write-ahead state is visible while the commit is in flight: the service wrote "committing"
	// (and its audit event) before it called the application.
	if got := srv.preparationState(t, w, prep.SessionID, prep.ID); got != "committing" {
		t.Fatalf("while the commit is in flight the preparation is %q, want committing", got)
	}

	killedAt := time.Now()
	srv.kill()
	select {
	case o := <-res:
		if o.err == nil && o.status == http.StatusOK {
			t.Fatalf("the confirmation was answered with a receipt although the service was killed mid-commit: %s", o.body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the client of the killed service never gave up")
	}
	e.proxy.releaseAll()
	if got := e.proxy.calls(bookCommitTool) - proxyBefore; got != 1 {
		t.Fatalf("the commit reached the application's door %d times before the restart, want 1", got)
	}

	// ---- the restart ----
	srv2 := d.startServe()
	restartAfter := time.Since(killedAt)
	recordTiming("service restart after a kill "+strings.ReplaceAll(string(point), "_", " "), srv2.ready,
		"start to listening, including resolving the interrupted commit; %s after the kill", restartAfter.Round(time.Millisecond))
	logs := srv2.stderrText()
	if !strings.Contains(logs, "interrupted commits resolved before serving") || !strings.Contains(logs, `"examined":1`) {
		t.Errorf("the restarted service did not report resolving one interrupted commit:\n%s", logs)
	}

	if got := srv2.preparationState(t, w, prep.SessionID, prep.ID); got != wantState {
		t.Fatalf("after the restart the preparation is %q, want %q", got, wantState)
	}
	status, body := srv2.receipt(t, w, prep.SessionID, prep.ID)
	if wantReceipt == "" {
		if status == http.StatusOK {
			t.Fatalf("a receipt exists for a commit the application never saw: %s", body)
		}
	} else if status != http.StatusOK || !strings.Contains(string(body), `"status":"`+wantReceipt+`"`) {
		t.Fatalf("the receipt after the restart = %d %s, want a %s receipt", status, body, wantReceipt)
	}
	if got := e.clinic.stats(t).commitsOf("appointments_book") - commitsBefore; got != wantCommits {
		t.Fatalf("the application was asked to commit %d time(s), want %d: a recovery must never send the commit again", got, wantCommits)
	}
	if got := e.proxy.calls(bookCommitTool) - proxyBefore; got != 1 {
		t.Fatalf("the commit tool was called %d times in all, want exactly the one that was interrupted", got)
	}

	// The confirmation secret was spent by the first request: presenting it again is a replay and
	// reaches no application.
	status, body, err := srv2.call(http.MethodPost, "/v1/actions/"+prep.ID+"/confirm", w.hostToken(t, prep.SessionID), map[string]any{"token": prep.Secret})
	if err != nil || status != http.StatusConflict {
		t.Fatalf("presenting the spent confirmation again = %d, %v: %s, want 409", status, err, body)
	}

	if point == killBeforeApply {
		// What the user does after a commit that was proven not to have been applied: starts again
		// from a new preparation. The slot is still free and the booking is applied exactly once.
		if status, _, err := srv2.call(http.MethodPost, "/v1/actions/"+prep.ID+"/reject", w.hostToken(t, prep.SessionID), nil); err != nil || status != http.StatusOK {
			t.Fatalf("declining the reopened preparation = %d, %v", status, err)
		}
		c := d.openCore()
		again := c.prepareBooking(w, 0)
		c.close()
		status, body, err := srv2.call(http.MethodPost, "/v1/actions/"+again.ID+"/confirm", w.hostToken(t, again.SessionID), map[string]any{"token": again.Secret})
		if err != nil || status != http.StatusOK || !strings.Contains(string(body), `"status":"committed"`) {
			t.Fatalf("the fresh booking after the recovery = %d, %v: %s", status, err, body)
		}
		if got := e.clinic.stats(t).commitsOf("appointments_book") - commitsBefore; got != 1 {
			t.Fatalf("after the fresh booking the application holds %d commits for this drill, want 1", got)
		}
	}
	srv2.kill()

	// ---- what the store says ----
	snap := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := snap.chainsOK(); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, d.cfgPath, d.dbPath())
	defer db.Close()
	events := eventsOf(t, db, prep.ID)
	for _, typ := range []string{"action.prepared", "action.confirmed", "action.committing"} {
		if !hasEvent(events, typ) {
			t.Errorf("the audit trail of %s lacks %s: %v", prep.ID, typ, events)
		}
	}
	if got := hasEvent(events, "action.committed"); got != (wantReceipt == "committed") {
		t.Errorf("audit trail %v: committed event present = %v, want %v", events, got, wantReceipt == "committed")
	}
	r, haveReceipt := snap.receipt(prep.ID)
	if haveReceipt != (wantReceipt != "") || (haveReceipt && r.Status != wantReceipt) {
		t.Errorf("receipt rows for %s: have %v %+v, want status %q", prep.ID, haveReceipt, r, wantReceipt)
	}
	n := 0
	for _, rr := range snap.Receipts {
		if rr.PreparationID == prep.ID {
			n++
		}
	}
	if n > 1 {
		t.Errorf("%d receipts for one preparation", n)
	}
	trail := ""
	if point == killBeforeApply {
		trail = " (the second action.confirmed is the recovery reopening the preparation; action.rejected is the user declining it before booking again)"
	}
	recordObservation(scenario, "killed while the commit was in flight; after the restart the preparation is %s, the application was asked to commit %d time(s), audit trail %v%s; the database now holds %s",
		wantState, wantCommits, events, trail, snap.Summary())
	t.Logf("%s: preparation %s, application commits %d, audit trail %v; %s", scenario, wantState, wantCommits, events, snap)
}
