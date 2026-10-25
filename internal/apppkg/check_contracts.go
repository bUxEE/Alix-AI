package apppkg

import (
	"errors"
	"regexp"
	"sort"
	"strings"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

var (
	factPathRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,3}$`)
	fieldNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// placeholders extracts the {fact.path} and {input.field} placeholders of a
// response template. Literal braces are written {{ and }}. A single unmatched
// brace, an empty placeholder or one with characters outside the fact grammar
// is an error: a template that cannot be filled deterministically is refused.
func placeholders(t string) ([]string, error) {
	var out []string
	for i := 0; i < len(t); {
		switch t[i] {
		case '{':
			if i+1 < len(t) && t[i+1] == '{' {
				i += 2
				continue
			}
			j := strings.IndexByte(t[i+1:], '}')
			if j < 0 {
				return nil, errors.New("a '{' is never closed")
			}
			name := t[i+1 : i+1+j]
			if strings.ContainsAny(name, "{") {
				return nil, errors.New("a '{' is nested inside a placeholder")
			}
			if name == "" {
				return nil, errors.New("a placeholder is empty")
			}
			if strings.HasPrefix(name, "input.") {
				if !fieldNameRE.MatchString(strings.TrimPrefix(name, "input.")) {
					return nil, errors.New("placeholder " + q(name) + " does not name an input field")
				}
			} else if !factPathRE.MatchString(name) {
				return nil, errors.New("placeholder " + q(name) + " is not a fact path")
			}
			out = append(out, name)
			i += j + 2
		case '}':
			if i+1 < len(t) && t[i+1] == '}' {
				i += 2
				continue
			}
			return nil, errors.New("a '}' has no matching '{'")
		default:
			i++
		}
	}
	return out, nil
}

// slot is one template of a conversation contract with where it lives.
type slot struct {
	where, text string
}

func contractSlots(c ap.ConversationContract) []slot {
	var s []slot
	for _, p := range c.FieldPrompts {
		s = append(s, slot{"field_prompts." + string(p.Field), string(p.Prompt)})
	}
	for _, p := range c.Clarifications {
		s = append(s, slot{"clarifications." + string(p.RuleID), string(p.Prompt)})
	}
	s = append(s, slot{"corrections.acknowledgement", string(c.Corrections.Acknowledgement)})
	if c.Corrections.PreparedActionInvalidated != nil {
		s = append(s, slot{"corrections.prepared_action_invalidated", string(*c.Corrections.PreparedActionInvalidated)})
	}
	s = append(s, slot{"cancellations.acknowledgement", string(c.Cancellations.Acknowledgement)})
	if cf := c.Confirmations; cf != nil {
		s = append(s, slot{"confirmations.request", string(cf.Request)}, slot{"confirmations.declined", string(cf.Declined)}, slot{"confirmations.expired", string(cf.Expired)})
		if cf.StepUpRequired != nil {
			s = append(s, slot{"confirmations.step_up_required", string(*cf.StepUpRequired)})
		}
	}
	for _, r := range c.Success {
		s = append(s, slot{"success." + string(r.Outcome), string(r.Text)})
	}
	for _, r := range c.Failures {
		s = append(s, slot{"failures." + string(r.Code), string(r.Text)})
	}
	return s
}

func (md *model) checkContracts() {
	// contracts that belong to nothing, in an unsupported language, or to an admin capability
	keys := make([]ckey, 0, len(md.contracts))
	for k := range md.contracts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cap != keys[j].cap {
			return keys[i].cap < keys[j].cap
		}
		return keys[i].lang < keys[j].lang
	})
	for _, k := range keys {
		ce := md.contracts[k]
		if _, listed := md.m.Capabilities[k.cap]; !listed {
			md.c.add(chkContractCapability, k.cap, ce.path, "the contract belongs to capability %s, which the manifest does not declare", q(k.cap))
			continue
		}
		if !md.langs[k.lang] {
			md.c.add(chkContractLang, k.cap, ce.path, "the contract is in language %s, which the manifest does not support", q(k.lang))
		}
		if cp, ok := md.caps[k.cap]; ok && string(cp.c.Audience) == "admin" {
			md.c.add(chkContractAdmin, k.cap, ce.path, "capability %s has audience admin, so it has no conversation contract", q(k.cap))
		}
	}

	langs := make([]string, 0, len(md.langs))
	for l := range md.langs {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	for _, id := range md.capIDs() {
		cp := md.caps[id]
		if string(cp.c.Audience) != "user" {
			continue
		}
		documentedOutcomes := md.documentedOutcomes(id)
		for _, lang := range langs {
			ce, ok := md.contracts[ckey{id, lang}]
			if !ok {
				md.c.add(chkContractMissing, id, "conversation-contracts", "audience user capability %s has no conversation contract in language %s", q(id), q(lang))
				continue
			}
			md.checkContract(id, cp, ce, documentedOutcomes)
		}
	}
}

// documentedOutcomes returns the user_visible_outcome entries documented for
// the capability, in any language.
func (md *model) documentedOutcomes(id string) map[string]bool {
	out := map[string]bool{}
	if md.docs == nil {
		return out
	}
	for _, e := range md.docs.Entries {
		if string(e.CapabilityID) == id && string(e.Facet) == "user_visible_outcome" && e.Outcome != nil {
			out[string(*e.Outcome)] = true
		}
	}
	return out
}

func (md *model) checkContract(id string, cp *capEntry, ce *contractEntry, documentedOutcomes map[string]bool) {
	c, k := cp.c, ce.c
	path, lang := ce.path, string(k.Language)
	add := func(ch *check, format string, args ...any) { md.c.add(ch, id, path, format, args...) }

	fields := map[string]ap.CapabilityField{}
	for _, f := range c.Fields {
		fields[string(f.Name)] = f
	}

	// uniqueness inside the contract
	seen := map[string]bool{}
	for _, p := range k.FieldPrompts {
		if seen[string(p.Field)] {
			add(chkDuplicate, "field prompt for %s appears more than once", q(string(p.Field)))
		}
		seen[string(p.Field)] = true
	}
	seen = map[string]bool{}
	for _, s := range k.Success {
		if seen[string(s.Outcome)] {
			add(chkDuplicate, "success outcome %s appears more than once", q(string(s.Outcome)))
		}
		seen[string(s.Outcome)] = true
	}

	// a prompt for every required field
	prompted := map[string]bool{}
	for _, p := range k.FieldPrompts {
		prompted[string(p.Field)] = true
		if _, ok := fields[string(p.Field)]; !ok {
			add(chkFieldPromptExtra, "a prompt names field %s, which the capability does not declare", q(string(p.Field)))
		}
	}
	for _, f := range c.Fields {
		if f.Required && !prompted[string(f.Name)] {
			add(chkFieldPrompt, "required field %s has no prompt in language %s", q(string(f.Name)), q(lang))
		}
	}

	// a clarification for every ask_user ambiguity rule
	asks := map[string]bool{}
	for _, r := range c.Ambiguity {
		if string(r.Action) == "ask_user" {
			asks[string(r.ID)] = true
		}
	}
	clarified := map[string]bool{}
	for _, cl := range k.Clarifications {
		clarified[string(cl.RuleID)] = true
		if !asks[string(cl.RuleID)] {
			add(chkClarificationExtra, "clarification %s does not belong to an ambiguity rule with action ask_user", q(string(cl.RuleID)))
		}
	}
	for _, r := range sortedKeys(asks) {
		if !clarified[r] {
			add(chkClarification, "ambiguity rule %s asks the user but has no clarification in language %s", q(r), q(lang))
		}
	}

	// a failure response for every failure state
	responded := map[string]bool{}
	for _, f := range k.Failures {
		responded[failureKey(string(f.Code), f.ApplicationCode)] = true
	}
	declared := map[string]bool{}
	for _, fs := range c.FailureStates {
		key := failureKey(string(fs.Code), fs.ApplicationCode)
		declared[key] = true
		if !responded[key] {
			add(chkFailureResponse, "failure state %s has no failure response in language %s", q(key), q(lang))
		}
	}
	for _, f := range k.Failures {
		if key := failureKey(string(f.Code), f.ApplicationCode); !declared[key] {
			add(chkFailureExtra, "failure response %s does not belong to a failure state of the capability", q(key))
		}
	}

	// confirmations exactly when the mode is not none
	mode := string(c.Confirmation.Mode)
	switch {
	case mode != "none" && k.Confirmations == nil:
		add(chkConfirmations, "confirmation mode is %s but the contract has no confirmations", mode)
	case mode == "none" && k.Confirmations != nil:
		add(chkConfirmations, "confirmation mode is none but the contract has confirmations")
	}
	if mode == "step_up" && (k.Confirmations == nil || k.Confirmations.StepUpRequired == nil) {
		add(chkStepUpWording, "confirmation mode is step_up but the contract has no confirmations.step_up_required wording")
	}
	if c.Operations.Prepare != nil && k.Corrections.PreparedActionInvalidated == nil {
		add(chkCorrectionWording, "the capability has a prepare operation but the contract has no corrections.prepared_action_invalidated wording")
	}
	for _, f := range k.Corrections.CorrectableFields {
		if _, ok := fields[string(f)]; !ok {
			add(chkCorrectable, "correctable field %s is not a field of the capability", q(string(f)))
		}
	}

	// outcomes: the documented outcomes must all appear in the contract
	have := map[string]bool{}
	for _, s := range k.Success {
		have[string(s.Outcome)] = true
	}
	for _, o := range sortedKeys(documentedOutcomes) {
		if !have[o] {
			add(chkOutcomes, "outcome %s is documented but the contract in language %s has no success response for it", q(o), q(lang))
		}
	}

	// next-action labels
	labelled := map[string]bool{}
	for _, l := range k.NextActions {
		labelled[string(l.ID)] = true
	}
	for _, n := range c.NextActions {
		if !labelled[string(n.ID)] {
			add(chkNextLabel, "next action %s has no label in language %s", q(string(n.ID)), q(lang))
		}
	}

	// terminology
	for _, t := range k.Terminology {
		te, ok := md.terms[string(t)]
		if !ok {
			add(chkTermUnknown, "the contract relies on term %s, which terminology/ does not define", q(string(t)))
			continue
		}
		if _, has := te.t.Labels[lang]; !has {
			add(chkTermLabel, "term %s has no label in language %s", q(string(t)), q(lang))
		}
	}

	md.checkTemplates(id, path, c, k, fields)
}

// checkTemplates implements section 14 item 12.
func (md *model) checkTemplates(id, path string, c ap.Capability, k ap.ConversationContract, fields map[string]ap.CapabilityField) {
	facts := map[string]bool{}
	for _, o := range c.ResponseObligations {
		facts[string(o.Fact)] = true
	}
	used := func(text string) map[string]bool {
		out := map[string]bool{}
		ph, err := placeholders(text)
		if err != nil {
			return out
		}
		for _, p := range ph {
			out[p] = true
		}
		return out
	}

	for _, s := range contractSlots(k) {
		ph, err := placeholders(s.text)
		if err != nil {
			md.c.add(chkTemplate, id, path, "%s: %s", s.where, err.Error())
			continue
		}
		for _, p := range ph {
			if name, isInput := strings.CutPrefix(p, "input."); isInput {
				if _, ok := fields[name]; !ok {
					md.c.add(chkPlaceholder, id, path, "%s: placeholder {%s} names a field the capability does not declare", s.where, p)
				}
				continue
			}
			if !facts[p] {
				md.c.add(chkPlaceholder, id, path, "%s: placeholder {%s} names a fact that is not declared in response_obligations", s.where, p)
			}
		}
	}

	// obligations must be stated where they apply
	stated := map[string]bool{}
	for _, r := range k.Success {
		for p := range used(string(r.Text)) {
			stated[p] = true
		}
	}
	request := map[string]bool{}
	if k.Confirmations != nil {
		request = used(string(k.Confirmations.Request))
	}
	for _, o := range c.ResponseObligations {
		fact, when := string(o.Fact), string(o.When)
		if (when == "success" || when == "always") && !stated[fact] {
			md.c.add(chkObligation, id, path, "response obligation %s (when %s) is not stated by any success response in language %s", q(fact), when, q(string(k.Language)))
		}
		if (when == "prepared" || when == "always") && k.Confirmations != nil && !request[fact] {
			md.c.add(chkObligation, id, path, "response obligation %s (when %s) is not stated by the confirmation request in language %s", q(fact), when, q(string(k.Language)))
		}
	}
}
