package gateway

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"

	"github.com/buxee/alix-ai/pkg/contracts"
	decisiongen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The decision-layer part of the public API (master plan 8.5 and 14.2 "Operations",
// docs/architecture/decision-layer.md section 8). The contract is
// schemas/api/paths/decision.yaml; TestRoutesMatchOpenAPISpec fails on any
// difference between the routes registered here and the document.
//
//	GET /v1/decision/status   provider, model, revision, mode, fallback counters, accepted and rejected counts
//	GET /v1/decision/config   the per-profile configuration (operator only)
//	PUT /v1/decision/config   enable, shadow or disable one profile and set its threshold (operator only)
//
// # What the area is and is not
//
// The decision layer is optional, bounded, non-generative and non-authoritative: it
// scores a short list of candidates that were already authorised and it decides,
// authorises, validates, prepares, confirms and commits nothing. So the status is an
// aggregate operational view (decisiongen.DecisionStatus, the D-02 schema) and the
// route can hand out nothing else: the handler takes the generated type from the
// service, so a member the schema does not have cannot reach the wire, and it
// refuses to send a status that contradicts itself (a rejected count that is not the
// sum of the fallback counts). There is no score, threshold, gate, candidate, key or
// tenant datum in it, and no counter in it can be read as a permission.
//
// # Who may call
//
// Every route takes the application, tenant, workspace and principal from the
// verified single-use context token and from nothing else: the routes take no
// identifier in the path, the query or the body (a query string and any unknown body
// member are validation_failed), and the service counts and configures per scope.
//
//   - Status needs the exact capability decision.status.read in the token.
//   - The configuration needs an OPERATOR, decided by DecisionService.AuthorizeOperator
//     from the deployment's own configuration of operators, never from a role or a
//     capability scope the host application asserts (a token that says "admin" or
//     "operator" is exactly as powerless as one that says nothing). With no
//     operator configured, nobody passes: permission_denied, 403, before any body is
//     read. The settings API (M3-07) has no member and no route for any of it.
//   - A threshold is never accepted below the platform floor: the larger of the
//     service's configured floor and decisionHardFloor, which no configuration lowers.
//
// The area needs the ApplicationService (a quarantined application serves no
// traffic) and uses the DecisionService when there is one; until the composition
// root provides it every route answers unavailable, never an empty success. This is
// what lets the test rigs that provide only the ApplicationService keep working.

func init() { RegisterArea("v1_decision", registerV1Decision) }

// DecisionCapabilityStatusRead is the capability a token must carry to read the
// decision status. The match is exact; no role stands in for it.
const DecisionCapabilityStatusRead = "decision.status.read"

// decisionHardFloor is the lowest confidence threshold the platform ever allows:
// no deployment configuration, and no operator, can set a profile below it.
const decisionHardFloor = 0.5

var decisionProfilePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// DecisionService is what the decision area needs. The composition root provides
// the implementation over the decision engine and its lifecycle counters; tests
// stand in their own. Every method takes the verified Scope and answers for that
// scope alone.
type DecisionService interface {
	// Status is the aggregate state of the layer for the scope's application and
	// tenant. RejectedCount must equal the sum of FallbackCounts.
	Status(ctx context.Context, scope contracts.Scope) (decisiongen.DecisionStatus, error)
	// AuthorizeOperator returns nil only for an operator of the deployment's own
	// configuration (never a role or capability the token asserts) and
	// permission_denied for everybody else, including when none is configured.
	AuthorizeOperator(ctx context.Context, scope contracts.Scope) error
	// PlatformFloor is the lowest threshold the deployment allows. The handler never
	// accepts a threshold below the larger of this and decisionHardFloor.
	PlatformFloor() float64
	// Config returns the configuration of every approved profile of the scope's
	// application.
	Config(ctx context.Context, scope contracts.Scope) ([]DecisionProfileConfig, error)
	// SetConfig changes one profile and returns the profile as it now stands. A
	// profile that is not approved is validation_failed. A nil MinConfidence keeps
	// the profile's current threshold. The change is audited by the implementation.
	SetConfig(ctx context.Context, scope contracts.Scope, update DecisionConfigUpdate) (DecisionProfileConfig, error)
}

// DecisionConfigUpdate is the body of PUT /v1/decision/config: one profile, its
// mode (off disables it) and, optionally, its confidence threshold.
type DecisionConfigUpdate struct {
	Profile       string                         `json:"profile"`
	Mode          decisiongen.DecisionStatusMode `json:"mode"`
	MinConfidence *float64                       `json:"min_confidence,omitempty"`
}

// DecisionProfileConfig is one profile's configuration as an operator sees it.
type DecisionProfileConfig struct {
	Profile       string                         `json:"profile"`
	Mode          decisiongen.DecisionStatusMode `json:"mode"`
	MinConfidence float64                        `json:"min_confidence"`
}

// DecisionConfigView is the body of GET /v1/decision/config.
type DecisionConfigView struct {
	PlatformFloor float64                 `json:"platform_floor"`
	Profiles      []DecisionProfileConfig `json:"profiles"`
}

// DecisionConfigResult is the body of a successful PUT /v1/decision/config.
type DecisionConfigResult struct {
	PlatformFloor float64               `json:"platform_floor"`
	Profile       DecisionProfileConfig `json:"profile"`
}

type decisionHandler struct {
	apps ApplicationService
	svc  DecisionService // nil while none is provided
}

func registerV1Decision(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[DecisionService](n)
	if !n.Ready() {
		return
	}
	h := &decisionHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /decision/status", h.status)
	v1.HandleFunc("GET /decision/config", h.getConfig)
	v1.HandleFunc("PUT /decision/config", h.putConfig)
}

// begin is the start of every handler: the verified scope, no client-supplied
// identity, an active application and a provided service, or the refusal already
// written.
func (h *decisionHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if len(r.URL.Query()) > 0 || r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the decision routes take no query: the scope is the verified token"))
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "the decision layer is not available on this server"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

// beginOperator is begin plus the operator gate, before anything is read.
func (h *decisionHandler) beginOperator(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := h.begin(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if err := h.svc.AuthorizeOperator(r.Context(), scope); err != nil {
		var typed *contracts.Error
		if !errors.As(err, &typed) {
			// An error that is not typed is not a decision: deny.
			err = contracts.New(contracts.ErrPermissionDenied, "the decision configuration is for operators")
		}
		WriteError(w, r, err)
		return contracts.Scope{}, false
	}
	return scope, true
}

// floor is the lowest threshold accepted now, or false when the service reports
// one that cannot be trusted.
func (h *decisionHandler) floor() (float64, bool) {
	f := h.svc.PlatformFloor()
	if math.IsNaN(f) || f <= 0 || f > 1 {
		return 0, false
	}
	return math.Max(f, decisionHardFloor), true
}

func (h *decisionHandler) status(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if !scope.Permits(DecisionCapabilityStatusRead) {
		WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "the token does not carry the capability that reads the decision status").
			WithDetails("capability", DecisionCapabilityStatusRead))
		return
	}
	st, err := h.svc.Status(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := decisionCheckStatus(st); err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// decisionCheckStatus refuses a status that contradicts itself: the counters are
// aggregate facts, so rejected_count is exactly the sum of the fallback counts and
// every reason appears once. An inconsistent status is an internal error, never a
// success-looking one.
func decisionCheckStatus(st decisiongen.DecisionStatus) error {
	bad := func(why string) error {
		return contracts.New(contracts.ErrInternal, "the decision status is inconsistent: "+why)
	}
	if !st.Mode.Valid() || st.ProviderID == "" || st.Since == "" || st.GeneratedAt == "" {
		return bad("a required member is missing")
	}
	if st.AcceptedCount < 0 || st.RejectedCount < 0 || len(st.FallbackCounts) > 13 {
		return bad("a counter is out of range")
	}
	var sum int64
	seen := map[decisiongen.FallbackReason]bool{}
	for _, fc := range st.FallbackCounts {
		if !fc.Reason.Valid() || fc.Count < 0 || seen[fc.Reason] {
			return bad("a fallback count is malformed or repeated")
		}
		seen[fc.Reason] = true
		sum += fc.Count
	}
	if sum != st.RejectedCount {
		return bad("rejected_count is not the sum of fallback_counts")
	}
	return nil
}

func (h *decisionHandler) getConfig(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginOperator(w, r)
	if !ok {
		return
	}
	floor, ok := h.floor()
	if !ok {
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the platform floor is not configured"))
		return
	}
	profiles, err := h.svc.Config(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if profiles == nil {
		profiles = []DecisionProfileConfig{}
	}
	for _, p := range profiles {
		if !decisionProfilePattern.MatchString(p.Profile) || !p.Mode.Valid() || math.IsNaN(p.MinConfidence) || p.MinConfidence < floor || p.MinConfidence > 1 {
			WriteError(w, r, contracts.New(contracts.ErrInternal, "a stored profile configuration is invalid"))
			return
		}
	}
	writeJSON(w, http.StatusOK, DecisionConfigView{PlatformFloor: floor, Profiles: profiles})
}

func (h *decisionHandler) putConfig(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.beginOperator(w, r)
	if !ok {
		return
	}
	floor, ok := h.floor()
	if !ok {
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the platform floor is not configured"))
		return
	}
	var up DecisionConfigUpdate
	if !decodeJSON(w, r, &up) {
		return
	}
	if !decisionProfilePattern.MatchString(up.Profile) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "profile is required: a profile name of lowercase letters, digits, - and _"))
		return
	}
	if !up.Mode.Valid() {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "mode is required: off, shadow or advisory"))
		return
	}
	if t := up.MinConfidence; t != nil {
		switch {
		case math.IsNaN(*t) || *t > 1:
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "min_confidence must be a number up to 1"))
			return
		case *t < floor:
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "min_confidence is below the platform floor").
				WithDetails("reason", "below_platform_floor").WithDetails("platform_floor", floor))
			return
		}
	}
	got, err := h.svc.SetConfig(r.Context(), scope, up)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Profile != up.Profile || !got.Mode.Valid() || math.IsNaN(got.MinConfidence) || got.MinConfidence < floor || got.MinConfidence > 1 {
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the stored profile configuration is invalid"))
		return
	}
	writeJSON(w, http.StatusOK, DecisionConfigResult{PlatformFloor: floor, Profile: got})
}
