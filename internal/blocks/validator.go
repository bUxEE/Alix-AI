package blocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Limits bound what one block may carry. The published schema bounds each
// member but not their product (1000 rows of 32 cells of 1024 characters is a
// block of tens of megabytes), so the core holds every block to a total size as
// well. A zero field takes its default; a limit above the schema's own maximum
// has no effect, since the schema is checked too.
type Limits struct {
	// MaxBlockBytes is the encoded JSON size of one block. Default 256 KiB.
	MaxBlockBytes int
	// MaxMarkdownBytes is the size of the Markdown source of one markdown
	// block. Default 64 KiB.
	MaxMarkdownBytes int
	// MaxResponseBytes is the size of a whole response text given to
	// SanitizeResponse. Default 256 KiB.
	MaxResponseBytes int
	// MaxTableRows, MaxTableColumns and MaxTableCells bound a table. Defaults
	// 200, 16 and 2000.
	MaxTableRows    int
	MaxTableColumns int
	MaxTableCells   int
	// MaxChartPoints bounds the records of a chart. Default 500.
	MaxChartPoints int
	// MaxDiagramBytes and MaxDiagramLines bound a diagram source. Defaults
	// 20000 and 400.
	MaxDiagramBytes int
	MaxDiagramLines int
}

// DefaultLimits are the limits a Validator uses unless told otherwise.
func DefaultLimits() Limits {
	return Limits{
		MaxBlockBytes:    256 << 10,
		MaxMarkdownBytes: 64 << 10,
		MaxResponseBytes: 256 << 10,
		MaxTableRows:     200,
		MaxTableColumns:  16,
		MaxTableCells:    2000,
		MaxChartPoints:   500,
		MaxDiagramBytes:  20000,
		MaxDiagramLines:  400,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	return Limits{
		MaxBlockBytes:    pick(l.MaxBlockBytes, d.MaxBlockBytes),
		MaxMarkdownBytes: pick(l.MaxMarkdownBytes, d.MaxMarkdownBytes),
		MaxResponseBytes: pick(l.MaxResponseBytes, d.MaxResponseBytes),
		MaxTableRows:     pick(l.MaxTableRows, d.MaxTableRows),
		MaxTableColumns:  pick(l.MaxTableColumns, d.MaxTableColumns),
		MaxTableCells:    pick(l.MaxTableCells, d.MaxTableCells),
		MaxChartPoints:   pick(l.MaxChartPoints, d.MaxChartPoints),
		MaxDiagramBytes:  pick(l.MaxDiagramBytes, d.MaxDiagramBytes),
		MaxDiagramLines:  pick(l.MaxDiagramLines, d.MaxDiagramLines),
	}
}

// Options configure a Validator. The zero value is the safest one: default
// limits and no link allowed in Markdown.
type Options struct {
	Limits Limits
	// LinkSchemes are the URL schemes a Markdown link may use ("https",
	// "mailto"). With none, every link is reduced to its text. javascript,
	// data, vbscript and file are never honoured, even if listed: a host that
	// lists one has made a mistake the core does not carry out.
	LinkSchemes []string
}

// neverSchemes cannot be enabled.
var neverSchemes = map[string]bool{"javascript": true, "data": true, "vbscript": true, "livescript": true, "file": true, "blob": true, "filesystem": true}

// Context is what a block is checked against. It belongs to one turn.
type Context struct {
	// Facts are the authoritative numbers of the turn: the facts of the
	// DialogueDirective (and the receipt). A chart may show only numbers that
	// are in the set; with no facts, a chart that carries any number is
	// refused. A chart is therefore never a way to state a number the
	// application did not state.
	Facts *FactSet
}

// Validator validates and sanitises rich blocks. It is safe for concurrent use.
type Validator struct {
	limits Limits
	md     *markdownSanitizer
}

// New returns a Validator.
func New(opts Options) *Validator {
	var schemes []string
	for _, s := range opts.LinkSchemes {
		if !neverSchemes[lower(s)] {
			schemes = append(schemes, s)
		}
	}
	lim := opts.Limits.withDefaults()
	return &Validator{limits: lim, md: newMarkdownSanitizer(schemes)}
}

var (
	defaultOnce sync.Once
	defaultVal  *Validator
)

// Default is the Validator with the zero Options.
func Default() *Validator {
	defaultOnce.Do(func() { defaultVal = New(Options{}) })
	return defaultVal
}

// SanitizeMarkdown returns the canonical, sanitised form of a Markdown text as
// it would appear in a markdown block (at most Limits.MaxMarkdownBytes).
func (v *Validator) SanitizeMarkdown(text string) (string, MarkdownReport, error) {
	return v.md.sanitize(text, v.limits.MaxMarkdownBytes)
}

// SanitizeResponse sanitises the whole text of a response before it is cut into
// token events (at most Limits.MaxResponseBytes). It must be given the whole
// text: a fragment cut at an arbitrary place is not a Markdown document, and
// sanitising pieces separately would change what the pieces say together.
func (v *Validator) SanitizeResponse(text string) (string, MarkdownReport, error) {
	return v.md.sanitize(text, v.limits.MaxResponseBytes)
}

// JSON validates the JSON form of a block (what a model or a host produced) and
// returns the sanitised block. The raw document is checked against the
// published schema first, so an unknown member, a wrong type or a missing
// member is refused before anything is decoded.
func (v *Validator) JSON(raw []byte, ctx Context) (conversation.RichBlock, error) {
	if len(raw) > v.limits.MaxBlockBytes {
		return conversation.RichBlock{}, reject("", Violation{Rule: RuleBlockTooLarge})
	}
	viol, err := schemaViolations(raw)
	if err != nil {
		return conversation.RichBlock{}, err
	}
	if len(viol) > 0 {
		return conversation.RichBlock{}, reject(blockTypeOf(raw), viol...)
	}
	var b conversation.RichBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return conversation.RichBlock{}, reject(blockTypeOf(raw), Violation{Rule: RuleSchema})
	}
	return v.Block(b, ctx)
}

// modelKinds are the block types a model may author: text and the data blocks
// whose every value the validator checks (a chart against the facts of the
// turn). Citation lists, action cards, clarification choices and confirmation
// panels are assembled by the core from host and core data; one written by a
// model would put a model's words next to a confirm button.
var modelKinds = map[string]bool{"markdown": true, "code": true, "table": true, "chart": true, "diagram": true}

// ModelAuthorable reports whether a model may author a block of type typ.
func ModelAuthorable(typ string) bool { return modelKinds[typ] }

// ModelJSON is JSON for a block a model wrote (a provider's reply, untrusted):
// a block of a kind only the core assembles is refused (RuleNotModelKind)
// before anything else is checked, and the rest is validated as JSON does.
func (v *Validator) ModelJSON(raw []byte, ctx Context) (conversation.RichBlock, error) {
	if len(raw) > v.limits.MaxBlockBytes {
		return conversation.RichBlock{}, reject("", Violation{Rule: RuleBlockTooLarge})
	}
	if typ := blockTypeOf(raw); !ModelAuthorable(typ) {
		return conversation.RichBlock{}, reject(typ, Violation{Rule: RuleNotModelKind, Path: "/type"})
	}
	b, err := v.JSON(raw, ctx)
	if err != nil {
		return conversation.RichBlock{}, err
	}
	// The kind is checked again on what was decoded, so no reading of the raw text can disagree with it.
	if enc, err := json.Marshal(b); err != nil || !ModelAuthorable(blockTypeOf(enc)) {
		return conversation.RichBlock{}, reject(blockTypeOf(enc), Violation{Rule: RuleNotModelKind, Path: "/type"})
	}
	return b, nil
}

// Block validates a block and returns the sanitised block: the Markdown of a
// markdown block is replaced by its canonical sanitised form; every other block
// is returned as it came if it passes and refused if it does not. An error is a
// contracts.Error with code schema_invalid whose cause lists the violations
// (see Violations); the block is not emitted, and never repaired by the model.
func (v *Validator) Block(b conversation.RichBlock, ctx Context) (conversation.RichBlock, error) {
	if b.Value == nil {
		return conversation.RichBlock{}, reject("", Violation{Rule: RuleNoVariant})
	}
	raw, err := json.Marshal(b)
	if err != nil {
		// The generated encoder refuses an enum value outside the schema's.
		return conversation.RichBlock{}, reject("", Violation{Rule: RuleSchema})
	}
	if len(raw) > v.limits.MaxBlockBytes {
		return conversation.RichBlock{}, reject(blockTypeOf(raw), Violation{Rule: RuleBlockTooLarge})
	}
	// Work on what is on the wire: decode the encoded form, so a pointer variant
	// or a value the encoder normalised is checked as it would be received.
	var cur conversation.RichBlock
	if err := json.Unmarshal(raw, &cur); err != nil {
		return conversation.RichBlock{}, reject(blockTypeOf(raw), Violation{Rule: RuleSchema})
	}
	if md, ok := cur.Value.(conversation.MarkdownBlock); ok {
		clean, _, err := v.md.sanitize(md.Text, v.limits.MaxMarkdownBytes)
		if err != nil {
			return conversation.RichBlock{}, err
		}
		md.Text = clean
		cur.Value = md
		if raw, err = json.Marshal(cur); err != nil {
			return conversation.RichBlock{}, reject("markdown", Violation{Rule: RuleSchema})
		}
	}
	typ := blockTypeOf(raw)
	viol, err := schemaViolations(raw)
	if err != nil {
		return conversation.RichBlock{}, err
	}
	if len(viol) > 0 {
		return conversation.RichBlock{}, reject(typ, viol...)
	}
	col := &collector{}
	v.checkBlock(cur.Value, ctx, raw, col)
	if err := col.err(typ); err != nil {
		return conversation.RichBlock{}, err
	}
	return cur, nil
}

// checkBlock runs the checks only the core can make.
func (v *Validator) checkBlock(val conversation.RichBlockValue, ctx Context, raw []byte, col *collector) {
	switch b := val.(type) {
	case conversation.MarkdownBlock:
		// sanitised above; the output is held to the schema's text rules too.
	case conversation.CodeBlock:
		if rule := textRule(b.Code, kindCode); rule != "" {
			col.add(rule, "/code")
		}
	case conversation.TableBlock:
		v.checkTable(b, ctx, col)
	case conversation.ChartBlock:
		v.checkChart(b, ctx, col)
	case conversation.DiagramBlock:
		v.checkDiagram(b, col)
	case conversation.ClarificationChoicesBlock:
		if b.Prompt != nil {
			if rule := textRule(*b.Prompt, kindLine); rule != "" {
				col.add(rule, "/prompt")
			}
		}
		checkHostStrings(raw, col)
	default:
		// citation_list, action_card, confirmation_panel: assembled by the core
		// from host and core data. Their strings are host text, never model text.
		checkHostStrings(raw, col)
	}
}

// checkHostStrings holds every string of a block the core assembled to the
// host profile: no control character, no bidirectional override, no hidden tag
// character. The text is shown to the user next to a confirm button, so what
// the reader sees must be what the string says.
func checkHostStrings(raw []byte, col *collector) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		col.add(RuleNotJSON, "")
		return
	}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		if len(col.vs) >= maxReportedViolations {
			return
		}
		switch x := v.(type) {
		case string:
			if rule := textRule(x, kindHost); rule != "" {
				col.add(rule, path)
			}
		case []any:
			for i, e := range x {
				walk(e, fmt.Sprintf("%s/%d", path, i))
			}
		case map[string]any:
			for k, e := range x {
				walk(e, path+"/"+safeToken(k))
			}
		}
	}
	walk(doc, "")
}

// Event validates one TurnEvent before it is sent. Block and clarification
// events carry a rich block, which is validated and sanitised (the returned
// event holds the sanitised block). A token event's text is checked for what
// no fragment may hold (invalid UTF-8, control and invisible characters); its
// Markdown is sanitised with SanitizeResponse over the whole response before
// the response is cut into tokens, never per fragment: internal/stream's
// Turn.PublishResponse does exactly that, and a producer that publishes token
// events by any other route has not sanitised its text. Every other event is
// core data and is returned unchanged.
func (v *Validator) Event(ev conversation.TurnEvent, ctx Context) (conversation.TurnEvent, error) {
	switch e := ev.Value.(type) {
	case conversation.BlockEvent:
		b, err := v.Block(e.Block, ctx)
		if err != nil {
			return conversation.TurnEvent{}, err
		}
		e.Block = b
		return conversation.TurnEvent{Value: e}, nil
	case conversation.ClarificationEvent:
		b, err := v.Block(conversation.RichBlock{Value: e.Clarification}, ctx)
		if err != nil {
			return conversation.TurnEvent{}, err
		}
		cb, ok := b.Value.(conversation.ClarificationChoicesBlock)
		if !ok {
			return conversation.TurnEvent{}, reject("clarification_choices", Violation{Rule: RuleSchema})
		}
		e.Clarification = cb
		return conversation.TurnEvent{Value: e}, nil
	case conversation.TokenEvent:
		if rule := textRule(e.Text, kindLines); rule != "" {
			return conversation.TurnEvent{}, reject("token", Violation{Rule: rule, Path: "/text"})
		}
		return ev, nil
	case nil:
		return conversation.TurnEvent{}, reject("", Violation{Rule: RuleNoVariant})
	case conversation.DirectiveEvent, conversation.PreparedActionEvent, conversation.ReceiptEvent,
		conversation.CitationEvent, conversation.ProgressEvent, conversation.ErrorEvent, conversation.DoneEvent:
		return ev, nil
	}
	// A pointer variant: check it as it is on the wire.
	raw, err := json.Marshal(ev)
	if err != nil {
		return conversation.TurnEvent{}, reject("", Violation{Rule: RuleSchema})
	}
	var cur conversation.TurnEvent
	if err := json.Unmarshal(raw, &cur); err != nil {
		return conversation.TurnEvent{}, reject("", Violation{Rule: RuleSchema})
	}
	switch cur.Value.(type) {
	case conversation.BlockEvent, conversation.ClarificationEvent, conversation.TokenEvent:
		return v.Event(cur, ctx)
	}
	return cur, nil
}

// blockTypeOf reads the "type" member of an encoded block, for messages.
func blockTypeOf(raw []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	for _, r := range probe.Type {
		if r < 'a' || r > 'z' {
			if r != '_' {
				return ""
			}
		}
	}
	if len(probe.Type) > 32 {
		return ""
	}
	return probe.Type
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
