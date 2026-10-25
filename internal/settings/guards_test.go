package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The guards of master plan 14.2 ("A client cannot use settings to enable an
// undeclared tool, enlarge a scope, bypass confirmation or expose LocalAI
// administration") and of the additive external-mode and decision-layer rules.
// Every guard test proves the same four things about each attempt:
//
//  1. it is refused with a typed code (and, for an unknown member, names the rule
//     it runs into);
//  2. the stored documents are byte-for-byte what they were;
//  3. no audit event of a change is appended;
//  4. what the policy gateway decides, at exposure and at commit, is unchanged,
//     including after a set of VALID writes at every level.
//
// A submitted secret never appears in the refusal.

// attempt is one forbidden document.
type attempt struct {
	name string
	doc  string
	// want is the typed code the refusal carries.
	want contracts.ErrCode
	// guard is the rule an unknown member runs into (details.guard), when checked.
	guard string
}

// refuseAll sends every attempt to every level and asserts the four properties.
func refuseAll(t *testing.T, f *fixture, sc contracts.Scope, attempts []attempt, levels ...Level) {
	t.Helper()
	if len(levels) == 0 {
		levels = Levels()
	}
	dump, events, policyBefore := f.storedDump(), len(f.settingsEvents()), f.policyFingerprint(sc)
	for _, level := range levels {
		for _, a := range attempts {
			version := f.get(sc, level).Version
			err := f.tryPut(sc, level, version, a.doc)
			if err == nil {
				t.Errorf("%s / %s: accepted: %s", level, a.name, a.doc)
				continue
			}
			var ce *contracts.Error
			if !asErr(err, &ce) {
				t.Errorf("%s / %s: error %v is not typed", level, a.name, err)
				continue
			}
			if ce.Code != a.want {
				t.Errorf("%s / %s: code %s, want %s (%v)", level, a.name, ce.Code, a.want, err)
			}
			if a.guard != "" && ce.Details["guard"] != a.guard {
				t.Errorf("%s / %s: guard %v, want %s", level, a.name, ce.Details["guard"], a.guard)
			}
			// Nothing the caller submitted as a secret comes back.
			wire, _ := json.Marshal(ce)
			for _, secret := range []string{"sk-live-SECRET-0123456789", "hunter2-PASSWORD", "Bearer abc.def.ghi"} {
				if strings.Contains(string(wire), secret) || strings.Contains(err.Error(), secret) {
					t.Errorf("%s / %s: the refusal repeats a submitted secret: %s", level, a.name, wire)
				}
			}
		}
	}
	if got := f.storedDump(); got != dump {
		t.Errorf("refused attempts changed the stored documents:\nbefore:\n%safter:\n%s", dump, got)
	}
	if got := len(f.settingsEvents()); got != events {
		t.Errorf("refused attempts appended %d audit events", got-events)
	}
	if got := f.policyFingerprint(sc); got != policyBefore {
		t.Errorf("refused attempts changed what the policy gateway decides:\n before %s\n after  %s", policyBefore, got)
	}
}

// policyFingerprint is everything the policy gateway decides for the scope that a
// setting could conceivably have moved: the capabilities exposed (with their risk,
// requirements and fields), and the outcome of a commit with no evidence, with a
// confirmation only and with both a confirmation and a step-up, for every
// capability the package declares (including the ones the caller may not use).
func (f *fixture) policyFingerprint(sc contracts.Scope) string {
	f.t.Helper()
	ctx := context.Background()
	exp := f.gw.Expose(ctx, policy.ExposeRequest{Scope: sc, Package: f.pkg, Binding: policy.BindingOf(sc)})
	var parts []string
	for _, d := range exp.Visible {
		parts = append(parts, fmt.Sprintf("visible:%s:risk=%d:conf=%t:stepup=%t:fields=%s", d.Record.CapabilityID, d.EffectiveRisk,
			d.Requires.Confirmation, d.Requires.StepUp, strings.Join(d.VisibleFields, ",")))
	}
	ids := append(append([]string{}, f.pkg.CapabilityIDs()...), capGhost)
	for _, id := range ids {
		for _, ev := range []policy.Evidence{{}, {Confirmed: true}, {Confirmed: true, StepUp: true}} {
			d := f.gw.Decide(ctx, policy.Request{Scope: sc, Package: f.pkg, CapabilityID: id, Binding: policy.BindingOf(sc), Phase: policy.PhaseCommit, Evidence: ev})
			parts = append(parts, fmt.Sprintf("commit:%s:confirmed=%t:stepup=%t:allowed=%t:code=%s", id, ev.Confirmed, ev.StepUp, d.Allowed, d.Code))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// validEverywhere writes a valid document at every level that narrows or raises as
// far as settings are allowed to, so a guard test can show the policy gateway is
// unmoved by the strongest legitimate use of settings.
func (f *fixture) validEverywhere(sc contracts.Scope) {
	f.t.Helper()
	f.put(sc, LevelDeployment, f.get(sc, LevelDeployment).Version,
		`{"default_language":"it","default_model_profile":"balanced","voice":{"allowed":true},"history":{"retention_days":30,"export_allowed":true},"attachments":{"allowed":true,"max_file_mb":5},"capabilities":{"disabled":["items.delete"]},"confirmation":{"extra_from_risk_level":0}}`)
	f.put(sc, LevelTenant, f.get(sc, LevelTenant).Version,
		`{"voice":{"allowed":false},"notifications":{"allowed":false},"capabilities":{"disabled":["items.create"]},"confirmation":{"extra_from_risk_level":0}}`)
	f.put(sc, LevelUser, f.get(sc, LevelUser).Version,
		`{"language":"en","model_profile":"lite","capabilities":{"disabled":["items.get"]},"confirmation":{"extra_from_risk_level":3}}`)
}

// propertyNames lists every property name any settings schema defines (document
// members, nested members and $defs members), for the schema-level guard checks.
func propertyNames(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := schemaFS.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	var walk func(file string, v any)
	walk = func(file string, v any) {
		switch n := v.(type) {
		case map[string]any:
			if props, ok := n["properties"].(map[string]any); ok {
				for name := range props {
					out[name] = file
				}
			}
			for _, c := range n {
				walk(file, c)
			}
		case []any:
			for _, c := range n {
				walk(file, c)
			}
		}
	}
	for _, e := range entries {
		if e.Name() == "ids.schema.json" {
			continue
		}
		raw, err := schemaFS.ReadFile("schemas/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		walk(e.Name(), doc)
	}
	// The effective-settings schema is published but not needed at runtime.
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "settings", "effective-settings.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walk("effective-settings.schema.json", doc)
	return out
}

// noPropertyMatches fails for every schema property whose name matches re.
func noPropertyMatches(t *testing.T, re *regexp.Regexp, why string, except ...string) {
	t.Helper()
	for name, file := range propertyNames(t) {
		if slices.Contains(except, name) {
			continue
		}
		if re.MatchString(name) {
			t.Errorf("schema %s defines the member %q: %s", file, name, why)
		}
	}
}

// SEC-054: a client or setting registers an arbitrary tool, API, source or agent
// that was never declared in an audited package. A setting can only switch OFF a
// capability the caller may itself use; there is no member that enables one.
func TestSettingsCannotEnableUndeclaredTool(t *testing.T) {
	f := newFixture(t)
	sc := ada()

	t.Run("no member enables a tool, a server or an agent", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"capabilities.enabled", `{"capabilities":{"enabled":["items.ghost"]}}`, contracts.ErrValidationFailed, "tool"},
			{"enabled_tools", `{"enabled_tools":["admin.purge"]}`, contracts.ErrValidationFailed, "tool"},
			{"tools", `{"tools":{"enabled":["shell.exec"]}}`, contracts.ErrValidationFailed, "tool"},
			{"mcp server", `{"mcp_servers":[{"url":"http://127.0.0.1:9000","name":"x"}]}`, contracts.ErrValidationFailed, "tool"},
			{"agent", `{"agents":[{"id":"helper"}]}`, contracts.ErrValidationFailed, "tool"},
			{"plugin", `{"plugins":["evil"]}`, contracts.ErrValidationFailed, "tool"},
			{"shell", `{"shell":true}`, contracts.ErrValidationFailed, "tool"},
			{"free-form capability definition", `{"capabilities":{"definitions":[{"id":"items.ghost","effect":"none"}]}}`, contracts.ErrValidationFailed, "tool"},
			{"enabled in a section", `{"voice":{"enable_tool":"items.ghost"}}`, contracts.ErrValidationFailed, "tool"},
		})
	})

	t.Run("a capability to switch off must be one the caller may use", func(t *testing.T) {
		// Each of these is refused with the very same answer, so a client can not
		// tell an undeclared capability from an admin-only one or one it lacks.
		visitor := withScopes(sc, append(append([]string{}, settingsScopes...), capGet)...)
		visitor.Roles = []string{"visitor"}
		answers := map[string]string{}
		for name, tc := range map[string]struct {
			sc  contracts.Scope
			doc string
		}{
			"undeclared":           {sc, `{"capabilities":{"disabled":["items.ghost"]}}`},
			"admin audience":       {sc, `{"capabilities":{"disabled":["admin.purge"]}}`},
			"not in the token":     {withScopes(sc, append(append([]string{}, settingsScopes...), capGet)...), `{"capabilities":{"disabled":["items.delete"]}}`},
			"not granted the role": {visitor, `{"capabilities":{"disabled":["items.create"]}}`},
			"among valid ones":     {sc, `{"capabilities":{"disabled":["items.get","items.ghost"]}}`},
		} {
			dump := f.storedDump()
			err := f.tryPut(tc.sc, LevelUser, 0, tc.doc)
			wantCode(t, err, contracts.ErrUnknownCapability)
			if f.storedDump() != dump {
				t.Errorf("%s: stored something", name)
			}
			var ce *contracts.Error
			asErr(err, &ce)
			answers[name] = ce.Message
			if strings.Contains(ce.Message, "ghost") || strings.Contains(ce.Message, "admin.purge") {
				t.Errorf("%s: the refusal echoes the capability id: %s", name, ce.Message)
			}
		}
		for name, msg := range answers {
			if msg != answers["undeclared"] {
				t.Errorf("%s: answer %q differs from the undeclared one %q", name, msg, answers["undeclared"])
			}
		}
		// ...at the tenant and the deployment level too.
		for _, level := range []Level{LevelTenant, LevelDeployment} {
			wantCode(t, f.tryPut(sc, level, 0, `{"capabilities":{"disabled":["items.ghost"]}}`), contracts.ErrUnknownCapability)
			wantCode(t, f.tryPut(sc, level, 0, `{"capabilities":{"disabled":["admin.purge"]}}`), contracts.ErrUnknownCapability)
		}
		if f.storedRows() != 0 || len(f.settingsEvents()) != 0 {
			t.Fatal("a refused capability name was stored or recorded")
		}
	})

	t.Run("the schemas have no member that could enable one", func(t *testing.T) {
		noPropertyMatches(t, regexp.MustCompile(`(?i)tool|mcp|agent|plugin|function|shell|exec|(^|_)enabled?_|server|definition`), "a setting must not be able to name a tool")
		for name, file := range propertyNames(t) {
			if name == "enabled" && !strings.Contains(file, "user-settings") && !strings.Contains(file, "effective") {
				t.Errorf("schema %s defines a bare 'enabled' member outside user preferences", file)
			}
		}
	})

	t.Run("the strongest valid use of settings never widens what the gateway exposes", func(t *testing.T) {
		before := f.policyFingerprint(sc)
		visibleBefore := f.visibleIDs(sc)
		f.validEverywhere(sc)
		after := f.policyFingerprint(sc)
		if after != before {
			t.Errorf("settings moved the policy gateway:\n before %s\n after  %s", before, after)
		}
		// What the settings API reports is a subset of the gateway's exposure.
		view, err := f.svc.Capabilities(context.Background(), sc)
		if err != nil {
			t.Fatal(err)
		}
		exposed := map[string]bool{}
		for _, id := range visibleBefore {
			exposed[id] = true
		}
		for _, c := range view.Capabilities {
			if !exposed[c.ID] {
				t.Errorf("the settings view lists %s, which the gateway does not expose", c.ID)
			}
			if c.ID == capGhost || c.ID == capAdmin {
				t.Errorf("the settings view lists %s", c.ID)
			}
		}
		if len(view.Capabilities) != len(visibleBefore) {
			t.Errorf("the view lists %d capabilities, the gateway exposes %d", len(view.Capabilities), len(visibleBefore))
		}
		// The undeclared and admin capabilities are still denied by the gateway.
		for _, id := range []string{capGhost, capAdmin} {
			d := f.gw.Decide(context.Background(), policy.Request{Scope: sc, Package: f.pkg, CapabilityID: id, Binding: policy.BindingOf(sc), Phase: policy.PhaseExpose})
			if d.Allowed {
				t.Errorf("the gateway allows %s after settings were written", id)
			}
		}
	})
}

// visibleIDs lists the capabilities the gateway exposes to the scope.
func (f *fixture) visibleIDs(sc contracts.Scope) []string {
	exp := f.gw.Expose(context.Background(), policy.ExposeRequest{Scope: sc, Package: f.pkg, Binding: policy.BindingOf(sc)})
	var ids []string
	for _, d := range exp.Visible {
		ids = append(ids, d.Record.CapabilityID)
	}
	sort.Strings(ids)
	return ids
}

// SEC-043: the minimum risk level or step-up requirement is bypassed, or a setting
// lowers a platform floor. Settings can only ADD confirmation; the floor is not a
// setting.
func TestSettingsCannotLowerConfirmation(t *testing.T) {
	f := newFixture(t)
	sc := ada()

	t.Run("no member lowers, skips or switches off a confirmation", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"confirmation mode none", `{"confirmation":{"mode":"none"}}`, contracts.ErrValidationFailed, "confirmation"},
			{"skip", `{"confirmation":{"skip":true}}`, contracts.ErrValidationFailed, "confirmation"},
			{"skip_confirmation", `{"skip_confirmation":true}`, contracts.ErrValidationFailed, "confirmation"},
			{"auto approve", `{"auto_approve":true}`, contracts.ErrValidationFailed, "confirmation"},
			{"risk floor", `{"risk_floor":0}`, contracts.ErrValidationFailed, "confirmation"},
			{"minimum risk level", `{"confirmation":{"minimum_risk_level":0}}`, contracts.ErrValidationFailed, "confirmation"},
			{"step-up off", `{"step_up":false}`, contracts.ErrValidationFailed, "confirmation"},
			{"per-capability bypass", `{"capabilities":{"confirmation":"none"}}`, contracts.ErrValidationFailed, "confirmation"},
			{"below the scale", `{"confirmation":{"extra_from_risk_level":-1}}`, contracts.ErrValidationFailed, ""},
			{"above the scale", `{"confirmation":{"extra_from_risk_level":4}}`, contracts.ErrValidationFailed, ""},
			{"as a string", `{"confirmation":{"extra_from_risk_level":"1"}}`, contracts.ErrValidationFailed, ""},
			{"as null", `{"confirmation":{"extra_from_risk_level":null}}`, contracts.ErrValidationFailed, ""},
			{"as a float", `{"confirmation":{"extra_from_risk_level":1.5}}`, contracts.ErrValidationFailed, ""},
		})
	})

	t.Run("the schemas can only add confirmation", func(t *testing.T) {
		props := propertyNames(t)
		var confirm []string
		for name := range props {
			if regexp.MustCompile(`(?i)confirm|step_?up|risk|floor|approv|bypass|skip`).MatchString(name) {
				confirm = append(confirm, name)
			}
		}
		sort.Strings(confirm)
		// The one member that touches confirmation is the raise, its section and
		// the closed enumeration it is bounded by; nothing is spelled "mode",
		// "required", "skip" or the like.
		want := []string{"confirmation", "extra_from_risk_level"}
		for _, name := range confirm {
			if name != "confirmation" && name != "extra_from_risk_level" {
				t.Errorf("schema member %q touches confirmation: only %v may", name, want)
			}
		}
	})

	t.Run("extra confirmation only adds to what the platform requires", func(t *testing.T) {
		before := f.capabilityStates(sc)
		if before[capGet].RequiresConfirmation || !before[capCreate].RequiresConfirmation || !before[capDelete].RequiresStepUp {
			t.Fatalf("setup: %+v", before)
		}
		f.put(sc, LevelUser, 0, `{"confirmation":{"extra_from_risk_level":0}}`)
		after := f.capabilityStates(sc)
		if !after[capGet].RequiresConfirmation || !after[capGet].ExtraConfirmation {
			t.Errorf("a level-0 capability did not gain the confirmation that was asked for: %+v", after[capGet])
		}
		for _, id := range []string{capCreate, capDelete} {
			if !after[id].RequiresConfirmation || after[id].ExtraConfirmation != false {
				t.Errorf("%s: platform confirmation was changed: %+v", id, after[id])
			}
		}
		if !after[capDelete].RequiresStepUp {
			t.Error("a level-3 capability lost its step-up")
		}
	})

	t.Run("a lower level can not relax what a higher level asked for", func(t *testing.T) {
		g := newFixture(t)
		g.put(sc, LevelTenant, 0, `{"confirmation":{"extra_from_risk_level":0}}`)
		g.put(sc, LevelUser, 0, `{"confirmation":{"extra_from_risk_level":3}}`) // the user asks for the weakest extra
		eff, err := g.svc.Effective(context.Background(), sc)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Confirmation.ExtraFromRiskLevel == nil || *eff.Confirmation.ExtraFromRiskLevel != 0 || eff.DecidedBy["confirmation.extra_from_risk_level"] != "tenant" {
			t.Errorf("effective confirmation = %+v, decided by %v", eff.Confirmation, eff.DecidedBy["confirmation.extra_from_risk_level"])
		}
		if st := g.capabilityStates(sc)[capGet]; !st.RequiresConfirmation {
			t.Errorf("the user relaxed the tenant's extra confirmation: %+v", st)
		}
		// Removing the tenant's document returns the user's own, weaker, threshold.
		g.put(sc, LevelTenant, 1, `{}`)
		eff, _ = g.svc.Effective(context.Background(), sc)
		if eff.Confirmation.ExtraFromRiskLevel == nil || *eff.Confirmation.ExtraFromRiskLevel != 3 {
			t.Errorf("after the tenant document was emptied: %+v", eff.Confirmation)
		}
	})

	t.Run("the platform floor is the gateway's and no setting moves it", func(t *testing.T) {
		g := newFixture(t)
		before := g.policyFingerprint(sc)
		g.validEverywhere(sc)
		g.put(sc, LevelUser, g.get(sc, LevelUser).Version, `{"confirmation":{"extra_from_risk_level":3}}`) // the weakest thing a setting can say
		if after := g.policyFingerprint(sc); after != before {
			t.Fatalf("settings moved the policy gateway:\n before %s\n after  %s", before, after)
		}
		ctx := context.Background()
		commit := func(id string, ev policy.Evidence) policy.Decision {
			return g.gw.Decide(ctx, policy.Request{Scope: sc, Package: g.pkg, CapabilityID: id, Binding: policy.BindingOf(sc), Phase: policy.PhaseCommit, Evidence: ev})
		}
		if d := commit(capCreate, policy.Evidence{}); d.Allowed || d.Code != contracts.ErrConfirmationRequired {
			t.Errorf("level 2 without a confirmation: allowed=%t code=%s", d.Allowed, d.Code)
		}
		if d := commit(capDelete, policy.Evidence{Confirmed: true}); d.Allowed || d.Code != contracts.ErrStepUpRequired {
			t.Errorf("level 3 without a step-up: allowed=%t code=%s", d.Allowed, d.Code)
		}
		if d := commit(capCreate, policy.Evidence{Confirmed: true}); !d.Allowed {
			t.Errorf("level 2 with a confirmation was denied: %v", d.Err())
		}
	})
}

// capabilityStates is the settings API's view of the capabilities by id.
func (f *fixture) capabilityStates(sc contracts.Scope) map[string]CapabilityState {
	f.t.Helper()
	view, err := f.svc.Capabilities(context.Background(), sc)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]CapabilityState{}
	for _, c := range view.Capabilities {
		out[c.ID] = c
	}
	return out
}

// SEC-067: AlixAI forwards or calls LocalAI administration, model management,
// agent, gallery or settings endpoints, or a setting exposes them to clients.
func TestSettingsCannotExposeRuntimeAdmin(t *testing.T) {
	f := newFixture(t)
	sc := ada()

	t.Run("no member reaches the runtime", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"runtime", `{"runtime":{"admin":true}}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"localai", `{"localai":{"enabled":true}}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"gallery", `{"gallery":{"enabled":true}}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"web ui", `{"webui":true}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"admin", `{"admin":{"models":true}}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"backend", `{"backend":"llama-cpp"}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"base url", `{"base_url":"http://127.0.0.1:8080"}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"endpoint", `{"endpoint":"/models/apply"}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"model path", `{"model_path":"/models/x.gguf"}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"nested", `{"voice":{"backend":"piper"}}`, contracts.ErrValidationFailed, "runtime_admin"},
			{"model install", `{"models":{"apply":{"url":"https://example.invalid/m.yaml"}}}`, contracts.ErrValidationFailed, ""},
		})
	})

	t.Run("a profile is chosen from the operator's list, never named by address", func(t *testing.T) {
		attempts := []attempt{
			{"url", `{"model_profile":"http://127.0.0.1:8080"}`, contracts.ErrValidationFailed, ""},
			{"path", `{"model_profile":"../models/x"}`, contracts.ErrValidationFailed, ""},
			{"a runtime name", `{"model_profile":"localai"}`, contracts.ErrValidationFailed, ""},
			{"not approved", `{"model_profile":"experimental-4g"}`, contracts.ErrValidationFailed, ""},
			{"a voice that is not installed", `{"voice":{"voice_id":"localai-admin"}}`, contracts.ErrValidationFailed, ""},
		}
		refuseAll(t, f, sc, attempts, LevelUser)
		refuseAll(t, f, sc, attempts[:4], LevelTenant) // tenant and deployment states a default_ profile instead
	})

	t.Run("the schemas have no member that names a runtime", func(t *testing.T) {
		noPropertyMatches(t, regexp.MustCompile(`(?i)runtime|localai|local_ai|gallery|backend|webui|web_ui|admin|base_?url|endpoint|model_?path|inference|url|uri|(^|_)host($|_)|(^|_)port($|_)`),
			"a setting must not be able to reach or expose a runtime")
	})

	t.Run("provider health names a role, a status and an effect, nothing behind them", func(t *testing.T) {
		f.provs.health = ProviderHealth{Status: "weird", Roles: []ProviderRole{
			{Role: "controller", Configured: true, Status: "ok"},
			{Role: "http://127.0.0.1:8080/models/apply", Configured: true, Status: "LocalAI is up", Effect: "see http://127.0.0.1:8080"},
			{Role: "embedder", Configured: false, Status: "unavailable", Effect: "reduced_semantic_recall"},
			{Role: "reranker", Configured: true, Status: "degraded", Effect: "localai: gallery enabled at /models/available"},
		}}
		h, err := f.svc.ProviderHealth(context.Background(), sc)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(h)
		for _, banned := range []string{"localai", "LocalAI", "127.0.0.1", "http", "gallery", "/models", "8080"} {
			if strings.Contains(string(wire), banned) {
				t.Errorf("provider health leaks %q: %s", banned, wire)
			}
		}
		if h.Status != "unavailable" {
			t.Errorf("an unknown status was reported as %q, want unavailable", h.Status)
		}
		// Only the closed vocabularies survive, in a stable order.
		var roles []string
		for _, r := range h.Roles {
			roles = append(roles, r.Role+"/"+r.Status+"/"+r.Effect)
		}
		want := []string{"controller/ok/", "embedder/unavailable/reduced_semantic_recall", "reranker/degraded/", "unknown/unavailable/"}
		if strings.Join(roles, ",") != strings.Join(want, ",") {
			t.Errorf("roles = %v, want %v", roles, want)
		}
		// The type itself has no member a runtime name or address could ride in.
		var m map[string]any
		_ = json.Unmarshal(wire, &m)
		for _, r := range m["roles"].([]any) {
			for k := range r.(map[string]any) {
				if k != "role" && k != "configured" && k != "status" && k != "effect" {
					t.Errorf("a provider role carries the member %q", k)
				}
			}
		}
	})

	t.Run("a source error is typed and reveals nothing", func(t *testing.T) {
		f.provs.err = fmt.Errorf("dial tcp 127.0.0.1:8080: connection refused")
		_, err := f.svc.ProviderHealth(context.Background(), sc)
		wantCode(t, err, contracts.ErrUnavailable)
		var ce *contracts.Error
		asErr(err, &ce)
		if strings.Contains(ce.Message, "127.0.0.1") {
			t.Errorf("the message carries the address: %s", ce.Message)
		}
		f.provs.err = nil
	})

	t.Run("the effective settings carry no runtime member", func(t *testing.T) {
		eff, err := f.svc.Effective(context.Background(), sc)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(eff)
		for _, banned := range []string{"localai", "runtime", "backend", "gallery", "admin", "http"} {
			if strings.Contains(strings.ToLower(string(wire)), banned) {
				t.Errorf("the effective settings mention %q: %s", banned, wire)
			}
		}
	})
}

// TestSettingsCannotEnlargeScope: the key of a document, and what the caller may do
// to it, come from the verified token alone; no member names a scope, a role, a
// tenant or a permission, and no document is an input to authorisation.
func TestSettingsCannotEnlargeScope(t *testing.T) {
	f := newFixture(t)
	sc := ada()

	t.Run("no member names a scope, a role, a tenant or a permission", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"capability scopes", `{"capability_scopes":["admin.purge"]}`, contracts.ErrValidationFailed, "scope"},
			{"roles", `{"roles":["admin"]}`, contracts.ErrValidationFailed, "scope"},
			{"tenant in the body", `{"tenant_id":"ten_globex"}`, contracts.ErrValidationFailed, "scope"},
			{"workspace in the body", `{"workspace_id":"wsp_second"}`, contracts.ErrValidationFailed, "scope"},
			{"application in the body", `{"application_id":"app_other"}`, contracts.ErrValidationFailed, "scope"},
			{"principal in the body", `{"principal_id":"usr_bob"}`, contracts.ErrValidationFailed, "scope"},
			{"a whole scope", `{"scope":{"tenant_id":"ten_globex"}}`, contracts.ErrValidationFailed, "scope"},
			{"permissions", `{"permissions":["settings.deployment.write"]}`, contracts.ErrValidationFailed, "scope"},
			{"grant", `{"grants":[{"role":"member","capabilities":["admin.purge"]}]}`, contracts.ErrValidationFailed, "scope"},
			{"session", `{"session_id":"ses_other"}`, contracts.ErrValidationFailed, "scope"},
		})
	})

	t.Run("the schemas have no scope member", func(t *testing.T) {
		noPropertyMatches(t, regexp.MustCompile(`(?i)scope|role|permission|grant|tenant|workspace|application|principal|session|deployment_id|owner|acl`),
			"a setting must not name a scope level, a role or a permission",
			"tenant", "deployment") // the revisions of the effective view, keyed by the level they came from
	})

	t.Run("the key comes from the token: a write never lands in another scope", func(t *testing.T) {
		g := newFixture(t)
		bob := scopeOf(tApp, tTenantA, tWsp1, tUser2, ada().CapabilityScopes...)
		carol := scopeOf(tApp, tTenantB, tWsp1, "usr_carol", ada().CapabilityScopes...)
		other := scopeOf(tOther, tTenantA, tWsp1, tUser1, ada().CapabilityScopes...)
		g.put(ada(), LevelUser, 0, `{"language":"it"}`)
		g.put(ada(), LevelTenant, 0, `{"default_language":"it"}`)
		g.put(ada(), LevelDeployment, 0, `{"default_language":"it"}`)
		if g.storedRows() != 3 {
			t.Fatalf("%d rows, want 3", g.storedRows())
		}
		// Everyone else sees nothing of ada's documents, at any level.
		for name, who := range map[string]contracts.Scope{"bob (same tenant)": bob, "carol (other tenant)": carol, "other application": other} {
			for _, level := range Levels() {
				rec := g.get(who, level)
				switch {
				case level == LevelUser && rec.Version != 0:
					t.Errorf("%s reads ada's user document: %+v", name, rec)
				case level == LevelTenant && who.TenantID != tTenantA:
					if rec.Version != 0 {
						t.Errorf("%s reads tenant A's document: %+v", name, rec)
					}
				case level == LevelTenant && who.ApplicationID != tApp:
					if rec.Version != 0 {
						t.Errorf("%s reads another application's tenant document: %+v", name, rec)
					}
				case level == LevelDeployment && who.ApplicationID != tApp:
					if rec.Version != 0 {
						t.Errorf("%s reads application %s's deployment document: %+v", name, tApp, rec)
					}
				}
			}
		}
		// Bob (same tenant) does share the tenant and deployment documents, by design.
		if g.get(bob, LevelTenant).Version != 1 || g.get(bob, LevelDeployment).Version != 1 {
			t.Error("a principal of the same tenant does not see the tenant and deployment documents")
		}
		// A write by bob, carol or the other application never touches ada's rows.
		g.put(bob, LevelUser, 0, `{"language":"en"}`)
		g.put(carol, LevelTenant, 0, `{"default_language":"en"}`)
		g.put(other, LevelDeployment, 0, `{"default_language":"en"}`)
		if got := g.get(ada(), LevelUser); string(got.Settings) != `{"language":"it"}` || got.Version != 1 {
			t.Errorf("ada's user document = %+v", got)
		}
		if got := g.get(ada(), LevelTenant); string(got.Settings) != `{"default_language":"it"}` || got.Version != 1 {
			t.Errorf("tenant A's document = %+v", got)
		}
		if got := g.get(ada(), LevelDeployment); string(got.Settings) != `{"default_language":"it"}` || got.Version != 1 {
			t.Errorf("application %s's deployment document = %+v", tApp, got)
		}
		if g.storedRows() != 6 {
			t.Errorf("%d rows, want 6", g.storedRows())
		}
	})

	t.Run("a token without a level's scope can not write that level, whatever is stored", func(t *testing.T) {
		g := newFixture(t)
		userOnly := withScopes(ada(), CapabilityUserRead, CapabilityUserWrite, capGet)
		g.put(userOnly, LevelUser, 0, `{"language":"it","history":{"retention_days":5}}`)
		for _, level := range []Level{LevelTenant, LevelDeployment} {
			wantCode(t, g.tryPut(userOnly, level, 0, `{"history":{"retention_days":3650}}`), contracts.ErrPermissionDenied)
		}
		// The stored user document grants nothing: the same token is still refused.
		wantCode(t, g.tryPut(userOnly, LevelTenant, 0, `{}`), contracts.ErrPermissionDenied)
		if g.storedRows() != 1 {
			t.Errorf("%d rows, want 1", g.storedRows())
		}
	})

	t.Run("the effective view of one tenant never carries another tenant's policy", func(t *testing.T) {
		g := newFixture(t)
		carol := scopeOf(tApp, tTenantB, tWsp1, "usr_carol", ada().CapabilityScopes...)
		g.put(ada(), LevelTenant, 0, `{"history":{"retention_days":7},"capabilities":{"disabled":["items.create"]}}`)
		eff, err := g.svc.Effective(context.Background(), carol)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Levels.Tenant != 0 || int64(eff.History.RetentionDays) != 90 || len(eff.Capabilities.Disabled) != 0 {
			t.Errorf("tenant B sees tenant A's policy: %+v", eff)
		}
	})
}

// The external-provider additive plan: settings must not enable external or hybrid
// mode, change data-egress policy, hold provider credentials or raise a budget;
// those belong to the operator-only providers API of the external-mode milestone.
func TestSettingsCannotEnableExternalModeOrChangeEgress(t *testing.T) {
	f := newFixture(t)
	sc := ada()
	const secret = "sk-live-SECRET-0123456789"

	t.Run("external and hybrid mode", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"external_mode", `{"external_mode":true}`, contracts.ErrValidationFailed, "external_mode"},
			{"mode external", `{"mode":"external"}`, contracts.ErrValidationFailed, "external_mode"},
			{"mode hybrid", `{"mode":"hybrid"}`, contracts.ErrValidationFailed, "external_mode"},
			{"external block", `{"external":{"enabled":true}}`, contracts.ErrValidationFailed, "external_mode"},
			{"hybrid", `{"hybrid":true}`, contracts.ErrValidationFailed, "external_mode"},
			{"provider", `{"provider":"openai"}`, contracts.ErrValidationFailed, "external_mode"},
			{"providers list", `{"providers":[{"id":"anthropic"}]}`, contracts.ErrValidationFailed, "external_mode"},
			{"inference mode", `{"voice":{"mode":"hosted"}}`, contracts.ErrValidationFailed, "external_mode"},
		})
	})

	t.Run("a profile can not select an external provider", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"a hosted provider slug", `{"model_profile":"openai"}`, contracts.ErrValidationFailed, ""},
			{"a hybrid profile", `{"model_profile":"hybrid"}`, contracts.ErrValidationFailed, ""},
			{"a minted provider profile id", `{"model_profile":"pprf_01J0000000000000000000000A"}`, contracts.ErrValidationFailed, ""},
			{"an external model name", `{"model_profile":"gpt-5"}`, contracts.ErrValidationFailed, ""},
		}, LevelUser)
		refuseAll(t, f, sc, []attempt{
			{"a hosted provider slug", `{"default_model_profile":"openai"}`, contracts.ErrValidationFailed, ""},
			{"a hybrid profile", `{"default_model_profile":"hybrid"}`, contracts.ErrValidationFailed, ""},
			{"a minted provider profile id", `{"default_model_profile":"pprf_01J0000000000000000000000A"}`, contracts.ErrValidationFailed, ""},
		}, LevelTenant, LevelDeployment)
	})

	t.Run("data egress", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"egress", `{"egress":{"allow":["confidential"]}}`, contracts.ErrValidationFailed, "data_egress"},
			{"data_egress", `{"data_egress":{"allow_restricted":true}}`, contracts.ErrValidationFailed, "data_egress"},
			{"egress_policy", `{"egress_policy":{"classes":["restricted"]}}`, contracts.ErrValidationFailed, "data_egress"},
			{"destinations", `{"allowed_destinations":["api.openai.com"]}`, contracts.ErrValidationFailed, "data_egress"},
			{"allowlist", `{"allowlist":["api.openai.com"]}`, contracts.ErrValidationFailed, "data_egress"},
			{"classification", `{"data_classification":"public"}`, contracts.ErrValidationFailed, "data_egress"},
			{"rag sources leave", `{"sources":{"egress":"allowed"}}`, contracts.ErrValidationFailed, ""},
			{"attachments leave", `{"attachments":{"classification":"internal"}}`, contracts.ErrValidationFailed, "data_egress"},
		})
	})

	t.Run("provider credentials", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"api key", `{"api_key":"` + secret + `"}`, contracts.ErrValidationFailed, "credentials"},
			{"nested api key", `{"provider":{"api_key":"` + secret + `"}}`, contracts.ErrValidationFailed, "external_mode"},
			{"credentials", `{"credentials":{"token":"` + secret + `"}}`, contracts.ErrValidationFailed, "credentials"},
			{"a password", `{"password":"hunter2-PASSWORD"}`, contracts.ErrValidationFailed, "credentials"},
			{"a bearer token", `{"authorization":"Bearer abc.def.ghi"}`, contracts.ErrValidationFailed, "credentials"},
			{"oauth", `{"oauth":{"client_secret":"` + secret + `"}}`, contracts.ErrValidationFailed, "credentials"},
			{"a secret where a profile goes", `{"model_profile":"` + secret + `"}`, contracts.ErrValidationFailed, ""},
			{"a secret where a voice goes", `{"voice":{"voice_id":"` + secret + `"}}`, contracts.ErrValidationFailed, ""},
			{"a secret where a language goes", `{"language":"` + secret + `"}`, contracts.ErrValidationFailed, ""},
		})
		refuseAll(t, f, sc, []attempt{
			{"a secret where a capability id goes", `{"capabilities":{"disabled":["` + secret + `"]}}`, contracts.ErrUnknownCapability, ""},
		})
	})

	t.Run("budgets", func(t *testing.T) {
		refuseAll(t, f, sc, []attempt{
			{"budget", `{"budget":{"daily_micro_units":99999999}}`, contracts.ErrValidationFailed, "budget"},
			{"quota", `{"quota":{"requests":100000}}`, contracts.ErrValidationFailed, "budget"},
			{"spend limit", `{"max_spend":1000000}`, contracts.ErrValidationFailed, "budget"},
			{"provider budget", `{"provider_budget":{"monthly_micro_units":1}}`, contracts.ErrValidationFailed, "budget"},
			{"cost", `{"cost_ceiling":5}`, contracts.ErrValidationFailed, "budget"},
		})
	})

	t.Run("the schemas have no member for any of it", func(t *testing.T) {
		noPropertyMatches(t, regexp.MustCompile(`(?i)external|hybrid|egress|destination|allow_?list|classif|credential|secret|token|api_?key|password|oauth|auth|budget|quota|spend|cost|price|micro|billing|provider|currency`),
			"a setting must not touch external mode, data egress, credentials or a budget")
	})

	t.Run("the operator's approved list is not something a setting can grow", func(t *testing.T) {
		g := newFixture(t, func(d *Deps) { d.Limits.ApprovedProfiles, d.Limits.DefaultProfile = []string{"lite"}, "lite" })
		wantCode(t, g.tryPut(sc, LevelUser, 0, `{"model_profile":"balanced"}`), contracts.ErrValidationFailed)
		wantCode(t, g.tryPut(sc, LevelDeployment, 0, `{"approved_profiles":["lite","balanced"]}`), contracts.ErrValidationFailed)
		if g.put(sc, LevelUser, 0, `{"model_profile":"lite"}`).Version != 1 {
			t.Error("the approved profile was refused")
		}
	})

	t.Run("nothing external or hybrid appears after the strongest valid use of settings", func(t *testing.T) {
		g := newFixture(t)
		g.validEverywhere(sc)
		eff, err := g.svc.Effective(context.Background(), sc)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(eff)
		for _, banned := range []string{"external", "hybrid", "egress", "provider", "credential", "budget", "api_key"} {
			if strings.Contains(strings.ToLower(string(wire)), banned) {
				t.Errorf("the effective settings mention %q: %s", banned, wire)
			}
		}
		for _, level := range Levels() {
			doc := string(g.get(sc, level).Settings)
			for _, banned := range []string{"external", "hybrid", "egress", "api_key", "secret"} {
				if strings.Contains(strings.ToLower(doc), banned) {
					t.Errorf("the %s document mentions %q: %s", level, banned, doc)
				}
			}
		}
	})
}

// The decision-layer guard (master plan 8.5, ADR-0029): settings can not change the
// decision-layer configuration. The layer is optional, bounded and non-authoritative
// and is configured by the operator, never by an application's settings.
func TestSettingsCannotChangeDecisionLayer(t *testing.T) {
	f := newFixture(t)
	sc := ada()
	refuseAll(t, f, sc, []attempt{
		{"decision", `{"decision":{"enabled":true}}`, contracts.ErrValidationFailed, "decision_layer"},
		{"decision_layer", `{"decision_layer":{"provider":"clm"}}`, contracts.ErrValidationFailed, "decision_layer"},
		{"scorer", `{"scorer":"clm-v0.1-8b"}`, contracts.ErrValidationFailed, "decision_layer"},
		{"clm", `{"clm":{"enabled":true}}`, contracts.ErrValidationFailed, "decision_layer"},
		{"candidate set", `{"candidates":["items.create"]}`, contracts.ErrValidationFailed, "decision_layer"},
		{"confidence threshold", `{"decision":{"threshold":0}}`, contracts.ErrValidationFailed, "decision_layer"},
		{"a scorer as a profile", `{"model_profile":"clm-8b"}`, contracts.ErrValidationFailed, ""},
	})
	noPropertyMatches(t, regexp.MustCompile(`(?i)decision|clm|scorer|candidate|threshold|calibrat|fallback`), "a setting must not touch the decision layer")
	before := f.policyFingerprint(sc)
	f.validEverywhere(sc)
	if f.policyFingerprint(sc) != before {
		t.Error("settings changed what the policy gateway decides")
	}
}
