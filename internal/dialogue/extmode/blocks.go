package extmode

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// BlockRefusal says why a part of a provider reply did not become a block: the kind of part and the
// rule it failed, never its content.
type BlockRefusal struct {
	Part int
	Kind providers.Modality
	Rule string
}

// RichBlocks turns the rich reply of a provider into validated AlixAI response blocks (additive plan
// 10.3). The provider's Markdown, HTML, image, chart and diagram output is untrusted data: text becomes
// a Markdown block rewritten to canonical, sanitised Markdown (raw HTML dropped); a text part that is
// a JSON block (a table, a chart, a diagram) is validated against the published block schema and,
// for a chart, against the facts of the turn in ctx; image, audio and file parts are never a block.
// What fails is refused and reported by rule, not repaired.
func RichBlocks(v *blocks.Validator, ctx blocks.Context, res providers.RichResult) ([]conversation.RichBlock, []BlockRefusal) {
	if v == nil {
		v = blocks.Default()
	}
	var out []conversation.RichBlock
	var refused []BlockRefusal
	refuse := func(i int, k providers.Modality, err error) {
		rule := "invalid"
		if vs := blocks.Violations(err); len(vs) > 0 {
			rule = string(vs[0].Rule)
		}
		refused = append(refused, BlockRefusal{Part: i, Kind: k, Rule: rule})
	}
	for i, p := range res.Parts {
		if p.Kind != providers.ModText {
			refused = append(refused, BlockRefusal{Part: i, Kind: p.Kind, Rule: "media_is_not_a_block"})
			continue
		}
		raw := bytes.TrimSpace([]byte(p.Text))
		var b conversation.RichBlock
		var err error
		if len(raw) > 0 && raw[0] == '{' && json.Valid(raw) {
			b, err = v.ModelJSON(raw, ctx) // never an action card, a confirmation panel or another core-only kind
		} else if strings.TrimSpace(p.Text) != "" {
			b, err = v.Block(conversation.RichBlock{Value: conversation.MarkdownBlock{Type: "markdown", Text: p.Text}}, ctx)
		} else {
			continue
		}
		if err != nil {
			refuse(i, p.Kind, err)
			continue
		}
		out = append(out, b)
	}
	return out, refused
}
