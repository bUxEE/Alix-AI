package dctx

import (
	"fmt"
	"sort"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Profile is the context budget of one deployment profile: a hard cap on the estimated
// prompt, a separate cap on what derives from history, and how many of each history
// section a prompt may hold. Every limit must be positive: there is no "unlimited".
//
// The values below are the token caps of docs/operations/resource-budgets.md section 6.2
// (M0-15), which owns them: the document is the source of truth and this table is its
// copy, so a number is changed there first and here second. The Lite row is the one
// proposed by docs/efficiency/M1-14.md (2048 estimated prompt tokens, 256 output tokens, a
// shortlist of 5), which M0-15 confirmed against measured prompts; the others are scaled
// from it on the profile's memory (4 GB, 8 GB, 16 GB, more) so a profile with less memory
// never gets a larger context. The document's basis column says how firm each number is
// (a proposal measured against real prompts, not a certified figure).
type Profile struct {
	// Name is the profile name as the configuration and the audit use it.
	Name string

	// MaxPromptTokens is the HARD cap on the estimated prompt of one request, the output
	// constraint included (providers.Budget.MaxPromptTokens).
	MaxPromptTokens int
	// MaxOutputTokens caps generated tokens (providers.Budget.MaxOutputTokens).
	MaxOutputTokens int
	// MaxShortlist is how many shortlisted capabilities a prompt may describe.
	MaxShortlist int
	// MaxUtteranceRunes is the longest user message accepted. A longer one is rejected,
	// not clipped: cutting what the user wrote changes what they asked.
	MaxUtteranceRunes int
	// MaxConstraintTokens caps the estimated tokens of the output constraint (the
	// "tool schema": the JSON schema or grammar of the proposal), which count against
	// MaxPromptTokens. The constraint grows with the shortlist, so the builder lowers the
	// shortlist until it fits; if even one capability does not fit, the build fails with a
	// budget violation (max_constraint_tokens).
	MaxConstraintTokens int
	// MaxResponsePromptTokens caps the estimated tokens of the response prompt (the rules
	// and the directive's facts of the second model call), which has no history.
	MaxResponsePromptTokens int

	// MaxHistoryTokens caps the estimated tokens of everything derived from history (the
	// replayed turn rows, the capsules and the goals set aside) whatever the total cap is.
	MaxHistoryTokens int
	// CandidateRows is how many rows of the recent window are read for selection. It is
	// the whole cost of history: the transcript is never read.
	CandidateRows int
	// MaxTurnRows is how many selected turn rows a prompt may replay.
	MaxTurnRows int
	// RecentRows is how many of the newest rows are always eligible, relevant or not: the
	// answer to the last question needs its question.
	RecentRows int
	// MaxRowRunes clips each replayed row.
	MaxRowRunes int
	// MaxCapsulesFull is how many of the newest capsules are rendered in full; older ones
	// are folded into one digest line.
	MaxCapsulesFull int
	// MaxDigestEntries bounds the distinct (capability, outcome) entries of the digest.
	MaxDigestEntries int
	// MaxSuspended is how many goals set aside are listed.
	MaxSuspended int
	// MaxSlots is how many collected slots of a goal are listed.
	MaxSlots int
	// MaxValueRunes clips every slot value, capsule fact and choice label.
	MaxValueRunes int
}

// Names of the profiles (the profile names of the configuration).
const (
	ProfileExperimental4G = "experimental-4g"
	ProfileLite           = "lite"
	ProfileBalanced       = "balanced"
	ProfilePerformance    = "performance"
)

// profiles holds the table of docs/operations/resource-budgets.md section 6.2: prompt,
// output, shortlist (the default; the document's hard ceiling is the operator's limit and
// nothing here raises a shortlist), utterance, constraint, response prompt and history caps.
// The row, window and value-length limits are this package's own (M3-03). A function
// returns copies so a caller cannot widen a shared budget.
func profiles() map[string]Profile {
	return map[string]Profile{
		ProfileExperimental4G: {
			Name: ProfileExperimental4G, MaxPromptTokens: 1536, MaxOutputTokens: 192, MaxShortlist: 3, MaxUtteranceRunes: 600,
			MaxConstraintTokens: 380, MaxResponsePromptTokens: 512,
			MaxHistoryTokens: 224, CandidateRows: 16, MaxTurnRows: 4, RecentRows: 2, MaxRowRunes: 160,
			MaxCapsulesFull: 2, MaxDigestEntries: 4, MaxSuspended: 2, MaxSlots: 8, MaxValueRunes: 60,
		},
		ProfileLite: {
			Name: ProfileLite, MaxPromptTokens: 2048, MaxOutputTokens: 256, MaxShortlist: 5, MaxUtteranceRunes: 1000,
			MaxConstraintTokens: 540, MaxResponsePromptTokens: 512,
			MaxHistoryTokens: 384, CandidateRows: 24, MaxTurnRows: 6, RecentRows: 2, MaxRowRunes: 240,
			MaxCapsulesFull: 3, MaxDigestEntries: 6, MaxSuspended: 3, MaxSlots: 12, MaxValueRunes: 80,
		},
		ProfileBalanced: {
			Name: ProfileBalanced, MaxPromptTokens: 4096, MaxOutputTokens: 384, MaxShortlist: 8, MaxUtteranceRunes: 2000,
			MaxConstraintTokens: 540, MaxResponsePromptTokens: 768,
			MaxHistoryTokens: 1024, CandidateRows: 40, MaxTurnRows: 10, RecentRows: 4, MaxRowRunes: 400,
			MaxCapsulesFull: 5, MaxDigestEntries: 8, MaxSuspended: 3, MaxSlots: 16, MaxValueRunes: 120,
		},
		ProfilePerformance: {
			Name: ProfilePerformance, MaxPromptTokens: 8192, MaxOutputTokens: 512, MaxShortlist: 12, MaxUtteranceRunes: 4000,
			MaxConstraintTokens: 540, MaxResponsePromptTokens: 1024,
			MaxHistoryTokens: 2048, CandidateRows: 40, MaxTurnRows: 16, RecentRows: 6, MaxRowRunes: 600,
			MaxCapsulesFull: 8, MaxDigestEntries: 12, MaxSuspended: 3, MaxSlots: 24, MaxValueRunes: 160,
		},
	}
}

// ProfileNames lists the known profile names, sorted.
func ProfileNames() []string {
	m := profiles()
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ProfileByName returns the context budget of a named profile. An unknown or empty name
// is an error, never a default: a deployment whose profile cannot be found has no
// budget to enforce, and guessing one would either starve it or let it overflow.
func ProfileByName(name string) (Profile, error) {
	p, ok := profiles()[name]
	if !ok {
		return Profile{}, contracts.Newf(contracts.ErrValidationFailed, "dctx: %q is not a context profile (want one of %v)", name, ProfileNames())
	}
	return p, nil
}

// Lite returns the Lite profile, the default of the engine.
func Lite() Profile { return profiles()[ProfileLite] }

// Validate rejects a profile with a non-positive limit or incoherent limits.
func (p Profile) Validate() error {
	fields := []struct {
		name string
		v    int
	}{
		{"MaxPromptTokens", p.MaxPromptTokens}, {"MaxOutputTokens", p.MaxOutputTokens}, {"MaxShortlist", p.MaxShortlist},
		{"MaxUtteranceRunes", p.MaxUtteranceRunes}, {"MaxConstraintTokens", p.MaxConstraintTokens},
		{"MaxResponsePromptTokens", p.MaxResponsePromptTokens}, {"MaxHistoryTokens", p.MaxHistoryTokens}, {"CandidateRows", p.CandidateRows},
		{"MaxTurnRows", p.MaxTurnRows}, {"RecentRows", p.RecentRows}, {"MaxRowRunes", p.MaxRowRunes},
		{"MaxCapsulesFull", p.MaxCapsulesFull}, {"MaxDigestEntries", p.MaxDigestEntries}, {"MaxSuspended", p.MaxSuspended},
		{"MaxSlots", p.MaxSlots}, {"MaxValueRunes", p.MaxValueRunes},
	}
	for _, f := range fields {
		if f.v <= 0 {
			return contracts.Newf(contracts.ErrValidationFailed, "dctx: profile %q: %s must be positive", p.Name, f.name)
		}
	}
	switch {
	case p.MaxHistoryTokens >= p.MaxPromptTokens:
		return contracts.Newf(contracts.ErrValidationFailed, "dctx: profile %q: MaxHistoryTokens (%d) must be below MaxPromptTokens (%d)", p.Name, p.MaxHistoryTokens, p.MaxPromptTokens)
	case p.MaxTurnRows > p.CandidateRows:
		return contracts.Newf(contracts.ErrValidationFailed, "dctx: profile %q: MaxTurnRows (%d) is above CandidateRows (%d)", p.Name, p.MaxTurnRows, p.CandidateRows)
	case p.RecentRows > p.MaxTurnRows:
		return contracts.Newf(contracts.ErrValidationFailed, "dctx: profile %q: RecentRows (%d) is above MaxTurnRows (%d)", p.Name, p.RecentRows, p.MaxTurnRows)
	}
	return nil
}

// Budget returns the request budget of the profile, the one the runtime boundary
// (providers.WithBudget) must be given so both enforce the same numbers.
func (p Profile) Budget() providers.Budget {
	b := providers.DefaultBudget()
	b.MaxPromptTokens, b.MaxOutputTokens = p.MaxPromptTokens, p.MaxOutputTokens
	return b
}

// ResponseBudget returns the request budget of the response prompt (model call 2): the
// profile's budget with the prompt cap lowered to Profile.MaxResponsePromptTokens, since that
// prompt is the rules and the directive's facts and nothing else. It is the budget the engine
// builds the response prompt against; the runtime boundary keeps the profile's Budget, which is
// the larger of the two.
func (p Profile) ResponseBudget() providers.Budget {
	b := p.Budget()
	b.MaxPromptTokens = min(b.MaxPromptTokens, p.MaxResponsePromptTokens)
	return b
}

// String names the profile and its main caps, for logs.
func (p Profile) String() string {
	return fmt.Sprintf("%s(prompt<=%d, out<=%d, shortlist<=%d)", p.Name, p.MaxPromptTokens, p.MaxOutputTokens, p.MaxShortlist)
}

// ClientContextTokens is the share of the prompt that what the client's screen supplied may take (Client): half of
// the history cap, so it scales with the profile like the history does and a smaller machine never gets a larger
// block. It is derived, not a field of the table of docs/operations/resource-budgets.md 6.2: that table has no column
// for it, and the prompt cap still has the last word.
func (p Profile) ClientContextTokens() int { return p.MaxHistoryTokens / 2 }
