package agui

// What the endpoint claims, stated once so the capabilities route (M3-25) and the documentation cannot drift from the
// mapping. The document is deliberately conservative: a feature is listed as supported only when this package produces it
// and a test covers it; everything else is listed as not claimed, with the reason in docs/integration/agui.md.

// The two features whose claim depends on what the deployment behind the endpoint has wired (Wiring). Everything else in
// NotClaimed is not claimed whatever the wiring.
const (
	// FeatureClientContext is the page's readable context, shared state and earlier messages reaching the engine as
	// fenced, bounded, untrusted data (docs/integration/agui.md section 4).
	FeatureClientContext = "client_context"
	// FeatureClientToolCalls is the model proposing a call of a UI tool the client declared and the host registered
	// (section 5).
	FeatureClientToolCalls = "client_tool_calls"
)

// NotClaimed lists the features the endpoint does not claim, as stable identifiers a client can test for. It is the
// document of a deployment that has wired nothing (Capabilities); CapabilitiesFor removes the two features of Wiring
// that a deployment has wired.
var NotClaimed = []string{
	"reasoning_events",
	"encrypted_reasoning",
	"subagent_events",
	"raw_events",
	"chunk_events",
	FeatureClientContext,
	FeatureClientToolCalls,
	"voice_full_duplex",
	"predictive_state_updates",
	"inline_attachments",
	"model_generated_suggestions",
}

// Wiring says which of the two wiring-dependent features the deployment behind the endpoint has wired. The gateway fills
// it per caller; the zero value is a deployment that has wired nothing, which claims neither.
type Wiring struct {
	// ClientContext: the route hands the client's readable context to the engine (Request.EngineContext).
	ClientContext bool
	// ClientTools: the host registered at least one UI tool the model could be offered for the caller's application
	// (dialogue.Deps.UITools). Without one, a declared tool is accepted and never offered, so nothing is claimed.
	ClientTools bool
}

// CapabilitiesDocument is the body of GET /v1/agui/capabilities.
type CapabilitiesDocument struct {
	// ProtocolVersion is the AG-UI protocol version implemented and Package the release the schemas come from.
	ProtocolVersion string `json:"protocolVersion"`
	Package         string `json:"package"`
	// EventsEmitted and EventsNotEmitted partition the event types of the release.
	EventsEmitted    []string `json:"eventsEmitted"`
	EventsNotEmitted []string `json:"eventsNotEmitted"`
	// NotClaimed lists the features that are not claimed (see NotClaimed).
	NotClaimed []string `json:"notClaimed"`
	// Agent is the AG-UI AgentCapabilities object of the release: validated against the vendored schema by a test.
	Agent map[string]any `json:"agent"`
}

// Capabilities returns the capabilities document of a deployment that has wired nothing (the zero Wiring): the
// conservative default. version is the version of the AlixAI core (for the agent's identity).
func Capabilities(version string) CapabilitiesDocument { return CapabilitiesFor(version, Wiring{}) }

// CapabilitiesFor returns the capabilities document for a deployment with the given wiring. A feature is claimed only
// when it is wired: client context removes FeatureClientContext from NotClaimed, and a registered UI tool removes
// FeatureClientToolCalls and sets tools.clientProvided. Nothing else changes with the wiring.
func CapabilitiesFor(version string, w Wiring) CapabilitiesDocument {
	strs := func(types []EventType) []string {
		out := make([]string, len(types))
		for i, t := range types {
			out[i] = string(t)
		}
		return out
	}
	tool := func(name, desc string) map[string]any { return map[string]any{"name": name, "description": desc} }
	return CapabilitiesDocument{
		ProtocolVersion:  ProtocolVersion,
		Package:          PinnedPackage,
		EventsEmitted:    strs(EmittedTypes()),
		EventsNotEmitted: strs(NotEmittedTypes()),
		NotClaimed:       notClaimedFor(w),
		Agent: map[string]any{
			"identity": map[string]any{
				"name": "AlixAI", "type": "application-bound assistant", "provider": "self-hosted", "version": version,
				"description": "Prepares actions, shows their exact effects and commits them only after the user's explicit confirmation.",
			},
			"transport": map[string]any{"streaming": true, "websocket": true, "resumable": true, "httpBinary": false, "pushNotifications": false},
			"tools": map[string]any{
				// The agent's own tool calls are two UI tools. A tool a client declares is a UI declaration: the model may
				// propose a call of it only when the host registered it for the application (docs/integration/agui.md
				// section 5), so clientProvided is true only for a deployment that has registered one.
				"supported": true, "clientProvided": w.ClientTools, "parallelCalls": false,
				"items": []any{
					tool(ConfirmToolName, "Shows the exact effects of a prepared action and waits for the user's approval."),
					tool(ContinueToolName, "Hands the user to the application's own authenticated interface."),
				},
			},
			"state":      map[string]any{"snapshots": true, "deltas": true, "persistentState": true, "memory": false},
			"output":     map[string]any{"structuredOutput": true, "supportedMimeTypes": []any{"text/plain", "text/markdown"}},
			"multiAgent": map[string]any{"supported": false},
			"reasoning":  map[string]any{"supported": false, "streaming": false, "encrypted": false},
			"multimodal": map[string]any{
				"input":  map[string]any{"image": true, "file": true, "audio": false, "video": false, "pdf": false},
				"output": map[string]any{"image": false, "audio": false},
			},
			"execution":      map[string]any{"codeExecution": false},
			"humanInTheLoop": map[string]any{"supported": true, "approvals": true, "interrupts": true, "approveWithEdits": false, "feedback": false, "interventions": false},
		},
	}
}

// notClaimedFor is NotClaimed without the features the wiring claims.
func notClaimedFor(w Wiring) []string {
	out := make([]string, 0, len(NotClaimed))
	for _, f := range NotClaimed {
		if (f == FeatureClientContext && w.ClientContext) || (f == FeatureClientToolCalls && w.ClientTools) {
			continue
		}
		out = append(out, f)
	}
	return out
}
