package extmodal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// GeneratedStore is the only storage a provider reply can reach: the local file service, narrowed to
// storing a new generated file and withdrawing it. There is no way to open, overwrite or replace an
// existing file through it, and it is never handed to a provider adapter: the bytes of a reply are
// copied into it here, by this package. *files.Service satisfies it.
type GeneratedStore interface {
	Put(ctx context.Context, scope contracts.Scope, in files.PutInput) (conversation.FileRef, error)
	Delete(ctx context.Context, scope contracts.Scope, id string) error
}

// IngestOptions adjust how a reply is turned into AlixAI output.
type IngestOptions struct {
	// Facts are the turn's authoritative numbers: a chart may show only these. With none, a chart that
	// carries a number is refused.
	Facts *blocks.FactSet
	// Treatment is how generated files are kept. Empty means ephemeral turn context. An application
	// object and a RAG source are never produced from a provider reply.
	Treatment files.Treatment
}

// Rejection is an output block that was dropped. It names the kind and the first rule, never content.
type Rejection struct {
	Index int
	Kind  string
	Rule  string
}

// Output is a provider reply after validation: only AlixAI-owned things.
type Output struct {
	// Blocks are validated, sanitised rich blocks.
	Blocks []conversation.RichBlock
	// Files are AlixAI-managed references to media that was stored locally first.
	Files []conversation.FileRef
	// Rejected are blocks that failed validation and were dropped, not repaired.
	Rejected []Rejection
	// Sanitised sums what the Markdown sanitiser removed from the reply's text.
	Sanitised blocks.MarkdownReport
	Usage     providers.CostUsage
	// Disclosures are what the provider keeps of the modalities this call used.
	Disclosures []Disclosure
}

const (
	maxReplyParts  = 32
	maxReplyBlocks = 16
)

func outputInvalid(msg string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "extmodal: "+msg).WithDetails("reason", string(ReasonOutputInvalid))
}

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func extFor(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "application/pdf":
		return "pdf"
	case "text/markdown":
		return "md"
	case "text/csv":
		return "csv"
	case "application/json":
		return "json"
	case "text/plain":
		return "txt"
	}
	return "bin"
}

func modalityOfPart(k providers.Modality) Modality {
	switch k {
	case providers.ModImage:
		return ModImageGeneration
	case providers.ModAudio:
		return ModTTS
	}
	return ModFileInput
}

// Ingest turns a provider reply into AlixAI output. The reply is untrusted data:
//
//  1. Every part is validated first, before anything is stored: a provider cannot name a reference,
//     a path or a URL, media types are an allowlist (no SVG, no HTML), sizes are bounded, and a kind
//     of media is accepted only when its modality is admitted (image: image generation, audio: TTS,
//     file: file input).
//  2. Media is then stored locally, one new generated file each, with a name chosen here; a store
//     failure withdraws what was stored and fails the whole reply. Only the AlixAI reference leaves.
//  3. Text becomes sanitised markdown blocks; block JSON is validated by internal/blocks (charts
//     against the turn's facts). An invalid block is dropped and reported, never repaired.
//
// Tool-call proposals are not handled here: they go through the normal proposal validation.
func (s *Service) Ingest(ctx context.Context, scope contracts.Scope, t Target, res providers.RichResult, opts IngestOptions) (Output, error) {
	if err := scope.Validate(); err != nil {
		return Output{}, err
	}
	treatment := opts.Treatment
	if treatment == "" {
		treatment = files.TreatmentEphemeral
	}
	if treatment != files.TreatmentEphemeral {
		return Output{}, invalid(ModImageGeneration, "generated files are kept as ephemeral turn context only")
	}
	if len(res.Parts) > maxReplyParts {
		return Output{}, outputInvalid("the reply has too many parts")
	}
	var media []providers.ContentPart
	need := map[providers.Modality]bool{}
	images := 0
	for _, p := range res.Parts {
		if err := p.ValidateOutput(); err != nil {
			return Output{}, contracts.Wrap(contracts.ErrSchemaInvalid, err, "extmodal: the provider reply is not valid").WithDetails("reason", string(ReasonOutputInvalid))
		}
		if p.Kind == providers.ModText {
			continue
		}
		media = append(media, p)
		need[p.Kind] = true
		if p.Kind == providers.ModImage {
			images++
		}
	}
	if max := t.Profile.ImageGeneration.MaxImagesPerRequest; max != nil && int64(images) > *max {
		return Output{}, outputInvalid("the reply has more images than the provider profile allows")
	}
	var out Output
	out.Usage = res.Usage
	for _, k := range []providers.Modality{providers.ModImage, providers.ModAudio, providers.ModFile} {
		if !need[k] {
			continue
		}
		adm, err := s.Admit(ctx, scope, t, modalityOfPart(k))
		if err != nil {
			return Output{}, err
		}
		out.Disclosures = append(out.Disclosures, adm.Disclosure)
	}
	if len(media) > 0 && s.d.Store == nil {
		return Output{}, refuse(ModImageGeneration, ReasonStoreRefused, "no local store is configured for generated files")
	}

	// Store locally first. Nothing the provider produced is reachable except through these references.
	var stored []conversation.FileRef
	for i, p := range media {
		ref, err := s.d.Store.Put(ctx, scope, files.PutInput{
			Name:         fmt.Sprintf("generated-%d.%s", i+1, extFor(p.MediaType)),
			DeclaredMIME: p.MediaType,
			Treatment:    treatment,
			Origin:       files.OriginGenerated,
			Content:      bytes.NewReader(p.Data),
		})
		if err != nil {
			s.withdraw(scope, stored)
			return Output{}, contracts.Wrap(contracts.CodeOf(err), err, "extmodal: the local store refused the generated file").
				WithDetails("reason", string(ReasonStoreRefused)).WithDetails("modality", string(modalityOfPart(p.Kind)))
		}
		stored = append(stored, ref)
	}
	out.Files = stored

	// Text and blocks. Untrusted until they pass the validator.
	idx := 0
	for _, p := range res.Parts {
		if p.Kind != providers.ModText {
			continue
		}
		clean, rep, err := s.val.SanitizeMarkdown(p.Text)
		out.Sanitised.HTMLRemoved += rep.HTMLRemoved
		out.Sanitised.ImagesRemoved += rep.ImagesRemoved
		out.Sanitised.LinksRemoved += rep.LinksRemoved
		out.Sanitised.Degraded = out.Sanitised.Degraded || rep.Degraded
		if err != nil {
			out.Rejected = append(out.Rejected, Rejection{Index: idx, Kind: "markdown", Rule: ruleOf(err)})
		} else if strings.TrimSpace(clean) != "" {
			out.Blocks = append(out.Blocks, conversation.RichBlock{Value: conversation.MarkdownBlock{Text: clean}})
		}
		idx++
	}
	if len(res.JSON) > 0 {
		for _, raw := range splitBlocks(res.JSON) {
			if len(out.Blocks)+len(out.Rejected) >= maxReplyBlocks {
				out.Rejected = append(out.Rejected, Rejection{Index: idx, Kind: "block", Rule: "too_many_blocks"})
				break
			}
			// A provider writes only the kinds a model may author: never an action card or a confirmation panel.
			b, err := s.val.ModelJSON(raw, blocks.Context{Facts: opts.Facts})
			if err != nil {
				out.Rejected = append(out.Rejected, Rejection{Index: idx, Kind: kindOf(raw), Rule: ruleOf(err)})
			} else {
				out.Blocks = append(out.Blocks, b)
			}
			idx++
		}
	}
	return out, nil
}

func (s *Service) withdraw(scope contracts.Scope, refs []conversation.FileRef) {
	for _, r := range refs {
		// Best effort: the files are ephemeral and expire anyway.
		_ = s.d.Store.Delete(context.WithoutCancel(context.Background()), scope, string(r.FileID))
	}
}

// splitBlocks reads the reply's structured JSON as one block, a list of blocks, or {"blocks": [...]}.
func splitBlocks(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var wrapped struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Blocks != nil {
		return wrapped.Blocks
	}
	return []json.RawMessage{raw}
}

func kindOf(raw json.RawMessage) string {
	var h struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &h) != nil || len(h.Type) > 32 {
		return "unknown"
	}
	for _, r := range h.Type {
		if r < 'a' || r > 'z' {
			if r != '_' {
				return "unknown"
			}
		}
	}
	if h.Type == "" {
		return "unknown"
	}
	return h.Type
}

func ruleOf(err error) string {
	if v := blocks.Violations(err); len(v) > 0 {
		return string(v[0].Rule)
	}
	return "invalid"
}

// GenerateImage asks an image-capable provider for an image from a prompt. The modality gate runs
// first, then the egress guard on the prompt; the images that come back are stored locally and
// returned as AlixAI references.
func (s *Service) GenerateImage(ctx context.Context, scope contracts.Scope, t Target, gen providers.MultimodalGenerator, prompt string, opts IngestOptions) (Output, error) {
	if gen == nil {
		return Output{}, contracts.New(contracts.ErrUnavailable, "extmodal: no external generator is installed")
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > maxPromptBytes {
		return Output{}, invalid(ModImageGeneration, "the prompt is empty or too long")
	}
	adm, err := s.Admit(ctx, scope, t, ModImageGeneration)
	if err != nil {
		return Output{}, err
	}
	decision, err := s.allowText(ctx, scope, t, ModImageGeneration, PurposeImagePrompt, prompt)
	if err != nil {
		return Output{}, err
	}
	rr := providers.RichRequest{
		Messages:         []providers.RichMessage{{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart(prompt)}}},
		OutputModalities: []providers.Modality{providers.ModImage},
	}
	if err := rr.Validate(); err != nil {
		return Output{}, err
	}
	if err := t.Caps.Check(rr); err != nil {
		return Output{}, err
	}
	// The bound: the prompt, the most images a reply may carry (Ingest refuses more) and, where the profile prices
	// text output, the most text the model can add.
	units := richUnits(rr)
	units.ImagesOut = maxReplyParts
	if n := t.Profile.ImageGeneration.MaxImagesPerRequest; n != nil && *n > 0 && *n < maxReplyParts {
		units.ImagesOut = pv.Counter(*n)
	}
	if t.Profile.Pricing.Prices.ToksOut != nil {
		if units.ToksOut = pv.Counter(t.outputCap()); units.ToksOut == 0 {
			return Output{}, notEstimable(ModImageGeneration, "the reply has no output bound")
		}
	}
	h, err := s.reserve(ctx, scope, t, ModImageGeneration, decision, units)
	if err != nil {
		return Output{}, err
	}
	start := s.clk.Now()
	res, err := gen.Generate(ctx, scope, rr)
	if err != nil {
		h.failed(ctx, err)
		return Output{}, err
	}
	h.succeeded(ctx, res.Usage)
	out, err := s.Ingest(ctx, scope, t, res, opts)
	if err != nil {
		return Output{}, err
	}
	var recv int64
	for _, p := range res.Parts {
		recv += int64(len(p.Data))
	}
	s.observe(Event{
		Modality: ModImageGeneration, Provider: adm.Disclosure.Provider, Model: adm.Disclosure.Model, Allowed: true, Route: "external",
		BytesSent: int64(len(prompt)), BytesReceived: recv, Latency: s.clk.Now().Sub(start),
	})
	if len(out.Files) == 0 {
		return Output{}, outputInvalid("the provider returned no image")
	}
	return out, nil
}
