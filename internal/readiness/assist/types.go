package assist

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/readiness/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// AssistClassLocalOnly is the class a project has until the operator names another one. Content of
// this class never leaves the machine.
const AssistClassLocalOnly = "local_only"

// AssistModel is the model the composition root chose, with what the engine needs to know about it.
type AssistModel struct {
	// Name identifies the provider in the guard request and in errors ("local", a provider id).
	Name string
	// ModelID is what the report calls the model: no spaces, at most 128 bytes.
	ModelID string
	// Local is true for the operator's own runtime. Anything else is external.
	Local bool
	// Qualified is true only when the model registry shows a qualified status for this use. The zero
	// value, and every uncertified model such as the Lite controller, is labelled
	// "unqualified, advisory".
	Qualified bool
	// Gen is the text generator. Wrap it with providers.WithBudget before passing it in.
	Gen providers.TextGenerator
}

// AssistEgressItem is one message of a request as the guard sees it, with the metadata the
// guard decides on (the shape of dialogue.ContextItem).
type AssistEgressItem struct {
	Role    providers.MessageRole
	Trusted bool
	Content string
	// Kind is "frame" for text AlixAI wrote (template, requirement list) and "excerpt" for project
	// content.
	Kind string
	// SourceID names the file an excerpt came from; empty for a frame.
	SourceID string
	// Class is the data class of the content. A frame is "public"; an excerpt has the project's class.
	Class string
	// LocalOnly marks content that must never leave the machine.
	LocalOnly bool
}

// AssistEgressRequest is what the guard is asked about, before anything is sent.
type AssistEgressRequest struct {
	Scope        contracts.Scope
	Provider     string
	ModelID      string
	ProjectClass string
	Items        []AssistEgressItem
}

// AssistEgressGate is the optional guard the CLI composition root builds from the X data-egress
// guard (internal/dataegress). It returns the items that may be sent, possibly filtered, or an
// error, which is a denial: nothing is sent and no other provider is tried. The engine never
// sends anything but what the gate returned.
type AssistEgressGate interface {
	Release(ctx context.Context, req AssistEgressRequest) ([]AssistEgressItem, error)
}

// AssistBudget bounds one scan's use of the model. A zero field takes its default.
type AssistBudget struct {
	// MaxCalls is the most model calls one scan makes.
	MaxCalls int
	// MaxFiles is the most files excerpted.
	MaxFiles int
	// MaxFileBytes is the per-file excerpt cap in bytes of text shown.
	MaxFileBytes int
	// MaxLinesPerFile is the per-file excerpt cap in lines.
	MaxLinesPerFile int
	// MaxPromptToks caps the estimated size of one prompt.
	MaxPromptToks int
	// MaxOutputToks caps what the model may generate per call.
	MaxOutputToks int
	// MaxTotalToks caps the estimated input plus output of all calls of one scan.
	MaxTotalToks int
	// MaxProposals caps the proposals taken from all replies; the rest are dropped and counted.
	MaxProposals int
}

// Defaults and hard ceilings of AssistBudget.
const (
	assistDefMaxCalls      = 4
	assistDefMaxFiles      = 24
	assistDefMaxFileBytes  = 6000
	assistDefMaxLines      = 150
	assistDefMaxPromptToks = 6000
	assistDefMaxOutputToks = 1200
	assistDefMaxTotalToks  = 30000
	assistDefMaxProposals  = 100

	assistCeilCalls     = 16
	assistCeilFiles     = 200
	assistCeilProposals = 400
)

func (b AssistBudget) assistWithDefaults() AssistBudget {
	def := func(v *int, d, ceil int) {
		if *v <= 0 {
			*v = d
		}
		if ceil > 0 && *v > ceil {
			*v = ceil
		}
	}
	def(&b.MaxCalls, assistDefMaxCalls, assistCeilCalls)
	def(&b.MaxFiles, assistDefMaxFiles, assistCeilFiles)
	def(&b.MaxFileBytes, assistDefMaxFileBytes, 0)
	def(&b.MaxLinesPerFile, assistDefMaxLines, 0)
	def(&b.MaxPromptToks, assistDefMaxPromptToks, 0)
	def(&b.MaxOutputToks, assistDefMaxOutputToks, 0)
	def(&b.MaxTotalToks, assistDefMaxTotalToks, 0)
	def(&b.MaxProposals, assistDefMaxProposals, assistCeilProposals)
	return b
}

// AssistConfig is one scan's model setup.
type AssistConfig struct {
	// Model is nil when no model is configured: the deterministic leg runs alone.
	Model *AssistModel
	// Gate is the optional data-egress guard. An external model needs it.
	Gate AssistEgressGate
	// AllowExternal is the operator's switch for a model that is not local. It is off by default.
	AllowExternal bool
	// ProjectClass is the data class of the project's content: local_only (also the meaning of
	// empty and of any unknown value), public, internal, confidential or restricted. An external
	// model is refused while it is local_only.
	ProjectClass string
	// Profile is an optional model profile hint passed to the generator.
	Profile string
	Budget  AssistBudget
}

// AssistOutcome is what the assist leg produced. It never decides a status: Proposals go to
// report.RdyBuild through Apply.
type AssistOutcome struct {
	// Leg is recorded in the report. Its Status says whether a model ran and why not.
	Leg report.RdyModelLeg
	// Model is set exactly when the leg ran.
	Model *report.RdyModel
	// Proposals are the findings the model proposed that passed this package's checks. The
	// verifier decides each one.
	Proposals []verify.RdyProposal
	// Cause is why the leg did not run (a typed error, permission_denied for a refusal), or nil.
	Cause error
}

// Apply fills the model fields of a report build input. It leaves everything else alone.
func (o *AssistOutcome) Apply(in *report.RdyBuildInput) {
	in.Model = o.Model
	in.ModelLeg = &o.Leg
	in.Proposals = o.Proposals
}

// assistProjectClass normalises the class: empty and unknown mean local_only.
func assistProjectClass(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "public", "internal", "confidential", "restricted":
		return strings.ToLower(strings.TrimSpace(c))
	}
	return AssistClassLocalOnly
}

func assistValidModelID(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id) && !strings.ContainsAny(id, " \t\r\n*")
}

func (m *AssistModel) assistRuntime() readiness.ReportModelRuntime {
	if m.Local {
		return readiness.ReportModelRuntimeLocal
	}
	return readiness.ReportModelRuntimeExternal
}

func (m *AssistModel) assistValidate() error {
	switch {
	case m == nil || m.Gen == nil:
		return errors.New("readiness assist: the model has no generator")
	case !assistValidModelID(m.ModelID):
		return errors.New("readiness assist: the model id is empty or malformed")
	}
	return nil
}
