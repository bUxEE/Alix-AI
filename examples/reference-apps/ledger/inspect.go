package ledger

import (
	"context"
	"strings"

	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The inspection document for the runtime and system half of the integration audit
// (master plan 4.5, "Audit targets"): one report per requested aspect, in the
// canonical order, each reported with at least one fact. The ledger reports all nine
// aspects, so none is left unverified.
//
// Nothing here is a secret or a tenant's data: credentials are described by whether
// they are required, never by value, and no count of anyone's records is given.

var inspectAspects = []string{"versions", "modules", "dependencies", "health", "scopes", "credentials", "limits", "storage", "network"}

type aspectReport struct {
	Aspect string         `json:"aspect"`
	State  string         `json:"state"`
	Facts  []actions.Fact `json:"facts"`
}

func (l *Ledger) inspect(_ context.Context, _ bridge.Identity, aspects []string) (any, error) {
	l.mu.Lock()
	rt, support := l.runtime, l.cfg.TestSupport
	l.mu.Unlock()
	want := map[string]bool{}
	for _, a := range aspects {
		want[a] = true
	}
	facts := map[string][]actions.Fact{
		"versions": {textFact("application_version", Version), intFact("contract_version", 1)},
		"modules": {
			textFact("native_bridge", "enabled"), textFact("document_api", enabledIf(rt.DocsListen != "")),
			textFact("signed_events", "enabled"), textFact("test_support", enabledIf(support)),
		},
		"dependencies": {textFact("database", "in-process memory store"), textFact("external_services", "none")},
		"health":       {textFact("status", "ok")},
		"scopes": {
			textFact("tenant_meaning", "organisation"), textFact("workspace_meaning", "team"),
			textFact("roles", strings.Join([]string{string(RoleAccountant), string(RoleManager), string(RoleViewer)}, ", ")),
		},
		"credentials": {
			textFact("bridge_key_ref", "env: or file: reference in the adapter configuration"), textFact("bridge_key_id", firstNonEmpty(rt.KeyID, "none")),
			boolFact("document_api_credential_required", true), textFact("event_signing_key", "ed25519, registered with AlixAI as the events key"),
		},
		"limits": {
			intFact("max_amount_minor", MaxMinor), intFact("max_found", MaxFound), intFact("max_candidates", MaxCandidates),
			intFact("max_description_chars", MaxDescription), intFact("max_quote_days", MaxQuoteDays),
			intFact("max_due_days", MaxDueDays), intFact("idempotency_window_seconds", int64(IdempotencyWindow.Seconds())),
			intFact("max_event_validity_seconds", int64(MaxEventValidity.Seconds())),
		},
		"storage": {textFact("database_kind", "memory"), textFact("data_dir", firstNonEmpty(l.cfg.DataDir, "none"))},
		"network": {
			textFact("bridge_address", firstNonEmpty(rt.Listen, "not listening")),
			textFact("document_api_address", firstNonEmpty(rt.DocsListen, "not listening")),
			textFact("outbound_connections", "the event intake of AlixAI on loopback only"),
		},
	}
	var reports []aspectReport
	for _, a := range inspectAspects {
		if len(aspects) == 0 || want[a] {
			reports = append(reports, aspectReport{Aspect: a, State: "reported", Facts: facts[a]})
		}
	}
	return map[string]any{"reports": reports}, nil
}

func enabledIf(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
