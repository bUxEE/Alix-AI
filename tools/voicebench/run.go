package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	lvoice "github.com/buxee/alix-ai/adapters/localai/voice"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// clip is one audio input of an stt or vad phase.
type clip struct {
	Set, ID, Lang string
	PCM           []byte
	Rate          int
	Ref           string
	SHA           string
}

type ttsIndexEntry struct {
	ID     string `json:"id"`
	Lang   string `json:"lang"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}

func benchScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_voicebench", ApplicationID: "app_voicebench", TenantID: "ten_voicebench", WorkspaceID: "wsp_voicebench",
		PrincipalID: "usr_voicebench", Roles: []string{"member"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses_voicebench",
	}
}

type runFlags struct {
	phase, model, backend, files, modelsDir, dataDir, sets, manifest, fleursDir, lang, sentences, audioDir string
	endpoint, container, docker, profile, out, repo                                                        string
	maxActive                                                                                              int
	timeout                                                                                                time.Duration
	ttsStream                                                                                              bool
}

func runCommand(args []string) error {
	var f runFlags
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&f.phase, "phase", "", "stt, vad or tts (required)")
	fs.StringVar(&f.model, "model", "", "the LocalAI model name (whisper-base, silero-vad, piper-it-paola) (required)")
	fs.StringVar(&f.backend, "backend", "", "the LocalAI backend that serves it (whisper, piper) (required)")
	fs.StringVar(&f.files, "files", "", "comma-separated model files in -models-dir, hashed into the result (required)")
	fs.StringVar(&f.modelsDir, "models-dir", "benchmarks/tmp/voice-data/models", "directory holding the model files")
	fs.StringVar(&f.dataDir, "data-dir", "benchmarks/tmp/voice-data", "LocalAI data directory (backend digests are read from it)")
	fs.StringVar(&f.sets, "sets", "fleurs,nonspeech", "stt/vad clip sets: fleurs, nonspeech, tts:<model>")
	fs.StringVar(&f.manifest, "manifest", "docs/evidence/m4-voice/fleurs-manifest.json", "FLEURS manifest")
	fs.StringVar(&f.fleursDir, "fleurs-dir", "benchmarks/tmp/voice/fleurs", "FLEURS clips")
	fs.StringVar(&f.lang, "lang", "", "tts: language of the sentences (en, it)")
	fs.StringVar(&f.sentences, "sentences", "", "tts: sentences file, one per line")
	fs.StringVar(&f.audioDir, "audio-dir", "benchmarks/tmp/voice/tts", "tts: where the synthesized audio is written (and where tts:<model> sets are read)")
	fs.StringVar(&f.endpoint, "endpoint", "http://127.0.0.1:8080", "LocalAI endpoint (loopback)")
	fs.StringVar(&f.container, "container", "alixai-localai", "container to sample memory from")
	fs.StringVar(&f.docker, "docker", "docker", "container CLI")
	fs.StringVar(&f.profile, "profile", "lite", "profile the container runs under (lite, balanced, 4g)")
	fs.StringVar(&f.out, "out", "benchmarks/results/m4", "result directory")
	fs.StringVar(&f.repo, "repo", ".", "repository root (for git and the protocol commit)")
	fs.IntVar(&f.maxActive, "max-active", 1, "LOCALAI_MAX_ACTIVE_BACKENDS the container runs with (recorded)")
	fs.DurationVar(&f.timeout, "timeout", 5*time.Minute, "timeout of one request")
	fs.BoolVar(&f.ttsStream, "tts-stream", false, "ask the runtime to stream TTS (LocalAI v4.10.0 hangs on it for Piper: see the report)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.phase == "" || f.model == "" || f.backend == "" || f.files == "" {
		return errors.New("run: -phase, -model, -backend and -files are required")
	}
	key := os.Getenv("ALIXAI_LOCALAI_KEY")
	if key == "" {
		return errors.New("run: set ALIXAI_LOCALAI_KEY to the LocalAI API key")
	}
	prof, err := ParseProfile(f.profile)
	if err != nil {
		return err
	}
	limits, _ := limitsFor(prof)

	cfg := lvoice.Config{MaxAudioBytes: 16 << 20, TTSNoStream: !f.ttsStream}
	switch f.phase {
	case PhaseSTT:
		cfg.Models.STT = f.model
	case PhaseVAD:
		cfg.Models.VAD = f.model
	case PhaseTTS:
		cfg.Models.TTS = f.model
	default:
		return fmt.Errorf("run: unknown phase %q", f.phase)
	}
	client, err := lvoice.New(localai.Config{Endpoint: f.endpoint, RequestTimeout: f.timeout}, cfg, lvoice.WithAPIKey(key))
	if err != nil {
		return err
	}

	ctx := context.Background()
	res := &Result{
		FormatVersion: VoiceFormatVersion, RunID: ids.NewGenerator(clock.Real{}).New("run"), Timestamp: time.Now().UTC().Format(time.RFC3339),
		Git: CollectGit(ctx, f.repo), Host: CollectHost(ctx, f.docker, f.container), Profile: string(prof), Emulated: isEmulated(prof), Limits: limits,
		Phase: f.phase, Model: ModelRef{Name: f.model, Backend: f.backend},
		Protocol: ProtocolRef{Path: "docs/evidence/m4-voice/protocol.md", Commit: protocolCommit(ctx, f.repo, "docs/evidence/m4-voice/protocol.md")},
		Config:   RunConfig{Language: f.lang, TTSStreamed: f.ttsStream, MaxActiveBackend: f.maxActive, Threads: 4, Greedy: true, PercentileMethod: PercentileMethod},
	}
	for _, name := range strings.Split(f.files, ",") {
		sha, size, err := sha256File(filepath.Join(f.modelsDir, strings.TrimSpace(name)))
		if err != nil {
			return err
		}
		res.Model.Files = append(res.Model.Files, FileRef{Name: strings.TrimSpace(name), SHA256: sha, SizeBytes: size})
	}
	if res.Runtime, err = runtimeInfo(f.repo, f.dataDir); err != nil {
		return err
	}

	sampler, err := StartSampler(ctx, f.docker, f.container, time.Second)
	if err != nil {
		return err
	}
	started := time.Now()
	switch f.phase {
	case PhaseSTT, PhaseVAD:
		res.Config.Sets = strings.Split(f.sets, ",")
		clips, err := loadClips(res.Config.Sets, f)
		if err != nil {
			sampler.Stop()
			return err
		}
		if f.phase == PhaseSTT {
			res.ColdMS, res.Records = runSTT(ctx, client, clips, sampler)
		} else {
			res.ColdMS, res.Records = runVAD(ctx, client, clips, sampler)
		}
	case PhaseTTS:
		if f.lang == "" || f.sentences == "" {
			sampler.Stop()
			return errors.New("run: -phase tts needs -lang and -sentences")
		}
		res.ColdMS, res.Records, err = runTTS(ctx, client, f)
		if err != nil {
			sampler.Stop()
			return err
		}
	}
	res.DurationMS = float64(time.Since(started).Milliseconds())
	res.Memory = sampler.Stop()
	p, err := writeResult(f.out, res)
	if err != nil {
		return err
	}
	fmt.Println(filepath.ToSlash(p))
	for _, g := range res.Summary {
		fmt.Printf("  %-22s %-3s clips=%d errors=%d wer=%s rtf_p50=%s ttfa_p50=%s\n", g.Set, g.Lang, g.Clips, g.Errors, fmtP(g.WER), fmtP(g.RTF.P50), fmtP(g.TTFAMS.P50))
	}
	for c, m := range res.Memory.Classes {
		fmt.Printf("  memory %-8s peak=%d MiB steady=%d MiB\n", c, m.PeakKB>>10, m.SteadyKB>>10)
	}
	return nil
}

func fmtP(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.3f", *p)
}

var imageRE = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)`)

// runtimeInfo reads the pinned image from the shipped compose file and the digests of the installed backends from their metadata.
func runtimeInfo(repo, dataDir string) (RuntimeInfo, error) {
	info := RuntimeInfo{LocalAI: "v4.10.0", Backends: map[string]string{}}
	b, err := os.ReadFile(filepath.Join(repo, "deploy", "container", "localai", "compose.yaml"))
	if err != nil {
		return info, err
	}
	if m := imageRE.FindSubmatch(b); m != nil {
		info.Image = string(m[1])
	}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "backends"))
	for _, e := range entries {
		mb, err := os.ReadFile(filepath.Join(dataDir, "backends", e.Name(), "metadata.json"))
		if err != nil {
			continue
		}
		var md struct {
			Digest string `json:"digest"`
		}
		if json.Unmarshal(mb, &md) == nil && md.Digest != "" {
			info.Backends[e.Name()] = md.Digest
		}
	}
	return info, nil
}

func errCode(err error) *string {
	code := "internal"
	var ce *contracts.Error
	if errors.As(err, &ce) {
		code = string(ce.Code)
	}
	return &code
}

// loadClips builds the clips of the requested sets. A FLEURS clip whose bytes differ from the manifest is refused (fail closed).
func loadClips(sets []string, f runFlags) ([]clip, error) {
	var out []clip
	for _, set := range sets {
		switch {
		case set == "fleurs":
			m, err := readFleursManifest(f.manifest)
			if err != nil {
				return nil, err
			}
			for _, c := range m.Clips {
				data, err := os.ReadFile(filepath.Join(f.fleursDir, filepath.FromSlash(c.File)))
				if err != nil {
					return nil, fmt.Errorf("%w (run `voicebench fetch-fleurs` first)", err)
				}
				if sha256Hex(data) != c.SHA256 {
					return nil, fmt.Errorf("fleurs clip %s: sha256 differs from the manifest", c.ID)
				}
				pcm, rate, err := ParseWAV(data)
				if err != nil {
					return nil, fmt.Errorf("fleurs clip %s: %w", c.ID, err)
				}
				out = append(out, clip{Set: "fleurs", ID: c.ID, Lang: c.Lang, PCM: pcm, Rate: rate, Ref: c.Ref, SHA: c.SHA256})
			}
		case set == "nonspeech":
			for _, lang := range []string{"en", "it"} {
				for _, kind := range NonSpeechKinds {
					pcm := NonSpeechPCM(kind)
					out = append(out, clip{Set: "nonspeech", ID: "ns-" + kind + "-" + lang, Lang: lang, PCM: pcm, Rate: NonSpeechRate, SHA: sha256Hex(WriteWAV(pcm, NonSpeechRate))})
				}
			}
		case strings.HasPrefix(set, "tts:"):
			model := strings.TrimPrefix(set, "tts:")
			dir := filepath.Join(f.audioDir, model)
			ib, err := os.ReadFile(filepath.Join(dir, "index.json"))
			if err != nil {
				return nil, fmt.Errorf("%w (run the tts phase of %s first)", err, model)
			}
			var idx []ttsIndexEntry
			if err := json.Unmarshal(ib, &idx); err != nil {
				return nil, err
			}
			for _, e := range idx {
				data, err := os.ReadFile(filepath.Join(dir, e.ID+".wav"))
				if err != nil {
					return nil, err
				}
				if sha256Hex(data) != e.SHA256 {
					return nil, fmt.Errorf("tts clip %s/%s: sha256 differs from the index", model, e.ID)
				}
				pcm, rate, err := ParseWAV(data)
				if err != nil {
					return nil, err
				}
				out = append(out, clip{Set: set, ID: e.ID, Lang: e.Lang, PCM: pcm, Rate: rate, Ref: e.Text, SHA: e.SHA256})
			}
		default:
			return nil, fmt.Errorf("unknown clip set %q", set)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no clips")
	}
	return out, nil
}

func runSTT(ctx context.Context, c *lvoice.Client, clips []clip, s *Sampler) (*float64, []Record) {
	req := func(cl clip) pv.TranscribeRequest {
		return pv.TranscribeRequest{Audio: cl.PCM, Format: pv.PCM16Mono(cl.Rate), Language: cl.Lang}
	}
	// The first call after the container started loads the backend and the model: recorded as cold_ms, not counted.
	t0 := time.Now()
	_, coldErr := c.Transcribe(ctx, benchScope(), req(clips[0]))
	var cold *float64
	if coldErr == nil {
		cold = ptrF(float64(time.Since(t0).Milliseconds()))
	}
	s.MarkMeasure()
	recs := make([]Record, 0, len(clips))
	for _, cl := range clips {
		ref := cl.Ref
		r := Record{Set: cl.Set, ID: cl.ID, Lang: cl.Lang, AudioSHA: cl.SHA, AudioMS: pcmMS(cl.PCM, cl.Rate), Ref: &ref}
		start := time.Now()
		tr, err := c.Transcribe(ctx, benchScope(), req(cl))
		wall := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			r.Status, r.ErrorCode = StatusError, errCode(err)
		} else {
			r.Status, r.WallMS, r.Hyp = StatusOK, ptrF(wall), &tr.Text
		}
		scoreRecord(&r)
		recs = append(recs, r)
	}
	return cold, recs
}

func runVAD(ctx context.Context, c *lvoice.Client, clips []clip, s *Sampler) (*float64, []Record) {
	req := func(cl clip) pv.DetectRequest { return pv.DetectRequest{Audio: cl.PCM, Format: pv.PCM16Mono(cl.Rate)} }
	t0 := time.Now()
	_, coldErr := c.DetectSpeech(ctx, benchScope(), req(clips[0]))
	var cold *float64
	if coldErr == nil {
		cold = ptrF(float64(time.Since(t0).Milliseconds()))
	}
	s.MarkMeasure()
	recs := make([]Record, 0, len(clips))
	for _, cl := range clips {
		r := Record{Set: cl.Set, ID: cl.ID, Lang: cl.Lang, AudioSHA: cl.SHA, AudioMS: pcmMS(cl.PCM, cl.Rate)}
		start := time.Now()
		res, err := c.DetectSpeech(ctx, benchScope(), req(cl))
		wall := float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			r.Status, r.ErrorCode = StatusError, errCode(err)
		} else {
			n := len(res.Segments)
			var speech time.Duration
			for _, sg := range res.Segments {
				speech += sg.End - sg.Start
			}
			r.Status, r.WallMS, r.Segments, r.SpeechMS = StatusOK, ptrF(wall), &n, ptrF(float64(speech.Microseconds())/1000)
		}
		scoreRecord(&r)
		recs = append(recs, r)
	}
	return cold, recs
}

func readLines(p string) ([]string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// synth runs one request and returns the PCM, its rate, the time to the first non-empty chunk and the total time.
func synth(ctx context.Context, c *lvoice.Client, text, lang string) (pcm []byte, rate int, ttfa, total time.Duration, err error) {
	start := time.Now()
	st, err := c.SynthesizeStream(ctx, benchScope(), pv.SynthesizeRequest{Text: text, Language: lang})
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer st.Close()
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, 0, 0, err
		}
		if len(ch.Data) > 0 {
			if ttfa == 0 {
				ttfa = time.Since(start)
			}
			rate = ch.Format.SampleRate
			pcm = append(pcm, ch.Data...)
		}
		if ch.Done {
			break
		}
	}
	if len(pcm) == 0 {
		return nil, 0, 0, 0, contracts.New(contracts.ErrSchemaInvalid, "no audio came back")
	}
	return pcm, rate, ttfa, time.Since(start), nil
}

func runTTS(ctx context.Context, c *lvoice.Client, f runFlags) (*float64, []Record, error) {
	lines, err := readLines(f.sentences)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(f.audioDir, f.model)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	t0 := time.Now()
	_, _, _, _, coldErr := synth(ctx, c, lines[0], f.lang)
	var cold *float64
	if coldErr == nil {
		cold = ptrF(float64(time.Since(t0).Milliseconds()))
	}
	var recs []Record
	var idx []ttsIndexEntry
	for i, text := range lines {
		id := fmt.Sprintf("s%02d", i+1)
		txt := text
		r := Record{Set: "tts", ID: id, Lang: f.lang, Ref: &txt}
		pcm, rate, ttfa, total, err := synth(ctx, c, text, f.lang)
		if err != nil {
			r.Status, r.ErrorCode = StatusError, errCode(err)
			recs = append(recs, r)
			continue
		}
		wav := WriteWAV(pcm, rate)
		if err := os.WriteFile(filepath.Join(dir, id+".wav"), wav, 0o644); err != nil {
			return nil, nil, err
		}
		r.Status, r.AudioSHA, r.AudioMS = StatusOK, sha256Hex(wav), pcmMS(pcm, rate)
		r.WallMS, r.TTFAMS = ptrF(float64(total.Microseconds())/1000), ptrF(float64(ttfa.Microseconds())/1000)
		scoreRecord(&r)
		recs = append(recs, r)
		idx = append(idx, ttsIndexEntry{ID: id, Lang: f.lang, Text: text, SHA256: r.AudioSHA})
	}
	ib, _ := json.MarshalIndent(idx, "", "  ")
	return cold, recs, os.WriteFile(filepath.Join(dir, "index.json"), append(ib, '\n'), 0o644)
}
