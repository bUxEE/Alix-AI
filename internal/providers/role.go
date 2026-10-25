package providers

import "fmt"

// Role is a model job (master plan 8.1). The registry maps a Role to the
// provider that serves it.
type Role string

const (
	// RoleController is the conversational controller model: it proposes
	// typed tasks and realises natural-language responses. It is the only
	// required role.
	RoleController Role = "controller"
	// RoleRouter is the optional small decision model for bounded
	// classification and routing. Served by a TextGenerator.
	RoleRouter Role = "router"
	// RoleEmbedder creates document and query embeddings.
	RoleEmbedder Role = "embedder"
	// RoleReranker is the optional cross-encoder over a small authorized
	// shortlist.
	RoleReranker Role = "reranker"

	// RoleSTT, RoleTTS and RoleVAD are reserved for M4. The registry accepts
	// providers for them, typed access comes from internal/providers/voice.
	RoleSTT Role = "stt"
	RoleTTS Role = "tts"
	RoleVAD Role = "vad"
)

// Degradation effect codes. They are stable strings other packages may label
// results with (for example rag marks hybrid search as reduced_semantic_recall).
const (
	EffectLLMUnavailable        = "llm_unavailable"
	EffectDeterministicRouting  = "deterministic_routing"
	EffectReducedSemanticRecall = "reduced_semantic_recall"
	EffectFusedOrder            = "fused_retrieval_order"
	EffectTextInputOnly         = "text_input_only"
	EffectVerifiedTextOnly      = "verified_text_only"
	EffectManualTurnTaking      = "manual_turn_taking"
)

type roleInfo struct {
	role     Role
	required bool
	reserved bool
	effect   string
	behavior string // master plan section 18
}

// roleTable is the single source of truth for roles, in canonical order.
var roleTable = [...]roleInfo{
	{RoleController, true, false, EffectLLMUnavailable,
		"Application and search remain usable; return a clear local failure; never execute from partial model output."},
	{RoleRouter, false, false, EffectDeterministicRouting,
		"Route with deterministic rules and the controller model; no routing model is needed."},
	{RoleEmbedder, false, false, EffectReducedSemanticRecall,
		"Use authorized FTS and exact search and label reduced semantic recall."},
	{RoleReranker, false, false, EffectFusedOrder,
		"Use the fused retrieval order."},
	{RoleSTT, false, true, EffectTextInputOnly,
		"Text input remains available."},
	{RoleTTS, false, true, EffectVerifiedTextOnly,
		"Return verified text."},
	{RoleVAD, false, true, EffectManualTurnTaking,
		"Voice activity detection is off; use explicit turn taking or text."},
}

// Roles returns every known role in canonical order.
func Roles() []Role {
	out := make([]Role, len(roleTable))
	for i, ri := range roleTable {
		out[i] = ri.role
	}
	return out
}

func (r Role) info() (roleInfo, bool) {
	for _, ri := range roleTable {
		if ri.role == r {
			return ri, true
		}
	}
	return roleInfo{}, false
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { _, ok := r.info(); return ok }

// Required reports whether the service cannot answer without this role.
func (r Role) Required() bool { ri, ok := r.info(); return ok && ri.required }

// Reserved reports whether the role is reserved for a later milestone (voice).
func (r Role) Reserved() bool { ri, ok := r.info(); return ok && ri.reserved }

// IsText reports whether the role is served by a TextGenerator.
func (r Role) IsText() bool { return r == RoleController || r == RoleRouter }

// Effect returns the stable degradation effect code for the role ("" for an
// unknown role).
func (r Role) Effect() string { ri, _ := r.info(); return ri.effect }

// Behavior returns the required behaviour when the role is unavailable, as
// stated in master plan section 18.
func (r Role) Behavior() string { ri, _ := r.info(); return ri.behavior }

func (r Role) String() string { return string(r) }

// Health is a role's operational state.
type Health string

const (
	HealthOK          Health = "ok"
	HealthDegraded    Health = "degraded"
	HealthUnavailable Health = "unavailable"
)

// Valid reports whether h is one of the three defined states.
func (h Health) Valid() bool {
	return h == HealthOK || h == HealthDegraded || h == HealthUnavailable
}

func (h Health) String() string { return string(h) }

func unknownRoleError(r Role) error {
	return fmt.Errorf("providers: unknown role %q", string(r))
}
