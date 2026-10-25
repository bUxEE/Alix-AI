package capabilities

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// SignalKind says why a capability is on the shortlist.
type SignalKind string

// The kinds of evidence, in the order a candidate lists them.
const (
	// KindGoal: the session has an active goal on this capability.
	KindGoal SignalKind = "active_goal"
	// KindNextAction: the capability is a permitted next action of the goal's
	// capability in the goal's current phase.
	KindNextAction SignalKind = "next_action"
	// KindTerminology: the utterance uses a term (or a synonym of a term) of the
	// package's terminology that the capability's conversation contract relies on.
	KindTerminology SignalKind = "terminology"
	// KindKeyword: the utterance uses a word of the capability's title.
	KindKeyword SignalKind = "keyword"
	// KindEmbedding: the optional embedder found the utterance close to the
	// capability's description.
	KindEmbedding SignalKind = "embedding"
)

// Outcome is the router's explicit answer.
type Outcome string

const (
	// OutcomeShortlist: at least one policy-visible capability is a candidate.
	OutcomeShortlist Outcome = "shortlist"
	// OutcomeOutOfScope: no policy-visible capability is a candidate. The router
	// never falls back to "everything visible": an empty shortlist is a refusal
	// or a clarification for the caller, never a guess (overview.md section 5).
	OutcomeOutOfScope Outcome = "out_of_scope"
)

// OutOfScopeReason says why the outcome is out_of_scope.
type OutOfScopeReason string

const (
	// ReasonNoVisibleCapability: the policy gateway exposed nothing to this
	// caller, so there was nothing to route to.
	ReasonNoVisibleCapability OutOfScopeReason = "no_visible_capability"
	// ReasonNoMatch: capabilities are visible, and nothing in the request points
	// at any of them.
	ReasonNoMatch OutOfScopeReason = "no_match"
)

// EmbeddingStatus says what the optional embedding stage did for a request.
type EmbeddingStatus string

const (
	// EmbeddingOff: no embedder is configured. The router is rules only.
	EmbeddingOff EmbeddingStatus = "off"
	// EmbeddingUnavailable: an embedder is configured and reported unavailable.
	EmbeddingUnavailable EmbeddingStatus = "unavailable"
	// EmbeddingSkipped: the rules were decisive, so the embedder was not called.
	EmbeddingSkipped EmbeddingStatus = "skipped"
	// EmbeddingUsed: the embedder answered and its similarity was applied.
	EmbeddingUsed EmbeddingStatus = "used"
	// EmbeddingFailed: the embedder was called and failed (error, timeout, a
	// malformed vector). The result is the rules-only result.
	EmbeddingFailed EmbeddingStatus = "failed"
)

// Phases of an ActiveGoal, as the capability schema names them in next_actions.
const (
	// PhaseOpen: the goal is still being filled in or discussed.
	PhaseOpen = ""
	// PhaseSuccess: the goal's capability has just produced a result.
	PhaseSuccess = "success"
	// PhasePrepared: an action is prepared and awaits confirmation.
	PhasePrepared = "prepared"
	// PhaseFailure: the goal's capability has just failed.
	PhaseFailure = "failure"
)

// Scores of the non-textual evidence. A utterance with no evidence at all keeps
// the active goal first; a clear request for something else (more evidence than
// goalBonus) outranks it, and the goal stays on the shortlist either way.
const (
	goalBonus       = 0.75
	nextActionBonus = 0.5
	embedWeight     = 1.5
	// maxTokens bounds how many tokens of the utterance are matched.
	maxTokens = 128
)

// ShortlistCeiling is the largest shortlist a Config may ask for. The shortlist
// exists to keep the prompt small, so it has a hard bound whatever the operator
// configures.
const ShortlistCeiling = 12

// ActiveGoal is the session's goal as far as routing needs it. It is a local
// type: internal/sessions sits above this package and is the one that fills it.
type ActiveGoal struct {
	// CapabilityID is the capability the goal concerns. It is only used if the
	// caller may see it; a goal on a capability that has become invisible
	// (role change, narrower token, package replaced) is dropped.
	CapabilityID string
	// Phase is PhaseOpen while the goal is being filled in, or PhaseSuccess,
	// PhasePrepared or PhaseFailure once it has that state. The permitted next
	// actions of the capability for that phase are candidates too.
	Phase string
}

// Request is one routing question: which capabilities could this turn be about.
type Request struct {
	// Scope is the verified context. It must be valid.
	Scope contracts.Scope
	// Binding is the application, tenant and workspace of the session as the core
	// stored them. It must equal the Scope's, and is what the policy gateway is
	// asked about.
	Binding policy.Binding
	// Utterance is the user's text for this turn (or the finalised transcript).
	Utterance string
	// Language is the user's language tag ("it", "en-GB"), if the host knows it.
	// Empty or unsupported means every language of the package is matched.
	Language string
	// Goal is the session's active goal, if any.
	Goal *ActiveGoal
	// TaskFields are the fields the current task needs, passed through to the
	// policy gateway's field projection.
	TaskFields []string
}

// Signal is one reason a capability is on the shortlist.
type Signal struct {
	Kind SignalKind
	// Key is the term ID (terminology), the matched title word (keyword), or empty.
	Key string
	// Label is the package's own text that matched, clipped; empty for goal,
	// next action and embedding.
	Label string
	// Language is the language of the matched label, empty for language-neutral
	// evidence.
	Language string
	// Weight is what the signal added to the candidate's score.
	Weight float64
	// Similarity is the cosine similarity for an embedding signal, else zero.
	Similarity float64
}

// Candidate is one capability on the shortlist. It is policy-visible by
// construction: the Decision is the allowed Expose decision for it, and carries
// the visible fields and effective risk the prompt builder must use.
type Candidate struct {
	CapabilityID string
	Decision     policy.Decision
	// Score orders the shortlist. It is a ranking, not a probability and not a
	// permission.
	Score   float64
	Signals []Signal
}

// Result is the router's answer.
type Result struct {
	Outcome Outcome
	// Reason is set when Outcome is OutcomeOutOfScope.
	Reason OutOfScopeReason
	// Candidates are at most Config.MaxShortlist capabilities, best first. Empty
	// exactly when the outcome is out_of_scope.
	Candidates []Candidate
	// Visible is how many capabilities the caller may see in total, before the
	// shortlist was cut; Considered is how many the gateway evaluated.
	Visible    int
	Considered int
	// Languages are the languages whose labels the utterance was matched against.
	Languages []string
	// Embedding says what the optional embedding stage did.
	Embedding EmbeddingStatus
	// Decisive is true when the rules found one clear candidate (a single one, or
	// a leader ahead of the second by at least Config.ConfidentMargin). It is
	// advisory: a caller may use it to skip a model call that would only choose
	// between candidates, never to skip policy, validation or confirmation.
	Decisive bool
}

// IDs returns the shortlisted capability IDs, best first.
func (r Result) IDs() []string {
	out := make([]string, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		out = append(out, c.CapabilityID)
	}
	return out
}

// Config bounds the router. A zero field takes its default; a value past a
// ceiling is clamped to it.
type Config struct {
	// MaxShortlist is the most candidates returned (default 5, ceiling
	// ShortlistCeiling). It is the Lite context budget for tool descriptions.
	MaxShortlist int
	// MaxUtteranceRunes is how much of the utterance is read (default 1000).
	MaxUtteranceRunes int
	// EmbedTimeout bounds the embedding stage as a whole (default 750 ms).
	EmbedTimeout time.Duration
	// MinSimilarity is the cosine similarity below which the embedder's opinion
	// is ignored (default 0.5). It is provisional until it has been calibrated
	// against the real embedding model; see docs/efficiency/M1-08.md.
	MinSimilarity float64
	// ConfidentMargin is the lead in score that makes the rules decisive, and
	// lets the router skip the embedder (default 1.0).
	ConfidentMargin float64
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{MaxShortlist: 5, MaxUtteranceRunes: 1000, EmbedTimeout: 750 * time.Millisecond, MinSimilarity: 0.5, ConfidentMargin: 1.0}
}

func (c Config) normalized() Config {
	d := DefaultConfig()
	if c.MaxShortlist <= 0 {
		c.MaxShortlist = d.MaxShortlist
	}
	if c.MaxShortlist > ShortlistCeiling {
		c.MaxShortlist = ShortlistCeiling
	}
	if c.MaxUtteranceRunes <= 0 {
		c.MaxUtteranceRunes = d.MaxUtteranceRunes
	}
	if c.EmbedTimeout <= 0 {
		c.EmbedTimeout = d.EmbedTimeout
	}
	if c.MinSimilarity <= 0 || c.MinSimilarity >= 1 {
		c.MinSimilarity = d.MinSimilarity
	}
	if c.ConfidentMargin <= 0 {
		c.ConfidentMargin = d.ConfidentMargin
	}
	return c
}

// Router builds the capability shortlist of a turn (master plan 11.1: "capability
// shortlist"). It is the deterministic-first stage: the policy gateway decides
// what the caller may see, rules and lookups rank those capabilities, and an
// embedding model, if one exists, only adds evidence about capabilities that are
// already visible. It is independent of any scorer; the optional decision stage
// runs after it, on the shortlist it returns.
//
// A Router holds no per-request state and is safe for concurrent use.
type Router struct {
	reg  *Registry
	gw   *policy.Gateway
	cfg  Config
	emb  providers.Embedder
	prov *providers.Registry
}

// RouterOption configures NewRouter.
type RouterOption func(*Router)

// WithConfig sets the bounds.
func WithConfig(c Config) RouterOption { return func(r *Router) { r.cfg = c.normalized() } }

// WithEmbedder gives the router an embedder directly.
func WithEmbedder(e providers.Embedder) RouterOption { return func(r *Router) { r.emb = e } }

// WithProviders lets the router look the embedder up in the provider registry on
// each request, so it follows the role's health: a role that is not registered
// or is unavailable means rules only. WithEmbedder takes precedence.
func WithProviders(p *providers.Registry) RouterOption { return func(r *Router) { r.prov = p } }

// NewRouter returns a router over a registry and a policy gateway.
func NewRouter(reg *Registry, gw *policy.Gateway, opts ...RouterOption) (*Router, error) {
	if reg == nil || gw == nil {
		return nil, contracts.New(contracts.ErrInternal, "capabilities: a router needs a registry and a policy gateway")
	}
	r := &Router{reg: reg, gw: gw, cfg: DefaultConfig()}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// embedder resolves the optional embedder for one request. The status is only
// meaningful when the embedder is nil: why there is none.
func (rt *Router) embedder() (providers.Embedder, EmbeddingStatus) {
	if rt.emb != nil {
		return rt.emb, ""
	}
	if rt.prov == nil || !rt.prov.Has(providers.RoleEmbedder) {
		return nil, EmbeddingOff
	}
	if !rt.prov.Usable(providers.RoleEmbedder) {
		return nil, EmbeddingUnavailable
	}
	e, err := rt.prov.Embedder()
	if err != nil {
		return nil, EmbeddingUnavailable
	}
	return e, ""
}

// cand is a candidate under construction.
type cand struct {
	id      string
	score   float64
	goal    bool
	signals []Signal
}

func (c *cand) add(s Signal) {
	c.score += s.Weight
	c.signals = append(c.signals, s)
}

// Route returns the shortlist for one turn.
//
// The order is fixed and every step fails closed:
//
//  1. the Scope is valid and equals the Binding (an error otherwise, never an
//     out_of_scope result: a confused scope is not "unrelated text");
//  2. the application of the Scope has an active package in the registry (an
//     unavailable error otherwise);
//  3. the policy gateway's Expose decides which capabilities the caller may see;
//     that set is the only pool every later step draws from;
//  4. the active goal and its next actions, then terminology and title keywords,
//     score the visible capabilities;
//  5. if the rules are not decisive and an embedder exists, its similarity adds
//     evidence about the visible capabilities;
//  6. the best Config.MaxShortlist are returned, or an out_of_scope result.
func (rt *Router) Route(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, providers.ContextError(err)
	}
	if err := req.Scope.Validate(); err != nil {
		return Result{}, err
	}
	if req.Binding.ApplicationID == "" || req.Binding.TenantID == "" || req.Binding.WorkspaceID == "" {
		return Result{}, contracts.New(contracts.ErrScopeMissing, "capabilities: the request has no session binding")
	}
	if req.Binding != policy.BindingOf(req.Scope) {
		return Result{}, contracts.New(contracts.ErrScopeViolation, "capabilities: the session binding does not match the verified scope")
	}
	utt := strings.TrimSpace(req.Utterance)
	if utt == "" {
		return Result{}, contracts.New(contracts.ErrValidationFailed, "capabilities: the request has no text")
	}
	utt = truncateRunes(utt, rt.cfg.MaxUtteranceRunes)

	ix, err := rt.reg.lookup(ctx, req.Scope.ApplicationID)
	if err != nil {
		return Result{}, err
	}

	exp := rt.gw.Expose(ctx, policy.ExposeRequest{
		Scope: req.Scope, Package: ix.pkg, Binding: req.Binding, TaskFields: req.TaskFields,
	})
	if err := ctx.Err(); err != nil {
		return Result{}, providers.ContextError(err)
	}
	if exp.Code != "" {
		// The exposure as a whole was refused (its audit record could not be written, or
		// the package is not servable): that is a typed failure, not "nothing matches".
		return Result{}, contracts.New(exp.Code, "capabilities: the policy gateway refused the exposure")
	}
	visible := make(map[string]policy.Decision, len(exp.Visible))
	for _, d := range exp.Visible {
		id := d.Record.CapabilityID
		// Only capabilities the registry indexed are routable: an ID the gateway
		// allowed that this package does not declare as a user capability is not.
		if _, ok := ix.caps[id]; ok && d.Allowed {
			visible[id] = d
		}
	}
	res := Result{Visible: len(visible), Considered: exp.Considered, Embedding: EmbeddingOff}
	langs := ix.resolveLanguages(req.Language)
	res.Languages = langs
	if len(visible) == 0 {
		res.Outcome, res.Reason = OutcomeOutOfScope, ReasonNoVisibleCapability
		if emb, st := rt.embedder(); emb == nil {
			res.Embedding = st
		} else {
			res.Embedding = EmbeddingSkipped // nothing visible: nothing to embed against
		}
		return res, nil
	}

	cands := map[string]*cand{}
	get := func(id string) *cand {
		c := cands[id]
		if c == nil {
			c = &cand{id: id}
			cands[id] = c
		}
		return c
	}

	// 4a. The active goal and the next actions it allows.
	if g := req.Goal; g != nil {
		if _, ok := visible[g.CapabilityID]; ok {
			c := get(g.CapabilityID)
			c.goal = true
			c.add(Signal{Kind: KindGoal, Weight: goalBonus})
			if g.Phase != PhaseOpen {
				for _, na := range ix.caps[g.CapabilityID].nextActions {
					if na.after != g.Phase || na.capabilityID == g.CapabilityID {
						continue
					}
					if _, ok := visible[na.capabilityID]; !ok {
						continue
					}
					get(na.capabilityID).add(Signal{Kind: KindNextAction, Key: na.capabilityID, Weight: nextActionBonus})
				}
			}
		}
	}

	// 4b. Terminology and title keywords.
	rt.matchUnits(ix, visible, langs, tokenize(utt, maxTokens), get)

	ranked := rank(cands)
	// 5. The optional embedder, only when the rules are not decisive.
	res.Embedding = rt.embedStage(ctx, req.Scope, ix, utt, langs, visible, get, ranked)
	if res.Embedding == EmbeddingUsed {
		ranked = rank(cands)
	}

	if len(ranked) == 0 {
		res.Outcome, res.Reason = OutcomeOutOfScope, ReasonNoMatch
		return res, nil
	}
	if len(ranked) > rt.cfg.MaxShortlist {
		ranked = ranked[:rt.cfg.MaxShortlist]
	}
	res.Outcome = OutcomeShortlist
	res.Decisive = decisive(ranked, rt.cfg.ConfidentMargin)
	res.Candidates = make([]Candidate, 0, len(ranked))
	for _, c := range ranked {
		res.Candidates = append(res.Candidates, Candidate{
			CapabilityID: c.id, Decision: visible[c.id], Score: c.score, Signals: c.signals,
		})
	}
	return res, nil
}

// truncateRunes returns the first n runes of s without copying the rest.
func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// rank orders candidates: higher score first; on a tie the goal first (the
// continuation is preferred); then capability ID, so the order is deterministic.
func rank(cands map[string]*cand) []*cand {
	out := make([]*cand, 0, len(cands))
	for _, c := range cands {
		if c.score > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.goal != b.goal {
			return a.goal
		}
		return a.id < b.id
	})
	return out
}

// decisiveFloor is the least score a leader needs to be called decisive: a
// continuation of the active goal, or a term only that capability carries. A
// lone match on a shared term or a title word is weaker than that, and is left
// to the embedder (when there is one) and to the model.
const decisiveFloor = goalBonus

// decisive reports whether the leader is clearly ahead: strong enough, and
// either alone or ahead of the runner-up by at least margin.
func decisive(ranked []*cand, margin float64) bool {
	if len(ranked) == 0 || ranked[0].score < decisiveFloor {
		return false
	}
	return len(ranked) == 1 || ranked[0].score-ranked[1].score >= margin
}

// hit is the best match of a unit for one capability.
type hit struct {
	kind   SignalKind
	key    string
	weight float64
	label  string
	lang   string
}

// matchUnits scores the visible capabilities by terminology and title keywords.
//
// Each matched unit adds weight/df, where df is the number of visible
// capabilities that carry the same unit in the active languages, so a term
// shared by many capabilities separates them less than a term one capability
// owns. The count is over visible capabilities only, so what the caller may not
// see cannot change the order of what it may.
func (rt *Router) matchUnits(ix *index, visible map[string]policy.Decision, langs []string, utt []string, get func(string) *cand) {
	if len(utt) == 0 {
		return
	}
	sources := make([]string, 0, len(langs)+1)
	sources = append(sources, "") // language-neutral evidence first
	sources = append(sources, langs...)

	ids := make([]string, 0, len(visible))
	for id := range visible {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	df := map[string]int{}
	matched := map[string]map[string]hit{}
	for _, id := range ids {
		carried := map[string]bool{}
		for _, lang := range sources {
			for i := range ix.units[lang][id] {
				u := &ix.units[lang][id][i]
				if !carried[u.key] {
					carried[u.key] = true
					df[u.key]++
				}
				best, ok := bestPhrase(u, utt)
				if !ok {
					continue
				}
				m := matched[id]
				if m == nil {
					m = map[string]hit{}
					matched[id] = m
				}
				if prev, seen := m[u.key]; !seen || best.weight > prev.weight {
					m[u.key] = best
				}
			}
		}
	}
	for _, id := range ids {
		m := matched[id]
		if len(m) == 0 {
			continue
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		c := get(id)
		for _, k := range keys {
			h := m[k]
			d := df[k]
			if d < 1 {
				d = 1
			}
			c.add(Signal{
				Kind: h.kind, Key: strings.TrimPrefix(strings.TrimPrefix(h.key, "term:"), "kw:"),
				Label: h.label, Language: h.lang, Weight: h.weight / float64(d),
			})
		}
	}
}

// bestPhrase returns the strongest matching phrase of a unit: a longer phrase
// that matches is more specific than a one-word one, so it carries more weight
// (up to one and a half times the unit's weight).
func bestPhrase(u *unit, utt []string) (hit, bool) {
	var best hit
	found := false
	for _, p := range u.phrases {
		if !p.matches(utt) {
			continue
		}
		boost := 1 + 0.25*float64(len(p.tokens)-1)
		if boost > 1.5 {
			boost = 1.5
		}
		w := u.weight * boost
		if !found || w > best.weight {
			best = hit{kind: u.kind, key: u.key, weight: w, label: p.label, lang: p.lang}
			found = true
		}
	}
	return best, found
}
