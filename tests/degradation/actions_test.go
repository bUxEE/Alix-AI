package degradation

import (
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// auditCount counts the audit events of one type.
func (w *world) auditCount(eventType string) int {
	w.t.Helper()
	var n int
	if err := w.db.QueryRowContext(ctxb(), `SELECT count(*) FROM audit_events WHERE event_type = ?`, eventType).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) goal() (preparedID string, confirmed bool, present bool) {
	w.t.Helper()
	s, err := w.store.Open(ctxb(), memberScope())
	if err != nil {
		w.t.Fatal(err)
	}
	if s.State.Goal == nil {
		return "", false, false
	}
	return s.State.Goal.PreparedID, s.State.Goal.Confirmed, true
}

// Section 18, "MCP/API adapter unavailable": preserve session and prepared-state safety; report no commit.
func TestDegradeAdapterUnavailable(t *testing.T) {
	t.Run("the host is down when the user confirms", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		id := string(prep.Prepared.PreparationID)
		// Control: the preparation exists, nothing has reached the host's Commit, the session holds the goal.
		if got := w.prepState(id); got != "prepared" {
			t.Fatalf("setup: state = %s, want prepared", got)
		}
		if p, c, ok := w.goal(); !ok || p != id || c {
			t.Fatalf("setup: goal prepared=%q confirmed=%v present=%v", p, c, ok)
		}

		w.app.SetAvailable(false)
		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}

		// Report no commit: a typed failure, no receipt, nothing applied, no committed event.
		if string(res.Directive.Act) != "report_failure" || res.Code != contracts.ErrUnavailable {
			t.Fatalf("act %s code %q, want report_failure / unavailable", res.Directive.Act, res.Code)
		}
		if res.Receipt != nil || w.app.Effects() != 0 || w.hostCalls(adapters.OpCommit) != 0 {
			t.Fatalf("a commit was reported or applied: receipt %+v effects %d commits %d", res.Receipt, w.app.Effects(), w.hostCalls(adapters.OpCommit))
		}
		if n := w.auditCount("action.committed"); n != 0 {
			t.Fatalf("%d committed events recorded for a commit that never reached the host", n)
		}
		// Prepared-state safety: still confirmed (not committed, not invalidated, not lost), the single-use
		// confirmation is not asked for again, and the session still holds the goal that points at it.
		if got := w.prepState(id); got != "confirmed" {
			t.Fatalf("state = %s, want confirmed: the preparation must wait for the host", got)
		}
		if p, c, ok := w.goal(); !ok || p != id || !c {
			t.Fatalf("session goal prepared=%q confirmed=%v present=%v, want the confirmed preparation kept", p, c, ok)
		}

		// The host comes back: the SAME confirmation commits exactly once, and is not asked for twice.
		w.app.SetAvailable(true)
		retry, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if retry.Receipt == nil || retry.Receipt.Status != act.ActionReceiptStatusCommitted || w.app.Effects() != 1 {
			t.Fatalf("retry: receipt %+v effects %d, want one committed effect", retry.Receipt, w.app.Effects())
		}
		if n := w.auditCount("action.confirmed"); n != 1 {
			t.Fatalf("%d confirmations recorded, want 1", n)
		}
	})

	t.Run("the host is down when the action is prepared", func(t *testing.T) {
		w := newWorld(t)
		w.app.SetAvailable(false)
		w.model.On(pfakeProposal()).Reply(createProposal).Times(1)
		res := w.say(sayCreate)
		if res.Code != contracts.ErrUnavailable || string(res.Directive.Act) != "report_failure" {
			t.Fatalf("act %s code %q, want report_failure / unavailable", res.Directive.Act, res.Code)
		}
		if res.Prepared != nil || w.app.Effects() != 0 {
			t.Fatalf("prepared %v effects %d", res.Prepared, w.app.Effects())
		}
		var n int
		if err := w.db.QueryRowContext(ctxb(), `SELECT count(*) FROM action_preparations`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d preparations stored for a host that was down (err %v)", n, err)
		}
		// The session is intact: the values the user gave are kept, no preparation is referenced, and the next turn
		// works once the host is back.
		if p, c, _ := w.goal(); p != "" || c {
			t.Errorf("a failed prepare left the goal pointing at preparation %q (confirmed %v)", p, c)
		}
		w.app.SetAvailable(true)
		w.model.On(pfakeProposal()).Reply(createProposal).Times(1)
		w.model.On(pfakeAct("request_confirmation")).Reply("Please confirm: create an item named widget.")
		if again := w.say(sayCreate); again.Prepared == nil {
			t.Fatalf("after the host came back: act %s code %q", again.Directive.Act, again.Code)
		}
	})

	t.Run("the host reports a failed dependency", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		w.app.Inject(adapters.OpCommit, fake.FaultDependencyDown)
		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatal(err)
		}
		if res.Code != contracts.ErrUnavailable || res.Receipt != nil || w.app.Effects() != 0 {
			t.Fatalf("code %q receipt %+v effects %d: unavailable is not success", res.Code, res.Receipt, w.app.Effects())
		}
		if got := w.prepState(string(prep.Prepared.PreparationID)); got != "confirmed" {
			t.Fatalf("state = %s, want confirmed", got)
		}
	})
}

// Section 18, "Commit response uncertain": query idempotency/status before retrying.
func TestDegradeCommitUncertain(t *testing.T) {
	commitKeys := func(w *world) []string {
		var keys []string
		for _, c := range w.app.Calls() {
			if c.Operation == adapters.OpCommit {
				keys = append(keys, c.IdempotencyKey)
			}
		}
		return keys
	}

	t.Run("control: a healthy commit asks no status", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		w.model.On(pfakeAct("report_outcome")).Reply("Done. The item widget was created.")
		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil || res.Receipt == nil || res.Receipt.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("receipt %+v err %v", res.Receipt, err)
		}
		if w.hostCalls(adapters.OpStatus) != 0 || w.hostCalls(adapters.OpCommit) != 1 {
			t.Fatalf("host ops %v: a certain commit must not ask Status", w.hostOps())
		}
	})

	t.Run("the reply was lost but the host applied it: status says so, nothing is sent again", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		w.app.Inject(adapters.OpCommit, fake.FaultDropReply)
		w.model.On(pfakeAct("report_outcome")).Reply("Done. The item widget was created.")
		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatal(err)
		}
		if res.Receipt == nil || res.Receipt.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("receipt %+v, want committed once Status proved it", res.Receipt)
		}
		if w.hostCalls(adapters.OpCommit) != 1 || w.hostCalls(adapters.OpStatus) != 1 || w.app.Effects() != 1 {
			t.Fatalf("host ops %v effects %d: want one Commit and one Status", w.hostOps(), w.app.Effects())
		}
		// Status was asked AFTER the commit and before anything else touched the host.
		ops := w.hostOps()
		if ops[len(ops)-1] != adapters.OpStatus || ops[len(ops)-2] != adapters.OpCommit {
			t.Fatalf("host operations %v: Status must follow the uncertain Commit", ops)
		}
	})

	t.Run("status is lost as well: uncertain, never retried blindly, resolved only by asking the host", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		id := string(prep.Prepared.PreparationID)
		w.app.Inject(adapters.OpCommit, fake.FaultDropReply)   // applied; the reply is lost
		w.app.Inject(adapters.OpStatus, fake.FaultDropRequest) // and so is the question about it
		w.model.On(pfakeAct("report_outcome")).Reply("Done. The item widget was created.")

		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatal(err)
		}
		if res.Receipt == nil || res.Receipt.Status != act.ActionReceiptStatusUncertain || res.Code != contracts.ErrUncertainOutcome {
			t.Fatalf("receipt %+v code %q, want uncertain / uncertain_outcome", res.Receipt, res.Code)
		}
		// The model claimed completion; nothing proves it, so the template speaks and does not claim it either.
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response %+v, want the template", res.Response)
		}
		if got := w.prepState(id); got != "uncertain" {
			t.Fatalf("state = %s, want uncertain", got)
		}

		// Asking to commit again does not send it again: the coordinator asks the host first, and the only
		// thing that may reach the host is a question.
		before := len(commitKeys(w))
		for i := 0; i < 3; i++ {
			if _, err := w.co.Commit(ctxb(), memberScope(), id); err != nil && contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
				t.Fatalf("Commit #%d: %v", i, err)
			}
		}
		if got := len(commitKeys(w)); got != before {
			t.Fatalf("%d Commit calls reached the host after an uncertain outcome (had %d): a blind retry", got, before)
		}
		// Reconcile asks the host through Status and the idempotency key, and learns the truth.
		rc, err := w.co.Reconcile(ctxb(), memberScope(), id)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if rc.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("reconciled receipt %+v, want committed (the host did apply it)", rc)
		}
		if w.app.Effects() != 1 || len(commitKeys(w)) != 1 {
			t.Fatalf("effects %d commits %d, want exactly one of each", w.app.Effects(), len(commitKeys(w)))
		}
		// Idempotent from here on: the same receipt, nothing sent.
		again, err := w.co.Commit(ctxb(), memberScope(), id)
		if err != nil || again.ReceiptID != rc.ReceiptID || len(commitKeys(w)) != 1 {
			t.Fatalf("second commit: %+v err %v commits %d", again, err, len(commitKeys(w)))
		}
	})

	t.Run("the request was lost: status shows no record, and only then is it sent again, with the same key", func(t *testing.T) {
		w := newWorld(t)
		prep := w.prepareCreate()
		id := string(prep.Prepared.PreparationID)
		w.app.Inject(adapters.OpCommit, fake.FaultDropRequest)
		res, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil {
			t.Fatal(err)
		}
		if res.Receipt != nil || w.app.Effects() != 0 {
			t.Fatalf("receipt %+v effects %d: nothing was applied", res.Receipt, w.app.Effects())
		}
		if w.hostCalls(adapters.OpStatus) != 1 {
			t.Fatalf("host ops %v: Status must be asked before anything is sent again", w.hostOps())
		}
		w.model.On(pfakeAct("report_outcome")).Reply("Done. The item widget was created.")
		retry, err := w.eng.Turn(ctxb(), memberScope(), confirmInput(prep))
		if err != nil || retry.Receipt == nil || retry.Receipt.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("retry: %+v err %v", retry.Receipt, err)
		}
		keys := commitKeys(w)
		if len(keys) != 2 || keys[0] != keys[1] || keys[0] == "" {
			t.Fatalf("commit idempotency keys %v: the retry must reuse the key", keys)
		}
		if w.app.Effects() != 1 {
			t.Fatalf("effects %d, want 1", w.app.Effects())
		}
		_ = id
	})
}
