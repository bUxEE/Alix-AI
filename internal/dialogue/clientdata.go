package dialogue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// What the client's screen gives a turn (M3-32, CopilotKit readable context and frontend actions).
//
// Two things reach the engine from a browser the engine does not trust, both through the context of the call
// (like attachments), so TurnInput, the router, the decision stage and the policy gateway never see either:
//
//   - client context (WithClientContext): readable context, state and earlier messages as bounded one-line items. The
//     context builder places them in ONE untrusted message of the proposal prompt, between <client_context> tags, with
//     a share of the budget of its own (dctx.Client). They are data, never an instruction, never a capability, and they
//     are not the user's message: not routed on, not in the transcript, not in the session state, not audited.
//
//   - UI tools (WithUITools): the tools the client declared for this run. The model may propose a call of one of them
//     (the ui_action member of the TaskProposal) only if the host ALSO registered the name for the application
//     (Deps.UITools: an allow-list per application, nil registers none). A UI action is an instruction to the client's
//     own screen: it can never name a capability, a prepare or a commit; its arguments are validated after decoding
//     against the JSON Schema the client declared and are size-bounded; whatever the screen returns is, in the next
//     turn, one more line of untrusted client context. Anything else the model proposes fails the whole proposal
//     closed (validation_failed) before anything is executed.
//
// The descriptions and the parameter schemas of the tools are the client's words: they never reach the trusted system
// frame or the output constraint, only the bare, checked names do. The descriptions go inside the fenced client block.

// Bounds of the client data.
const (
	// ClientContextMaxLines is the most lines of client context one turn carries; more are counted, not kept.
	ClientContextMaxLines = dctx.MaxClientLines
	// UIToolMaxDeclarations is how many declared tools are read.
	UIToolMaxDeclarations = 32
	// UIToolMaxSchemaBytes bounds the declared parameters of one tool.
	UIToolMaxSchemaBytes = 16 << 10
	// UIToolMaxOffers is the most tools the model is offered in one turn.
	UIToolMaxOffers = 4
	// UIActionMaxArgsBytes bounds the encoded arguments of a UI action.
	UIActionMaxArgsBytes = 4 << 10
	// uiActionMaxDepth bounds the nesting of the arguments.
	uiActionMaxDepth = 8

	uiToolDescRunes = 100
	uiToolMaxProps  = 8
)

// ClientContext is the readable context of the client as the proposal prompt may carry it: one labelled line of plain
// text per item. The caller (internal/agui builds it) has already bounded and cleaned the lines; the context builder
// cleans and clips them again.
type ClientContext struct {
	Lines []string
	// Dropped is how many items the caller left out.
	Dropped int
}

type (
	clientContextKey struct{}
	uiToolsKey       struct{}
)

// WithClientContext returns a context that carries the client context of the turn run with it (Engine.Turn). It is
// bounded: more than ClientContextMaxLines lines are counted as dropped. An empty context changes nothing.
func WithClientContext(ctx context.Context, c ClientContext) context.Context {
	if len(c.Lines) == 0 {
		return ctx
	}
	if len(c.Lines) > ClientContextMaxLines {
		c.Dropped += len(c.Lines) - ClientContextMaxLines
		c.Lines = c.Lines[:ClientContextMaxLines]
	}
	c.Lines = append([]string(nil), c.Lines...)
	return context.WithValue(ctx, clientContextKey{}, c)
}

func clientContextFrom(ctx context.Context) ClientContext {
	c, _ := ctx.Value(clientContextKey{}).(ClientContext)
	return c
}

// UIToolDeclaration is one tool the client declared for the run (RunAgentInput.tools). All of it is client data:
// a name and a JSON Schema for the arguments, and a description the model reads only inside the fenced client block.
type UIToolDeclaration struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the arguments, an object schema.
	Parameters json.RawMessage
}

// WithUITools returns a context that carries the tools the client declared for the turn run with it. Declarations
// beyond UIToolMaxDeclarations are ignored. A declaration is a claim of the client, not a permission: the model is
// offered a tool only if the host registered its name too (Deps.UITools).
func WithUITools(ctx context.Context, decls []UIToolDeclaration) context.Context {
	if len(decls) == 0 {
		return ctx
	}
	if len(decls) > UIToolMaxDeclarations {
		decls = decls[:UIToolMaxDeclarations]
	}
	out := make([]UIToolDeclaration, len(decls))
	for i, d := range decls {
		d.Parameters = append(json.RawMessage(nil), d.Parameters...)
		out[i] = d
	}
	return context.WithValue(ctx, uiToolsKey{}, out)
}

func uiToolsFrom(ctx context.Context) []UIToolDeclaration {
	d, _ := ctx.Value(uiToolsKey{}).([]UIToolDeclaration)
	return d
}

// UIToolRegistry is the host's allow-list of UI-only tools, per application (configuration of the composition root).
// A tool is UI-only: the client's screen runs it, nothing in the application does. The names are exact; one that is a
// capability of the application, reserved or malformed is never offered whatever the registry says.
type UIToolRegistry interface {
	UITools(ctx context.Context, applicationID string) ([]string, error)
}

// StaticUITools is a UIToolRegistry over a fixed map of application id to tool names.
func StaticUITools(byApplication map[string][]string) UIToolRegistry {
	cp := make(map[string][]string, len(byApplication))
	for app, names := range byApplication {
		cp[app] = append([]string(nil), names...)
	}
	return staticUITools(cp)
}

type staticUITools map[string][]string

func (s staticUITools) UITools(_ context.Context, applicationID string) ([]string, error) {
	return s[applicationID], nil
}

// UIAction is a validated call of a UI tool for the client's screen: the name the host registered and the client
// declared, and arguments that satisfy the declared schema. It is data for the client; nothing in the core acts on it.
type UIAction struct {
	Name string
	// Arguments is the encoded JSON object, at most UIActionMaxArgsBytes.
	Arguments json.RawMessage
}

// The statuses of Trace.UIAction.
const (
	// UIActionOffered: UI tools were offered to the model and it proposed no call.
	UIActionOffered = "offered"
	// UIActionEmitted: the proposal's UI action was valid and rides on the result.
	UIActionEmitted = "emitted"
	// UIActionNotOffered: tools were declared and registered but not offered (budget or limits).
	UIActionNotOffered = "not_offered"
	// UIActionDropped: a valid UI action was not carried because the turn ended in a confirmation, a refusal or a failure.
	UIActionDropped = "dropped"
	// UIActionRefusedPrefix starts the status of a refused proposal; the rest is the reason (the UIReason constants).
	UIActionRefusedPrefix = "refused:"
)

// The reasons a UI action is refused. The turn fails closed with validation_failed and nothing is executed.
const (
	UIReasonReserved     = "reserved_name"     // a reserved name, or one that names a prepare or a commit
	UIReasonCapability   = "capability_name"   // the name of a capability of the application
	UIReasonUndeclared   = "undeclared"        // the client did not declare it for this run
	UIReasonUnregistered = "unregistered"      // the host did not register it for the application
	UIReasonNotOffered   = "not_offered"       // declared and registered, but not among the tools offered this turn
	UIReasonArguments    = "invalid_arguments" // not an object, too large, too deep or not valid against the declared schema
)

var (
	uiToolNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,63}$`)
	uiPropNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,31}$`)
)

// uiNameReserved reports whether a tool name is never allowed: the reserved alixai namespace (the confirmation and
// continue-in-app tools of the AG-UI mapping live there) and any name that has a "prepare" or a "commit" in it as a
// word. A UI tool runs on the client's screen; it is not how anything is prepared or committed.
func uiNameReserved(name string) bool {
	if strings.HasPrefix(strings.ToLower(name), "alixai") {
		return true
	}
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '.' || r == '_' || r == ':' || r == '-' }) {
		if w == "prepare" || w == "commit" {
			return true
		}
	}
	return false
}

// uiOffer is one tool the model is offered this turn.
type uiOffer struct {
	decl UIToolDeclaration
	line string
}

// uiState is what a turn knows about UI tools.
type uiState struct {
	offers   []uiOffer
	declared map[string]*UIToolDeclaration
	// registered is the host's allow-list for the application; nil when there is none or it could not be read.
	registered map[string]bool
}

func (u *uiState) offered(name string) *uiOffer {
	if u == nil {
		return nil
	}
	for i := range u.offers {
		if u.offers[i].decl.Name == name {
			return &u.offers[i]
		}
	}
	return nil
}

func (u *uiState) names() []string {
	if u == nil || len(u.offers) == 0 {
		return nil
	}
	out := make([]string, len(u.offers))
	for i, o := range u.offers {
		out[i] = o.decl.Name
	}
	return out
}

func (u *uiState) lines() []string {
	if u == nil {
		return nil
	}
	out := make([]string, len(u.offers))
	for i, o := range u.offers {
		out[i] = o.line
	}
	return out
}

// uiTools works out which tools the model is offered: declared by the client AND registered by the host for the
// application, a well-formed name that is neither reserved nor a capability's, a declaration that is a bounded
// object schema, and as many as the limits and the client block's share of the budget allow. Nothing here fails the
// turn: a tool that does not qualify is simply not offered (and a proposal naming it is refused later).
func (t *turn) uiTools(ctx context.Context) *uiState {
	decls := uiToolsFrom(ctx)
	if len(decls) == 0 {
		return nil
	}
	u := &uiState{declared: map[string]*UIToolDeclaration{}}
	for i := range decls {
		if _, dup := u.declared[decls[i].Name]; !dup {
			u.declared[decls[i].Name] = &decls[i]
		}
	}
	if t.e.uiTools != nil {
		names, err := t.e.uiTools.UITools(ctx, t.scope.ApplicationID)
		if err != nil {
			t.log("the ui tool registry failed", "code", string(contracts.CodeOf(err)))
		} else {
			u.registered = make(map[string]bool, len(names))
			for _, n := range names {
				u.registered[n] = true
			}
		}
	}
	share := t.e.cfg.Profile.ClientContextTokens() / 2 // the tool lines may take half of the block's share
	used := 0
	for _, d := range decls {
		if len(u.offers) == UIToolMaxOffers {
			break
		}
		if u.registered == nil || !u.registered[d.Name] || !uiToolNameRE.MatchString(d.Name) || uiNameReserved(d.Name) || t.namesCapability(d.Name) {
			continue
		}
		if u.offered(d.Name) != nil || !uiSchemaUsable(d.Parameters) {
			continue
		}
		line := uiToolLine(d)
		toks := t.e.builder.ClientTokens(line)
		if used+toks > share {
			continue
		}
		used += toks
		u.offers = append(u.offers, uiOffer{decl: d, line: line})
	}
	return u
}

// uiSchemaUsable is the cheap check made when a tool is offered: the declared parameters are a bounded JSON object
// schema. The schema is compiled only when the model chose the tool (compileUISchema).
func uiSchemaUsable(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return true // a tool with no parameters
	}
	if len(raw) > UIToolMaxSchemaBytes {
		return false
	}
	var s struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &s) == nil && (s.Type == "object" || s.Type == "") && raw[0] == '{'
}

// uiToolLine describes one tool inside the client block: its name, the client's description (one line, clipped) and a
// summary of its arguments made only of checked property names and fixed type words.
func uiToolLine(d UIToolDeclaration) string {
	desc := clipRunes(strings.Join(strings.Fields(sanitize(d.Description)), " "), uiToolDescRunes)
	if desc == "" {
		desc = "no description"
	}
	return "UI tool " + d.Name + ": " + desc + " Arguments: " + uiArgSummary(d.Parameters)
}

// uiArgSummary lists the top-level properties of a declared object schema as name:type, a star marking a required
// one, in name order. A property whose name is not a plain identifier is left out; the type is one of a fixed set.
func uiArgSummary(raw json.RawMessage) string {
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &s) != nil || len(s.Properties) == 0 {
		return "none"
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		if uiPropNameRE.MatchString(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) > uiToolMaxProps {
		names = names[:uiToolMaxProps]
	}
	if len(names) == 0 {
		return "none"
	}
	parts := make([]string, len(names))
	for i, n := range names {
		typ := "any"
		if ts, ok := s.Properties[n].Type.(string); ok {
			switch ts {
			case "string", "number", "integer", "boolean", "array", "object":
				typ = ts
			}
		}
		parts[i] = n + ":" + typ
		if required[n] {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, ", ")
}

// refuseLoader is the schema loader of the client's schemas: it loads nothing. A declared schema that points at a
// file or an address ($ref) does not compile, so no URL a client names is ever fetched or read.
type refuseLoader struct{}

func (refuseLoader) Load(string) (any, error) {
	return nil, errors.New("a declared tool schema may not reference another resource")
}

// compileUISchema compiles the JSON Schema a client declared for a tool, with no way to load anything outside it.
func compileUISchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{"type":"object"}`)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(refuseLoader{})
	const url = "urn:alixai:schema:dialogue:ui-tool-declared:v1"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// checkUIAction validates the UI action of a proposal. It returns the action to carry, or the reason it is refused. The
// order is the order of severity: a name that is reserved or that names a capability is refused whatever else is true
// of it. A refusal fails the whole proposal.
func (t *turn) checkUIAction(a conversation.TaskProposalUIAction, u *uiState) (*UIAction, string) {
	name := a.Name
	switch {
	case uiNameReserved(name):
		return nil, UIReasonReserved
	case t.namesCapability(name):
		return nil, UIReasonCapability
	case u == nil || u.declared[name] == nil:
		return nil, UIReasonUndeclared
	case u.registered == nil || !u.registered[name]:
		return nil, UIReasonUnregistered
	}
	o := u.offered(name)
	if o == nil {
		return nil, UIReasonNotOffered
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a.Arguments); err != nil || a.Arguments == nil {
		return nil, UIReasonArguments
	}
	raw := bytes.TrimSpace(buf.Bytes())
	if len(raw) > UIActionMaxArgsBytes || jsonDepth(a.Arguments) > uiActionMaxDepth {
		return nil, UIReasonArguments
	}
	sch, err := compileUISchema(o.decl.Parameters)
	if err != nil {
		return nil, UIReasonArguments
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil || sch.Validate(inst) != nil {
		return nil, UIReasonArguments
	}
	return &UIAction{Name: name, Arguments: append(json.RawMessage(nil), raw...)}, ""
}

// namesCapability reports whether a name is the id of a capability of the application, in any case.
func (t *turn) namesCapability(name string) bool {
	if t.pkg == nil {
		return false
	}
	for _, id := range t.pkg.CapabilityIDs() {
		if strings.EqualFold(id, name) {
			return true
		}
	}
	return false
}

// jsonDepth is the nesting depth of a decoded JSON value.
func jsonDepth(v any) int {
	switch x := v.(type) {
	case map[string]any:
		d := 0
		for _, e := range x {
			d = max(d, jsonDepth(e))
		}
		return d + 1
	case []any:
		d := 0
		for _, e := range x {
			d = max(d, jsonDepth(e))
		}
		return d + 1
	}
	return 0
}

// uiActionActs are the acts of a directive a UI action may ride on: the turn asked for something, answered with a
// result or listed what can be done. Never a confirmation, a step-up, a refusal or a failure.
func uiActionAct(a conversation.DialogueDirectiveAct) bool {
	switch a {
	case conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActOfferHelp:
		return true
	}
	return false
}

// buildWithClientData builds the proposal prompt with what the client's screen supplied: the UI tools the model is
// offered (their descriptions pinned at the head of the client block, their bare names in the frame and the
// constraint) and the readable context. If offering the tools would cost the turn anything it has without them (the
// build fails, or the shortlist has to be cut further), the turn goes on without the tools and the trace says so: a
// UI tool never takes a capability's place in the prompt.
func (t *turn) buildWithClientData(ctx context.Context, in proposalInput) (builtPrompt, error) {
	t.ui = t.uiTools(ctx)
	cc := clientContextFrom(ctx)
	in.Client = clientBlock(nil, cc)
	if len(in.Client.Lines) == 0 {
		in.Client = dctx.Client{}
	}
	names := t.ui.names()
	if len(names) == 0 {
		if t.ui != nil {
			t.trace.UIAction = UIActionNotOffered
		}
		return buildProposal(ctx, t.e.builder, t.scope, in)
	}
	plain, perr := buildProposal(ctx, t.e.builder, t.scope, in)
	in.UINames, in.Client = names, clientBlock(t.ui, cc)
	built, berr := buildProposal(ctx, t.e.builder, t.scope, in)
	if berr != nil || (perr == nil && len(built.Caps) < len(plain.Caps)) {
		t.ui.offers = nil
		t.trace.UIAction = UIActionNotOffered
		return plain, perr
	}
	t.trace.UIAction = UIActionOffered
	return built, nil
}

// clientBlock assembles the client block of the proposal prompt: the descriptions of the offered UI tools first,
// pinned, then the lines of readable context.
func clientBlock(u *uiState, cc ClientContext) dctx.Client {
	tools := u.lines()
	return dctx.Client{Lines: append(tools, cc.Lines...), Pinned: len(tools), Dropped: cc.Dropped}
}
