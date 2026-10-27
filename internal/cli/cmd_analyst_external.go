//go:build !noexternal

package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/analyst/web"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// External-mode seams for ADR 0047. Untagged cmd_analyst.go must not import
// credentials / dataegress / providerbudget so -tags noexternal stays free.

func init() {
	analystWebQueryAllows = analystWebQueryAllowsDisclose
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
}

// serveExternalEgress / serveExternalControllerProfile are filled by extWireProvide.
var (
	serveExternalEgress            = func() dataegress.Guard { return nil }
	serveExternalControllerProfile = func(context.Context, contracts.Scope) (pv.ProviderProfile, bool) {
		return pv.ProviderProfile{}, false
	}
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
	// Providerbudget accounting for the actual send is owned by modes on Chat.
	// Holding a second reservation here double-counts and trips budget_exhausted
	// before the step reaches the provider. Probe egress readiness only.
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
	// modes settles the Chat reservation; loop-side ids are anb_* probes only.
	return nil
}

// analystWebQueryAllowsDisclose uses Disclose (records nothing) so a probe does
// not look like a real provider send in the egress audit log.
func analystWebQueryAllowsDisclose(ctx context.Context, scope contracts.Scope) bool {
	ext := serveExternalEgress()
	if ext == nil {
		return false
	}
	prof, ok := serveExternalControllerProfile(ctx, scope)
	if !ok {
		return false
	}
	type discloser interface {
		Disclose(context.Context, contracts.Scope, pv.ProviderProfile, dataegress.DisclosureInput) (pv.EgressDisclosure, error)
	}
	d, ok := ext.(discloser)
	if !ok {
		return false
	}
	disc, err := d.Disclose(ctx, scope, prof, dataegress.DisclosureInput{Role: pv.RoleConversationalController})
	if err != nil || !disc.ExternalEnabled {
		return false
	}
	for _, c := range disc.DataClasses {
		if c == pv.EgressEligibleClassInternal || c == pv.EgressEligibleClassConfidential {
			return true
		}
	}
	return false
}
