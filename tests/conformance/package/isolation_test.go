package conformancepkg

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	ragsources "github.com/buxee/alix-ai/internal/rag/sources"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// ADR 0016, owed check 2: a session, a prepared action, a confirmation, a receipt, an audit
// event and a RAG source of application A never reach application B of the same process, and
// B's host never hears of any of it. B's scope is A's in every level but the application: the
// same tenant, workspace, principal and session identifiers, the case where a leak would be
// easiest, since only the application separates them.
func TestTwoApplicationsShareNothing(t *testing.T) {
	r := newRig(t)
	root := t.TempDir()
	a := r.register(signedCopy(t, root, "clinic-a"))
	b := r.register(signedCopy(t, root, "clinic-b"))
	const session = "ses-shared-0001"
	scA, scB := a.scope("receptionist", session), b.scope("receptionist", session)
	if scA.TenantID != scB.TenantID || scA.WorkspaceID != scB.WorkspaceID || scA.PrincipalID != scB.PrincipalID || scA.SessionID != scB.SessionID || scA.ApplicationID == scB.ApplicationID {
		t.Fatalf("the scopes must differ in the application and nothing else:\n%+v\n%+v", scA, scB)
	}
	ctx := context.Background()

	if got := r.reg.Applications(); len(got) != 2 {
		t.Fatalf("registry = %v, want both applications", got)
	}
	for _, app := range []*tenantApp{a, b} {
		info, err := r.reg.Info(ctx, app.id)
		if err != nil || len(info.Capabilities) != 5 {
			t.Fatalf("registry info of %s = %+v, %v: each application serves its own five capabilities", app.id, info, err)
		}
	}

	// A: a booking is prepared and waits for the confirmation only A's client holds.
	r.propose("appointments.book", bookLuca)
	prep := r.say(scA, "en", sayBook)
	if string(prep.Directive.Act) != "request_confirmation" || prep.Prepared == nil || prep.Confirmation.IsZero() {
		t.Fatalf("A's booking was not prepared: act %s code %q", prep.Directive.Act, prep.Code)
	}
	prepID := string(prep.Prepared.PreparationID)
	if a.calls.count(adapters.OpPrepare) != 1 || a.calls.count(adapters.OpCommit) != 0 {
		t.Fatalf("A's host was asked %v", a.calls.ops)
	}

	// a RAG source registered for A
	srcs, err := ragsources.New(r.db, r.clk)
	if err != nil {
		t.Fatal(err)
	}
	srcA := ids.New(source.IDPrefix)
	if err := srcs.Put(ctx, ragsources.Registration{Scope: scA, SourceID: srcA, PackageSourceID: "clinic-policies", PackageVersion: "1.0.0", ManifestHash: "hash-of-a"}); err != nil {
		t.Fatal(err)
	}

	t.Run("a session is not visible to another application", func(t *testing.T) {
		if st := mustOpen(t, r, scA).State; st.Goal == nil || st.Goal.CapabilityID != "appointments.book" {
			t.Fatalf("A's own session lost its goal: %+v", st.Goal)
		}
		other := mustOpen(t, r, scB)
		if other.State.Goal != nil {
			t.Fatalf("B's session, with A's session id, holds A's goal: %+v", other.State.Goal)
		}
		if n, err := r.store.TranscriptLen(ctx, scB); err != nil || n != 0 {
			t.Fatalf("B's transcript has %d turn(s) (%v): A's conversation leaked", n, err)
		}
		if n, err := r.store.TranscriptLen(ctx, scA); err != nil || n == 0 {
			t.Fatalf("A's own transcript is empty (%v)", err)
		}
	})

	t.Run("a prepared action cannot be read, confirmed, rejected or committed by another application", func(t *testing.T) {
		_, getErr := r.co.Get(ctx, scB, prepID)
		t.Logf("B reads A's preparation: %s", describeErr(getErr))
		if getErr == nil {
			t.Error("B read A's preparation")
		}
		_, confErr := r.co.Confirm(ctx, scB, actions.ConfirmInput{PreparationID: prepID, Token: prep.Confirmation})
		t.Logf("B confirms A's preparation with A's token: %s", describeErr(confErr))
		if confErr == nil {
			t.Error("B confirmed A's preparation with A's own confirmation token")
		}
		if _, err := r.co.Commit(ctx, scB, prepID); err == nil {
			t.Error("B committed A's preparation")
		}
		if _, err := r.co.Reject(ctx, scB, prepID); err == nil {
			t.Error("B rejected A's preparation")
		}
		if _, err := r.co.Receipt(ctx, scB, prepID); err == nil {
			t.Error("B read the receipt of A's preparation")
		}
		// the same attempt as a conversation turn: the confirmation of A's client, replayed in B's session
		res, err := r.eng.Turn(ctx, scB, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: prepID, Token: prep.Confirmation}})
		if err == nil && (res.Receipt != nil || res.Code == "") {
			t.Errorf("B's session confirmed A's preparation: act %s code %q receipt %v", res.Directive.Act, res.Code, res.Receipt)
		}
		// nothing happened: A's preparation still waits, and neither host was asked to commit
		if got, err := r.co.Get(ctx, scA, prepID); err != nil || got.State != act.PreparedActionStatePrepared {
			t.Errorf("A's preparation = %+v, %v: B's attempts must leave it as it was", got.State, err)
		}
		if a.calls.count(adapters.OpCommit) != 0 || b.calls.count(adapters.OpCommit) != 0 {
			t.Errorf("a commit reached a host: A %v, B %v", a.calls.ops, b.calls.ops)
		}
		if b.calls.total() != 0 {
			t.Errorf("B's host was asked %v although B did nothing of its own", b.calls.ops)
		}
	})

	t.Run("a source is not visible to another application", func(t *testing.T) {
		if list, err := srcs.List(ctx, scB); err != nil || len(list) != 0 {
			t.Fatalf("B lists %d source(s) (%v): A's source leaked", len(list), err)
		}
		if _, ok, err := srcs.Get(ctx, scB, srcA); err != nil || ok {
			t.Fatalf("B reads A's source by its id: ok=%v err=%v", ok, err)
		}
		if ok, err := srcs.Delete(ctx, scB, srcA); err != nil || ok {
			t.Fatalf("B removed A's source: ok=%v err=%v", ok, err)
		}
		// B registers its own source of the same package-level name: no collision, and each lists its own
		srcB := ids.New(source.IDPrefix)
		if err := srcs.Put(ctx, ragsources.Registration{Scope: scB, SourceID: srcB, PackageSourceID: "clinic-policies", PackageVersion: "1.0.0", ManifestHash: "hash-of-b"}); err != nil {
			t.Fatal(err)
		}
		la, _ := srcs.List(ctx, scA)
		lb, _ := srcs.List(ctx, scB)
		if len(la) != 1 || la[0].SourceID != srcA || la[0].ManifestHash != "hash-of-a" || len(lb) != 1 || lb[0].SourceID != srcB || lb[0].ManifestHash != "hash-of-b" {
			t.Fatalf("A lists %+v, B lists %+v", la, lb)
		}
	})

	t.Run("an application's audit events are its own", func(t *testing.T) {
		// B attempted things above, so it has events of its own to export
		textA, nA := r.auditText(scA)
		textB, nB := r.auditText(scB)
		if nA == 0 || nB == 0 {
			t.Fatalf("exported %d events for A and %d for B: both made requests", nA, nB)
		}
		if !strings.Contains(textA, `"application_id":"clinic-a"`) || strings.Contains(textA, "clinic-b") {
			t.Errorf("A's export names B or lacks A's own events")
		}
		if !strings.Contains(textB, `"application_id":"clinic-b"`) || strings.Contains(textB, "clinic-a") {
			t.Errorf("B's export names A or lacks B's own events")
		}
		if strings.Contains(textB, prepID) {
			t.Errorf("B's audit export holds A's preparation id")
		}
	})

	t.Run("each application's own flow is undisturbed, and its data is its own", func(t *testing.T) {
		// A's client confirms: one commit, at A's host only
		done, err := r.eng.Turn(ctx, scA, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: prepID, Token: prep.Confirmation}})
		if err != nil || done.Receipt == nil || done.Code != "" {
			t.Fatalf("A's confirmation: %+v, %v", done, err)
		}
		if a.calls.count(adapters.OpCommit) != 1 || b.calls.count(adapters.OpCommit) != 0 {
			t.Fatalf("commits: A %d, B %d, want 1 and 0", a.calls.count(adapters.OpCommit), b.calls.count(adapters.OpCommit))
		}
		if _, err := r.co.Receipt(ctx, scB, prepID); err == nil {
			t.Error("B reads A's receipt")
		}
		// B books the very same slot of its own host: A's booking took nothing from B
		r.propose("appointments.book", bookLuca)
		bprep := r.say(scB, "en", sayBook)
		if string(bprep.Directive.Act) != "request_confirmation" || bprep.Prepared == nil {
			t.Fatalf("B's booking of the slot A just took: act %s code %q: the applications share data", bprep.Directive.Act, bprep.Code)
		}
		bdone, err := r.eng.Turn(ctx, scB, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(bprep.Prepared.PreparationID), Token: bprep.Confirmation}})
		if err != nil || bdone.Receipt == nil {
			t.Fatalf("B's confirmation: %+v, %v", bdone, err)
		}
		if bprep.Prepared.PreparationID == prep.Prepared.PreparationID {
			t.Error("two applications were given the same preparation id")
		}
		// every request each host saw carried its own application id and no other
		if got := a.calls.applications(); len(got) != 1 || got["clinic-a"] == 0 {
			t.Errorf("A's host saw applications %v", got)
		}
		if got := b.calls.applications(); len(got) != 1 || got["clinic-b"] == 0 {
			t.Errorf("B's host saw applications %v", got)
		}
	})
}

func mustOpen(t *testing.T, r *rig, sc contracts.Scope) sessions.Session {
	t.Helper()
	s, err := r.store.Open(context.Background(), sc)
	if err != nil {
		t.Fatalf("open the session of %s: %v", sc.ApplicationID, err)
	}
	return s
}
