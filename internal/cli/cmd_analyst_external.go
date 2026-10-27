//go:build !noexternal

package cli

import (
	"context"
	"sync/atomic"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/analyst/web"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// External-mode seams for ADR 0047 / 0050. Untagged cmd_analyst.go must not import
// credentials / dataegress / providerbudget so -tags noexternal stays free.

func init() {
	analystWebQueryAllows = analystWebQueryAllowsEgress
	analystWebQueryProviderOK = analystWebQueryAllowsProvider
	analystNativeProfileOK = analystNativeProfileHasSearch
	newAnalystFetcher = func() *web.Fetcher {
		return web.NewFetcher(nil, nil, ids.NewGenerator(nil))
	}
	analystCapableLive = func(ctx context.Context, scope contracts.Scope) bool {
		prof, ok := serveExternalControllerProfile(ctx, scope)
		if !ok || prof.AnalystCapable == nil {
			return false
		}
		return *prof.AnalystCapable
	}
	analystNativeCaller = nativeCallerFunc(analystNativeCallerLive)
}

// serveExternalEgress / serveExternalControllerProfile are filled by extWireProvide.
var (
	serveExternalEgress            = func() dataegress.Guard { return nil }
	serveExternalControllerProfile = func(context.Context, contracts.Scope) (pv.ProviderProfile, bool) {
		return pv.ProviderProfile{}, false
	}
	serveExternalSender atomic.Pointer[extWireSender]
)

type analystCredSecrets struct {
	r *credentials.Resolver
}

func (s analystCredSecrets) Resolve(ctx context.Context, ref string) (string, error) {
	if s.r == nil {
		return "", contracts.New(contracts.ErrUnavailable, "analyst: credentials resolver is not wired for web search")
	}
	sec, err := s.r.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return string(sec.Reveal()), nil
}

// bindAnalystWebSecrets stores a credentials Resolver for dialogue.analyst.web.cred_ref.
func bindAnalystWebSecrets(r *credentials.Resolver) {
	analystWire.mu.Lock()
	defer analystWire.mu.Unlock()
	if r == nil {
		analystWire.secrets = nil
		return
	}
	analystWire.secrets = analystCredSecrets{r: r}
}

type modesBudgets interface {
	Reserve(ctx context.Context, scope contracts.Scope, call providerbudget.Call) (providerbudget.Reservation, error)
	Settle(ctx context.Context, scope contracts.Scope, id pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error)
}

var analystBudgets modesBudgets

// bindAnalystBudgets wires providerbudget into the analyst loop Budget adapter.
func bindAnalystBudgets(b modesBudgets) {
	analystWire.mu.Lock()
	analystBudgets = b
	analystWire.mu.Unlock()
	if b == nil {
		analystExternalReserve = nil
		analystExternalSettle = nil
		return
	}
	analystExternalReserve = analystReserveAgainstBudget
	analystExternalSettle = analystSettleAgainstBudget
}

func analystReserveAgainstBudget(ctx context.Context, scope contracts.Scope, estToks int) (loop.BudgetReservation, error) {
	_ = estToks
	prof, ok := serveExternalControllerProfile(ctx, scope)
	if !ok {
		return loop.BudgetReservation{ID: ids.New("anb")}, nil
	}
	guard := serveExternalEgress()
	if guard == nil {
		return loop.BudgetReservation{ID: ids.New("anb")}, nil
	}
	res, err := guard.Evaluate(ctx, dataegress.Candidate{
		Scope: scope, Profile: prof, Role: pv.RoleConversationalController,
		Channel: pv.ChannelInteractive, TraceID: ids.New("trc"),
		Instructions: "Analyst step budget reservation.",
		Messages: []dataegress.Message{{
			Role: pv.RequestEnvelopeMessageRoleUser,
			Parts: []dataegress.Part{{
				Kind: dataegress.PartText, Text: "step", Category: string(pv.SendCategoryUserMessage),
				Class: string(pv.DataClassificationInternal),
			}},
		}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  512, TimeoutMS: 1000,
	})
	if err != nil || res.Envelope == nil {
		return loop.BudgetReservation{}, contracts.New(contracts.ErrResourceExhausted, "analyst: egress refused the budget reservation")
	}
	return loop.BudgetReservation{ID: ids.New("anb")}, nil
}

func analystSettleAgainstBudget(context.Context, contracts.Scope, loop.BudgetReservation, loop.BudgetActual) error {
	return nil
}

// analystWebQueryAllowsEgress is the ONLY web gate: dataegress.WebQuery for the
// configured search backend (not the disclosure heuristic).
func analystWebQueryAllowsEgress(ctx context.Context, scope contracts.Scope) bool {
	return analystWebQueryAllowsProvider(ctx, scope, analystConfiguredWebProvider())
}

func analystWebQueryAllowsProvider(ctx context.Context, scope contracts.Scope, provider pv.EgressPolicyWebQueryProvider) bool {
	ext := serveExternalEgress()
	if ext == nil {
		return false
	}
	type webQ interface {
		WebQuery(context.Context, contracts.Scope, pv.EgressPolicyWebQueryProvider) (dataegress.WebQueryDecision, error)
	}
	w, ok := ext.(webQ)
	if !ok {
		return false
	}
	d, err := w.WebQuery(ctx, scope, provider)
	return err == nil && d.Allowed
}

func analystNativeProfileHasSearch(ctx context.Context, scope contracts.Scope) bool {
	prof, ok := serveExternalControllerProfile(ctx, scope)
	return ok && prof.NativeWebSearch != nil && *prof.NativeWebSearch
}

type nativeSearchClient interface {
	SearchWeb(ctx context.Context, scope contracts.Scope, req external.NativeSearchRequest) (external.NativeSearchResult, error)
}

func analystNativeCallerLive(ctx context.Context, scope contracts.Scope, query string, allowDomains []string) (web.NativeCallResult, error) {
	prof, ok := serveExternalControllerProfile(ctx, scope)
	if !ok {
		return web.NativeCallResult{}, contracts.New(contracts.ErrWebSearchUnavailable, "web: no controller profile for native search")
	}
	if prof.NativeWebSearch == nil || !*prof.NativeWebSearch {
		return web.NativeCallResult{}, contracts.New(contracts.ErrWebSearchUnavailable, "web: routed profile has native_web_search false")
	}
	guard := serveExternalEgress()
	if guard == nil {
		return web.NativeCallResult{}, contracts.New(contracts.ErrPermissionDenied, "web: data-egress guard is not wired for native search")
	}
	eg, err := guard.Evaluate(ctx, dataegress.Candidate{
		Scope: scope, Profile: prof, Role: pv.RoleConversationalController,
		Channel: pv.ChannelInteractive, TraceID: ids.New("trc"),
		Instructions: web.NativeSearchInstruction,
		Messages: []dataegress.Message{{
			Role: pv.RequestEnvelopeMessageRoleUser,
			Parts: []dataegress.Part{{
				Kind: dataegress.PartText, Text: query, Category: string(pv.SendCategoryWebQuery),
				Class: string(pv.DataClassificationInternal),
			}},
		}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  1024, TimeoutMS: 30000,
	})
	if err != nil || eg.Envelope == nil {
		if err != nil {
			return web.NativeCallResult{}, err
		}
		return web.NativeCallResult{}, contracts.New(contracts.ErrPermissionDenied, "web: egress refused the native search")
	}
	if len(eg.Parts) != 1 || !eg.Parts[0].Kept {
		return web.NativeCallResult{}, contracts.New(contracts.ErrPermissionDenied, "web: egress did not keep the web_query part")
	}
	sendQuery := query
	if eg.Parts[0].Why != "" {
		if q, ok := envelopeTextQuery(eg.Envelope); ok {
			sendQuery = q
		}
	}

	sender := serveExternalSender.Load()
	if sender == nil {
		return web.NativeCallResult{}, contracts.New(contracts.ErrWebSearchUnavailable, "web: native search sender is not wired")
	}
	gen, err := sender.generatorFor(prof)
	if err != nil {
		return web.NativeCallResult{}, err
	}
	sw, ok := gen.(nativeSearchClient)
	if !ok {
		return web.NativeCallResult{}, contracts.New(contracts.ErrWebSearchUnavailable, "web: provider adapter has no SearchWeb")
	}

	units := providerbudget.Units{Requests: 1, ToksIn: 256, ToksOut: 512}
	one := pv.Counter(1)
	units.WebSearches = &one
	var rsv providerbudget.Reservation
	if analystBudgets != nil {
		rsv, err = analystBudgets.Reserve(ctx, scope, providerbudget.Call{
			Provider: pv.ProviderRef{
				ProfileID: prof.ProfileID, ProviderID: prof.ProviderID,
				ModelID: prof.ModelID, ModelVersion: prof.ModelVersion,
			},
			Role:       pv.RoleConversationalController,
			Channel:    pv.ChannelInteractive,
			EnvelopeID: eg.Envelope.EnvelopeID,
			DecisionID: eg.Decision.DecisionID,
			Prices:     prof.Pricing,
			Units:      units,
		})
		if err != nil {
			return web.NativeCallResult{}, err
		}
	}

	res, err := sw.SearchWeb(ctx, scope, external.NativeSearchRequest{
		Query: sendQuery, AllowDomains: allowDomains, MaxUses: 1,
	})
	if analystBudgets != nil && rsv.ID != "" {
		actual := providerbudget.Actual{
			Metering: providerbudget.Units{
				ToksIn: pv.Counter(res.ToksIn), ToksOut: pv.Counter(res.ToksOut), Requests: 1,
			},
		}
		if err != nil {
			kind := pv.ProviderErrorKindTimeout
			actual.Outcome = pv.UsageRecordOutcomeFailed
			actual.ErrorKind = &kind
		} else {
			actual.Outcome = pv.UsageRecordOutcomeSucceeded
			if res.SearchCalls > 0 {
				n := pv.Counter(res.SearchCalls)
				actual.Metering.WebSearches = &n
			}
		}
		_, _ = analystBudgets.Settle(ctx, scope, rsv.ID, actual)
	}
	if err != nil {
		return web.NativeCallResult{}, err
	}
	hits := make([]web.SearchHit, len(res.Hits))
	for i, h := range res.Hits {
		hits[i] = web.SearchHit{URL: h.URL, Title: h.Title, Snippet: h.Snippet}
	}
	return web.NativeCallResult{Hits: hits, SearchCalls: res.SearchCalls, ToksIn: res.ToksIn, ToksOut: res.ToksOut}, nil
}

func envelopeTextQuery(env *pv.RequestEnvelope) (string, bool) {
	if env == nil || len(env.Payload.Messages) == 0 || len(env.Payload.Messages[0].Parts) == 0 {
		return "", false
	}
	part := env.Payload.Messages[0].Parts[0]
	if t, ok := part.Value.(pv.RequestEnvelopeTextPart); ok {
		return t.Text, t.Text != ""
	}
	return "", false
}
