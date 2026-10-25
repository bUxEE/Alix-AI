package providersuite

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func multimodalChecks() []Check {
	return []Check{
		{"multimodal", "image_input_output", checkImageInputOutput},
		{"multimodal", "file_input_output", checkFileInputOutput},
		{"multimodal", "audio_input_output", checkAudioInputOutput},
		{"multimodal", "cancellation_and_partial_stream", checkCancellationAndPartialStream},
		{"multimodal", "sanitized_rich_blocks", checkSanitizedRichBlocks},
	}
}

// Minimal bytes of each produced media type.
var (
	pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	pdfBytes = []byte("%PDF-1.4\n%fixture\n")
	wavBytes = []byte("RIFF\x24\x00\x00\x00WAVEfmt ")
)

type mediaCase struct {
	kind      providers.Modality
	declared  bool
	in        providers.ContentPart
	outType   string
	outBytes  []byte
	inReason  string
	outReason string
	undeclare string
}

func (m mediaCase) request(withOutput bool) providers.RichRequest {
	req := providers.RichRequest{
		Messages: []providers.RichMessage{
			sysPart(sysPrompt),
			{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("Describe the attached item."), m.in}},
		},
		MaxToks: 256,
	}
	if withOutput {
		req.OutputModalities = []providers.Modality{m.kind}
	}
	return req
}

// checkMedia is the shared body of the three conditional checks. A provider
// that does not declare the capability must refuse the request before anything
// is sent (and the check is not_applicable); one that declares it must carry
// the input to the upstream and return only valid output parts.
func checkMedia(ctx context.Context, s Subject, m mediaCase) Outcome {
	sc := scopeOf(tenantA)
	if !m.declared {
		before := len(s.Upstream.Seen())
		_, err := generate(ctx, s, sc, m.request(false))
		if err == nil {
			return fail(m.undeclare, "a %s input was accepted by a provider that does not declare it", m.kind)
		}
		if _, ok := providers.IsUnsupported(err); !ok {
			return fail(m.undeclare, "a %s input was refused for the wrong reason (code %s)", m.kind, codeOf(err))
		}
		if len(s.Upstream.Seen()) != before {
			return fail(m.undeclare, "a refused %s input still reached the upstream", m.kind)
		}
		return notApplicable()
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: "It is an item.", ToksIn: 300, ToksOut: 6})
	if _, err := generate(ctx, s, sc, m.request(false)); err != nil {
		return fail("media_generate_failed", "a %s input failed: code %s", m.kind, codeOf(err))
	}
	seen := s.Upstream.Seen()
	got := false
	for _, k := range seen[len(seen)-1].Media {
		got = got || k == string(m.kind)
	}
	if !got {
		return fail(m.inReason, "the %s input did not reach the upstream", m.kind)
	}
	outDeclared := false
	for _, x := range s.Profile.Modalities.Output {
		outDeclared = outDeclared || string(x) == string(m.kind)
	}
	if outDeclared && !s.Natural {
		s.Upstream.Script(Scenario{Kind: ScMedia, Text: "Here it is.", Media: MediaOut{Kind: m.kind, MediaType: m.outType, Data: m.outBytes}, ToksIn: 100, ToksOut: 50})
		res, err := generate(ctx, s, sc, m.request(true))
		if err != nil {
			return fail("media_generate_failed", "a %s output failed: code %s", m.kind, codeOf(err))
		}
		found := false
		for _, p := range res.Parts {
			if err := p.ValidateOutput(); err != nil {
				return fail(m.outReason, "a returned part is not valid output")
			}
			if p.Kind == m.kind {
				found = true
				if !bytes.Equal(p.Data, m.outBytes) || p.RefID != "" {
					return fail(m.outReason, "the %s output changed or carries a reference", m.kind)
				}
			}
		}
		if !found {
			return fail(m.outReason, "the %s output is missing", m.kind)
		}
	}
	return pass()
}

// 15.4 image input/output where declared.
func checkImageInputOutput(ctx context.Context, s Subject) Outcome {
	img, _, _ := s.declared()
	return checkMedia(ctx, s, mediaCase{
		kind: providers.ModImage, declared: img, in: providers.ImageRef("img_conf_1", "image/png"),
		outType: "image/png", outBytes: pngBytes,
		inReason: "image_input_dropped", outReason: "image_output_invalid", undeclare: "undeclared_image_accepted",
	})
}

// 15.4 file input and output references where declared.
func checkFileInputOutput(ctx context.Context, s Subject) Outcome {
	_, file, _ := s.declared()
	return checkMedia(ctx, s, mediaCase{
		kind: providers.ModFile, declared: file, in: providers.FileRef("fil_conf_1", "application/pdf"),
		outType: "application/pdf", outBytes: pdfBytes,
		inReason: "file_input_dropped", outReason: "file_output_invalid", undeclare: "undeclared_file_accepted",
	})
}

// 15.4 audio input/output where declared.
func checkAudioInputOutput(ctx context.Context, s Subject) Outcome {
	_, _, audio := s.declared()
	return checkMedia(ctx, s, mediaCase{
		kind: providers.ModAudio, declared: audio, in: providers.AudioRef("aud_conf_1", "audio/wav"),
		outType: "audio/wav", outBytes: wavBytes,
		inReason: "audio_input_dropped", outReason: "audio_output_invalid", undeclare: "undeclared_audio_accepted",
	})
}

// 15.4 cancellation and partial-stream handling: a stream cut mid-answer is an
// error (never a complete answer), the error is sticky, and cancelling the
// context ends a stream that is waiting.
func checkCancellationAndPartialStream(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.Script(Scenario{Kind: ScTruncated, Text: "A partial answer that never completes.", ToksIn: 50, ToksOut: 8})
	st, err := s.Generator.GenerateStream(ctx, sc, textReq("hi"))
	if err != nil {
		return fail("stream_open_failed", "the stream did not open: code %s", codeOf(err))
	}
	_, derr := drain(st)
	if derr == nil {
		st.Close()
		return fail("partial_stream_completed", "a stream cut mid-answer ended as a complete one")
	}
	if _, again := st.Recv(); again == nil || again == io.EOF {
		st.Close()
		return fail("stream_error_not_sticky", "a failed stream recovered or reported a clean end")
	}
	st.Close()
	s.Upstream.Script(Scenario{Kind: ScTruncated, Text: "A partial answer that never completes.", ToksIn: 50, ToksOut: 8})
	if res, err := stream(ctx, s, sc, textReq("hi")); err == nil {
		_ = res
		return fail("partial_stream_completed", "a collected stream cut mid-answer returned a result")
	}

	// cancel while the stream waits after its first piece
	s.Upstream.Script(Scenario{Kind: ScStall, Text: "First piece of a long answer, then silence."})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err = s.Generator.GenerateStream(cctx, sc, textReq("hi"))
	if err != nil {
		return fail("stream_open_failed", "the stream did not open: code %s", codeOf(err))
	}
	defer st.Close()
	if _, err := st.Recv(); err != nil {
		return fail("stream_open_failed", "the first piece did not arrive: code %s", codeOf(err))
	}
	seenBefore := len(s.Upstream.Seen())
	time.AfterFunc(200*time.Millisecond, cancel)
	var rerr error
	if !callWithin(200*time.Millisecond+grace, func() { _, rerr = st.Recv() }) {
		s.Upstream.Release()
		return fail("cancel_not_honoured", "a waiting stream outlived its cancelled context by more than %s", grace)
	}
	if rerr == nil || rerr == io.EOF {
		return fail("cancel_not_honoured", "a cancelled stream reported a chunk or a clean end")
	}
	if len(s.Upstream.Seen()) != seenBefore {
		return fail("cancelled_stream_retried", "a cancelled stream was retried")
	}
	return pass()
}

const activeText = "<script>alert(1)</script> [open](javascript:alert(1)) ![t](http://tracker.example/p.png)"

// 15.4 sanitized rich response blocks: whatever the model writes arrives as a
// text part, untrusted and unmodified, for the block validator to sanitise;
// active media the model was not asked for (an SVG, an HTML page) is never
// passed on, and every returned part is valid output.
func checkSanitizedRichBlocks(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.Script(Scenario{Kind: ScText, Text: activeText, ToksIn: 40, ToksOut: 20})
	res, err := generate(ctx, s, sc, textReq("Show me a link."))
	if err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	for _, p := range res.Parts {
		if p.Kind != providers.ModText {
			return fail("active_content_accepted", "markup in the answer became a %s part", p.Kind)
		}
		if err := p.ValidateOutput(); err != nil {
			return fail("active_content_accepted", "a returned part is not valid output")
		}
	}
	if !s.Natural && !strings.Contains(res.Text(), "<script>") {
		return fail("rich_text_transformed", "the provider rewrote the model's markup instead of passing it on as data")
	}
	if s.Natural {
		return pass()
	}
	s.Upstream.Script(Scenario{Kind: ScActiveMedia, Text: "Here is a picture.", ToksIn: 40, ToksOut: 20})
	res, err = generate(ctx, s, sc, textReq("Show me a picture."))
	if err != nil {
		return pass()
	}
	for _, p := range res.Parts {
		if p.Kind != providers.ModText {
			return fail("active_content_accepted", "an unrequested %s part with active content was returned", p.Kind)
		}
	}
	return pass()
}

var _ = pv.ModalityImage
