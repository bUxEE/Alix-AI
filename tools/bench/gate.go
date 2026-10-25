package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/dialogue/scopegate"
)

// The out-of-scope gate in the harness (M0-21). tools/ is a composition root, so it may
// import the gate. The harness hands it what a first turn has: the utterance and the case's
// whole catalogue (every capability of a case is exposed to the caller), never an engaged
// session.

// gateCapabilities is the case's catalogue in the gate's terms.
func gateCapabilities(c Case) []scopegate.Capability {
	out := make([]scopegate.Capability, len(c.Catalogue))
	for i, k := range c.Catalogue {
		fields := make([]scopegate.Field, len(k.Fields))
		for j, f := range k.Fields {
			fields[j] = scopegate.Field{Name: f.Name, Enum: f.Enum}
		}
		out[i] = scopegate.Capability{ID: k.ID, Description: k.Description, Fields: fields}
	}
	return out
}

// gateDecide is the gate's decision for one case.
func gateDecide(c Case) scopegate.Decision {
	return scopegate.Decide(scopegate.Request{Utterance: c.Text, Catalogue: gateCapabilities(c)})
}

// gateAnswer is what the gate answers for a case instead of the model: the typed refusal as
// the model-authority part of a TaskProposal. refused is false when the model must answer.
func gateAnswer(c Case) (text string, refused bool) {
	ref, ok := gateDecide(c).Refusal()
	if !ok {
		return "", false
	}
	return string(ref.ProposalJSON()), true
}

// Groups of the isolation report. Each case is in exactly one.
const (
	gateGroupRefusal     = "refusal"
	gateGroupInjectionOS = "injection_expecting_out_of_scope"
	gateGroupCapability  = "routing_extraction_expecting_a_capability"
	gateGroupInjectionTK = "injection_expecting_a_capability"
	gateGroupControl     = "help_and_cancel"
)

// gateGroupOf places a case in the group the report counts it in.
func gateGroupOf(c Case) string {
	switch {
	case c.Category == CatRefusal:
		return gateGroupRefusal
	case c.Category == CatInjection && c.Expected.CapabilityID == nil:
		return gateGroupInjectionOS
	case c.Category == CatInjection:
		return gateGroupInjectionTK
	case c.Expected.CapabilityID != nil:
		return gateGroupCapability
	default:
		return gateGroupControl
	}
}

// GateCaseReport is one case's gate decision.
type GateCaseReport struct {
	ID                 string   `json:"id"`
	Language           string   `json:"language"`
	Category           string   `json:"category"`
	Group              string   `json:"group"`
	Verdict            string   `json:"verdict"`
	Reason             string   `json:"reason"`
	ReadAs             string   `json:"read_as"`
	Evidence           []string `json:"evidence"`
	ExpectedIntent     string   `json:"expected_intent"`
	ExpectedCapability *string  `json:"expected_capability"`
}

// GateGroupReport counts one group: how many cases, what the gate did with them, and the ids
// it refused. For a group that expects a capability or a help or cancel intent, every
// refusal is a false refusal.
type GateGroupReport struct {
	Cases     int            `json:"cases"`
	Refused   int            `json:"refused"`
	Passed    int            `json:"passed"`
	Abstained int            `json:"abstained"`
	RefusedBy map[string]int `json:"refused_by_language"`
	RefusedID []string       `json:"refused_ids"`
	// MissedID are the ids of a refusal or injection group the gate did not refuse.
	MissedID []string `json:"not_refused_ids"`
}

// GateReport is the isolation result: the gate on the corpus with no model.
type GateReport struct {
	Gate struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
	} `json:"gate"`
	Corpus struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
		Cases  int    `json:"cases"`
	} `json:"corpus"`
	Groups map[string]GateGroupReport `json:"groups"`
	// ByVerdict counts the cases by verdict and reason, "refuse/no_overlap" and so on.
	ByVerdict map[string]int `json:"by_verdict_reason"`
	// FalseRefusalsAgainstRouting is the number of refused cases that expect a capability:
	// each one is a miss of the capability_routing line (denominator 54).
	FalseRefusalsAgainstRouting int `json:"false_refusals_counted_against_routing"`
	// ElapsedUSPerCase is the gate's wall time per case on the machine that ran the report,
	// averaged over 200 passes of the corpus. It is a host timing, not a result of the gate.
	ElapsedUSPerCase float64          `json:"elapsed_us_per_case"`
	Cases            []GateCaseReport `json:"cases"`
}

// BuildGateReport runs the gate over the corpus. timing is how many extra passes to time
// (0 skips the timing, for tests that compare reports).
func BuildGateReport(corpus *Corpus, timing int) GateReport {
	var rep GateReport
	rep.Gate.ID, rep.Gate.Digest = scopegate.ID, scopegate.Digest()
	rep.Corpus.ID, rep.Corpus.SHA256, rep.Corpus.Cases = corpus.ID, corpus.SHA256, len(corpus.Cases)
	rep.Groups = map[string]GateGroupReport{}
	rep.ByVerdict = map[string]int{}
	for _, c := range corpus.Cases {
		d := gateDecide(c)
		g := gateGroupOf(c)
		gr := rep.Groups[g]
		if gr.RefusedBy == nil {
			gr.RefusedBy = map[string]int{}
		}
		gr.Cases++
		switch d.Verdict {
		case scopegate.VerdictRefuse:
			gr.Refused++
			gr.RefusedBy[c.Language]++
			gr.RefusedID = append(gr.RefusedID, c.ID)
			if c.Expected.CapabilityID != nil {
				rep.FalseRefusalsAgainstRouting++
			}
		case scopegate.VerdictPass:
			gr.Passed++
			gr.MissedID = append(gr.MissedID, c.ID)
		default:
			gr.Abstained++
			gr.MissedID = append(gr.MissedID, c.ID)
		}
		rep.Groups[g] = gr
		rep.ByVerdict[string(d.Verdict)+"/"+string(d.Reason)]++
		ev := []string{}
		for _, m := range d.Matches {
			ev = append(ev, m.Word+"~"+m.Label)
		}
		rep.Cases = append(rep.Cases, GateCaseReport{
			ID: c.ID, Language: c.Language, Category: c.Category, Group: g,
			Verdict: string(d.Verdict), Reason: string(d.Reason), ReadAs: d.Language, Evidence: ev,
			ExpectedIntent: c.Expected.Intent, ExpectedCapability: c.Expected.CapabilityID,
		})
	}
	for name, gr := range rep.Groups {
		sort.Strings(gr.RefusedID)
		sort.Strings(gr.MissedID)
		rep.Groups[name] = gr
	}
	if timing > 0 && len(corpus.Cases) > 0 {
		start := time.Now()
		for i := 0; i < timing; i++ {
			for _, c := range corpus.Cases {
				gateDecide(c)
			}
		}
		rep.ElapsedUSPerCase = float64(time.Since(start).Microseconds()) / float64(timing*len(corpus.Cases))
	}
	return rep
}

func gateCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpusPath := fs.String("corpus", "", "corpus file (JSONL)")
	out := fs.String("out", "", "write the report here (default: standard output)")
	timing := fs.Int("timing-passes", 200, "passes of the corpus to time the gate over (0: no timing)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bench gate --corpus <file.jsonl> [--out report.json]")
		fmt.Fprintln(stderr, "  runs the out-of-scope gate (internal/dialogue/scopegate) on every case, with no model, and")
		fmt.Fprintln(stderr, "  counts what it refused, what it passed and its false refusals")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || *corpusPath == "" || *timing < 0 {
		fs.Usage()
		return 2
	}
	corpus, err := LoadCorpus(*corpusPath)
	if err != nil {
		fmt.Fprintf(stderr, "bench gate: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(BuildGateReport(corpus, *timing), "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "bench gate: %v\n", err)
		return 1
	}
	data = append(data, '\n')
	if *out == "" {
		stdout.Write(data)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(stderr, "bench gate: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "bench gate: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "gate report: %s\n", filepath.ToSlash(*out))
	return 0
}
