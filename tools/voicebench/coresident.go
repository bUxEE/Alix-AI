package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	lvoice "github.com/buxee/alix-ai/adapters/localai/voice"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
)

// The Lite co-residency run (protocol gates 11 and 12): the controller and the embedder of the Lite default plus the voice workers, in a fixed
// mixed sequence, to see whether voice makes the runtime unload and reload a resident model (R3 of docs/operations/resource-budgets.md)
// and what the container holds when everything is resident. The text calls are plain HTTP to the loopback endpoint (a chat completion and an
// embedding); the voice calls go through the adapter as in every other phase.

// CoreResidentRoles is the number of resident roles the run declares: controller, embedder, STT, TTS, VAD.
const CoreResidentRoles = 5

// reloadSlackMS: a call slower than the warm median of its role by more than this is counted as a model reload.
const reloadSlackMS = 3000

// liteBudgetMiB is the Lite container budget of docs/operations/resource-budgets.md section 3.
const liteBudgetMiB = 5811

// CoCall is one call of the sequence.
type CoCall struct {
	Round  int     `json:"round"`
	Step   int     `json:"step"`
	Role   string  `json:"role"`
	WallMS float64 `json:"wall_ms"`
	Error  *string `json:"error"`
	Reload bool    `json:"reload"`
}

// CoResult is a .coresident file.
type CoResult struct {
	FormatVersion int                `json:"format_version"`
	RunID         string             `json:"run_id"`
	Timestamp     string             `json:"timestamp"`
	DurationMS    float64            `json:"duration_ms"`
	Git           GitInfo            `json:"git"`
	Host          HostInfo           `json:"host"`
	Profile       string             `json:"profile"`
	Emulated      bool               `json:"emulated"`
	Limits        ContainerLimits    `json:"container_limits"`
	Runtime       RuntimeInfo        `json:"runtime"`
	Protocol      ProtocolRef        `json:"protocol"`
	MaxActive     int                `json:"max_active_backends"`
	DeclaredRoles int                `json:"declared_resident_roles"`
	Models        map[string]string  `json:"models"`
	WarmMedianMS  map[string]float64 `json:"warm_median_ms"`
	Calls         []CoCall           `json:"calls"`
	Reloads       map[string]int     `json:"reloads_by_role"`
	ReloadsTotal  int                `json:"reloads_total"`
	BudgetMiB     int                `json:"container_budget_mib"`
	Memory        MemorySummary      `json:"memory"`
}

var coSequence = []string{"chat", "embed", "stt", "tts", "vad", "chat", "embed"}

func medianF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	return c[(len(c)-1)/2]
}

// countReloads marks every call slower than its role's warm median by more than reloadSlackMS and totals them by role. Pure, so it is tested.
func countReloads(calls []CoCall, warm map[string]float64) (map[string]int, int) {
	by := map[string]int{}
	total := 0
	for i := range calls {
		c := &calls[i]
		c.Reload = c.Error == nil && c.WallMS > warm[c.Role]+reloadSlackMS
		if c.Reload {
			by[c.Role]++
			total++
		}
	}
	return by, total
}

type textClient struct {
	endpoint, key string
	hc            *http.Client
}

func (t textClient) post(ctx context.Context, path string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.key)
	resp, err := t.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

func coresidentCommand(args []string) error {
	fs := flag.NewFlagSet("coresident", flag.ContinueOnError)
	controller := fs.String("controller", "granite-4.0-1b-q4_k_m", "LocalAI model name of the controller")
	embedder := fs.String("embedder", "granite-embedding-97m-multilingual-r2-q4_k_m", "LocalAI model name of the embedder")
	stt := fs.String("stt", "", "STT model (required)")
	tts := fs.String("tts", "", "TTS model (required)")
	vad := fs.String("vad", "silero-vad", "VAD model")
	lang := fs.String("lang", "en", "language of the voice calls")
	sentences := fs.String("sentences", "tests/performance/voice/tts-sentences-en.txt", "TTS sentences (the first is used)")
	manifest := fs.String("manifest", "docs/evidence/m4-voice/fleurs-manifest.json", "FLEURS manifest (the first clip of -lang is used)")
	fleursDir := fs.String("fleurs-dir", "benchmarks/tmp/voice/fleurs", "FLEURS clips")
	rounds := fs.Int("rounds", 3, "rounds of the sequence")
	maxActive := fs.Int("max-active", CoreResidentRoles, "LOCALAI_MAX_ACTIVE_BACKENDS the container runs with (recorded)")
	endpoint := fs.String("endpoint", "http://127.0.0.1:8080", "LocalAI endpoint (loopback)")
	container := fs.String("container", "alixai-localai", "container to sample")
	docker := fs.String("docker", "docker", "container CLI")
	dataDir := fs.String("data-dir", "benchmarks/tmp/voice-data", "LocalAI data directory")
	profile := fs.String("profile", "lite", "profile")
	out := fs.String("out", "benchmarks/results/m4", "result directory")
	repo := fs.String("repo", ".", "repository root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := os.Getenv("ALIXAI_LOCALAI_KEY")
	if key == "" || *stt == "" || *tts == "" {
		return errors.New("coresident: set ALIXAI_LOCALAI_KEY and pass -stt and -tts")
	}
	prof, err := ParseProfile(*profile)
	if err != nil {
		return err
	}
	limits, _ := limitsFor(prof)
	ctx := context.Background()
	vc, err := lvoice.New(localai.Config{Endpoint: *endpoint, RequestTimeout: 5 * time.Minute},
		lvoice.Config{Models: lvoice.ModelSet{STT: *stt, TTS: *tts, VAD: *vad}, TTSNoStream: true, MaxAudioBytes: 16 << 20}, lvoice.WithAPIKey(key))
	if err != nil {
		return err
	}
	tc := textClient{endpoint: *endpoint, key: key, hc: &http.Client{Timeout: 5 * time.Minute}}

	m, err := readFleursManifest(*manifest)
	if err != nil {
		return err
	}
	var cl *clip
	for _, c := range m.Clips {
		if c.Lang == *lang {
			data, err := os.ReadFile(filepath.Join(*fleursDir, filepath.FromSlash(c.File)))
			if err != nil {
				return err
			}
			pcm, rate, err := ParseWAV(data)
			if err != nil {
				return err
			}
			cl = &clip{PCM: pcm, Rate: rate, Lang: c.Lang}
			break
		}
	}
	if cl == nil {
		return fmt.Errorf("no FLEURS clip for language %q", *lang)
	}
	lines, err := readLines(*sentences)
	if err != nil {
		return err
	}

	call := map[string]func() error{
		"chat": func() error {
			return tc.post(ctx, "/v1/chat/completions", map[string]any{"model": *controller, "max_tokens": 8, "temperature": 0,
				"messages": []map[string]string{{"role": "user", "content": "Reply with the single word: ready"}}})
		},
		"embed": func() error {
			return tc.post(ctx, "/v1/embeddings", map[string]any{"model": *embedder, "input": []string{"clinic appointment on Tuesday"}})
		},
		"stt": func() error {
			_, err := vc.Transcribe(ctx, benchScope(), pv.TranscribeRequest{Audio: cl.PCM, Format: pv.PCM16Mono(cl.Rate), Language: cl.Lang})
			return err
		},
		"tts": func() error {
			_, _, _, _, err := synth(ctx, vc, lines[0], *lang)
			return err
		},
		"vad": func() error {
			_, err := vc.DetectSpeech(ctx, benchScope(), pv.DetectRequest{Audio: cl.PCM, Format: pv.PCM16Mono(cl.Rate)})
			return err
		},
	}
	timeCall := func(role string) CoCall {
		t0 := time.Now()
		err := call[role]()
		c := CoCall{Role: role, WallMS: float64(time.Since(t0).Microseconds()) / 1000}
		if err != nil {
			e := clipRunes(err.Error(), 120)
			c.Error = &e
		}
		return c
	}

	sampler, err := StartSampler(ctx, *docker, *container, time.Second)
	if err != nil {
		return err
	}
	started := time.Now()
	for _, role := range coSequence[:5] { // load every role once; with fewer slots than roles this is already where eviction starts
		timeCall(role)
	}
	sampler.MarkMeasure()
	warm := map[string]float64{}
	for _, role := range []string{"chat", "embed", "stt", "tts", "vad"} {
		var xs []float64
		for i := 0; i < 3; i++ { // consecutive calls of one role: no other model is requested in between, so none can have been evicted
			if c := timeCall(role); c.Error == nil {
				xs = append(xs, c.WallMS)
			}
		}
		warm[role] = medianF(xs)
	}
	var calls []CoCall
	for r := 1; r <= *rounds; r++ {
		for i, role := range coSequence {
			c := timeCall(role)
			c.Round, c.Step = r, i+1
			calls = append(calls, c)
		}
	}
	res := &CoResult{
		FormatVersion: VoiceFormatVersion, RunID: ids.NewGenerator(clock.Real{}).New("run"), Timestamp: started.UTC().Format(time.RFC3339),
		DurationMS: float64(time.Since(started).Milliseconds()), Git: CollectGit(ctx, *repo), Host: CollectHost(ctx, *docker, *container),
		Profile: string(prof), Emulated: isEmulated(prof), Limits: limits,
		Protocol:  ProtocolRef{Path: "docs/evidence/m4-voice/protocol.md", Commit: protocolCommit(ctx, *repo, "docs/evidence/m4-voice/protocol.md")},
		MaxActive: *maxActive, DeclaredRoles: CoreResidentRoles, BudgetMiB: liteBudgetMiB,
		Models:       map[string]string{"controller": *controller, "embedder": *embedder, "stt": *stt, "tts": *tts, "vad": *vad},
		WarmMedianMS: warm, Calls: calls,
	}
	res.Runtime, _ = runtimeInfo(*repo, *dataDir)
	res.Reloads, res.ReloadsTotal = countReloads(res.Calls, warm)
	res.Memory = sampler.Stop()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("coresident_max%d_%s_%s.coresident", *maxActive, res.Profile, strings.ToLower(res.RunID))
	b, _ := json.MarshalIndent(res, "", "  ")
	p := filepath.Join(*out, name)
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(filepath.ToSlash(p))
	fmt.Printf("  reloads=%d by role %v; R peak %d MiB, W peak %d MiB (budget %d MiB)\n", res.ReloadsTotal, res.Reloads, res.Memory.RTotalPeakKB>>10, res.Memory.WPeakKB>>10, liteBudgetMiB)
	return nil
}
