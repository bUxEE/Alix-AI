package extmode

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// toolSet is the allowlist of tools offered for one turn: one small schema per capability the
// engine put on the shortlist, with only the fields the policy gateway made visible. It is built
// from the output constraint of the prompt (which already holds exactly that) and from the typed
// package of the application, never from anything the provider or the client said.
type toolSet struct {
	specs []pv.RequestEnvelopeToolSpec
	// capability maps a tool name to its capability id.
	capability map[string]string
	// fields lists the field names each tool may carry.
	fields map[string]map[string]bool
}

var (
	toolNameClean = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	idLike        = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
)

const maxToolDescRunes = 600

// toolName derives a provider-safe tool name from a capability id.
func toolName(id string) string {
	n := toolNameClean.ReplaceAllString(id, "_")
	if n == "" || !(n[0] >= 'A' && n[0] <= 'Z' || n[0] >= 'a' && n[0] <= 'z') {
		n = "c_" + n
	}
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// tools builds the allowlisted tool schemas of a proposal prompt from its output constraint.
func (r *Runtime) tools(ctx context.Context, applicationID string, scope contracts.Scope, out providers.OutputConstraint) (toolSet, error) {
	ts := toolSet{capability: map[string]string{}, fields: map[string]map[string]bool{}}
	if out.Kind() != providers.ConstraintJSONSchema {
		return ts, nil
	}
	var sch struct {
		Properties struct {
			CapabilityID struct {
				Enum []any `json:"enum"`
			} `json:"capability_id"`
			Fields struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"fields"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(out.JSONSchema, &sch); err != nil {
		return ts, deny("schema_unreadable")
	}
	var ids []string
	for _, e := range sch.Properties.CapabilityID.Enum {
		if s, ok := e.(string); ok {
			ids = append(ids, s)
		}
	}
	if len(ids) == 0 {
		return ts, nil
	}
	pkg, err := r.cfg.Packages.Package(ctx, applicationID)
	if err != nil {
		return ts, deny("package_unavailable")
	}
	for _, id := range ids {
		spec, ok := pkg.Spec(id)
		if !ok {
			continue // a capability the package does not declare is not offered
		}
		props := map[string]any{}
		allowed := map[string]bool{}
		for _, f := range spec.Fields {
			name := string(f.Name)
			p, visible := sch.Properties.Fields.Properties[name]
			if !visible || (f.LocalOnly != nil && *f.LocalOnly) {
				continue
			}
			prop := map[string]any{}
			for k, v := range p {
				prop[k] = v
			}
			if len(f.Values) > 0 {
				prop["enum"] = f.Values
			}
			props[name], allowed[name] = prop, true
		}
		desc := strings.TrimSpace(string(spec.Title) + ": " + string(spec.Purpose))
		if utf8.RuneCountInString(desc) > maxToolDescRunes {
			desc = string([]rune(desc)[:maxToolDescRunes])
		}
		if desc == "" || desc == ":" {
			desc = "Capability " + id
		}
		name := toolName(id)
		if _, dup := ts.capability[name]; dup {
			continue
		}
		ts.capability[name], ts.fields[name] = id, allowed
		ts.specs = append(ts.specs, pv.RequestEnvelopeToolSpec{
			Name: name, Description: desc,
			Parameters: map[string]any{"type": "object", "additionalProperties": false, "properties": props},
		})
	}
	sort.Slice(ts.specs, func(i, j int) bool { return ts.specs[i].Name < ts.specs[j].Name })
	return ts, nil
}

// proposalFromCall turns the first tool-call proposal of a provider into the text of a
// TaskProposal. The provider proposed; nothing here runs anything. The result is validated by the
// engine against the model-authority schema, the shortlist, the policy gateway and the capability's
// own field definitions like any other proposal:
//
//   - a tool that was not offered is passed on under the name it came with, so the engine refuses it
//     as an unknown capability (a name that cannot be an id is an out_of_scope proposal);
//   - arguments that are not a JSON object, a field that was not offered and a value that is not a
//     plain scalar or list are dropped, never repaired, so the engine asks the user for what is missing.
func (st *staged) proposalFromCall(call providers.ToolCallProposal) json.RawMessage {
	capID, offered := st.tools.capability[call.Name]
	fields := map[string]json.RawMessage{}
	overall := 0.6
	if !offered {
		overall = 0
		if !idLike.MatchString(call.Name) {
			return json.RawMessage(`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"out_of_scope","uncertainty":{"overall":0}}`)
		}
		capID = call.Name
	} else {
		var args map[string]json.RawMessage
		if err := json.Unmarshal(call.Arguments, &args); err != nil || args == nil {
			overall = 0.1 // malformed arguments: the capability is kept, the data is not
		} else {
			for k, v := range args {
				if st.tools.fields[call.Name][k] && plainValue(v) {
					fields[k] = v
				}
			}
		}
	}
	b, err := json.Marshal(map[string]any{
		"capability_id": capID, "fields": fields, "entity_refs": []any{}, "intent": "task",
		"uncertainty": map[string]any{"overall": overall},
	})
	if err != nil {
		return json.RawMessage(`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"out_of_scope","uncertainty":{"overall":0}}`)
	}
	return b
}

// plainValue reports whether v is a string, number, boolean or a short list of those.
func plainValue(v json.RawMessage) bool {
	var x any
	if json.Unmarshal(v, &x) != nil {
		return false
	}
	scalar := func(a any) bool {
		switch a.(type) {
		case string, float64, bool:
			return true
		}
		return false
	}
	if l, ok := x.([]any); ok {
		if len(l) > 64 {
			return false
		}
		for _, e := range l {
			if !scalar(e) {
				return false
			}
		}
		return true
	}
	return scalar(x)
}
