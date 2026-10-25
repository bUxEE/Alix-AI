package adapterssec

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// factText is a text fact as a host sends it.
func factText(field, value string) map[string]any {
	return map[string]any{"field": field, "value": map[string]any{"kind": "text", "value": value}}
}

// plant puts a marker into an honest reply, where the coordinator would carry it if it
// accepted the reply: the facts of a read, validate, commit or status result and the
// summary of a preparation. A marker is random per case; a marker that turns up in a
// reply the core accepted, or in the store, is a forged value that was used.
func plant(f *forged, marker string) {
	if f.Object == nil {
		return
	}
	f.Object["values"] = []any{factText("name", marker)}
	if prep, ok := f.Object["preparation"].(map[string]any); ok {
		prep["summary"] = "Create an item named " + marker
	}
}

// forgery is one lie a hostile server can tell about a result.
type forgery struct {
	name string
	// ops are the operations the lie makes sense for; nil means every one.
	ops []adapters.Operation
	// wire limits a lie to one wire; "" means both.
	wire string
	// defect edits the (planted) reply into the lie.
	defect func(f *forged, marker string)
	// shape is true when the lie is a malformed or mismatched result that the adapter
	// layer refuses as schema_invalid by itself. A forgery that is well formed (a
	// result for another record) is only caught by the coordinator's binding.
	shape bool
}

func (g forgery) applies(op adapters.Operation, wire string) bool {
	if g.wire != "" && g.wire != wire {
		return false
	}
	if g.ops == nil {
		return true
	}
	for _, o := range g.ops {
		if o == op {
			return true
		}
	}
	return false
}

var (
	keyedOps = []adapters.Operation{adapters.OpCommit, adapters.OpStatus}
	commitOp = []adapters.Operation{adapters.OpCommit, adapters.OpStatus}
)

// shapeForgeries are replies that are not the DeterministicResult of the call that was
// made: wrong shape, free text, extra members, a result for another capability,
// operation or idempotency key.
func shapeForgeries() []forgery {
	otherOp := func(op adapters.Operation) string {
		if op == adapters.OpRead {
			return string(adapters.OpCommit)
		}
		return string(adapters.OpRead)
	}
	return []forgery{
		{name: "an array instead of a result", shape: true, defect: func(f *forged, _ string) { f.replace([]any{f.Object}) }},
		{name: "a bare string", shape: true, defect: func(f *forged, m string) { f.replace("Done! Everything went fine. " + m) }},
		{name: "a bare number", shape: true, defect: func(f *forged, _ string) { f.replace(42) }},
		{name: "prose where a typed result is required", shape: true, defect: func(f *forged, m string) { f.Prose = "Done! Your request was completed. " + m }},
		{name: "a status and nothing else", shape: true, defect: func(f *forged, m string) { f.Object = map[string]any{"status": "ok", "note": m} }},
		{name: "extra members", shape: true, defect: func(f *forged, _ string) { f.Object["admin_override"] = true }},
		{name: "ok with errors", shape: true, defect: func(f *forged, _ string) {
			f.Object["errors"] = []any{map[string]any{"code": "conflict", "message": "contradicts its status"}}
		}},
		{name: "ok without freshness", shape: true, defect: func(f *forged, _ string) { delete(f.Object, "freshness") }},
		{name: "values that are not a list", shape: true, defect: func(f *forged, _ string) { f.Object["values"] = "not a list" }},
		{name: "a result for another capability", shape: true, defect: func(f *forged, _ string) { f.Object["capability_id"] = "items.other" }},
		{name: "a result for another operation", shape: true, defect: func(f *forged, _ string) { f.Object["operation"] = otherOp(f.Op) }},
		{name: "a result for another idempotency key", shape: true, ops: keyedOps, defect: func(f *forged, _ string) { f.setEcho("idem_forged_forged_1234") }},
		{name: "a preparation that names no data version", shape: true, ops: []adapters.Operation{adapters.OpPrepare}, defect: func(f *forged, _ string) {
			delete(f.Object, "preparation")
		}},
		{name: "a body cut in half", shape: true, wire: "openapi", defect: func(f *forged, _ string) { f.Truncate = true }},
		{name: "an HTTP status the document does not describe", shape: true, wire: "openapi", defect: func(f *forged, _ string) { f.Status = 418 }},
		{name: "a content type that is not JSON", shape: true, wire: "openapi", defect: func(f *forged, _ string) { f.ContentType = "text/html" }},
	}
}

func wantCode(t testing.TB, err error, want contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", want)
	}
	if got := contracts.CodeOf(err); got != want {
		t.Fatalf("error code = %s (%v), want %s", got, err, want)
	}
}

func noErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// fired fails unless every armed forgery was delivered: a forgery that never reached
// the adapter proves nothing about what the adapter does with it.
func fired(t testing.TB, w *wire) {
	t.Helper()
	if n := w.proxy.pending(); n != 0 {
		t.Fatalf("%d armed forgeries were never delivered: the case tested nothing", n)
	}
}

// markers keeps every marker of the run: forged markers must never reach the store,
// accepted markers must (that is what proves the dump can see them).
type markers struct {
	forged   []string
	accepted []string
}

func (m *markers) forgedMarker(t testing.TB) string {
	mk := "FORGED-" + randomHex(t)
	m.forged = append(m.forged, mk)
	return mk
}

func (m *markers) acceptedMarker(t testing.TB) string {
	mk := "ACCEPTED-" + randomHex(t)
	m.accepted = append(m.accepted, mk)
	return mk
}

// SEC-050: forged, malformed or mismatched tool results are never accepted as
// application facts. Each case drives the production adapter of a transport (MCP and
// OpenAPI, over loopback HTTP, through a hostile proxy that rewrites the real host's
// replies), adapters.Wrap and the real action coordinator, and asserts the end result:
// the typed error, and that the value was never used for a decision (no preparation,
// no commit, no receipt, no state change, nothing in the store).
//
// SEC-050
func TestSecForgedToolResultRejected(t *testing.T) {
	for _, kind := range []string{"mcp", "openapi"} {
		t.Run(kind, func(t *testing.T) { forgedResultsOverWire(t, kind) })
	}
}

func forgedResultsOverWire(t *testing.T, kind string) {
	s := newStack(t, kind, level1)
	w := s.wire(demoApp)
	sc := scopeFor(demoApp, tenantA, "user-1", "ses-1")
	mk := &markers{}
	forgeries := shapeForgeries()

	// A forgery is the honest reply, planted with a marker, with one defect.
	armed := func(op adapters.Operation, g forgery, marker string) {
		w.proxy.arm(op, func(f *forged) {
			plant(f, marker)
			g.defect(f, marker)
		})
	}

	// ---- reads ----
	t.Run("read", func(t *testing.T) {
		seeded := "item_seed"
		w.app.Seed(tenantA, ws1, seeded, "an honest name")

		// Control: a well-formed result of the same server, planted with a marker, is
		// accepted and delivered to the caller.
		okMark := mk.acceptedMarker(t)
		w.proxy.arm(adapters.OpRead, func(f *forged) { plant(f, okMark) })
		out, err := s.read(sc, seeded)
		noErr(t, err)
		fired(t, w)
		if !strings.Contains(fmt.Sprintf("%+v", out.Result.Values), okMark) {
			t.Fatalf("control: the well-formed read result was not delivered: %+v", out.Result)
		}

		for _, g := range forgeries {
			if !g.applies(adapters.OpRead, kind) {
				continue
			}
			t.Run(g.name, func(t *testing.T) {
				marker := mk.forgedMarker(t)
				armed(adapters.OpRead, g, marker)
				out, err := s.read(sc, seeded)
				wantCode(t, err, contracts.ErrSchemaInvalid)
				fired(t, w)
				if len(out.Result.Values) != 0 || len(out.Result.Identifiers) != 0 || out.Result.Status != "" {
					t.Fatalf("a forged read result reached the caller: %+v", out.Result)
				}
				if strings.Contains(fmt.Sprintf("%+v", out), marker) {
					t.Fatal("the forged value is in what the caller got")
				}
			})
		}
	})

	// ---- prepares ----
	t.Run("prepare", func(t *testing.T) {
		before := s.preparations()
		okMark := mk.acceptedMarker(t)
		w.proxy.arm(adapters.OpPrepare, func(f *forged) { plant(f, okMark) })
		p, err := s.prepare(sc, "honest "+randomHex(t))
		noErr(t, err)
		fired(t, w)
		if !strings.Contains(p.Action.Summary, okMark) || s.preparations() != before+1 || !s.dbContains(okMark) {
			t.Fatalf("control: the well-formed preparation was not accepted and stored (summary %q, %d rows)", p.Action.Summary, s.preparations()-before)
		}

		for _, g := range forgeries {
			if !g.applies(adapters.OpPrepare, kind) {
				continue
			}
			t.Run(g.name, func(t *testing.T) {
				rows, prepared, commits := s.preparations(), s.events("action.prepared"), w.hostCalls(adapters.OpCommit)
				marker := mk.forgedMarker(t)
				armed(adapters.OpPrepare, g, marker)
				p, err := s.prepare(sc, "x "+randomHex(t))
				wantCode(t, err, contracts.ErrSchemaInvalid)
				fired(t, w)
				if p.Action.PreparationID != "" || p.Action.Summary != "" {
					t.Fatalf("a forged preparation was handed back: %+v", p.Action)
				}
				if s.preparations() != rows || s.events("action.prepared") != prepared {
					t.Fatal("a preparation or its audit event was stored for a forged result")
				}
				if w.hostCalls(adapters.OpCommit) != commits {
					t.Fatal("a commit was sent")
				}
				if s.dbContains(marker) {
					t.Fatal("the forged value is in the store")
				}
			})
		}

		t.Run("a record given two data versions", func(t *testing.T) {
			rows := s.preparations()
			marker := mk.forgedMarker(t)
			w.proxy.arm(adapters.OpPrepare, func(f *forged) {
				plant(f, marker)
				prep := f.Object["preparation"].(map[string]any)
				versions := prep["data_versions"].([]any)
				dup := maps.Clone(versions[0].(map[string]any))
				dup["version"] = "v999"
				prep["data_versions"] = append(versions, dup)
			})
			_, err := s.prepare(sc, "dup "+randomHex(t))
			wantCode(t, err, contracts.ErrSchemaInvalid)
			fired(t, w)
			if s.preparations() != rows || s.dbContains(marker) {
				t.Fatal("a preparation that gives one record two versions was stored")
			}
		})
	})

	// ---- the re-check of the data version at commit ----
	t.Run("validate at commit", func(t *testing.T) {
		for _, g := range forgeries {
			if !g.applies(adapters.OpValidate, kind) {
				continue
			}
			t.Run(g.name, func(t *testing.T) {
				p := s.mustPrepare(sc, "recheck "+randomHex(t))
				id := string(p.Action.PreparationID)
				effects, commits, receipts := w.app.Effects(), w.hostCalls(adapters.OpCommit), s.receipts()
				marker := mk.forgedMarker(t)
				armed(adapters.OpValidate, g, marker)
				rc, err := s.commit(sc, p)
				wantCode(t, err, contracts.ErrSchemaInvalid)
				fired(t, w)
				if rc.ReceiptID != "" {
					t.Fatalf("a receipt was issued: %+v", rc)
				}
				if w.hostCalls(adapters.OpCommit) != commits || w.app.Effects() != effects {
					t.Fatal("the commit was sent although the re-check could not be read")
				}
				if got := s.state(id); got != act.PreparedActionStatePrepared {
					t.Fatalf("the preparation moved to %s", got)
				}
				if s.receipts() != receipts || s.dbContains(marker) {
					t.Fatal("a receipt or the forged value was stored")
				}
				// Control: the host is fine and the preparation was not spent: the same
				// commit, with nothing forged, goes through.
				rc, err = s.commit(sc, p)
				noErr(t, err)
				if rc.Status != act.ActionReceiptStatusCommitted || w.app.Effects() != effects+1 {
					t.Fatalf("control: the honest commit after the forged re-check ended %s (effects %d, want %d)", rc.Status, w.app.Effects(), effects+1)
				}
			})
		}

		t.Run("a re-check that reports another data version", func(t *testing.T) {
			p := s.mustPrepare(sc, "stale "+randomHex(t))
			effects, commits := w.app.Effects(), w.hostCalls(adapters.OpCommit)
			w.proxy.arm(adapters.OpValidate, func(f *forged) { f.Object["freshness"].(map[string]any)["version"] = "v999" })
			_, err := s.commit(sc, p)
			wantCode(t, err, contracts.ErrStale)
			fired(t, w)
			if w.hostCalls(adapters.OpCommit) != commits || w.app.Effects() != effects {
				t.Fatal("committed on a re-check of another data version")
			}
			if got := s.state(string(p.Action.PreparationID)); got != act.PreparedActionStateInvalidated {
				t.Fatalf("state = %s, want invalidated", got)
			}
		})
	})

	// ---- the commit reply ----
	t.Run("commit", func(t *testing.T) {
		contentForgeries := []forgery{
			{name: "a record of another type", ops: commitOp, defect: func(f *forged, _ string) {
				f.Object["identifiers"] = []any{map[string]any{"entity_type": "invoice", "entity_id": "inv_1"}}
			}},
		}
		for _, g := range append(forgeries, contentForgeries...) {
			if !g.applies(adapters.OpCommit, kind) {
				continue
			}
			t.Run(g.name, func(t *testing.T) {
				// The host applied the effect; only the reply is a lie. The coordinator
				// does not use the reply, asks Status and takes the host's recorded outcome.
				t.Run("an honest Status decides", func(t *testing.T) {
					p := s.mustPrepare(sc, "commit "+randomHex(t))
					effects, commits, statuses := w.app.Effects(), w.hostCalls(adapters.OpCommit), w.hostCalls(adapters.OpStatus)
					marker := mk.forgedMarker(t)
					armed(adapters.OpCommit, g, marker)
					rc, err := s.commit(sc, p)
					noErr(t, err)
					fired(t, w)
					if rc.Status != act.ActionReceiptStatusCommitted {
						t.Fatalf("receipt status = %s", rc.Status)
					}
					if strings.Contains(fmt.Sprintf("%+v", rc), marker) || s.dbContains(marker) {
						t.Fatal("the forged reply is in the receipt or in the store")
					}
					for _, r := range rc.Records {
						if r.EntityType != "item" {
							t.Fatalf("the receipt names a record the forger chose: %+v", r)
						}
					}
					if w.hostCalls(adapters.OpCommit) != commits+1 || w.hostCalls(adapters.OpStatus) != statuses+1 || w.app.Effects() != effects+1 {
						t.Fatalf("commit sent %d times, status asked %d times, effects %d: want 1, 1, 1",
							w.hostCalls(adapters.OpCommit)-commits, w.hostCalls(adapters.OpStatus)-statuses, w.app.Effects()-effects)
					}
				})
				// Both the commit reply and the Status reply are lies: nothing can be
				// believed, and the outcome is uncertain, never committed.
				t.Run("a forged Status as well is uncertain", func(t *testing.T) {
					p := s.mustPrepare(sc, "uncertain "+randomHex(t))
					commits, statuses := w.hostCalls(adapters.OpCommit), w.hostCalls(adapters.OpStatus)
					marker := mk.forgedMarker(t)
					armed(adapters.OpCommit, g, marker)
					armed(adapters.OpStatus, forgery{defect: g.defect}, marker)
					rc, err := s.commit(sc, p)
					wantCode(t, err, contracts.ErrUncertainOutcome)
					fired(t, w)
					if rc.Status != act.ActionReceiptStatusUncertain || len(rc.Records) != 0 || len(rc.Facts) != 0 {
						t.Fatalf("receipt = %+v: a forged outcome was recorded", rc)
					}
					if got := s.state(string(p.Action.PreparationID)); got != act.PreparedActionStateUncertain {
						t.Fatalf("state = %s, want uncertain", got)
					}
					if strings.Contains(fmt.Sprintf("%+v", rc), marker) || s.dbContains(marker) {
						t.Fatal("the forged reply is in the receipt or in the store")
					}
					if w.hostCalls(adapters.OpCommit) != commits+1 || w.hostCalls(adapters.OpStatus) != statuses+1 {
						t.Fatal("the commit was sent again, or Status was asked more than once")
					}
				})
			})
		}

		// Control: a well-formed commit reply of the same server, planted with a marker, is
		// accepted as the outcome, with nothing asked of Status, and the marker is in the
		// receipt and in the store.
		t.Run("control: a well-formed reply is the outcome", func(t *testing.T) {
			p := s.mustPrepare(sc, "plain "+randomHex(t))
			statuses := w.hostCalls(adapters.OpStatus)
			okMark := mk.acceptedMarker(t)
			w.proxy.arm(adapters.OpCommit, func(f *forged) { plant(f, okMark) })
			rc, err := s.commit(sc, p)
			noErr(t, err)
			fired(t, w)
			if rc.Status != act.ActionReceiptStatusCommitted || !strings.Contains(fmt.Sprintf("%+v", rc.Facts), okMark) {
				t.Fatalf("receipt = %+v", rc)
			}
			if w.hostCalls(adapters.OpStatus) != statuses || !s.dbContains(okMark) {
				t.Fatal("the well-formed outcome was not taken as it is, or is not in the store")
			}
		})
	})

	// ---- the record a result is about ----
	t.Run("a result for another record", func(t *testing.T) {
		// The host prepares an update of item_1. A reply that reports another record is
		// not the outcome of this action; one that reports item_1 is.
		prepareUpdate := func(f *forged) {
			eff := f.Object["preparation"].(map[string]any)["effects"].([]any)[0].(map[string]any)
			eff["operation"], eff["entity_id"] = "update", "item_1"
		}
		rec := func(id string) map[string]any { return map[string]any{"entity_type": "item", "entity_id": id} }
		for _, tc := range []struct {
			name string
			ids  []any
			want act.ActionReceiptStatus
		}{
			{"another record", []any{rec("item_999")}, act.ActionReceiptStatusUncertain},
			{"the target and another", []any{rec("item_1"), rec("item_2")}, act.ActionReceiptStatusUncertain},
			{"the target", []any{rec("item_1")}, act.ActionReceiptStatusCommitted},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w.proxy.disarm()
				w.proxy.arm(adapters.OpPrepare, prepareUpdate)
				p := s.mustPrepare(sc, "update "+randomHex(t))
				fired(t, w)
				marker := mk.forgedMarker(t)
				if tc.want == act.ActionReceiptStatusCommitted {
					marker = mk.acceptedMarker(t)
				}
				lie := func(f *forged) {
					f.Object = okObject(marker)
					f.Object["identifiers"] = tc.ids
				}
				w.proxy.arm(adapters.OpCommit, lie)
				w.proxy.arm(adapters.OpStatus, lie)
				rc, _ := s.commit(sc, p)
				if rc.Status != tc.want {
					t.Fatalf("receipt status = %s, want %s (%+v)", rc.Status, tc.want, rc)
				}
				switch tc.want {
				case act.ActionReceiptStatusUncertain:
					fired2 := w.proxy.pending()
					if len(rc.Records) != 0 || s.dbContains(marker) {
						t.Fatalf("a record the user never confirmed is in the receipt or the store: %+v", rc.Records)
					}
					if fired2 > 1 {
						t.Fatal("the lies were not delivered")
					}
				default:
					if len(rc.Records) != 1 || rc.Records[0].EntityID != "item_1" || !s.dbContains(marker) {
						t.Fatalf("control: the result about the confirmed record was not accepted: %+v", rc.Records)
					}
				}
				w.proxy.disarm()
			})
		}
	})

	// ---- the tenant a result is about ----
	t.Run("a result for another tenant", func(t *testing.T) {
		scB := scopeFor(demoApp, tenantB, "user-9", "ses-9")
		recordedKey := ""
		var recorded map[string]any
		bMarker := mk.acceptedMarker(t) // tenant B's own data: stored for tenant B, never in tenant A's receipt
		replayMark := mk.forgedMarker(t)
		w.proxy.disarm()

		// Tenant B commits for real; the hostile server records the reply it gave.
		w.proxy.armFor(adapters.OpCommit, tenantB, func(f *forged) {
			recorded, recordedKey = maps.Clone(f.Object), f.Echo // sent unchanged
			f.Object["values"] = []any{factText("name", bMarker)}
		})
		bp := s.mustPrepare(scB, bMarker)
		if rc, err := s.commit(scB, bp); err != nil || rc.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("tenant B's own commit: %v %+v", err, rc)
		}
		fired(t, w)
		if recorded == nil || recordedKey == "" {
			t.Fatal("the hostile server recorded nothing of tenant B's reply")
		}

		// Replayed to tenant A with tenant B's idempotency key: a reply for another request.
		t.Run("replayed with its own key", func(t *testing.T) {
			p := s.mustPrepare(sc, "tenant A "+randomHex(t))
			w.proxy.armFor(adapters.OpCommit, tenantA, func(f *forged) {
				f.Object = maps.Clone(recorded)
				f.Object["values"] = []any{factText("name", replayMark)}
				f.setEcho(recordedKey)
			})
			rc, err := s.commit(sc, p)
			noErr(t, err) // the honest Status gave tenant A its own outcome
			fired(t, w)
			if rc.Status != act.ActionReceiptStatusCommitted || strings.Contains(fmt.Sprintf("%+v", rc), bMarker) || strings.Contains(fmt.Sprintf("%+v", rc), replayMark) || s.dbContains(replayMark) {
				t.Fatalf("tenant A's receipt took tenant B's reply: %+v", rc)
			}
		})

		// Replayed to tenant A with no key at all, for an action that targets a record:
		// the records are another tenant's, not the target.
		t.Run("replayed against an update of a record", func(t *testing.T) {
			w.proxy.arm(adapters.OpPrepare, func(f *forged) {
				eff := f.Object["preparation"].(map[string]any)["effects"].([]any)[0].(map[string]any)
				eff["operation"], eff["entity_id"] = "update", "item_1"
			})
			p := s.mustPrepare(sc, "tenant A update "+randomHex(t))
			replay := func(f *forged) {
				f.Object = maps.Clone(recorded)
				f.Object["values"] = []any{factText("name", replayMark)}
				f.setEcho("")
			}
			w.proxy.armFor(adapters.OpCommit, tenantA, replay)
			w.proxy.armFor(adapters.OpStatus, tenantA, replay)
			rc, _ := s.commit(sc, p)
			if rc.Status != act.ActionReceiptStatusUncertain || len(rc.Records) != 0 || strings.Contains(fmt.Sprintf("%+v", rc), bMarker) || s.dbContains(replayMark) {
				t.Fatalf("tenant B's reply was taken as tenant A's outcome: %+v", rc)
			}
			w.proxy.disarm()
		})

		// What the host saw: tenant A's calls carried tenant A's identity and tenant B's
		// carried tenant B's, never one for the other.
		t.Run("the identity the host saw", func(t *testing.T) {
			seen := map[string]bool{}
			for _, c := range w.app.Calls() {
				seen[c.Identity.TenantID] = true
				if c.Identity.ApplicationID != demoApp {
					t.Errorf("a call carried application %q", c.Identity.ApplicationID)
				}
			}
			if !seen[tenantA] || !seen[tenantB] || len(seen) != 2 {
				t.Fatalf("tenants the host saw: %v", seen)
			}
		})
	})

	// ---- the store, once the whole matrix has run ----
	t.Run("nothing forged is in the store", func(t *testing.T) {
		if len(mk.forged) < 50 || len(mk.accepted) < 4 {
			t.Fatalf("only %d forged and %d accepted markers were used: the matrix did not run", len(mk.forged), len(mk.accepted))
		}
		for _, m := range mk.forged {
			if s.dbContains(m) {
				t.Errorf("forged marker %s is in the store", m)
			}
		}
		stored := 0
		for _, m := range mk.accepted {
			if s.dbContains(m) {
				stored++
			}
		}
		if stored < 3 {
			t.Fatalf("only %d of %d accepted markers are in the store: the dump cannot see an accepted value", stored, len(mk.accepted))
		}
	})
}

// okObject is a complete, well-formed ok result naming the given records, as a lying
// host would write one: nothing in its shape gives it away.
func okObject(marker string, records ...map[string]any) map[string]any {
	ids := make([]any, len(records))
	for i, r := range records {
		ids[i] = r
	}
	return map[string]any{
		"status":               "ok",
		"identifiers":          ids,
		"values":               []any{factText("name", marker)},
		"errors":               []any{},
		"allowed_next_actions": []any{},
		"freshness":            map[string]any{"as_of": "2026-10-02T09:30:00Z", "version": "v1"},
	}
}
