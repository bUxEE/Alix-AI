package assist

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/buxee/alix-ai/internal/readiness/verify"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const (
	assistMaxReplyBytes = 256 << 10
	assistMaxItemEvid   = 4
	assistMaxQuoteBytes = 500
	assistMaxIDsPerItem = 3
)

// assistReply is the decoded outer shape of a reply: the items still raw, and how many top-level
// fields the schema does not have.
type assistReply struct {
	items   []json.RawMessage
	foreign int
}

type assistEv struct {
	Path      string `json:"path"`
	LineStart int64  `json:"line_start"`
	LineEnd   int64  `json:"line_end"`
	Quote     string `json:"quote"`
}

type assistItem struct {
	RequirementIDs []string   `json:"requirement_ids"`
	Category       string     `json:"category"`
	Status         string     `json:"status"`
	Claim          string     `json:"claim"`
	Evidence       []assistEv `json:"evidence"`
	Remediation    string     `json:"remediation"`
}

var assistItemKeys = map[string]bool{
	"requirement_ids": true, "category": true, "status": true, "claim": true, "evidence": true, "remediation": true,
}

// assistParseReply decodes the outer object. A reply that is not one JSON object with a
// "proposals" array is a failure of the call: nothing in it is used. Other top-level fields are
// counted, not used.
func assistParseReply(text string) (*assistReply, error) {
	if len(text) > assistMaxReplyBytes {
		return nil, errors.New("the reply is over the size cap")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil || top == nil {
		return nil, errors.New("the reply is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the reply has data after the JSON object")
	}
	raw, ok := top["proposals"]
	if !ok {
		return nil, errors.New("the reply has no proposals array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errors.New("proposals is not an array")
	}
	r := &assistReply{items: items}
	for k := range top {
		if k != "proposals" {
			r.foreign++
		}
	}
	return r, nil
}

// assistDecodeItem decodes one proposal item. foreign counts the fields outside the closed schema
// (an attempt to say something the schema has no place for, such as a removal).
func assistDecodeItem(raw json.RawMessage) (it assistItem, foreign int, ok bool) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return it, 0, false
	}
	for k := range keys {
		if !assistItemKeys[k] {
			foreign++
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&it); err != nil {
		// A foreign field fails a strict decode; decode again without it so the counts stay exact.
		if foreign == 0 {
			return it, 0, false
		}
		var loose assistItem
		if err := json.Unmarshal(raw, &loose); err != nil {
			return it, foreign, false
		}
		it = loose
	}
	return it, foreign, true
}

// assistVerdict is what the engine decided about one proposal before the verifier sees it.
type assistVerdict int

const (
	assistPass assistVerdict = iota
	assistDrop
	assistOverride
)

// assistTally counts a run. All counts are proposals, one per requirement id named.
type assistTally struct {
	received  int
	dropped   int
	overrides int
}

func (t *assistTally) drop(override bool, n int) {
	t.dropped += n
	if override {
		t.overrides += n
	}
}

// assistClassify turns the items of one reply into verifier proposals. shown maps the paths shown
// in this call to their views; taken is how many proposals earlier replies already contributed.
// Order of checks per proposal: shape, claim, requirement, evidence. The first that fails decides.
func (e *assistEngine) assistClassify(rep *assistReply, shown map[string]*assistView, taken int, t *assistTally) []verify.RdyProposal {
	t.drop(true, rep.foreign) // a foreign top-level field is an attempt; it carries no proposal
	t.received += rep.foreign
	var out []verify.RdyProposal
	for _, raw := range rep.items {
		it, foreign, ok := assistDecodeItem(raw)
		if !ok || foreign > 0 {
			// Malformed, or carrying fields the schema does not have: dropped whole. A foreign
			// field is an attempt to do more than add a finding.
			t.received++
			t.drop(foreign > 0, 1)
			continue
		}
		ids := assistDedupe(it.RequirementIDs)
		if len(ids) == 0 || len(ids) > assistMaxIDsPerItem || len(it.Evidence) == 0 || len(it.Evidence) > assistMaxItemEvid {
			t.received++
			t.drop(false, 1)
			continue
		}
		for _, id := range ids {
			t.received++
			if taken+len(out) >= e.cfg.Budget.MaxProposals {
				t.drop(false, 1)
				continue
			}
			p, verdict := e.assistOne(it, id, shown)
			switch verdict {
			case assistPass:
				out = append(out, p)
			case assistOverride:
				t.drop(true, 1)
			default:
				t.drop(false, 1)
			}
		}
	}
	return out
}

func assistDedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// assistOne checks one (item, requirement) pair.
func (e *assistEngine) assistOne(it assistItem, id string, shown map[string]*assistView) (verify.RdyProposal, assistVerdict) {
	// The claim: found or partial, nothing else. Missing, contradicted, unverified, "met",
	// "covered", an empty string: each is an attempt to set a status the model may not set.
	if !e.rules.allowedClaim(it.Status) {
		return verify.RdyProposal{}, assistOverride
	}
	req, ok := e.cat.Get(readiness.RequirementID(id))
	if !ok {
		return verify.RdyProposal{}, assistDrop
	}
	// A requirement a rule decided is final: the model may not restate or change it.
	if st, decided := e.decided[req.ID]; decided && e.rules.ruleFinal(st) {
		return verify.RdyProposal{}, assistOverride
	}
	if !e.liftable(req) {
		return verify.RdyProposal{}, assistDrop
	}
	if string(req.Category) != it.Category {
		return verify.RdyProposal{}, assistDrop
	}
	quotes := make([]string, len(it.Evidence))
	ev := make([]readiness.FindingEvidence, len(it.Evidence))
	for i, x := range it.Evidence {
		if len(x.Quote) > assistMaxQuoteBytes {
			return verify.RdyProposal{}, assistDrop
		}
		switch e.assistCheckEvidence(x, shown) {
		case assistDrop:
			return verify.RdyProposal{}, assistDrop
		case assistOverride:
			return verify.RdyProposal{}, assistOverride
		}
		quotes[i] = x.Quote
		ev[i] = readiness.FindingEvidence{
			Path:       readiness.RelativePath(x.Path),
			LineStart:  x.LineStart,
			LineEnd:    x.LineEnd,
			FileSHA256: readiness.SHA256(e.shaOf(x.Path)),
		}
	}
	if !e.rules.plausible(req.Category, quotes) {
		return verify.RdyProposal{}, assistDrop
	}
	claim := readiness.Text(it.Claim)
	f := readiness.Finding{
		SchemaVersion: 1,
		RequirementID: req.ID,
		Status:        readiness.Status(it.Status),
		Evidence:      ev,
		Remediation:   readiness.RemediationText{En: readiness.Text(it.Remediation)},
	}
	if strings.TrimSpace(it.Claim) != "" {
		f.Summary = &claim
	}
	return verify.RdyProposal{Finding: f, Quotes: quotes}, assistPass
}

// assistCheckEvidence enforces that the model cites only what it was shown, and never an
// instruction-like line. A path or span that simply does not exist is passed on: the verifier
// demotes it to model_unverified with a reason, which is what the report should show.
func (e *assistEngine) assistCheckEvidence(x assistEv, shown map[string]*assistView) assistVerdict {
	rec, inScan := e.files[x.Path]
	if !inScan || rec.Kind == assistKindSecret || e.secret[x.Path] {
		return assistPass // the verifier refuses it without opening the file
	}
	v, ok := shown[x.Path]
	if !ok {
		return assistDrop // a file of the scan that this call never showed
	}
	if x.LineStart < 1 || x.LineEnd < x.LineStart || x.LineEnd > int64(v.Total) {
		return assistPass // not a span of the file at all: the verifier says so
	}
	if x.LineEnd > int64(len(v.Lines)) {
		return assistDrop // a real line the model was not shown
	}
	if !e.rules.cleanSpan(v, x.LineStart, x.LineEnd) {
		return assistOverride
	}
	return assistPass
}
