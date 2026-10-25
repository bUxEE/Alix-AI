package dctx

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/sessions"
)

// Visible says what the model may see of the session state: Visible(capabilityID, "") is
// whether the capability may be mentioned at all and Visible(capabilityID, field) whether
// the value of one of its fields may be shown. The caller decides it from the policy
// decisions of the turn (internal/policy field projection). A nil Visible shows nothing:
// fail closed.
type Visible func(capabilityID, field string) bool

func (v Visible) capability(id string) bool { return v != nil && id != "" && v(id, "") }

func (v Visible) field(id, field string) bool { return v != nil && field != "" && v(id, field) }

// maxChoices bounds the choices rendered of a clarification.
const maxChoices = 8

// slotText is the short text form of a slot value.
func slotText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case fmt.Stringer: // json.Number
		return x.String()
	case []string:
		return strings.Join(x, ",")
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = slotText(e)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

func sortedSlotNames(g *sessions.Goal) []string {
	names := make([]string, 0, len(g.Slots))
	for n := range g.Slots {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// value renders one untrusted value on one line, clipped.
func value(s string, max int) string { return clipRunes(oneLine(s), max) }

// goalText renders the active goal and the last directive: the part of the session state
// that the next message answers. It is empty when there is no goal the model may see.
func goalText(p Profile, st *sessions.State, vis Visible, withChoices bool) string {
	g := st.Goal
	if g == nil || !vis.capability(g.CapabilityID) {
		return ""
	}
	var sb strings.Builder
	phase := g.Phase
	if phase == "" {
		phase = "open"
	}
	fmt.Fprintf(&sb, "Goal: %s (%s)\n", oneLine(g.CapabilityID), oneLine(phase))
	var parts []string
	for _, name := range sortedSlotNames(g) {
		if !vis.field(g.CapabilityID, name) {
			continue
		}
		parts = append(parts, oneLine(name)+"="+value(slotText(g.Slots[name].Value), p.MaxValueRunes))
		if len(parts) == p.MaxSlots {
			break
		}
	}
	if len(parts) > 0 {
		sb.WriteString("Collected: " + strings.Join(parts, "; ") + "\n")
	}
	if pend := g.Pending; pend != nil {
		fmt.Fprintf(&sb, "Waiting for: %s (%s)\n", oneLine(pend.Field), oneLine(string(pend.Kind)))
	}
	if l := st.Last; l != nil {
		fmt.Fprintf(&sb, "Last act: %s", oneLine(l.Act))
		if len(l.MissingFields) > 0 {
			sb.WriteString("; missing: " + strings.Join(cleanAll(l.MissingFields), ", "))
		}
		if len(l.InvalidFields) > 0 {
			sb.WriteString("; invalid: " + strings.Join(cleanAll(l.InvalidFields), ", "))
		}
		sb.WriteByte('\n')
		if withChoices && l.Question != nil && len(l.Question.Choices) > 0 {
			var cs []string
			for i, c := range l.Question.Choices {
				if i == maxChoices {
					break
				}
				cs = append(cs, oneLine(c.ID)+": "+value(c.Label, p.MaxValueRunes))
			}
			sb.WriteString("Choices offered: " + strings.Join(cs, "; ") + "\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func cleanAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = oneLine(s)
	}
	return out
}

// suspendedLines renders the goals set aside, newest suspended last: the capability, why
// it was set aside and the names of the fields it had collected. Never their values: a
// goal that is resumed becomes the active one and is then rendered with its values.
// Only the newest max are listed and only those the model may see.
func suspendedLines(p Profile, st *sessions.State, vis Visible) []string {
	var visible []*sessions.Goal
	for _, g := range st.Suspended {
		if g != nil && vis.capability(g.CapabilityID) {
			visible = append(visible, g)
		}
	}
	if len(visible) > p.MaxSuspended {
		visible = visible[len(visible)-p.MaxSuspended:]
	}
	out := make([]string, 0, len(visible))
	for _, g := range visible {
		line := "- " + oneLine(g.CapabilityID)
		if g.SuspendReason != "" {
			line += " (" + oneLine(string(g.SuspendReason)) + ")"
		}
		var names []string
		for _, n := range sortedSlotNames(g) {
			if vis.field(g.CapabilityID, n) {
				names = append(names, oneLine(n))
			}
		}
		if len(names) > p.MaxSlots {
			names = names[:p.MaxSlots]
		}
		if len(names) > 0 {
			line += " collected: " + strings.Join(names, ", ")
		}
		out = append(out, line)
	}
	return out
}

// visibleCapsules are the capsules whose capability the model may see, oldest first.
func visibleCapsules(st *sessions.State, vis Visible) []sessions.Capsule {
	var out []sessions.Capsule
	for _, c := range st.Capsules {
		if vis.capability(c.CapabilityID) {
			out = append(out, c)
		}
	}
	return out
}

// capsuleLine renders one completed goal: what it was, how it ended, which fields it had
// and the few values and records the writer chose to keep. Maps are read in sorted order,
// so the line is the same whatever order they were filled in. It carries no goal ID, no
// receipt ID and no time.
func capsuleLine(p Profile, c sessions.Capsule, vis Visible) string {
	line := "- " + oneLine(c.CapabilityID) + ": " + oneLine(string(c.Outcome))
	var fields []string
	for _, f := range c.Fields { // sorted by the writer; sort again, the input is not trusted
		if vis.field(c.CapabilityID, f) {
			fields = append(fields, oneLine(f))
		}
	}
	sort.Strings(fields)
	if len(fields) > p.MaxSlots {
		fields = fields[:p.MaxSlots]
	}
	if len(fields) > 0 {
		line += "; fields: " + strings.Join(fields, ", ")
	}
	var facts []string
	for _, k := range sortedKeys(c.Facts) {
		if vis.field(c.CapabilityID, k) {
			facts = append(facts, oneLine(k)+"="+value(c.Facts[k], p.MaxValueRunes))
		}
		if len(facts) == p.MaxSlots {
			break
		}
	}
	if len(facts) > 0 {
		line += "; kept: " + strings.Join(facts, ", ")
	}
	var refs []string
	for _, e := range c.Entities {
		if vis.field(c.CapabilityID, e.Field) {
			refs = append(refs, oneLine(e.Field)+"="+value(e.EntityID, p.MaxValueRunes))
		}
	}
	sort.Strings(refs)
	if len(refs) > p.MaxSlots {
		refs = refs[:p.MaxSlots]
	}
	if len(refs) > 0 {
		line += "; records: " + strings.Join(refs, ", ")
	}
	return line
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// digestLine folds older capsules into one line: how many goals of each capability ended
// each way, sorted. The line is bounded by p.MaxDigestEntries however many capsules there
// are, which is what keeps the cost of a long history flat.
func digestLine(p Profile, caps []sessions.Capsule) string {
	if len(caps) == 0 {
		return ""
	}
	type key struct{ capability, outcome string }
	counts := map[key]int{}
	for _, c := range caps {
		counts[key{oneLine(c.CapabilityID), oneLine(string(c.Outcome))}]++
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].capability != keys[j].capability {
			return keys[i].capability < keys[j].capability
		}
		return keys[i].outcome < keys[j].outcome
	})
	var parts []string
	for i, k := range keys {
		if i == p.MaxDigestEntries {
			parts = append(parts, fmt.Sprintf("%d more kinds", len(keys)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s x%d", k.capability, k.outcome, counts[k]))
	}
	return fmt.Sprintf("%d earlier goals: %s", len(caps), strings.Join(parts, "; "))
}
