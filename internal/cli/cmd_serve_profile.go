package cli

import (
	"log/slog"
	"strings"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/dialogue"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The deployment profile of `alixai serve` (M3-23). The profile is named once,
// in the `profile` section of the configuration (ALIXAI_PROFILE in the
// environment), and that name is resolved with dctx.ProfileByName into the one
// value both halves of the prompt path are built from:
//
//   - the dialogue engine's context budget (dialogue.Config.Profile): the hard
//     prompt cap, the output cap, the shortlist, the utterance and history caps;
//   - the runtime boundary (providers.WithBudget in localSelector): the same
//     prompt and output numbers, from Profile.Budget(), so the provider-side cap
//     and the engine's cap cannot differ.
//
// The numbers are the table of docs/operations/resource-budgets.md section 6.2.
// Before this, serve never selected a profile: the engine ran on its Lite
// default under every deployment, and the boundary on providers.DefaultBudget
// (4096 prompt, 1024 output), which is looser than every profile but the
// biggest.
//
// An unknown name refuses the start, before the store is opened or a port is
// touched. There is no fallback to a guessed profile: a deployment whose profile
// cannot be found has no budget to enforce. The one default is the absence of a
// name: the section is "lite" unless the operator sets it.

func init() { config.Register(serveProfileSectionName, &serveProfileSection) }

// serveProfileSectionName is the configuration section that names the profile.
const serveProfileSectionName = "profile"

// serveProfileNewEngine builds the dialogue engine. wireV1 calls it through this
// variable so a test can read the Deps (the profile and the provider selector)
// serve really hands the engine, which the engine itself does not expose.
var serveProfileNewEngine = dialogue.New

// serveProfileUnknownReason is the fixed reason an unknown profile name refuses
// the start. It never echoes the name it was given.
const serveProfileUnknownReason = "the deployment profile is not known"

// serveProfileName is the `profile` section: a profile name from dctx.ProfileNames.
type serveProfileName string

// serveProfileSection is the registered section; its initial value is the
// default the registry resets to on every load.
var serveProfileSection = serveProfileName(dctx.ProfileLite)

// Validate implements the section validation hook: `alixai config check` and
// the load of `alixai serve` refuse an unknown name.
func (n serveProfileName) Validate() error {
	_, err := serveProfileResolve(string(n))
	return err
}

// serveProfileResolve returns the context profile of a profile name. An empty
// name is the Lite default (a section that was never set); any other name must
// be one dctx knows, or the error is a validation_failed with a fixed reason.
func serveProfileResolve(name string) (dctx.Profile, error) {
	if name == "" {
		return dctx.Lite(), nil
	}
	p, err := dctx.ProfileByName(name)
	if err != nil {
		return dctx.Profile{}, contracts.Newf(contracts.ErrValidationFailed, "%s (want one of %s)",
			serveProfileUnknownReason, strings.Join(dctx.ProfileNames(), ", "))
	}
	return p, nil
}

// serveProfileOrLite is the profile wireV1 builds from: the zero value (a caller
// that selected none) is Lite, as the engine's own default is. It is never passed
// on as zero, because a zero Profile.Budget() would refuse every call.
func serveProfileOrLite(p dctx.Profile) dctx.Profile {
	if p == (dctx.Profile{}) {
		return dctx.Lite()
	}
	return p
}

// serveProfileLog says which profile the engine and the runtime boundary run on.
// The keys avoid the substring "token": the log redactor would hide the numbers.
func serveProfileLog(log *slog.Logger, p dctx.Profile) {
	log.Info("deployment profile",
		slog.String("profile", p.Name),
		slog.Int("max_prompt_toks", p.MaxPromptTokens),
		slog.Int("max_out_toks", p.MaxOutputTokens),
		slog.Int("max_shortlist", p.MaxShortlist))
}
