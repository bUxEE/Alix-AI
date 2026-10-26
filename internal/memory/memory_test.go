package memory

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

func TestScopeIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	me := scopeOf(testApp, tenA, ws1, alice)
	it := e.create(me, "prefer dark mode")

	for name, sc := range map[string]contracts.Scope{
		"other principal": scopeOf(testApp, tenA, ws1, bob),
		"other tenant":    scopeOf(testApp, tenB, ws1, alice),
		"other workspace": scopeOf(testApp, tenA, ws2, alice),
	} {
		t.Run(name, func(t *testing.T) {
			list, err := e.svc.List(ctx, sc, ListQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 0 {
				t.Fatalf("saw %d items", len(list.Items))
			}
			_, err = e.svc.Patch(ctx, sc, it.ID, mem.MemoryPatch{Text: ptrText("x")})
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Fatalf("patch = %v", err)
			}
			_, err = e.svc.Confirm(ctx, sc, it.ID)
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Fatalf("confirm = %v", err)
			}
		})
	}
}

func TestConsentProposalNeverActiveWithoutConfirm(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice)
	prop, err := e.svc.Propose(ctx, sc, ProposalDraft{Text: "user likes tea"})
	if err != nil {
		t.Fatal(err)
	}
	if prop.Status != mem.MemoryStatusProposed {
		t.Fatalf("status = %s", prop.Status)
	}
	list, err := e.svc.List(ctx, sc, ListQuery{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("active = %d", len(list.Items))
	}
	got, err := e.svc.Confirm(ctx, sc, prop.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != mem.MemoryStatusActive {
		t.Fatalf("after confirm = %s", got.Status)
	}
}

func TestProposalExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice)
	prop, err := e.svc.Propose(ctx, sc, ProposalDraft{Text: "likes coffee"})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	_, err = e.svc.Confirm(ctx, sc, prop.ID)
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("confirm = %v", err)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Details["reason"] != "memory_proposal_expired" {
		t.Fatalf("details = %#v", err)
	}
}

func TestProposalDayLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice)
	for i := 0; i < 20; i++ {
		if _, err := e.svc.Propose(ctx, sc, ProposalDraft{Text: "more " + strings.Repeat("x", i+1)}); err != nil {
			t.Fatalf("propose %d: %v", i+1, err)
		}
	}
	_, err := e.svc.Propose(ctx, sc, ProposalDraft{Text: "overflow"})
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("day limit = %v", err)
	}
}

func TestRetrieveRanksLexicalAndRecency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice)
	e.create(sc, "favourite colour is blue")
	e.clk.Advance(time.Minute)
	e.create(sc, "favourite drink is tea")
	got, err := e.svc.RetrieveActive(ctx, sc, "tea drink")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !strings.Contains(string(got[0].Item.Text), "tea") {
		t.Fatalf("got = %+v", got)
	}
}

func TestEraseAndExport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := scopeOf(testApp, tenA, ws1, alice)
	e.create(sc, "secret preference")
	var buf bytes.Buffer
	res, err := e.svc.Export(ctx, sc, &buf)
	if err != nil || res.Rows != 1 {
		t.Fatalf("export = %+v %v", res, err)
	}
	if !strings.Contains(buf.String(), `"kind":"memory"`) {
		t.Fatalf("body = %s", buf.String())
	}
	err = e.db.Tx(ctx, func(tx *store.Tx) error {
		out, err := EraseMemory(ctx, tx, EraseSpec{ApplicationID: testApp, TenantID: tenA, PrincipalID: alice})
		if err != nil {
			return err
		}
		if out.Items != 1 {
			t.Fatalf("erase items = %d", out.Items)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.List(ctx, sc, ListQuery{})
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("after erase list = %+v %v", list, err)
	}
}

func ptrText(s string) *mem.MemoryText {
	t := mem.MemoryText(s)
	return &t
}
