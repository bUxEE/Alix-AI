package dialogue

import (
	"context"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/internal/verify"
)

// Control decisions (master plan 8.5 and 11.3). Confirm, reject, correct, cancel,
// help, restart and resume are BOUNDED decisions: each is a yes or no about one
// utterance given a handful of facts about the session. The engine decides every one
// of them with exact rules and, when the rules do not decide, with the controller (the
// model's TaskProposal.intent), so the pipeline is complete and correct with nothing
// else. Each detection sits behind the small ControlDetector interface so that the
// optional decision layer (milestone D) can pre-empt the controller for the same
// decisions without the engine changing: it supplies its own detector for any of them.
//
// A detector is ADVISORY. It says "this utterance is a cancel", never "cancel it": the
// engine checks that the session is in a state where the transition applies (a
// rejection needs a live preparation, a resume needs a suspended goal, ...) and then
// takes the transition itself, deterministically. A detection that does not fit the
// state is ignored and the turn goes on through the pipeline as if there had been none.
// No transition reachable from here can prepare, confirm, commit or call a tool; a
// confirmation in particular is never taken from text (a "yes" re-presents the pending
// request and commits nothing).

// ControlState is everything a detector learns about the session: flags and counts,
// never a slot value, a record or a transcript.
type ControlState struct {
	// HasGoal is true when there is an active goal.
	HasGoal bool
	// Finished is true when the active goal already produced its result or failed.
	Finished bool
	// Prepared is true when the active goal has a preparation that can still be
	// confirmed: a reject or a confirm is meaningful.
	Prepared bool
	// Pending is true when the core is waiting for the answer to a question, and
	// Choices when that question offered choices.
	Pending bool
	Choices bool
	// Suspended is how many goals are set aside.
	Suspended int
}

// ControlRequest is what a detector is asked about: the user's text, the language of
// the turn ("en" or "it") and the state flags.
type ControlRequest struct {
	Text     string
	Language verify.Language
	State    ControlState
}

// ControlDetection is a detector's answer.
type ControlDetection struct {
	// Matched says the utterance is this decision.
	Matched bool
	// All widens a cancel to every goal of the session, the suspended ones included.
	All bool
	// Strong marks a resume that may replace an active goal that is still being
	// worked on ("go back"), as opposed to one that only fills an empty seat
	// ("continue").
	Strong bool
	// Target is what follows a resume phrase ("resume the item lookup"), for the
	// engine to match against the suspended goals; empty means the most recent one.
	Target string
}

// ControlDetector decides one kind of control utterance. Implementations must be
// deterministic functions of the request, must not block, and must not retain it. A
// panic is read as "no match".
type ControlDetector interface {
	Detect(ctx context.Context, req ControlRequest) ControlDetection
}

// ControlDetectorFunc adapts a function to ControlDetector.
type ControlDetectorFunc func(ctx context.Context, req ControlRequest) ControlDetection

// Detect calls f.
func (f ControlDetectorFunc) Detect(ctx context.Context, req ControlRequest) ControlDetection {
	return f(ctx, req)
}

// Detectors holds one detector per control decision. A nil member takes the rule
// detector. Replace a member to let another decision procedure pre-empt the
// controller for that decision; every transition stays the engine's.
type Detectors struct {
	// Help: the user asks what the application can do.
	Help ControlDetector
	// Cancel: the user drops the current goal (or, with All, everything).
	Cancel ControlDetector
	// Restart: the user wants the current goal begun again from nothing.
	Restart ControlDetector
	// Resume: the user wants a goal that was set aside back.
	Resume ControlDetector
	// Reject: the user declines the pending prepared action.
	Reject ControlDetector
	// Confirm: the user says yes to the pending prepared action. The engine never
	// commits on it: a confirmation is a structured input (TurnInput.Confirm).
	Confirm ControlDetector
	// Correct: the utterance changes a value already given. It only tells the engine
	// to read a proposal for the active goal as a correction; which slot changes is
	// decided by the values the validated proposal names.
	Correct ControlDetector
}

// RuleDetectors returns the default detectors: exact whole-utterance phrase tables in
// English and Italian, gated by the session state. They never match a phrase inside a
// longer sentence, so "no, make it 5" is a correction for the controller and not a
// rejection, and they need no model.
func RuleDetectors() Detectors {
	return Detectors{
		Help: ControlDetectorFunc(ruleHelp), Cancel: ControlDetectorFunc(ruleCancel),
		Restart: ControlDetectorFunc(ruleRestart), Resume: ControlDetectorFunc(ruleResume),
		Reject: ControlDetectorFunc(ruleReject), Confirm: ControlDetectorFunc(ruleConfirm),
		Correct: ControlDetectorFunc(ruleCorrect),
	}
}

// withDefaults fills the nil members.
func (d Detectors) withDefaults() Detectors {
	r := RuleDetectors()
	if d.Help == nil {
		d.Help = r.Help
	}
	if d.Cancel == nil {
		d.Cancel = r.Cancel
	}
	if d.Restart == nil {
		d.Restart = r.Restart
	}
	if d.Resume == nil {
		d.Resume = r.Resume
	}
	if d.Reject == nil {
		d.Reject = r.Reject
	}
	if d.Confirm == nil {
		d.Confirm = r.Confirm
	}
	if d.Correct == nil {
		d.Correct = r.Correct
	}
	return d
}

// safeDetect runs a detector; a panic is "no match".
func safeDetect(ctx context.Context, d ControlDetector, req ControlRequest) (out ControlDetection) {
	defer func() {
		if recover() != nil {
			out = ControlDetection{}
		}
	}()
	return d.Detect(ctx, req)
}

// ---- normalisation ----

// phraseOf normalises an utterance for an exact phrase lookup: lower case, one space
// between words, apostrophes straightened, no surrounding punctuation, no leading or
// trailing courtesy word.
func phraseOf(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := true
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '’' || r == '‘' || r == '`':
			b.WriteByte('\'')
			space = false
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '#' || r == 'º' || r == '°':
			b.WriteRune(r)
			space = false
		case r == '-' && !space:
			b.WriteByte('-')
		default:
			if !space {
				b.WriteByte(' ')
			}
			space = true
		}
	}
	s := strings.TrimSpace(b.String())
	s = strings.Trim(s, "-")
	for _, p := range courtesyPrefix {
		if rest, ok := strings.CutPrefix(s, p); ok && rest != "" {
			s = rest
		}
	}
	for _, p := range courtesySuffix {
		if rest, ok := strings.CutSuffix(s, p); ok && rest != "" {
			s = rest
		}
	}
	return s
}

var (
	courtesyPrefix = []string{"please ", "per favore ", "ok so ", "well "}
	courtesySuffix = []string{" please", " thanks", " thank you", " per favore", " grazie"}
)

func inSet(set map[string]bool, s string) bool { return set[s] }

func toSet(phrases ...string) map[string]bool {
	m := make(map[string]bool, len(phrases))
	for _, p := range phrases {
		m[p] = true
	}
	return m
}

// ---- the phrase tables ----

var (
	helpPhrases = toSet(
		"help", "help me", "what can you do", "what can i do", "what can you do for me",
		"aiuto", "aiutami", "cosa puoi fare", "cosa sai fare", "che cosa puoi fare",
	)

	cancelPhrases = toSet(
		"cancel", "cancel that", "cancel it", "cancel this", "cancel the request", "never mind", "nevermind",
		"forget it", "forget that", "drop it", "drop that", "stop", "stop that", "abort", "scrap that", "discard",
		"discard that", "i changed my mind", "no never mind", "no nevermind",
		"annulla", "annulla questo", "annullala", "annullalo", "lascia stare", "lascia perdere", "non importa",
		"dimentica", "dimenticalo", "ferma", "fermati", "basta", "ho cambiato idea", "no lascia stare",
	)
	cancelAllPhrases = toSet(
		"cancel everything", "cancel all", "forget everything", "forget all of it", "drop everything", "stop everything",
		"annulla tutto", "annullali tutti", "dimentica tutto", "lascia stare tutto", "ferma tutto",
	)

	restartPhrases = toSet(
		"start over", "restart", "start again", "begin again", "begin anew", "start afresh", "start from scratch",
		"from the top", "from the start", "let's start over", "lets start over", "let me start over", "reset",
		"ricomincia", "ricominciamo", "ricomincia da capo", "da capo", "ripartiamo da capo", "ripartiamo", "azzera",
		"ricomincia daccapo", "ricominciare", "ricominciare da capo",
	)

	// resumeStrong may replace an active goal that is still being worked on; resumeWeak
	// only takes the seat of a finished or an absent one.
	resumeStrong = toSet(
		"resume", "go back", "go back to that", "go back to what i was doing", "go back to what we were doing",
		"back to that", "back to what we were doing", "take me back", "resume that", "resume it",
		"riprendi", "riprendiamo", "riprendilo", "riprendila", "torna indietro", "torna a prima", "torniamo a prima",
		"torna a quello di prima",
	)
	resumeWeak = toSet(
		"continue", "carry on", "keep going", "go on", "where were we", "pick up where we left off",
		"continua", "andiamo avanti", "proseguiamo", "dove eravamo",
	)
	// resumePrefixes introduce the name of the goal to bring back.
	resumePrefixes = []struct {
		prefix string
		strong bool
	}{
		{"resume the ", true}, {"resume my ", true}, {"resume ", true}, {"go back to the ", true}, {"go back to my ", true},
		{"go back to ", true}, {"back to the ", true}, {"back to ", true}, {"continue with the ", false},
		{"continue with ", false}, {"continue the ", false},
		{"riprendi il ", true}, {"riprendi la ", true}, {"riprendi ", true}, {"torna al ", true}, {"torna alla ", true},
		{"torna a ", true}, {"continua con il ", false}, {"continua con la ", false}, {"continua con ", false},
	}

	rejectPhrases = toSet(
		"no", "no thanks", "no thank you", "nope", "don't", "do not", "don't do it", "do not do it", "not now",
		"reject", "reject it", "decline", "i decline", "negative", "no don't", "no do not",
		"no grazie", "non farlo", "rifiuta", "rifiuto", "non adesso", "assolutamente no", "no non farlo",
	)
	confirmPhrases = toSet(
		"yes", "yes please", "yeah", "yep", "ok", "okay", "sure", "confirm", "confirmed", "i confirm", "go ahead",
		"do it", "proceed", "sounds good", "that's right", "thats right", "yes go ahead", "yes do it", "yes confirm",
		"si", "sì", "certo", "conferma", "confermo", "va bene", "procedi", "vai", "fallo", "d'accordo", "si procedi",
		"sì procedi",
	)
)

// correctionLead are the words that open an utterance which changes a value, and
// correctionTrail those that close one ("make it 5 instead"). They are cues, not
// decisions: they only mark a proposal for the active goal as a correction.
var (
	correctionLead = []string{
		"no", "actually", "instead", "i meant", "i mean", "change", "make it", "make that", "set it to", "not", "wait",
		"sorry", "correction", "scratch that", "rather", "let's make", "lets make", "update",
		"anzi", "invece", "intendevo", "volevo dire", "cambia", "cambiala", "cambialo", "metti", "correggi", "scusa",
		"aspetta", "meglio",
	}
	correctionTrail = []string{"instead", "rather", "invece", "piuttosto"}
)

// ---- the rule detectors ----

func ruleHelp(_ context.Context, r ControlRequest) ControlDetection {
	return ControlDetection{Matched: inSet(helpPhrases, phraseOf(r.Text))}
}

func ruleCancel(_ context.Context, r ControlRequest) ControlDetection {
	p := phraseOf(r.Text)
	switch {
	case inSet(cancelAllPhrases, p):
		return ControlDetection{Matched: true, All: true}
	case inSet(cancelPhrases, p):
		return ControlDetection{Matched: true}
	}
	return ControlDetection{}
}

func ruleRestart(_ context.Context, r ControlRequest) ControlDetection {
	return ControlDetection{Matched: inSet(restartPhrases, phraseOf(r.Text))}
}

func ruleResume(_ context.Context, r ControlRequest) ControlDetection {
	p := phraseOf(r.Text)
	switch {
	case inSet(resumeStrong, p):
		return ControlDetection{Matched: true, Strong: true}
	case inSet(resumeWeak, p):
		return ControlDetection{Matched: true}
	}
	for _, rp := range resumePrefixes {
		if rest, ok := strings.CutPrefix(p, rp.prefix); ok && strings.TrimSpace(rest) != "" {
			return ControlDetection{Matched: true, Strong: rp.strong, Target: strings.TrimSpace(rest)}
		}
	}
	return ControlDetection{}
}

func ruleReject(_ context.Context, r ControlRequest) ControlDetection {
	return ControlDetection{Matched: inSet(rejectPhrases, phraseOf(r.Text))}
}

func ruleConfirm(_ context.Context, r ControlRequest) ControlDetection {
	return ControlDetection{Matched: inSet(confirmPhrases, phraseOf(r.Text))}
}

func ruleCorrect(_ context.Context, r ControlRequest) ControlDetection {
	p := phraseOf(r.Text)
	for _, lead := range correctionLead {
		if strings.HasPrefix(p, lead+" ") { // a bare "no" says nothing was changed to what
			return ControlDetection{Matched: true}
		}
	}
	for _, trail := range correctionTrail {
		if strings.HasSuffix(p, " "+trail) {
			return ControlDetection{Matched: true}
		}
	}
	return ControlDetection{}
}

// isHelpRequest is the deterministic rule for an exact request for help.
func isHelpRequest(text string) bool {
	return ruleHelp(context.Background(), ControlRequest{Text: text}).Matched
}
