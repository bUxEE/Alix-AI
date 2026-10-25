package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The AG-UI area (internal/gateway/routes_v1_agui.go) serves a run as AG-UI events over the same session store,
// dialogue engine, action service, application service and stream hub as /v1/sessions and the turn-events route, so
// it needs no service of its own to be mounted. What it keeps is the index of its runs: which turn of the hub a run
// is, its whole RunContext as JSON (so a reconnect maps the buffered TurnEvents to the very bytes the run sent) and how
// to cancel a run in flight. That index is built here because the process owns the clock, and it is provided as
// *gateway.AGUIRuns: without it the area would keep a private index on the same clock, so providing it is for the
// operator's benefit, not a precondition. It is bounded (a few hundred runs, each forgotten with its events) and holds
// no secret in its persisted part.
//
// # UI tools (M3-34)
//
// A page may declare UI tools (CopilotKit's frontend actions) in every run. The model is offered one only if the host
// REGISTERED it for the application: dialogue.Deps.UITools, an allow-list per application, where nil means none. This
// file is where the host registers them, in the "agui" section of the configuration:
//
//	agui:
//	  ui_tools:
//	    <application id>: [highlightRow, openPanel]
//
// With no section, or no entry for an application, no tool of that application is ever offered, whatever a page
// declares: none is the default. The names are exact, well-formed and outside the reserved alixai namespace; one that
// names a capability of the application is refused by the engine and by the intake whatever the section says. The
// section is read on every engine turn, through the registered pointer.
//
// The same value reaches two places: the engine (wireV1 builds it through serveProfileNewEngine, which this file wraps
// so that it is built with the allow-list, unless the composition gave it one of its own) and the gateway (provided as
// dialogue.UIToolRegistry, so that GET /v1/agui/capabilities claims client tool calls only for an application that has
// registered a tool). They cannot disagree. A name is not read from the application's package: the package format has
// no member for it, and a package is the application's claim, where this list is the operator's decision.

const aguiSectionName = "agui"

func init() {
	config.Register(aguiSectionName, &aguiSection)
	registerServeProvider("agui", aguiProvide)
	serveProfileNewEngine = aguiEngineWithUITools(serveProfileNewEngine, aguiUITools)
}

// aguiConfig is the "agui" section.
type aguiConfig struct {
	// UITools maps an application id to the names of the UI tools the host registers for it.
	UITools map[string][]string `yaml:"ui_tools"`
}

// aguiSection is the registered section; the zero value registers no tool.
var aguiSection aguiConfig

// Validate refuses a section that registers a name that could never be a UI tool, or a duplicate: a typo in the
// allow-list must stop `serve` from starting, not leave a tool silently unregistered or a reserved name half accepted.
func (c aguiConfig) Validate() error {
	apps := make([]string, 0, len(c.UITools))
	for app := range c.UITools {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		if app == "" {
			return contracts.New(contracts.ErrValidationFailed, "agui.ui_tools: an application id is empty")
		}
		seen := map[string]bool{}
		for _, name := range c.UITools[app] {
			if !agui.UIToolNameAllowed(name, nil) {
				return contracts.New(contracts.ErrValidationFailed, fmt.Sprintf("agui.ui_tools.%s: %q is not a usable UI tool name (letters, digits and _ . : -, at most 64, not starting with alixai)", app, name))
			}
			if seen[name] {
				return contracts.New(contracts.ErrValidationFailed, fmt.Sprintf("agui.ui_tools.%s: %q is registered twice", app, name))
			}
			seen[name] = true
		}
	}
	return nil
}

// aguiUIToolRegistry is the dialogue.UIToolRegistry of the "agui" section: the names registered for an application, and
// nothing for any other.
type aguiUIToolRegistry struct{ section *aguiConfig }

var _ dialogue.UIToolRegistry = aguiUIToolRegistry{}

func (r aguiUIToolRegistry) UITools(_ context.Context, applicationID string) ([]string, error) {
	if r.section == nil {
		return nil, nil
	}
	return slices.Clone(r.section.UITools[applicationID]), nil
}

// aguiUITools is the process's registry, over the registered section.
var aguiUITools dialogue.UIToolRegistry = aguiUIToolRegistry{section: &aguiSection}

// aguiEngineWithUITools wraps an engine builder so that the engine is built with the allow-list. A Deps that already has
// one keeps it: the composition that set it decided.
func aguiEngineWithUITools(next func(dialogue.Deps) (*dialogue.Engine, error), reg dialogue.UIToolRegistry) func(dialogue.Deps) (*dialogue.Engine, error) {
	return func(d dialogue.Deps) (*dialogue.Engine, error) {
		if d.UITools == nil {
			d.UITools = reg
		}
		return next(d)
	}
}

// aguiProvide builds the process's run index on the process clock and gives it, and the UI tool allow-list the engine
// was built with, to the gateway's service registry.
func aguiProvide(_ context.Context, d serveDeps) error {
	if err := gateway.Provide(d.Services, gateway.NewAGUIRuns(d.Clock)); err != nil {
		return err
	}
	return gateway.Provide(d.Services, aguiUITools)
}
