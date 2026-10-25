package extmode

import (
	"context"
	"strings"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// mapping remembers how the items of a prompt were laid out in the egress candidate, so the
// outcome of the guard can be mapped back onto the items.
type mapping struct {
	// msg[i] is the index of the item that became candidate message i.
	msg []int
	// maxClass is the most restrictive class of anything that may leave.
	maxClass pv.DataClassification
}

// categoryOf is the send category of a kind of context item. A kind with no category is
// unclassified, and unclassified data denies the request unless it is local-only.
func categoryOf(k dialogue.ItemKind) string {
	switch k {
	case dialogue.ItemUserMessage:
		return string(pv.SendCategoryUserMessage)
	case dialogue.ItemTurn:
		return string(pv.SendCategoryConversationHistory)
	case dialogue.ItemState:
		return string(pv.SendCategoryStructuredState)
	case dialogue.ItemDirective:
		return string(pv.SendCategoryDialogueDirective)
	}
	return ""
}

func classOfRank(rank int) pv.DataClassification {
	switch rank {
	case 0:
		return pv.DataClassificationPublic
	case 1:
		return pv.DataClassificationInternal
	case 2:
		return pv.DataClassificationConfidential
	}
	return pv.DataClassificationRestricted
}

// part maps one context item onto an egress part. The metadata the core stamped on the item
// decides; the text never does.
func (r *Runtime) part(it dialogue.ContextItem) dataegress.Part {
	class, localOnly := string(it.Meta.Class), it.Meta.LocalOnly
	switch it.Meta.Kind {
	case dialogue.ItemUserMessage, dialogue.ItemTurn, dialogue.ItemState:
		// What the principal told the assistant (the core classes it confidential) leaves at the class
		// the operator approved for conversation, internal; nothing wider is ever relaxed.
		if dialogue.DataClass(class).Rank() <= dialogue.ClassConfidential.Rank() && dialogue.DataClass(class).Rank() > dialogue.ClassInternal.Rank() {
			class = string(dialogue.ClassInternal)
		}
	case dialogue.ItemDirective:
		if r.cfg.DirectiveClass != "" {
			class, localOnly = r.cfg.DirectiveClass, false
		}
	}
	return dataegress.Part{
		Kind: dataegress.PartText, Text: it.Content, Category: categoryOf(it.Meta.Kind), Class: class, LocalOnly: localOnly,
	}
}

func envRole(role providers.MessageRole) pv.RequestEnvelopeMessageRole {
	switch role {
	case providers.MsgAssistant:
		return pv.RequestEnvelopeMessageRoleAssistant
	case providers.MsgTool:
		return pv.RequestEnvelopeMessageRoleToolResult
	}
	return pv.RequestEnvelopeMessageRoleUser
}

// candidate builds the egress candidate of a prompt. System items are the instructions the core
// wrote; every other item is one message with one part. A restricted instruction denies the request.
func (r *Runtime) candidate(ctx context.Context, in dialogue.GuardInput, st *staged) (dataegress.Candidate, *mapping, error) {
	c := dataegress.Candidate{Scope: in.Scope, Channel: pv.ChannelInteractive}
	m := &mapping{}
	maxRank := dialogue.ClassInternal.Rank()
	var instr []string
	for i, it := range in.Request.Items {
		if it.Role == providers.MsgSystem {
			if it.Meta.LocalOnly {
				continue
			}
			if rk := it.Meta.Class.Rank(); rk >= dialogue.ClassRestricted.Rank() {
				return c, m, deny("restricted_instruction")
			}
			instr = append(instr, it.Content)
			continue
		}
		p := r.part(it)
		c.Messages = append(c.Messages, dataegress.Message{Role: envRole(it.Role), Parts: []dataegress.Part{p}})
		m.msg = append(m.msg, i)
		if !p.LocalOnly {
			if rk := dialogue.DataClass(p.Class).Rank(); rk > maxRank {
				maxRank = rk
			}
		}
	}
	c.Instructions = strings.Join(instr, "\n\n")
	m.maxClass = classOfRank(maxRank)
	if st.commit {
		ts, err := r.tools(ctx, in.Scope.ApplicationID, in.Scope, in.Request.Output)
		if err != nil {
			return c, m, err
		}
		c.Tools, st.tools = ts.specs, ts
	}
	return c, m, nil
}

// envelopeText reads the text of one part of the filtered envelope.
func envelopeText(env *pv.RequestEnvelope, mi, pi int) (string, bool) {
	if mi < 0 || mi >= len(env.Payload.Messages) {
		return "", false
	}
	parts := env.Payload.Messages[mi].Parts
	if pi < 0 || pi >= len(parts) {
		return "", false
	}
	switch v := parts[pi].Value.(type) {
	case pv.RequestEnvelopeTextPart:
		return v.Text, true
	case *pv.RequestEnvelopeTextPart:
		if v != nil {
			return v.Text, true
		}
	}
	return "", false
}

// apply maps the guard's outcome back onto the items: it returns the request that may be sent and
// the candidate of what is left, which the router evaluates again at send time.
func (m *mapping) apply(orig dialogue.PromptRequest, cand dataegress.Candidate, eg dataegress.Result) (dialogue.PromptRequest, dataegress.Candidate, error) {
	env := eg.Envelope
	out := orig
	out.Items = nil
	filtered := cand
	filtered.Messages = nil
	filtered.Instructions = env.Payload.Instructions
	out.Items = append(out.Items, dialogue.ContextItem{
		Role: providers.MsgSystem, Trusted: true, Content: env.Payload.Instructions,
		Meta: dialogue.ItemMeta{Kind: dialogue.ItemFrame, SourceID: "core", Class: dialogue.ClassInternal},
	})
	user := false
	for _, po := range eg.Parts {
		if !po.Kept {
			continue
		}
		text, ok := envelopeText(env, po.EnvelopeMessage, po.EnvelopePart)
		if !ok || po.Message < 0 || po.Message >= len(m.msg) {
			return dialogue.PromptRequest{}, cand, deny("envelope_unreadable")
		}
		it := orig.Items[m.msg[po.Message]]
		it.Content = text
		it.Meta.LocalOnly = false
		out.Items = append(out.Items, it)
		user = user || it.Meta.Kind == dialogue.ItemUserMessage
		p := cand.Messages[po.Message].Parts[po.Part]
		p.Text, p.LocalOnly = text, false
		filtered.Messages = append(filtered.Messages, dataegress.Message{Role: cand.Messages[po.Message].Role, Parts: []dataegress.Part{p}})
	}
	if len(filtered.Messages) == 0 {
		return dialogue.PromptRequest{}, cand, deny("no_content")
	}
	if orig.Role == providers.RoleController && hasKind(orig.Items, dialogue.ItemUserMessage) && !user {
		return dialogue.PromptRequest{}, cand, deny("no_content")
	}
	return out, filtered, nil
}

func hasKind(items []dialogue.ContextItem, k dialogue.ItemKind) bool {
	for _, it := range items {
		if it.Meta.Kind == k {
			return true
		}
	}
	return false
}
