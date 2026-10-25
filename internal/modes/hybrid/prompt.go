package hybrid

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// traceID names the requests of this package in the egress decision record.
const traceID = "trc_hybrid"

// wireDirective is the directive as the model sees it: what the response may state, ask and offer,
// and nothing else. It carries no turn, capability, preparation or receipt id, and no source (a
// source is a separate part so the egress guard can decide on it).
type wireDirective struct {
	Act                  string                                  `json:"act"`
	Language             string                                  `json:"language"`
	Facts                []actions.Fact                          `json:"facts"`
	MustState            []common.FieldName                      `json:"must_state"`
	MissingFields        []common.FieldName                      `json:"missing_fields"`
	InvalidFields        []actions.Issue                         `json:"invalid_fields"`
	NextQuestion         *conversation.DialogueDirectiveQuestion `json:"next_question,omitempty"`
	AllowedSuggestions   []actions.Choice                        `json:"allowed_suggestions"`
	Confirmation         actions.ConfirmationRequirement         `json:"confirmation"`
	PermittedNextActions []actions.PermittedNextAction           `json:"permitted_next_actions"`
	ReasonCode           *common.ErrorCode                       `json:"reason_code,omitempty"`
}

// wireReceipt is the receipt as the model sees it.
type wireReceipt struct {
	Status   actions.ActionReceiptStatus `json:"status"`
	Facts    []actions.Fact              `json:"facts"`
	Errors   []actions.Issue             `json:"errors"`
	FollowUp *actions.FollowUp           `json:"follow_up,omitempty"`
}

func directiveJSON(d *conversation.DialogueDirective, lang verify.Language) (string, error) {
	w := wireDirective{
		Act: string(d.Act), Language: string(lang), Facts: d.Facts, MustState: d.MustState, MissingFields: d.MissingFields,
		InvalidFields: d.InvalidFields, NextQuestion: d.NextQuestion, AllowedSuggestions: d.AllowedSuggestions,
		Confirmation: d.Confirmation, PermittedNextActions: d.PermittedNextActions, ReasonCode: d.ReasonCode,
	}
	b, err := json.Marshal(w)
	return string(b), err
}

func receiptJSON(r *actions.ActionReceipt) (string, error) {
	b, err := json.Marshal(wireReceipt{Status: r.Status, Facts: r.Facts, Errors: r.Errors, FollowUp: r.FollowUp})
	return string(b), err
}

// instructions is the fixed text the core writes for a realisation request. It is the same for every
// turn; nothing a user, a document or a model wrote is part of it.
func instructions(lang verify.Language) string {
	return "You write the reply to the user for one dialogue act, in language \"" + string(lang) + "\". " +
		"Use only the dialogue_directive and action_receipt data you are given. " +
		"State every fact named in must_state exactly as given. " +
		"Do not add any number, date, time, amount or identifier that is not given. " +
		"Say that something was done only when the action_receipt status is committed. " +
		"Offer only the given suggestions and permitted next actions. " +
		"Cite a source only as [n], its position in the source list. " +
		"Reply with the text of the reply alone. Never call a tool."
}

// sourcePart is the part of one cited source: its claims as an excerpt, its name as the label. The
// egress guard decides what of it leaves; Required makes a source that may not leave deny the request.
func sourcePart(i int, c conversation.Citation, class string) dataegress.Part {
	label := fmt.Sprintf("[%d] %s", i+1, c.DisplayName)
	return dataegress.Part{
		Kind: dataegress.PartText, Category: string(pv.SendCategoryRAGExcerpt), Class: class,
		SourceID: string(c.SourceID), Label: label, Text: label + ": " + strings.Join(c.Claims, " "), Required: true,
	}
}

// candidate builds the request as the pipeline sees it before the guard.
func (h *Hybrid) candidate(t Turn, class pv.DataClassification) (dataegress.Candidate, error) {
	cls := string(class)
	dj, err := directiveJSON(t.Directive, t.Options.Language)
	if err != nil {
		return dataegress.Candidate{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "hybrid: the directive cannot be encoded")
	}
	parts := []dataegress.Part{{Kind: dataegress.PartText, Category: string(pv.SendCategoryDialogueDirective), Class: cls, Text: dj}}
	if t.Receipt != nil {
		rj, err := receiptJSON(t.Receipt)
		if err != nil {
			return dataegress.Candidate{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "hybrid: the receipt cannot be encoded")
		}
		parts = append(parts, dataegress.Part{Kind: dataegress.PartText, Category: string(pv.SendCategoryActionReceipt), Class: cls, Text: rj})
	}
	for i, c := range t.Directive.Sources {
		parts = append(parts, sourcePart(i, c, cls))
	}
	return dataegress.Candidate{
		Scope: t.Scope, TraceID: traceID, Instructions: instructions(t.Options.Language),
		Messages:       []dataegress.Message{{Role: pv.RequestEnvelopeMessageRoleUser, Parts: parts}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  int64(h.cfg.MaxOutputToks), TimeoutMS: h.cfg.Budget.Timeout.Milliseconds(),
	}, nil
}

// richOf builds the request for the provider from the candidate text: an upper bound of what the
// guard can release, used only to price the call.
func (h *Hybrid) richOf(c dataegress.Candidate) providers.RichRequest {
	out := providers.RichRequest{
		Role: providers.RoleController, MaxToks: h.cfg.MaxOutputToks,
		Messages: []providers.RichMessage{{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart(c.Instructions)}}},
	}
	user := providers.RichMessage{Role: providers.MsgUser}
	for _, m := range c.Messages {
		for _, p := range m.Parts {
			user.Parts = append(user.Parts, providers.TextPart(p.Text))
		}
	}
	out.Messages = append(out.Messages, user)
	return out
}

// estimator prices the worst case of the call for the profile an attempt would use.
func (h *Hybrid) estimator(c dataegress.Candidate, quote *quote) modes.Estimator {
	return func(p pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
		cu, err := h.cfg.Budget.EstimateRichCost(h.richOf(c), priceTable(p.Pricing))
		if err != nil {
			return providerbudget.Units{}, 0, err
		}
		quote.micros, quote.currency = cu.CostMicros, string(p.Pricing.Currency)
		return meteringOf(cu), pv.MicroUnits(cu.CostMicros), nil
	}
}

type quote struct {
	micros   int64
	currency string
}

func priceOf(p *pv.PriceTablePrice) *providers.Price {
	if p == nil {
		return nil
	}
	return &providers.Price{PerUnits: p.PerUnits, MicroUnits: int64(p.MicroUnits)}
}

func priceTable(t pv.PriceTable) providers.PriceTable {
	p := t.Prices
	return providers.PriceTable{
		Currency: string(t.Currency), ToksIn: priceOf(p.ToksIn), ToksOut: priceOf(p.ToksOut),
		ImagesIn: priceOf(p.ImagesIn), ImagesOut: priceOf(p.ImagesOut), Requests: priceOf(p.Requests),
	}
}

func meteringOf(u providers.CostUsage) providerbudget.Units {
	return providerbudget.Units{
		ToksIn: pv.Counter(u.ToksIn), ToksOut: pv.Counter(u.ToksOut),
		ImagesIn: pv.Counter(u.ImagesIn), ImagesOut: pv.Counter(u.ImagesOut), Requests: 1,
	}
}

// richFromEnvelope builds the request for the provider from the filtered envelope, and only from it.
func richFromEnvelope(env *pv.RequestEnvelope) (providers.RichRequest, error) {
	if env == nil {
		return providers.RichRequest{}, contracts.New(contracts.ErrPermissionDenied, "hybrid: no envelope")
	}
	out := providers.RichRequest{
		Role: providers.RoleController, MaxToks: int(env.Payload.Limits.MaxOutputToks),
		Messages: []providers.RichMessage{{
			Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart(env.Payload.Instructions)},
		}},
	}
	for _, m := range env.Payload.Messages {
		role := providers.MsgUser
		if m.Role == pv.RequestEnvelopeMessageRoleAssistant {
			role = providers.MsgAssistant
		}
		rm := providers.RichMessage{Role: role}
		for _, p := range m.Parts {
			var text string
			switch v := p.Value.(type) {
			case pv.RequestEnvelopeTextPart:
				text = v.Text
			case *pv.RequestEnvelopeTextPart:
				if v == nil {
					return out, contracts.New(contracts.ErrPermissionDenied, "hybrid: empty part")
				}
				text = v.Text
			default:
				return out, contracts.New(contracts.ErrPermissionDenied, "hybrid: media is not realised externally")
			}
			rm.Parts = append(rm.Parts, providers.TextPart(text))
		}
		out.Messages = append(out.Messages, rm)
	}
	return out, nil
}

// callError maps a sender failure to the router's (modes.CallErrorOf): the provider's own text is never copied,
// and a credential refusal that nothing was sent for stays sent=false.
func callError(err error) error { return modes.CallErrorOf(err) }
