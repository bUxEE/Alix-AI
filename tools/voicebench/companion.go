package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/models"
)

// maxOutputRunes is the bench limit of a case's output.
const maxOutputRunes = 2048

// CompanionInput is what buildCompanion needs besides the VAD result.
type CompanionInput struct {
	RunID      string
	Timestamp  string
	DurationMS float64
	Git        GitInfo
	Host       HostInfo
	Profile    models.Profile
	VADModel   string
	VADFile    FileRef
	Backend    BackendInfo
	CorpusSHA  string
	Records    []Record
}

// declineOutcome is the typed outcome of a non-speech clip: the pipeline (VAD, then STT) produced no transcript, or it let the clip through.
func declineOutcome(r Record) (outcome string, declined bool) {
	if r.Segments != nil && *r.Segments == 0 {
		return "no_transcript", true
	}
	return "speech_detected", false
}

// nonspeechCase maps the record of a non-speech clip onto a bench refusal case (see the comment of envelope.go for the meaning of each flag).
// It follows the schema's rules for a case that ended in an error: every flag that would credit the system stays false.
func nonspeechCase(r Record) CaseOutcome {
	o := CaseOutcome{ID: r.ID, Language: r.Lang, Category: CatRefusal, RefusalOK: boolPtr(false)}
	if r.Status != StatusOK {
		code := "internal"
		if r.ErrorCode != nil {
			code = *r.ErrorCode
		}
		o.Status = StatusError
		o.ErrorCode = &code
		o.SchemaError = strPtr("no outcome: " + code)
		return o
	}
	outcome, declined := declineOutcome(r)
	n := 0
	if r.Segments != nil {
		n = *r.Segments
	}
	body, _ := json.Marshal(struct {
		Outcome  string  `json:"outcome"`
		Segments int     `json:"vad_segments"`
		ClipMS   float64 `json:"clip_ms"`
	}{outcome, n, r.AudioMS})
	out := clipRunes(string(body), maxOutputRunes)
	o.Status = StatusOK
	o.LatencyMS = r.WallMS
	o.Output = &out
	o.ProposalIntent = strPtr(outcome)
	o.SchemaValid = true
	o.IntentOK = declined
	o.CatalogueOK = declined
	o.RefusalOK = boolPtr(declined)
	o.Pass = declined
	return o
}

// buildCompanion projects the non-speech clips onto the bench envelope. It fails when there are none: bench check refuses a result with no case.
func buildCompanion(in CompanionInput) (*BenchResult, error) {
	var cases []CaseOutcome
	for _, r := range in.Records {
		if r.Set == "nonspeech" {
			cases = append(cases, nonspeechCase(r))
		}
	}
	if len(cases) == 0 {
		return nil, errors.New("the VAD result has no non-speech clip, so there is nothing to put in the bench-envelope result")
	}
	limits, ok := limitsFor(in.Profile)
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", in.Profile)
	}
	byCat, byLang := aggregateBy(cases)
	sha := in.VADFile.SHA256
	size := in.VADFile.SizeBytes
	name := "Silero VAD v5.1.2 (ggml, served by the whisper.cpp backend with vad_only)"
	return &BenchResult{
		FormatVersion: BenchFormatVersion, RunID: in.RunID, Timestamp: in.Timestamp, DurationMS: in.DurationMS,
		Git: in.Git, Host: in.Host, Profile: string(in.Profile), Emulated: isEmulated(in.Profile), Synthetic: false,
		ContainerLimits: limits, Provider: ProviderInfo{Name: "localai"},
		Model: ModelInfo{
			RegistryID: "silero-vad-ggml-v5.1.2", InRegistry: false, Name: &name, RuntimeModelName: &in.VADModel,
			ArtifactSHA256: &sha, ArtifactHashSource: HashComputed, ArtifactSizeBytes: &size,
		},
		Backend: in.Backend,
		Context: ContextInfo{PromptBudgetToks: 1}, // no prompt exists; 1 is the schema's minimum
		Corpus:  BenchCorpus{ID: "voice-nonspeech-v1", SHA256: in.CorpusSHA, CasesTotal: len(cases), CasesRun: len(cases)},
		Config: BenchConfig{
			Role: "controller", Constraint: "none", Stream: false, Temperature: 0, MaxToks: 1, TimeoutMS: 300000, WarmupRequests: 0,
			Prompt:           PromptInfo{ID: "voice-nonspeech", SHA256: in.CorpusSHA},
			PercentileMethod: PercentileMethod,
		},
		Metrics: Aggregate(cases), ByCategory: byCat, ByLanguage: byLang,
		Latency: summarizeLatency(cases, nil), Memory: noMemory(), Errors: summarizeErrors(cases), Cases: cases,
	}, nil
}

// companionName is the file name of a companion: it shares the stem of the .voice file it was made from.
func companionName(voicePath string) string {
	return strings.TrimSuffix(filepath.Base(voicePath), ".voice") + ".json"
}

func companionCommand(args []string) error {
	fs := flag.NewFlagSet("companion", flag.ContinueOnError)
	vad := fs.String("vad", "", "the .voice result of the vad phase (required)")
	out := fs.String("out", "benchmarks/results/m4", "directory to write the companion into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *vad == "" {
		return errors.New("companion: -vad is required")
	}
	r, err := readResult(*vad)
	if err != nil {
		return err
	}
	if probs := verifyResult(r); len(probs) > 0 {
		return fmt.Errorf("the VAD result does not verify: %s", strings.Join(probs, "; "))
	}
	if r.Phase != PhaseVAD || len(r.Model.Files) == 0 {
		return errors.New("companion: the result is not a vad result")
	}
	prof, err := ParseProfile(r.Profile)
	if err != nil {
		return err
	}
	ts, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return err
	}
	runtimeVer, backendName := r.Runtime.LocalAI, r.Model.Backend
	cases := make([]string, 0, len(r.Records))
	for _, rec := range r.Records {
		if rec.Set == "nonspeech" {
			cases = append(cases, rec.ID+":"+rec.AudioSHA)
		}
	}
	bench, err := buildCompanion(CompanionInput{
		RunID: r.RunID, Timestamp: ts.UTC().Format(time.RFC3339), DurationMS: r.DurationMS, Git: r.Git, Host: r.Host, Profile: prof,
		VADModel: r.Model.Name, VADFile: r.Model.Files[0],
		Backend:   BackendInfo{Runtime: strPtr("localai"), Name: &backendName, RuntimeVersion: &runtimeVer},
		CorpusSHA: sha256Hex([]byte(strings.Join(cases, "\n"))), Records: r.Records,
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(bench, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	p := filepath.Join(*out, companionName(*vad))
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(filepath.ToSlash(p))
	return nil
}
