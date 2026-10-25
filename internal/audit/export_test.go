package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// markers returns the "marker" values of the events.
func markers(events []Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, string(e.Data))
	}
	return out
}

// SEC-026: audit export returns only rows of the caller's scope and only to a
// caller whose context permits it.
func TestExportScoped(t *testing.T) {
	h := newHarness(t)
	export := CapabilityExport
	a1 := scope("app-a", "tenant-1", "ws-1", export)
	a1w2 := scope("app-a", "tenant-1", "ws-2", export)
	t2 := scope("app-a", "tenant-2", "ws-1", export)
	b1 := scope("app-b", "tenant-1", "ws-1", export)
	noCap := scope("app-a", "tenant-1", "ws-1", "catalog.find_title")

	// Every scope writes events carrying its own marker.
	for name, s := range map[string]contracts.Scope{"a1": a1, "a1w2": a1w2, "t2": t2, "b1": b1} {
		for i := 0; i < 3; i++ {
			h.clk.Advance(time.Second)
			if _, err := h.log.Append(ctx, s, NewEvent{Type: EventSettingsChanged, Data: map[string]any{"marker": fmt.Sprintf("only-%s-%d", name, i)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// An application-level event and a platform event belong to no tenant.
	appLoc, _ := ForApplication("app-a", "operator")
	if _, err := h.log.AppendAt(ctx, appLoc, NewEvent{Type: EventPackageInstalled, Data: map[string]any{"marker": "only-app-level"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.log.AppendAt(ctx, Platform(), NewEvent{Type: EventSettingsChanged, Data: map[string]any{"marker": "only-platform"}}); err != nil {
		t.Fatal(err)
	}

	t.Run("a caller gets its own application, tenant and workspace and nothing else", func(t *testing.T) {
		events, raw, err := h.exportLines(a1, ExportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Three events plus the record of this very export.
		if len(events) != 4 {
			t.Fatalf("%d events exported, want 3 + the export record:\n%s", len(events), raw)
		}
		for _, e := range events {
			if e.ApplicationID != "app-a" || e.TenantID != "tenant-1" || e.WorkspaceID != "ws-1" {
				t.Errorf("exported an event of %s/%s/%s", e.ApplicationID, e.TenantID, e.WorkspaceID)
			}
			if err := e.HashValid(); err != nil {
				t.Errorf("exported event %s does not verify on its own: %v", e.ID, err)
			}
		}
		for _, foreign := range []string{"only-a1w2", "only-t2", "only-b1", "only-app-level", "only-platform"} {
			if strings.Contains(raw, foreign) {
				t.Errorf("the export contains %q, which belongs to another scope", foreign)
			}
		}
		if !strings.Contains(raw, "only-a1-0") || !strings.Contains(raw, "only-a1-2") {
			t.Errorf("the export misses the caller's own events:\n%s", raw)
		}
		if last := events[len(events)-1]; last.Type != EventAuditExported || last.PrincipalID != "usr_test" {
			t.Errorf("the export is not recorded as its last event: %+v", last)
		}
	})

	t.Run("another workspace of the same tenant", func(t *testing.T) {
		_, raw, err := h.exportLines(a1w2, ExportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, "only-a1w2-1") || strings.Contains(raw, "only-a1-") {
			t.Errorf("workspace 2 saw workspace 1 (or missed its own):\n%s", raw)
		}
	})

	t.Run("the same tenant id under another application is another scope", func(t *testing.T) {
		_, raw, err := h.exportLines(b1, ExportOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, "only-b1-0") || strings.Contains(raw, "only-a1") || strings.Contains(raw, "only-t2") {
			t.Errorf("%s", raw)
		}
	})

	t.Run("a caller whose token does not permit it gets nothing and leaves no record", func(t *testing.T) {
		before := h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.exported'`)
		var buf bytes.Buffer
		_, err := h.log.Export(ctx, noCap, &buf, ExportOptions{})
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Fatalf("error = %v, want permission_denied", err)
		}
		if buf.Len() != 0 {
			t.Errorf("%d bytes were written to an unauthorised caller", buf.Len())
		}
		if after := h.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'audit.exported'`); after != before {
			t.Errorf("a denied export was recorded as an export")
		}
	})

	t.Run("no stand-in for the capability", func(t *testing.T) {
		for name, caps := range map[string][]string{
			"prefix":   {"audit.export.all"},
			"parent":   {"audit"},
			"wildcard": {"audit.*"},
			"case":     {"Audit.Export"},
		} {
			s := scope("app-a", "tenant-1", "ws-1", caps...)
			var buf bytes.Buffer
			if _, err := h.log.Export(ctx, s, &buf, ExportOptions{}); err == nil || buf.Len() != 0 {
				t.Errorf("%s: export allowed (err %v, %d bytes)", name, err, buf.Len())
			}
		}
	})

	t.Run("an invalid scope is refused, not defaulted", func(t *testing.T) {
		broken := a1
		broken.TenantID = ""
		var buf bytes.Buffer
		_, err := h.log.Export(ctx, broken, &buf, ExportOptions{})
		if contracts.CodeOf(err) != contracts.ErrScopeMissing || buf.Len() != 0 {
			t.Errorf("err %v, %d bytes", err, buf.Len())
		}
		if _, err := h.log.Export(ctx, contracts.Scope{}, &buf, ExportOptions{}); err == nil || buf.Len() != 0 {
			t.Errorf("zero scope: err %v, %d bytes", err, buf.Len())
		}
	})

	t.Run("the platform application cannot be exported through a request scope", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := h.log.Export(ctx, scope("alixai.platform", "tenant-1", "ws-1", export), &buf, ExportOptions{})
		if err == nil || buf.Len() != 0 || strings.Contains(buf.String(), "only-platform") {
			t.Errorf("err %v, %q", err, buf.String())
		}
	})

	t.Run("filters narrow the export inside the scope", func(t *testing.T) {
		events, _, err := h.exportLines(a1, ExportOptions{Types: []EventType{EventAuditExported}})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Type != EventAuditExported {
				t.Errorf("type filter let %s through", e.Type)
			}
		}
		// Window: the second of the three events only (events are 1 s apart).
		all, _, _ := h.exportLines(a1, ExportOptions{Types: []EventType{EventSettingsChanged}})
		if len(all) != 3 {
			t.Fatalf("%d settings events", len(all))
		}
		win, _, err := h.exportLines(a1, ExportOptions{Types: []EventType{EventSettingsChanged}, Since: all[1].OccurredAt, Until: all[2].OccurredAt})
		if err != nil || len(win) != 1 || win[0].ID != all[1].ID {
			t.Errorf("window = %v, %v", markers(win), err)
		}
		if _, _, err := h.exportLines(a1, ExportOptions{Types: []EventType{"NOT A TYPE'; DROP TABLE audit_events; --"}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("malformed type filter: %v", err)
		}
		if n := h.count(`SELECT COUNT(*) FROM audit_events`); n == 0 {
			t.Error("the table is gone")
		}
	})

	t.Run("an additional authorizer can deny", func(t *testing.T) {
		deny := newHarness(t, func(o *Options) {
			o.ExportAuthorizer = func(ctx context.Context, s contracts.Scope) error { return errors.New("policy says no") }
		})
		deny.appendN(a1, 2)
		var buf bytes.Buffer
		_, err := deny.log.Export(ctx, a1, &buf, ExportOptions{})
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || buf.Len() != 0 {
			t.Errorf("err %v, %d bytes", err, buf.Len())
		}
		allow := newHarness(t, func(o *Options) {
			o.ExportAuthorizer = func(ctx context.Context, s contracts.Scope) error { return nil }
		})
		allow.appendN(a1, 2)
		if _, _, err := allow.exportLines(a1, ExportOptions{}); err != nil {
			t.Errorf("an authorizer that allows: %v", err)
		}
	})

	t.Run("no data leaves when the export cannot be recorded", func(t *testing.T) {
		reg := NewRegistry() // knows no event types, so the export record is refused
		_ = reg.Register(TypeSpec{Name: "x.only"})
		closed := newHarness(t, func(o *Options) { o.Registry = reg })
		if _, err := closed.log.Append(ctx, a1, NewEvent{Type: "x.only"}); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := closed.log.Export(ctx, a1, &buf, ExportOptions{}); err == nil || buf.Len() != 0 {
			t.Errorf("export without an audit record: err %v, %d bytes", err, buf.Len())
		}
	})

	t.Run("a writer that fails is an error, not a silent partial export", func(t *testing.T) {
		_, err := h.log.Export(ctx, a1, failingWriter{}, ExportOptions{})
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("err = %v", err)
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExportPagesThroughEveryEvent(t *testing.T) {
	h := newHarness(t)
	s := scope("app-a", "tenant-1", "ws-1", CapabilityExport)
	const n = 2*exportPage + 37
	for i := 0; i < n; i++ {
		if _, err := h.log.Append(ctx, s, settings(i)); err != nil {
			t.Fatal(err)
		}
	}
	events, _, err := h.exportLines(s, ExportOptions{Types: []EventType{EventSettingsChanged}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != n {
		t.Fatalf("%d events exported, want %d", len(events), n)
	}
	for i, e := range events {
		if e.ChainSeq != int64(i+1) {
			t.Fatalf("event %d has chain_seq %d: pages lost or repeated rows", i, e.ChainSeq)
		}
	}
}
