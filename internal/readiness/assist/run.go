package assist

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/internal/readiness/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const assistCallTimeout = 2 * time.Minute

// assistRules are the engine's own checks. They are fields only so a test can remove one and
// prove the tests notice (the house mutation pattern). Production code never replaces them.
type assistRules struct {
	neutralize   func(string) string
	flag         func(line string, hidden bool) bool
	skipSecret   func(path, kind string, secret map[string]bool) bool
	externalOK   func(cfg AssistConfig) error
	plausible    func(cat readiness.Category, quotes []string) bool
	ruleFinal    func(st readiness.Status) bool
	allowedClaim func(status string) bool
	cleanSpan    func(v *assistView, start, end int64) bool
	// redactedLine says whether a line that held a secret-looking value is withheld whole. The
	// redactor replaces the value it recognises, but a shape it half-matches can leave the rest of
	// the value in place, so no line that needed redacting is shown.
	redactedLine func(changed bool) bool
}

func assistDefaultRules() assistRules {
	return assistRules{
		neutralize: assistNeutralize,
		flag:       assistFlagLine,
		skipSecret: func(p, kind string, secret map[string]bool) bool { return kind == assistKindSecret || secret[p] },
		externalOK: assistExternalCheck,
		plausible:  assistPlausible,
		ruleFinal: func(st readiness.Status) bool {
			return st == readiness.StatusFound || st == readiness.StatusPartial || st == readiness.StatusContradicted
		},
		allowedClaim: func(s string) bool { return s == "found" || s == "partial" },
		redactedLine: func(changed bool) bool { return changed },
		cleanSpan: func(v *assistView, start, end int64) bool {
			for l := start; l <= end; l++ {
				if v.Flagged[l-1] {
					return false
				}
			}
			return true
		},
	}
}

func assistDenied(msg string) error {
	return contracts.New(contracts.ErrPermissionDenied, "readiness assist: "+msg)
}

// assistExternalCheck is the engine's own refusal of a model that is not local. It runs before a
// single project file is read. The data-egress guard still has the last word after it.
func assistExternalCheck(cfg AssistConfig) error {
	if cfg.Model.Local {
		return nil
	}
	switch {
	case !cfg.AllowExternal:
		return assistDenied("an external model is off; the operator has not enabled external mode")
	case cfg.Gate == nil:
		return assistDenied("an external model needs the data-egress guard and none was supplied")
	case assistProjectClass(cfg.ProjectClass) == AssistClassLocalOnly:
		return assistDenied("the project is classified local_only, so its content may not leave the machine")
	}
	return nil
}

type assistEngine struct {
	cfg     AssistConfig
	cat     *catalog.Catalog
	rules   assistRules
	files   map[string]scan.ScanFile
	secret  map[string]bool
	decided map[readiness.RequirementID]readiness.Status
}

func (e *assistEngine) liftable(req readiness.Requirement) bool {
	return slices.Contains(req.SatisfiedBy, readiness.SatisfiedByModelVerified)
}

// shaOf is the hash the scan recorded for a path, or an all-zero hash for a path the scan does not
// know, which the verifier then refuses as not scanned.
func (e *assistEngine) shaOf(p string) string {
	if f, ok := e.files[p]; ok && f.SHA256 != "" {
		return f.SHA256
	}
	return strings.Repeat("0", 64)
}

// AssistRun runs the model leg for one scan: it picks bounded excerpts, asks the model, checks
// every proposal and returns what survived. It never returns an error for a model that is missing,
// unavailable, failing or refused; the outcome says so and the deterministic leg stands alone.
// An error means the call itself was wrong (no scan result, a bad model id) or ctx ended.
func AssistRun(ctx context.Context, scope contracts.Scope, root string, res *scan.ScanResult, cfg AssistConfig) (*AssistOutcome, error) {
	return assistRun(ctx, scope, root, res, cfg, assistDefaultRules(), nil)
}

func assistRun(ctx context.Context, scope contracts.Scope, root string, res *scan.ScanResult, cfg AssistConfig, rules assistRules, fsys assistFS) (*AssistOutcome, error) {
	if res == nil {
		return nil, errors.New("readiness assist: no scan result")
	}
	cat, err := catalog.CatalogLoad()
	if err != nil {
		return nil, fmt.Errorf("readiness assist: %w", err)
	}
	if res.CatalogSHA256 != cat.SHA256() {
		return nil, errors.New("readiness assist: the scan was made with a different catalog")
	}
	cfg.Budget = cfg.Budget.assistWithDefaults()
	digest := AssistPromptDigest()
	out := &AssistOutcome{}
	leg := func(st readiness.ReportModelLegStatus, calls int) {
		out.Leg = report.RdyModelLeg{Status: st, Calls: calls}
		if st != readiness.ReportModelLegStatusNotConfigured {
			out.Leg.PromptTemplateSHA256 = digest
		}
	}

	if cfg.Model == nil || cfg.Model.Gen == nil {
		leg(readiness.ReportModelLegStatusNotConfigured, 0)
		return out, nil
	}
	if err := cfg.Model.assistValidate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The external check comes first: a refused model costs no file read and no model call.
	if err := rules.externalOK(cfg); err != nil {
		leg(readiness.ReportModelLegStatusRefused, 0)
		out.Cause = err
		return out, nil
	}

	e := &assistEngine{
		cfg: cfg, cat: cat, rules: rules,
		files: map[string]scan.ScanFile{}, secret: map[string]bool{}, decided: map[readiness.RequirementID]readiness.Status{},
	}
	for _, f := range res.Files {
		e.files[f.Path] = f
	}
	for _, p := range res.SecretFiles {
		e.secret[p] = true
	}
	for _, f := range append(append([]readiness.Finding(nil), res.Findings...), report.RdyConflicts(res)...) {
		if cur, ok := e.decided[f.RequirementID]; !ok || !rules.ruleFinal(cur) {
			e.decided[f.RequirementID] = f.Status
		}
	}
	targets := e.targets()

	var views []*assistView
	if len(targets) > 0 {
		if fsys == nil {
			if fsys, err = assistOpenRoot(root); err != nil {
				return nil, err
			}
		}
		defer fsys.Close()
		views = e.assistSelect(fsys, res)
	}
	batches, exhausted := e.batches(views, targets)

	var (
		tally     assistTally
		proposals []verify.RdyProposal
		used      int
		calls     int
	)
	m := cfg.Model
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items := e.items(batch, targets)
		req := e.chatRequest(items)
		est := providers.Budget{}.EstimateChat(req) + cfg.Budget.MaxOutputToks
		if used+est > cfg.Budget.MaxTotalToks {
			exhausted = true
			break
		}
		if cfg.Gate != nil {
			released, gerr := cfg.Gate.Release(ctx, AssistEgressRequest{
				Scope: scope, Provider: m.Name, ModelID: m.ModelID, ProjectClass: assistProjectClass(cfg.ProjectClass), Items: items,
			})
			if gerr != nil || len(released) == 0 {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				leg(readiness.ReportModelLegStatusRefused, calls)
				out.Cause = assistRefusal(gerr)
				return out, nil
			}
			req = e.chatRequest(e.reseal(released, items))
		}
		calls++
		callCtx, cancel := context.WithTimeout(ctx, assistCallTimeout)
		cr, cerr := m.Gen.Chat(callCtx, scope, req)
		cancel()
		if cerr == nil && cr.FinishReason == providers.FinishLength {
			cerr = contracts.New(contracts.ErrSchemaInvalid, "readiness assist: the reply was cut off at the output cap")
		}
		var rep *assistReply
		if cerr == nil {
			var perr error
			if rep, perr = assistParseReply(cr.Text); perr != nil {
				cerr = contracts.Wrap(contracts.ErrSchemaInvalid, perr, "readiness assist: the reply cannot be used")
			}
		}
		if cerr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// All or nothing: whatever an earlier call produced is discarded with the rest.
			leg(assistStatusOf(cerr), calls)
			out.Cause = cerr
			return out, nil
		}
		if u := cr.Usage.Total(); u > 0 {
			used += u
		} else {
			used += est
		}
		shown := map[string]*assistView{}
		for _, v := range batch {
			shown[v.Path] = v
		}
		proposals = append(proposals, e.assistClassify(rep, shown, len(proposals), &tally)...)
	}

	leg(readiness.ReportModelLegStatusRan, calls)
	out.Leg.Received, out.Leg.Dropped, out.Leg.AttemptedOverrides = tally.received, tally.dropped, tally.overrides
	out.Leg.BudgetExhausted = exhausted
	out.Model = &report.RdyModel{ModelID: m.ModelID, Runtime: m.assistRuntime(), Qualified: m.Qualified}
	out.Proposals = proposals
	return out, nil
}

// assistRefusal makes a guard failure a permission_denied, whatever the guard returned. A guard
// failure is never a reason to try another provider.
func assistRefusal(err error) error {
	if err != nil && contracts.HasCode(err, contracts.ErrPermissionDenied) {
		return err
	}
	if err == nil {
		return assistDenied("the data-egress guard released nothing")
	}
	return contracts.Wrap(contracts.ErrPermissionDenied, err, "readiness assist: the data-egress guard refused the request")
}

// assistStatusOf maps a failed call to the leg status the report shows.
func assistStatusOf(err error) readiness.ReportModelLegStatus {
	switch {
	case contracts.HasCode(err, contracts.ErrPermissionDenied):
		return readiness.ReportModelLegStatusRefused
	case contracts.HasCode(err, contracts.ErrUnavailable), contracts.HasCode(err, contracts.ErrTimeout),
		contracts.HasCode(err, contracts.ErrResourceExhausted), contracts.HasCode(err, contracts.ErrRateLimited),
		contracts.HasCode(err, contracts.ErrDegraded), errors.Is(err, context.DeadlineExceeded):
		return readiness.ReportModelLegStatusUnavailable
	}
	return readiness.ReportModelLegStatusFailed
}

// assistTarget is a requirement the model may propose a finding for.
type assistTarget struct {
	ID       string
	Category string
	Title    string
	Counts   string
}

// targets lists the requirements a verified model finding could still change: no rule has decided
// them as found, partial or contradicted, and the catalog lets a model_verified finding satisfy
// them. A requirement a rule decided is not offered, so the model is not even shown it.
func (e *assistEngine) targets() []assistTarget {
	var out []assistTarget
	for _, r := range e.cat.Requirements {
		if !e.liftable(r) {
			continue
		}
		if st, ok := e.decided[r.ID]; ok && e.rules.ruleFinal(st) {
			continue
		}
		counts := strings.Join(strings.Fields(string(r.HowSatisfied.En)), " ")
		if rs := []rune(counts); len(rs) > 220 {
			counts = string(rs[:220])
		}
		out = append(out, assistTarget{ID: string(r.ID), Category: string(r.Category), Title: string(r.Title.En), Counts: counts})
	}
	return out
}

func assistTargetsText(targets []assistTarget) string {
	var b strings.Builder
	b.WriteString(assistTargetsIntro)
	for _, t := range targets {
		fmt.Fprintf(&b, "\n%s | %s | %s | %s", t.ID, t.Category, t.Title, t.Counts)
	}
	return b.String()
}

// batches groups the views into prompts under the per-prompt cap and the call cap. exhausted says
// some selected file was left out for budget.
func (e *assistEngine) batches(views []*assistView, targets []assistTarget) ([][]*assistView, bool) {
	if len(views) == 0 || len(targets) == 0 {
		return nil, false
	}
	est := func(s string) int { return providers.EstimateTokens(s) + 8 }
	fixed := est(assistTemplate) + est(assistTargetsText(targets)) + est(assistFinalNote) + est(assistSchema)
	var (
		out       [][]*assistView
		cur       []*assistView
		curToks   = fixed
		exhausted bool
	)
	for _, v := range views {
		t := est(assistBlock(v))
		if len(cur) > 0 && curToks+t > e.cfg.Budget.MaxPromptToks {
			out = append(out, cur)
			cur, curToks = nil, fixed
		}
		if len(out) >= e.cfg.Budget.MaxCalls {
			exhausted = true
			cur = nil
			break
		}
		cur = append(cur, v)
		curToks += t
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, exhausted
}

// items builds the messages of one call with the metadata the guard decides on. The frames are
// written by AlixAI and trusted; every file block is untrusted project content.
func (e *assistEngine) items(batch []*assistView, targets []assistTarget) []AssistEgressItem {
	class := assistProjectClass(e.cfg.ProjectClass)
	frame := func(role providers.MessageRole, content string) AssistEgressItem {
		return AssistEgressItem{Role: role, Trusted: true, Content: content, Kind: "frame", Class: "public"}
	}
	items := []AssistEgressItem{
		frame(providers.MsgSystem, assistTemplate),
		frame(providers.MsgUser, assistTargetsText(targets)),
	}
	for _, v := range batch {
		items = append(items, AssistEgressItem{
			Role: providers.MsgUser, Trusted: false, Content: assistBlock(v), Kind: "excerpt",
			SourceID: v.Path, Class: class, LocalOnly: class == AssistClassLocalOnly,
		})
	}
	return append(items, frame(providers.MsgUser, assistFinalNote))
}

// reseal rebuilds the items a guard released. Only text that is one of our own frames, byte for
// byte, keeps the trusted mark; everything else the guard returned is untrusted, and no role but
// system and user is accepted.
func (e *assistEngine) reseal(released, original []AssistEgressItem) []AssistEgressItem {
	frames := map[string]providers.MessageRole{}
	for _, it := range original {
		if it.Kind == "frame" {
			frames[it.Content] = it.Role
		}
	}
	out := make([]AssistEgressItem, 0, len(released))
	for _, it := range released {
		if role, ok := frames[it.Content]; ok && role == it.Role {
			it.Trusted, it.Kind = true, "frame"
		} else {
			if it.Role != providers.MsgUser {
				it.Role = providers.MsgUser
			}
			it.Trusted, it.Kind = false, "excerpt"
		}
		out = append(out, it)
	}
	return out
}

// chatRequest is the request that is sent: plain chat, no tools (the type has no place for one),
// a closed output schema, temperature zero and a bounded output.
func (e *assistEngine) chatRequest(items []AssistEgressItem) providers.ChatRequest {
	msgs := make([]providers.Message, len(items))
	for i, it := range items {
		msgs[i] = providers.Message{Role: it.Role, Content: it.Content, Trusted: it.Trusted}
	}
	return providers.ChatRequest{
		Role:        providers.RoleController,
		Profile:     e.cfg.Profile,
		Messages:    msgs,
		Output:      providers.SchemaConstraint([]byte(assistSchema)),
		MaxTokens:   e.cfg.Budget.MaxOutputToks,
		Temperature: 0,
	}
}
