package verify

import (
	"strings"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Source says where the text of a verified response came from.
type Source string

const (
	// SourceModel: the model's own phrasing, and it passed Verify.
	SourceModel Source = "model"
	// SourceTemplate: the deterministic template for the directive.
	SourceTemplate Source = "template"
)

// Reason says why the response has the source it has.
type Reason string

const (
	// ReasonVerified: the model's text passed Verify.
	ReasonVerified Reason = "verified"
	// ReasonVerificationFailed: the model's text failed Verify; the template replaced it.
	ReasonVerificationFailed Reason = "verification_failed"
	// ReasonDegraded: no model text was produced (the model is unavailable, or the deployment
	// has none); the template is the response.
	ReasonDegraded Reason = "degraded"
)

// Outcome is a response that is safe to show or to speak: either the model's text, verified, or
// the template. Text is never unverified model output. Blocks, when set, are validated rich blocks
// (tables, charts) whose every number is an authoritative fact of the directive.
type Outcome struct {
	Text   string
	Source Source
	Reason Reason
	// Violations are why the model's text was rejected; empty unless Reason is
	// ReasonVerificationFailed.
	Violations []Violation
	// Blocks are verified rich blocks of the reply (tables and charts from a model or from the
	// deterministic template). Empty when the reply is text only.
	Blocks []conversation.RichBlock
}

// Realise is the last step of a turn (master plan 11.1, "verified natural response"). It verifies
// the model's text against the directive and receipt and returns it when it passes; otherwise it
// returns the deterministic template for the same directive, so the user always gets the correct
// facts, if in plainer words. Text-to-speech starts only from an Outcome (master plan 13).
//
// An error means the directive or receipt cannot be used (see Verify), or the template itself
// failed verification, which would be a bug in the templates; in both cases nothing is returned
// to say, and the caller reports a typed failure instead.
func Realise(modelText string, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (Outcome, error) {
	return RealiseRich(modelText, nil, d, r, opts)
}

// RealiseRich is Realise with optional rich blocks the model (or provider) proposed. Every block
// is validated against the directive's facts (numbers in table cells and chart data included); a
// refused block, or text that fails Verify, yields the deterministic template instead.
func RealiseRich(modelText string, proposed []conversation.RichBlock, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (Outcome, error) {
	v, err := prepare(d, r, opts)
	if err != nil {
		return Outcome{}, err
	}
	facts := blocks.NewFactSet(nil)
	if d != nil {
		facts.AddFacts(d.Facts)
	}
	if r != nil {
		facts.AddFacts(r.Facts)
	}
	var accepted []conversation.RichBlock
	for _, b := range proposed {
		clean, err := blocks.Default().Block(b, blocks.Context{Facts: facts})
		if err != nil {
			out, terr := v.template(ReasonVerificationFailed)
			out.Violations = append(out.Violations, Violation{Kind: ViolationUnknownNumber, Got: "block"})
			return out, terr
		}
		accepted = append(accepted, clean)
	}
	if res := v.check(modelText); res.OK {
		return Outcome{Text: strings.TrimSpace(modelText), Source: SourceModel, Reason: ReasonVerified, Blocks: accepted}, nil
	} else {
		out, err := v.template(ReasonVerificationFailed)
		out.Violations = res.Violations
		return out, err
	}
}

// Degraded returns the deterministic template for a directive without consulting a model: the
// response in degraded mode (the model is unavailable or not installed) and the fallback of
// Realise.
func Degraded(d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (Outcome, error) {
	v, err := prepare(d, r, opts)
	if err != nil {
		return Outcome{}, err
	}
	return v.template(ReasonDegraded)
}

// template renders the directive and checks the rendering with the same verifier. A template that
// does not verify is a bug; it is reported, not shown. When opts carry record rows (or a chart
// presentation hint), validated table/chart blocks are attached.
func (v *verification) template(reason Reason) (Outcome, error) {
	text := v.render()
	// An outcome with no receipt is rendered as unconfirmed, which is true. Only a model's text
	// cannot be trusted without the receipt, so that one violation is not held against a template.
	var bad []Violation
	for _, x := range v.check(text).Violations {
		if x.Kind != ViolationReceiptMissing {
			bad = append(bad, x)
		}
	}
	if len(bad) > 0 {
		return Outcome{}, failure(contracts.ErrInternal, ErrTemplateUnverified, "the %s template for act %q failed verification: %s",
			v.lang, string(v.d.Act), violationSummary(bad))
	}
	return Outcome{Text: text, Source: SourceTemplate, Reason: reason, Blocks: presentBlocks(v.d, v.opts)}, nil
}

func violationSummary(vs []Violation) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.String()
	}
	return strings.Join(parts, "; ")
}
