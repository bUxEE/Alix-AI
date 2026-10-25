package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// testTool is the approved-but-unmapped tool of these tests: the shape of the audit's
// test tools (top-level arguments, a structured reply that is not a DeterministicResult).
const testTool = "test_make"

const approvedCanary = "CANARY-TEXT-THE-SERVER-CHOSE"

// approvedAdapter is the standard HTTP adapter whose adapter configuration also
// approves testTool, over a server that offers it. extra lists more approved tools.
func approvedAdapter(t *testing.T, extra ...string) (*Adapter, *fixture.HTTPServer) {
	t.Helper()
	a, h, _ := newHTTPAdapter(t, func(c *Config) {
		allow := []string{
			fixture.ToolGet, fixture.ToolValidate, fixture.ToolPrepare, fixture.ToolCommit, fixture.ToolStatus, fixture.ToolCancel,
			testTool,
		}
		c.Endpoints[0].Allow = append(allow, extra...)
	})
	h.Server.Add(fixture.Tool{
		Name:  testTool,
		Input: map[string]any{"type": "object", "properties": map[string]any{"tenant_id": map[string]any{"type": "string"}}},
	}, func(_ context.Context, args json.RawMessage) fixture.Result {
		var in map[string]any
		_ = json.Unmarshal(args, &in)
		body := map[string]any{"status": "ok", "tenant_id": in["tenant_id"], "args": in}
		b, _ := json.Marshal(body)
		return fixture.Result{Structured: body, Text: string(b)}
	})
	return a, h
}

// A tool the package does not approve, one a capability maps and one the adapter
// manages are refused before anything is sent; and an approved tool that changed
// after it was approved, or that is gone, is refused as quarantined and not called.
func TestApprovedToolCallRefusesUnapprovedAndDriftedTools(t *testing.T) {
	t.Run("a tool the adapter configuration does not approve", func(t *testing.T) {
		a, h := approvedAdapter(t)
		h.Server.Add(fixture.Tool{Name: "test_other", Input: objectSchema()}, func(context.Context, json.RawMessage) fixture.Result {
			return fixture.Result{Structured: map[string]any{"status": "ok"}}
		})
		before := h.ToolCalls("")
		_, err := a.CallApprovedTool(testContext(t), testEP, "test_other", nil)
		if code(err) != contracts.ErrPermissionDenied || detail(err, "reason") != ReasonToolNotApproved {
			t.Fatalf("error = %v, want permission_denied (%s)", err, ReasonToolNotApproved)
		}
		if a.ApprovedTool(testEP, "test_other") {
			t.Error("ApprovedTool says an unapproved tool is approved")
		}
		if n := h.ToolCalls("test_other"); n != 0 || h.ToolCalls("") != before {
			t.Errorf("a refused call reached the server (%d calls of the tool)", n)
		}
	})

	t.Run("an endpoint with no tools.allow approves nothing", func(t *testing.T) {
		a, h, _ := newHTTPAdapter(t) // no Allow list: the host imposes none
		h.Server.Add(fixture.Tool{Name: testTool, Input: objectSchema()}, func(context.Context, json.RawMessage) fixture.Result {
			return fixture.Result{Structured: map[string]any{"status": "ok"}}
		})
		if _, err := a.CallApprovedTool(testContext(t), testEP, testTool, nil); code(err) != contracts.ErrPermissionDenied {
			t.Errorf("error = %v, want permission_denied", err)
		}
		if h.ToolCalls(testTool) != 0 {
			t.Error("a tool nobody approved was called")
		}
	})

	t.Run("a tool a capability maps or the adapter manages", func(t *testing.T) {
		a, h := approvedAdapter(t)
		before := h.ToolCalls("")
		for _, tool := range []string{fixture.ToolGet, fixture.ToolCommit, fixture.ToolDescribe, fixture.ToolHealth, fixture.ToolInspect} {
			_, err := a.CallApprovedTool(testContext(t), testEP, tool, map[string]any{"identity": map[string]any{}})
			if code(err) != contracts.ErrPermissionDenied || detail(err, "reason") != ReasonToolMapped {
				t.Errorf("%s: error = %v, want permission_denied (%s)", tool, err, ReasonToolMapped)
			}
		}
		if h.ToolCalls("") != before {
			t.Error("a call to a mapped tool reached the server through the approved-tool call")
		}
	})

	t.Run("an endpoint or a tool name that is not one", func(t *testing.T) {
		a, _ := approvedAdapter(t)
		if _, err := a.CallApprovedTool(testContext(t), "nowhere", testTool, nil); code(err) != contracts.ErrValidationFailed {
			t.Errorf("an unknown endpoint: %v, want validation_failed", err)
		}
		if _, err := a.CallApprovedTool(testContext(t), testEP, "a b", nil); code(err) != contracts.ErrValidationFailed {
			t.Errorf("a malformed tool name: %v, want validation_failed", err)
		}
	})

	t.Run("an approved tool is called with its own arguments", func(t *testing.T) {
		a, h := approvedAdapter(t)
		rep, err := a.CallApprovedTool(testContext(t), testEP, testTool, map[string]any{"tenant_id": "tnt_x"})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Status   string         `json:"status"`
			TenantID string         `json:"tenant_id"`
			Args     map[string]any `json:"args"`
		}
		if err := json.Unmarshal(rep.Structured, &got); err != nil || got.Status != "ok" || got.TenantID != "tnt_x" {
			t.Fatalf("reply = %s, %v", rep.Structured, err)
		}
		if len(got.Args) != 1 || got.Args["tenant_id"] != "tnt_x" {
			t.Errorf("the server received %v: a capability call's identity wrapper must not be added", got.Args)
		}
		if !rep.Sent || h.ToolCalls(testTool) != 1 {
			t.Errorf("Sent = %v, %d calls", rep.Sent, h.ToolCalls(testTool))
		}
		if !a.ApprovedTool(testEP, testTool) {
			t.Error("ApprovedTool does not know the tool the adapter configuration approves")
		}
	})

	for _, tc := range []struct {
		name   string
		change func(*fixture.Server)
		kind   DriftKind
	}{
		{"its input schema changed", func(s *fixture.Server) {
			s.Redefine(testTool, func(tl *fixture.Tool) { tl.Input = objectSchema("changed") })
		}, DriftSchemaChanged},
		{"its output schema appeared", func(s *fixture.Server) {
			s.Redefine(testTool, func(tl *fixture.Tool) { tl.Output = objectSchema("status") })
		}, DriftSchemaChanged},
		{"it was removed", func(s *fixture.Server) { s.Remove(testTool) }, DriftToolMissing},
		{"it is listed twice with different definitions", func(s *fixture.Server) {
			s.AddDuplicate(fixture.Tool{Name: testTool, Input: objectSchema("other")})
		}, DriftAmbiguousName},
	} {
		t.Run("an approved tool whose definition drifted: "+tc.name, func(t *testing.T) {
			var seen []Drift
			a, h := approvedAdapter(t)
			a.cfg.OnDrift = func(d Drift) { seen = append(seen, d) }
			ctx := testContext(t)
			if _, err := a.CallApprovedTool(ctx, testEP, testTool, map[string]any{"tenant_id": "tnt_x"}); err != nil {
				t.Fatalf("the approved tool: %v", err)
			}
			if a.ReauditRequired() {
				t.Fatal("the re-audit flag is up before anything changed")
			}
			if _, pinned := a.Pins()[testEP+"/"+testTool]; !pinned {
				t.Fatal("the tool was not pinned when it was first seen")
			}
			calls := h.ToolCalls(testTool)

			tc.change(h.Server)
			rep, err := a.CallApprovedTool(ctx, testEP, testTool, map[string]any{"tenant_id": "tnt_y"})
			if code(err) != contracts.ErrQuarantined || detail(err, "reason") != ReasonToolSchemaDrift || detail(err, "tool") != testTool {
				t.Fatalf("error = %v, want quarantined (%s) naming the tool", err, ReasonToolSchemaDrift)
			}
			if rep.Sent || len(rep.Structured) != 0 {
				t.Errorf("a refused call reports %+v", rep)
			}
			if h.ToolCalls(testTool) != calls {
				t.Error("the changed tool was called")
			}
			if !a.ReauditRequired() {
				t.Error("the re-audit flag is not up")
			}
			if len(seen) != 1 || seen[0].Tool != testTool || seen[0].Kind != tc.kind || seen[0].Endpoint != testEP {
				t.Errorf("OnDrift saw %+v, want one %s drift of %s", seen, tc.kind, testTool)
			}

			// sticky: the tool changing back does not bring it back, and the other
			// approved calls are refused with it
			h.Server.Remove(testTool)
			h.Server.Add(fixture.Tool{
				Name:  testTool,
				Input: map[string]any{"type": "object", "properties": map[string]any{"tenant_id": map[string]any{"type": "string"}}},
			}, func(context.Context, json.RawMessage) fixture.Result {
				return fixture.Result{Structured: map[string]any{"status": "ok"}}
			})
			if _, err := a.CallApprovedTool(ctx, testEP, testTool, nil); code(err) != contracts.ErrQuarantined {
				t.Errorf("after the tool changed back: %v, want it still quarantined", err)
			}
			if h.ToolCalls(testTool) != calls {
				t.Error("a quarantined tool was called")
			}
		})
	}

	t.Run("an approved tool is held to the pin the adapter was given", func(t *testing.T) {
		a, h := approvedAdapter(t)
		if _, err := a.CallApprovedTool(testContext(t), testEP, testTool, nil); err != nil {
			t.Fatal(err)
		}
		pin := a.Pins()[testEP+"/"+testTool]
		if len(pin) != 64 {
			t.Fatalf("pin = %q", pin)
		}
		// a later adapter, given the pin taken at audit time, over a server that changed
		h.Server.Redefine(testTool, func(tl *fixture.Tool) { tl.Input = objectSchema("changed") })
		b, err := New(func() Config {
			cfg := httpConfig(t, h)
			cfg.Endpoints[0].Allow = []string{
				fixture.ToolGet, fixture.ToolValidate, fixture.ToolPrepare, fixture.ToolCommit, fixture.ToolStatus, fixture.ToolCancel, testTool,
			}
			cfg.Pins = map[string]string{testEP + "/" + testTool: pin}
			return cfg
		}())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		calls := h.ToolCalls(testTool)
		if _, err := b.CallApprovedTool(testContext(t), testEP, testTool, nil); code(err) != contracts.ErrQuarantined {
			t.Errorf("a tool that no longer hashes as it did at audit time: %v, want quarantined", err)
		}
		if h.ToolCalls(testTool) != calls {
			t.Error("the changed tool was called")
		}
	})

	t.Run("a mapped tool that drifted refuses the approved calls too", func(t *testing.T) {
		a, h := approvedAdapter(t)
		ctx := testContext(t)
		if _, err := a.CallApprovedTool(ctx, testEP, testTool, nil); err != nil {
			t.Fatal(err)
		}
		calls := h.ToolCalls(testTool)
		h.Server.Redefine(fixture.ToolGet, func(tl *fixture.Tool) { tl.Input = objectSchema("identity", "fields", "extra") })
		_, err := a.CallApprovedTool(ctx, testEP, testTool, nil)
		if code(err) != contracts.ErrQuarantined || detail(err, "reason") != ReasonToolSchemaDrift {
			t.Fatalf("error = %v, want quarantined (%s): a package that drifted is audited again before anything is called", err, ReasonToolSchemaDrift)
		}
		if h.ToolCalls(testTool) != calls {
			t.Error("an approved tool was called on a server whose mapped tool drifted")
		}
	})
}

// Whether the server offers an approved tool is asked of the server, not guessed.
func TestApprovedToolsOffered(t *testing.T) {
	a, h := approvedAdapter(t, "test_absent")
	ctx := testContext(t)
	got, err := a.ApprovedToolsOffered(ctx, testEP, testTool, "test_absent")
	if err != nil || len(got) != 2 || !got[testTool] || got["test_absent"] {
		t.Errorf("an approved tool the server lists and one it does not: %v, %v", got, err)
	}
	if _, pinned := a.Pins()[testEP+"/"+testTool]; !pinned {
		t.Error("a tool the server offered was not pinned")
	}
	if _, err := a.ApprovedToolsOffered(ctx, testEP, testTool, "test_unapproved"); code(err) != contracts.ErrPermissionDenied {
		t.Errorf("a tool that is not approved: %v, want permission_denied", err)
	}
	if got, err := a.ApprovedToolsOffered(ctx, testEP); err != nil || len(got) != 0 {
		t.Errorf("no tools asked about: %v, %v", got, err)
	}
	h.Server.Remove(fixture.ToolGet) // a mapped tool vanishes: drift
	if got, err := a.ApprovedToolsOffered(ctx, testEP, testTool); got != nil || code(err) != contracts.ErrQuarantined {
		t.Errorf("after drift: %v, %v, want a quarantined error", got, err)
	}
}

// What the server answers is bounded and typed, and a word of it never reaches an error.
func TestApprovedToolCallIsBoundedAndTyped(t *testing.T) {
	huge := strings.Repeat("x", 4*testSize)
	for _, tc := range []struct {
		name   string
		result fixture.Result
		code   contracts.ErrCode
		reason string
		sent   bool
	}{
		{"text only", fixture.Result{Text: `{"status":"ok"}`}, contracts.ErrSchemaInvalid, ReasonTextOnlyResult, true},
		{"a tool error", fixture.Result{Text: approvedCanary, IsError: true}, contracts.ErrUnavailable, ReasonToolError, true},
		{"structured content that is not an object", fixture.Result{Structured: []any{1, 2}}, contracts.ErrSchemaInvalid, ReasonStructuredNotObject, true},
		{"a reply over the bound", fixture.Result{Structured: map[string]any{"status": "ok", "note": huge}}, contracts.ErrSchemaInvalid, "response_too_large", true},
		{"a JSON-RPC error", fixture.Result{RPCError: &fixture.RPCError{Code: -32000, Message: approvedCanary}}, contracts.ErrUncertainOutcome, "", true},
		{"a dropped connection", fixture.Result{Abort: true}, contracts.ErrUncertainOutcome, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, h := approvedAdapter(t)
			h.Server.Handle(testTool, func(context.Context, json.RawMessage) fixture.Result { return tc.result })
			rep, err := a.CallApprovedTool(testContext(t), testEP, testTool, nil)
			if code(err) != tc.code || (tc.reason != "" && detail(err, "reason") != tc.reason) {
				t.Fatalf("error = %v (code %q, reason %q), want %s %s", err, code(err), detail(err, "reason"), tc.code, tc.reason)
			}
			if err != nil && strings.Contains(err.Error(), approvedCanary) {
				t.Errorf("text the server chose reached an error: %v", err)
			}
			if rep.Sent != tc.sent || len(rep.Structured) != 0 {
				t.Errorf("reply = %+v, want Sent %v and no content", rep, tc.sent)
			}
		})
	}

	t.Run("an application that is down was not sent anything", func(t *testing.T) {
		a, h := approvedAdapter(t)
		if _, err := a.CallApprovedTool(testContext(t), testEP, testTool, nil); err != nil {
			t.Fatal(err)
		}
		h.Down()
		rep, err := a.CallApprovedTool(testContext(t), testEP, testTool, nil)
		if code(err) != contracts.ErrUnavailable || rep.Sent {
			t.Errorf("a call to an application that is down: %+v, %v, want unavailable and not sent", rep, err)
		}
	})

	t.Run("a caller that gave up is told so with its own error", func(t *testing.T) {
		a, _ := approvedAdapter(t)
		ctx, cancel := context.WithCancel(testContext(t))
		cancel()
		if _, err := a.CallApprovedTool(ctx, testEP, testTool, nil); err != context.Canceled {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("a closed adapter calls nothing", func(t *testing.T) {
		a, h := approvedAdapter(t)
		_ = a.Close()
		rep, err := a.CallApprovedTool(testContext(t), testEP, testTool, nil)
		if code(err) != contracts.ErrUnavailable || rep.Sent || h.ToolCalls(testTool) != 0 {
			t.Errorf("a call on a closed adapter: %+v, %v", rep, err)
		}
	})
}
