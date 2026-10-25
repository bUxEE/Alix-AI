package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
)

// VoiceFormatVersion is result.format_version of a .voice file.
const VoiceFormatVersion = 1

// Voice result phases.
const (
	PhaseSTT = "stt"
	PhaseVAD = "vad"
	PhaseTTS = "tts"
)

// Record is the raw outcome of one clip (stt, vad) or one sentence (tts). Every key is always written; a value that does not apply is null.
type Record struct {
	// Set is where the clip comes from: fleurs, nonspeech, tts:<model> (a TTS output judged by STT), or tts (the sentences of a tts phase).
	Set       string   `json:"set"`
	ID        string   `json:"id"`
	Lang      string   `json:"lang"`
	Status    string   `json:"status"`
	ErrorCode *string  `json:"error_code"`
	AudioSHA  string   `json:"audio_sha256"`
	AudioMS   float64  `json:"audio_ms"`
	WallMS    *float64 `json:"wall_ms"`
	// TTFAMS is the time from the request to the first non-empty audio chunk the adapter returned (tts only).
	TTFAMS *float64 `json:"ttfa_ms"`
	// RTF is wall time over audio duration (stt: transcription time over the clip; tts: synthesis time over the audio produced).
	RTF *float64 `json:"rtf"`
	// Ref and Hyp are the reference and the transcript (stt) or the input text (tts: Ref only).
	Ref      *string `json:"ref"`
	Hyp      *string `json:"hyp"`
	RefWords int     `json:"ref_words"`
	Edits    int     `json:"edits"`
	// Segments and SpeechMS are the VAD's speech segments (vad only).
	Segments *int     `json:"segments"`
	SpeechMS *float64 `json:"speech_ms"`
}

// Group summarises the records of one set and language.
type Group struct {
	Set      string `json:"set"`
	Lang     string `json:"lang"`
	Clips    int    `json:"clips"`
	OK       int    `json:"ok"`
	Errors   int    `json:"errors"`
	RefWords int    `json:"ref_words"`
	Edits    int    `json:"edits"`
	// WER is micro-averaged: total edits over total reference words; null without reference words.
	WER *float64 `json:"wer"`
	// NonEmpty counts ok stt records whose transcript has at least one word; SpeechClips counts ok vad records with at least one segment.
	NonEmpty    int   `json:"non_empty_transcripts"`
	SpeechClips int   `json:"speech_clips"`
	RTF         Stats `json:"rtf"`
	WallMS      Stats `json:"wall_ms"`
	TTFAMS      Stats `json:"ttfa_ms"`
}

// FileRef pins one model file the run used.
type FileRef struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Result is one .voice file: one candidate, one phase.
type Result struct {
	FormatVersion int             `json:"format_version"`
	RunID         string          `json:"run_id"`
	Timestamp     string          `json:"timestamp"`
	DurationMS    float64         `json:"duration_ms"`
	Git           GitInfo         `json:"git"`
	Host          HostInfo        `json:"host"`
	Profile       string          `json:"profile"`
	Emulated      bool            `json:"emulated"`
	Limits        ContainerLimits `json:"container_limits"`
	Runtime       RuntimeInfo     `json:"runtime"`
	Phase         string          `json:"phase"`
	Model         ModelRef        `json:"model"`
	Protocol      ProtocolRef     `json:"protocol"`
	Config        RunConfig       `json:"config"`
	// ColdMS is the first call of the model after the container started: backend start, model load and one request.
	ColdMS  *float64      `json:"cold_ms"`
	Records []Record      `json:"records"`
	Summary []Group       `json:"summary"`
	Memory  MemorySummary `json:"memory"`
}

// RuntimeInfo identifies what was measured on.
type RuntimeInfo struct {
	LocalAI  string            `json:"localai"`
	Image    string            `json:"image"`
	Backends map[string]string `json:"backends"`
}

// ModelRef is the LocalAI model and its files.
type ModelRef struct {
	Name    string    `json:"name"`
	Backend string    `json:"backend"`
	Files   []FileRef `json:"files"`
}

// ProtocolRef names the pre-registered protocol the run follows.
type ProtocolRef struct {
	Path   string  `json:"path"`
	Commit *string `json:"commit"`
}

// RunConfig is how the run was configured.
type RunConfig struct {
	Sets             []string `json:"sets"`
	Language         string   `json:"language"`
	TTSStreamed      bool     `json:"tts_streamed"`
	MaxActiveBackend int      `json:"max_active_backends"`
	Threads          int      `json:"threads"`
	Greedy           bool     `json:"greedy"`
	PercentileMethod string   `json:"percentile_method"`
}

func ptrF(x float64) *float64 { return &x }

// summarizeGroups derives the per-set, per-language summary from the records alone, which is how check verifies a stored one.
func summarizeGroups(recs []Record) []Group {
	type key struct{ set, lang string }
	idx := map[key]*Group{}
	vals := map[key]*struct{ rtf, wall, ttfa []float64 }{}
	var order []key
	for _, r := range recs {
		k := key{r.Set, r.Lang}
		g := idx[k]
		if g == nil {
			g = &Group{Set: r.Set, Lang: r.Lang}
			idx[k] = g
			vals[k] = &struct{ rtf, wall, ttfa []float64 }{}
			order = append(order, k)
		}
		g.Clips++
		g.RefWords += r.RefWords
		g.Edits += r.Edits
		if r.Status != StatusOK {
			g.Errors++
			continue
		}
		g.OK++
		if r.Hyp != nil && len(Normalize(*r.Hyp)) > 0 {
			g.NonEmpty++
		}
		if r.Segments != nil && *r.Segments > 0 {
			g.SpeechClips++
		}
		if r.RTF != nil {
			vals[k].rtf = append(vals[k].rtf, *r.RTF)
		}
		if r.WallMS != nil {
			vals[k].wall = append(vals[k].wall, *r.WallMS)
		}
		if r.TTFAMS != nil {
			vals[k].ttfa = append(vals[k].ttfa, *r.TTFAMS)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].set != order[j].set {
			return order[i].set < order[j].set
		}
		return order[i].lang < order[j].lang
	})
	out := make([]Group, 0, len(order))
	for _, k := range order {
		g := idx[k]
		if g.RefWords > 0 {
			g.WER = round6(float64(g.Edits) / float64(g.RefWords))
		}
		g.RTF, g.WallMS, g.TTFAMS = Summarize(vals[k].rtf), Summarize(vals[k].wall), Summarize(vals[k].ttfa)
		out = append(out, *g)
	}
	return out
}

// scoreRecord scores a record: the edits are computed here from the two texts, never passed in. A transcription that failed counts every
// reference word as a deletion; the sentences of a tts phase have no transcript and no word error rate.
func scoreRecord(r *Record) {
	switch {
	case r.Ref != nil && r.Hyp != nil:
		r.Edits, r.RefWords = WER(*r.Ref, *r.Hyp)
	case r.Ref != nil && r.Status == StatusError && r.Set != "tts":
		n := len(Normalize(*r.Ref))
		r.Edits, r.RefWords = n, n
	}
	if r.WallMS != nil && r.AudioMS > 0 {
		r.RTF = round3(*r.WallMS / r.AudioMS)
	}
}

// writeResult writes res as <dir>/<phase>_<model>_<profile>_<run id>.voice and returns the path. A file is never overwritten.
func writeResult(dir string, res *Result) (string, error) {
	res.Summary = summarizeGroups(res.Records)
	if probs := verifyResult(res); len(probs) > 0 {
		return "", fmt.Errorf("the result does not verify: %s", strings.Join(probs, "; "))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s_%s_%s.voice", res.Phase, res.Model.Name, res.Profile, strings.ToLower(res.RunID))
	p := filepath.Join(dir, name)
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return "", err
	}
	return p, nil
}

func readResult(p string) (*Result, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var res Result
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &res, nil
}

// verifyResult applies the checks of `voicebench check`: honest profile label, every record re-scored from its texts, the summary equal to
// what the records say, sane numbers.
func verifyResult(r *Result) []string {
	var ps []string
	add := func(f string, a ...any) { ps = append(ps, fmt.Sprintf(f, a...)) }
	if r.FormatVersion != VoiceFormatVersion {
		add("format_version %d is not %d", r.FormatVersion, VoiceFormatVersion)
	}
	p := models.Profile(r.Profile)
	if limits, ok := limitsFor(p); !ok {
		add("profile %q is not a known profile", r.Profile)
	} else {
		if r.Emulated != isEmulated(p) {
			add("emulated is %v but results under profile %s are emulated (never certified)", r.Emulated, r.Profile)
		}
		if r.Limits != limits {
			add("container_limits %+v are not the limits of profile %s", r.Limits, r.Profile)
		}
	}
	if r.Phase != PhaseSTT && r.Phase != PhaseVAD && r.Phase != PhaseTTS {
		add("phase %q is not stt, vad or tts", r.Phase)
	}
	if len(r.Records) == 0 {
		add("no records")
	}
	seen := map[string]bool{}
	for _, rec := range r.Records {
		id := rec.Set + "/" + rec.Lang + "/" + rec.ID
		if seen[id] {
			add("record %s appears twice", id)
		}
		seen[id] = true
		if rec.Status != StatusOK && rec.Status != StatusError {
			add("record %s: status %q", id, rec.Status)
		}
		if rec.Status == StatusOK {
			if rec.WallMS == nil || math.IsNaN(*rec.WallMS) || *rec.WallMS < 0 {
				add("record %s: ok without a wall time", id)
			}
			if rec.TTFAMS != nil && rec.WallMS != nil && *rec.TTFAMS > *rec.WallMS+0.5 {
				add("record %s: ttfa exceeds the wall time", id)
			}
		} else if rec.ErrorCode == nil {
			add("record %s: error without a code", id)
		}
		if rec.Ref != nil && r.Phase == PhaseSTT {
			want := rec
			if rec.Status != StatusOK {
				want.Hyp = nil
			}
			scoreRecord(&want)
			if want.Edits != rec.Edits || want.RefWords != rec.RefWords {
				add("record %s: edits %d/%d do not match its texts (%d/%d)", id, rec.Edits, rec.RefWords, want.Edits, want.RefWords)
			}
		}
	}
	got, _ := json.Marshal(r.Summary)
	want, _ := json.Marshal(summarizeGroups(r.Records))
	if !bytes.Equal(got, want) {
		add("summary does not match the records")
	}
	return ps
}

func checkCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: voicebench check <dir or .voice file>...")
	}
	var files []string
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			files = append(files, a)
			continue
		}
		err = filepath.WalkDir(a, func(p string, d fs.DirEntry, werr error) error {
			if werr == nil && !d.IsDir() && strings.HasSuffix(p, ".voice") {
				files = append(files, p)
			}
			return werr
		})
		if err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return errors.New("no .voice result files found")
	}
	sort.Strings(files)
	failed := 0
	for _, f := range files {
		res, err := readResult(f)
		var probs []string
		if err != nil {
			probs = []string{err.Error()}
		} else {
			probs = verifyResult(res)
		}
		if len(probs) == 0 {
			fmt.Printf("PASS  %s\n", filepath.ToSlash(f))
			continue
		}
		failed++
		fmt.Printf("FAIL  %s\n", filepath.ToSlash(f))
		for _, p := range probs {
			fmt.Printf("      %s\n", p)
		}
	}
	if failed > 0 {
		return fmt.Errorf("voicebench check: %d of %d result file(s) are invalid", failed, len(files))
	}
	fmt.Printf("voicebench check: ok - %d result file(s)\n", len(files))
	return nil
}
