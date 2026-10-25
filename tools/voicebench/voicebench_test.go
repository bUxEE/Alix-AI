package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/models"
)

func TestNormalizeAndWER(t *testing.T) {
	tests := []struct {
		name, ref, hyp string
		edits, words   int
	}{
		{"identical", "Hello, world!", "hello world", 0, 2},
		{"apostrophes and hyphens separate words in both texts", "L'incidente è avvenuto", "l incidente è avvenuto", 0, 4},
		{"one substitution", "the cat sat", "the hat sat", 1, 3},
		{"one deletion", "the cat sat", "the sat", 1, 3},
		{"one insertion", "the cat", "the big cat", 1, 2},
		{"an accent is a different word", "perché sì", "perche si", 2, 2},
		{"empty hypothesis deletes everything", "uno due tre", "", 3, 3},
		{"empty reference counts insertions", "", "thank you", 2, 0},
		{"punctuation only", "...", "?!", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, n := WER(tt.ref, tt.hyp)
			if e != tt.edits || n != tt.words {
				t.Fatalf("WER(%q, %q) = %d/%d, want %d/%d", tt.ref, tt.hyp, e, n, tt.edits, tt.words)
			}
		})
	}
}

func TestWAVRoundTripAndRefusals(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	got, rate, err := ParseWAV(WriteWAV(pcm, 22050))
	if err != nil || rate != 22050 || !bytes.Equal(got, pcm) {
		t.Fatalf("round trip: %v rate=%d pcm=%v", err, rate, got)
	}
	if ms := pcmMS(make([]byte, 32000), 16000); ms != 1000 {
		t.Fatalf("1 s of 16 kHz PCM16 is %v ms", ms)
	}
	// float32 WAV (FLEURS en_us) is converted to PCM16
	fl := WriteWAV(make([]byte, 12), 16000)
	binary.LittleEndian.PutUint16(fl[20:], 3)
	binary.LittleEndian.PutUint16(fl[34:], 32)
	binary.LittleEndian.PutUint32(fl[44:], math.Float32bits(0.5))
	binary.LittleEndian.PutUint32(fl[48:], math.Float32bits(-1.5))
	if got, _, err := ParseWAV(fl); err != nil || len(got) != 6 || int16(binary.LittleEndian.Uint16(got)) != 16384 || int16(binary.LittleEndian.Uint16(got[2:])) != -32768 {
		t.Fatalf("float32 conversion: %v %v", err, got)
	}
	stereo := WriteWAV(pcm, 16000)
	binary.LittleEndian.PutUint16(stereo[22:], 2)
	for name, b := range map[string][]byte{"stereo": stereo, "not riff": []byte("hello world, not a wav"), "empty": nil} {
		if _, _, err := ParseWAV(b); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// a streamed header carries 0xFFFFFFFF as the data size: take what is there
	streamed := WriteWAV(pcm, 16000)
	binary.LittleEndian.PutUint32(streamed[40:], 0xFFFFFFFF)
	if got, _, err := ParseWAV(streamed); err != nil || !bytes.Equal(got, pcm) {
		t.Fatalf("streamed header: %v %v", err, got)
	}
}

func TestNonSpeechIsDeterministicAndAtItsLevel(t *testing.T) {
	rms := func(pcm []byte) float64 {
		var sum float64
		for i := 0; i+1 < len(pcm); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
			sum += v * v
		}
		return 20 * math.Log10(math.Sqrt(sum/float64(len(pcm)/2))/32768)
	}
	want := map[string]struct{ dbfs, tol float64 }{"white": {-50, 0.5}, "hum": {-40, 0.5}, "lowpass": {-30, 0.5}}
	for _, k := range NonSpeechKinds {
		a, b := NonSpeechPCM(k), NonSpeechPCM(k)
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Fatalf("%s: empty or not deterministic", k)
		}
		if k == "silence" {
			if rms := bytes.Count(a, []byte{0}); rms != len(a) {
				t.Errorf("silence is not digital silence")
			}
			continue
		}
		if got := rms(a); math.Abs(got-want[k].dbfs) > want[k].tol {
			t.Errorf("%s: %.1f dBFS, want %.0f", k, got, want[k].dbfs)
		}
	}
	if NonSpeechPCM("speech") != nil {
		t.Error("an unknown kind must give nothing")
	}
}

func TestSummarizeMemoryAttributesPerClass(t *testing.T) {
	t0 := time.Unix(1000, 0)
	mk := func(sec int, procs ...procSample) memSample {
		return memSample{T: t0.Add(time.Duration(sec) * time.Second), Procs: procs, CurrB: 3 << 30, InactFB: 1 << 30}
	}
	svc := procSample{PID: 1, Class: "service", RSSKB: 100 << 10}
	samples := []memSample{
		mk(0, svc, procSample{PID: 7, Class: "whisper", RSSKB: 400 << 10, AnonKB: 100 << 10, FileKB: 300 << 10}), // loading peak
		mk(1, svc, procSample{PID: 7, Class: "whisper", RSSKB: 200 << 10}),
		mk(2, svc, procSample{PID: 7, Class: "whisper", RSSKB: 220 << 10}),
		mk(3, svc, procSample{PID: 7, Class: "whisper", RSSKB: 210 << 10}),
	}
	m := summarizeMemory(samples, t0.Add(time.Second), "c", time.Second, 0)
	w := m.Classes["whisper"]
	if w.PeakKB != 400<<10 || w.SteadyKB != 210<<10 || w.PeakAnon != 100<<10 || w.PeakFile != 300<<10 {
		t.Errorf("whisper = %+v", w)
	}
	if m.Classes["service"].PeakKB != 100<<10 || m.RTotalPeakKB != 500<<10 {
		t.Errorf("service %+v, total peak %d", m.Classes["service"], m.RTotalPeakKB)
	}
	if m.WPeakKB != 2<<20 {
		t.Errorf("W peak = %d KiB, want %d (memory.current minus inactive_file)", m.WPeakKB, 2<<20)
	}
	if m.SteadySamples != 3 {
		t.Errorf("steady samples = %d, want the 3 taken at or after the mark", m.SteadySamples)
	}
}

func TestParseSampleAndClassify(t *testing.T) {
	out := "P|1|120000|90000|30000|./local-ai run \nP|42|17000|9000|8000|/backends/piper/lib/ld.so /backends/piper/piper --addr 127.0.0.1:43773 \n" +
		"P|50|9|9|9|/backends/whisper/lib/ld.so /backends/whisper/whisper --addr 127.0.0.1:1 \nC|3221225472|1073741824\nnoise\n"
	s := parseSample(out, time.Now())
	if len(s.Procs) != 3 || s.CurrB != 3<<30 || s.InactFB != 1<<30 {
		t.Fatalf("parsed %+v", s)
	}
	for i, want := range []string{"service", "piper", "whisper"} {
		if s.Procs[i].Class != want {
			t.Errorf("process %d is %q, want %q", i, s.Procs[i].Class, want)
		}
	}
	if _, err := StartSampler(nil, "docker", "bad name; rm -rf", time.Second); err == nil { //nolint:staticcheck // the check is before any use of ctx
		t.Error("a container name with shell characters must be refused")
	}
}

func rec(set, id, lang, ref, hyp string, wall, audio float64) Record {
	r := Record{Set: set, ID: id, Lang: lang, Status: StatusOK, AudioSHA: "x", AudioMS: audio, WallMS: &wall, Ref: &ref, Hyp: &hyp}
	scoreRecord(&r)
	return r
}

func TestSummaryIsMicroAveragedAndErrorsCountAsDeletions(t *testing.T) {
	failed := Record{Set: "fleurs", ID: "c", Lang: "en", Status: StatusError, ErrorCode: strPtr("timeout"), Ref: strPtr("one two three four"), AudioMS: 1000}
	scoreRecord(&failed)
	recs := []Record{
		rec("fleurs", "a", "en", "one two", "one two", 500, 1000),                 // 0/2
		rec("fleurs", "b", "en", "one two three four", "one X three", 1500, 3000), // 2/4
		failed, // 4/4
	}
	g := summarizeGroups(recs)
	if len(g) != 1 || g[0].RefWords != 10 || g[0].Edits != 6 || g[0].OK != 2 || g[0].Errors != 1 {
		t.Fatalf("group = %+v", g)
	}
	if *g[0].WER != 0.6 { // a mean of the per-clip rates would be 0.5
		t.Errorf("WER = %v, want 0.6 (micro-averaged)", *g[0].WER)
	}
	if *g[0].RTF.P50 != 0.5 {
		t.Errorf("RTF p50 = %v", *g[0].RTF.P50)
	}
}

func validResult(recs ...Record) *Result {
	prof := models.ProfileLite
	limits, _ := limitsFor(prof)
	return &Result{FormatVersion: VoiceFormatVersion, RunID: "run_X", Profile: string(prof), Emulated: true, Limits: limits, Phase: PhaseSTT, Records: recs,
		Model: ModelRef{Name: "whisper-base", Files: []FileRef{{Name: "m.bin", SizeBytes: 10}}}}
}

func TestVerifyResultRejectsTamperingAndMislabel(t *testing.T) {
	good := validResult(rec("fleurs", "a", "en", "one two", "one two", 500, 1000))
	good.Summary = summarizeGroups(good.Records)
	if probs := verifyResult(good); len(probs) != 0 {
		t.Fatalf("a good result fails: %v", probs)
	}
	edit := func(f func(r *Result)) []string {
		b, _ := json.Marshal(good)
		var c Result
		_ = json.Unmarshal(b, &c)
		f(&c)
		return verifyResult(&c)
	}
	cases := map[string]func(r *Result){
		"edits changed by hand":   func(r *Result) { r.Records[0].Edits = 0; *r.Records[0].Hyp = "one" },
		"summary changed by hand": func(r *Result) { r.Summary[0].Edits = 5 },
		"emulated cleared":        func(r *Result) { r.Emulated = false },
		"limits changed":          func(r *Result) { r.Limits.MemMB = 4096 },
		"unknown profile":         func(r *Result) { r.Profile = "certified" },
		"error without a code":    func(r *Result) { r.Records[0].Status = StatusError },
		"duplicate record":        func(r *Result) { r.Records = append(r.Records, r.Records[0]) },
		"no records":              func(r *Result) { r.Records = nil },
	}
	for name, f := range cases {
		if probs := edit(f); len(probs) == 0 {
			t.Errorf("%s: verifyResult accepted it", name)
		}
	}
}

func TestEvaluateAppliesTheGatesAndFailsClosed(t *testing.T) {
	mkSTT := func(name string, size int64, enWER, itWER, rtf, memMiB float64) *Result {
		mk := func(lang string, wer float64) Record {
			words := 100
			edits := int(wer * float64(words))
			ref := strings.TrimSpace(strings.Repeat("w ", words))
			hyp := ref
			if edits > 0 {
				hyp = strings.TrimSpace(strings.Repeat("x ", edits) + strings.Repeat("w ", words-edits))
			}
			wall := rtf * 1000
			r := Record{Set: "fleurs", ID: "c-" + lang, Lang: lang, Status: StatusOK, AudioMS: 1000, WallMS: &wall, Ref: &ref, Hyp: &hyp}
			scoreRecord(&r)
			return r
		}
		r := validResult(mk("en", enWER), mk("it", itWER))
		r.Model = ModelRef{Name: name, Files: []FileRef{{Name: name, SizeBytes: size}}}
		r.Summary = summarizeGroups(r.Records)
		r.Memory = MemorySummary{Classes: map[string]ClassMem{"whisper": {PeakKB: int64(memMiB * 1024)}}}
		return r
	}
	tiny := mkSTT("whisper-tiny", 32, 0.12, 0.30, 0.2, 200)
	base := mkSTT("whisper-base", 60, 0.09, 0.19, 0.4, 350)    // passes en, misses it
	small := mkSTT("whisper-small", 190, 0.06, 0.10, 0.9, 900) // passes WER, breaks the memory gate
	v := Evaluate([]*Result{small, base, tiny}, nil)
	byLang := map[string]LangOutcome{}
	for _, o := range v.Languages {
		byLang[o.Lang] = o
	}
	if byLang["en"].STT != "whisper-tiny" {
		t.Errorf("en: the smallest passing candidate must win, got %q", byLang["en"].STT)
	}
	if byLang["it"].STT != "" {
		t.Errorf("it: base misses the WER bar and small the memory bar, so nobody wins; got %q", byLang["it"].STT)
	}
	for _, o := range v.Languages {
		if o.LiteOK {
			t.Errorf("%s: no TTS and no VAD result was given, so Lite voice cannot be OK", o.Lang)
		}
	}
	// a candidate whose memory was never sampled fails closed
	noMem := mkSTT("whisper-base", 60, 0.01, 0.01, 0.1, 0)
	noMem.Memory = MemorySummary{}
	if v := Evaluate([]*Result{noMem}, nil); v.Languages[0].STT != "" {
		t.Error("an unmeasured memory figure must fail the gate")
	}
}

func TestCountReloads(t *testing.T) {
	warm := map[string]float64{"chat": 800, "stt": 1500}
	calls := []CoCall{
		{Role: "chat", WallMS: 900}, {Role: "chat", WallMS: 4200}, // 4200 > 800 + 3000
		{Role: "stt", WallMS: 1600}, {Role: "stt", WallMS: 9000},
		{Role: "chat", WallMS: 99999, Error: strPtr("x")}, // an error is not a reload
	}
	by, total := countReloads(calls, warm)
	if total != 2 || by["chat"] != 1 || by["stt"] != 1 {
		t.Fatalf("reloads = %v (%d)", by, total)
	}
}

func companionRecords() []Record {
	zero, one := 0, 1
	mk := func(id, lang string, seg *int) Record {
		w := 40.0
		return Record{Set: "nonspeech", ID: id, Lang: lang, Status: StatusOK, AudioSHA: "a", AudioMS: 3000, WallMS: &w, Segments: seg}
	}
	return []Record{
		mk("ns-silence-en", "en", &zero), mk("ns-white-en", "en", &zero), mk("ns-hum-it", "it", &zero),
		mk("ns-lowpass-it", "it", &one), // the VAD let this clip through
		{Set: "nonspeech", ID: "ns-silence-it", Lang: "it", Status: StatusError, ErrorCode: strPtr("timeout"), AudioSHA: "a", AudioMS: 3000},
	}
}

func TestCompanionMapsDeclinesAndFailsClosed(t *testing.T) {
	b, err := buildCompanion(CompanionInput{
		RunID: "run_X", Timestamp: "2026-10-04T00:00:00Z", Profile: models.ProfileLite, VADModel: "silero-vad", Records: companionRecords(),
		VADFile: FileRef{Name: "v.bin", SHA256: strings.Repeat("a", 64), SizeBytes: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := b.Metrics.RefusalCorrectness
	if m.Total != 5 || m.Passed != 3 { // 3 declined; the clip let through and the failed call do not count as refusals
		t.Errorf("refusal_correctness = %d/%d, want 3/5", m.Passed, m.Total)
	}
	if b.Metrics.Pass.Passed != 3 || b.Errors.Count != 1 || b.Emulated != true || b.Synthetic {
		t.Errorf("pass=%d errors=%d emulated=%v synthetic=%v", b.Metrics.Pass.Passed, b.Errors.Count, b.Emulated, b.Synthetic)
	}
	if b.Metrics.CapabilityRouting.Total != 0 || b.Metrics.InjectionResistance.Total != 0 {
		t.Error("metrics that do not apply must have total 0")
	}
	if _, err := buildCompanion(CompanionInput{Profile: models.ProfileLite, Records: []Record{rec("fleurs", "a", "en", "x", "x", 1, 1)}}); err == nil {
		t.Error("a result with no non-speech clip has nothing to put in the companion")
	}
}

// TestCompanionPassesTheRealBenchCheck runs the repository's own `bench check` over a generated companion, so a drift between the copied
// envelope and tools/bench cannot go unseen.
func TestCompanionPassesTheRealBenchCheck(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	b, err := buildCompanion(CompanionInput{
		RunID: "run_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D", Timestamp: "2026-10-04T00:00:00Z", DurationMS: 1234, Profile: models.ProfileLite, VADModel: "silero-vad",
		Records: companionRecords(), CorpusSHA: strings.Repeat("b", 64), VADFile: FileRef{Name: "v.bin", SHA256: strings.Repeat("a", 64), SizeBytes: 5},
		Host: HostInfo{LogicalCores: 4, OS: runtime.GOOS, Arch: runtime.GOARCH}, Backend: BackendInfo{Runtime: strPtr("localai"), Name: strPtr("whisper"), RuntimeVersion: strPtr("v4.10.0")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	data, _ := json.MarshalIndent(b, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "companion.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBin, "run", "./tools/bench", "check", dir)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bench check: %v\n%s", err, out)
	}
	var back BenchResult
	if json.Unmarshal(data, &back) != nil || !reflect.DeepEqual(back.Metrics, b.Metrics) {
		t.Error("the companion does not survive a JSON round trip")
	}
}

func TestEvaluateReadsRunsTogetherAndWorstCaseWins(t *testing.T) {
	mk := func(rtf, memMiB float64) *Result {
		wall := rtf * 1000
		ref := "a b c d e f g h i j"
		r := Record{Set: "fleurs", ID: "c", Lang: "en", Status: StatusOK, AudioMS: 1000, WallMS: &wall, Ref: &ref, Hyp: &ref}
		scoreRecord(&r)
		res := validResult(r)
		res.Model = ModelRef{Name: "whisper-base", Files: []FileRef{{Name: "m", SizeBytes: 60}}}
		res.Summary = summarizeGroups(res.Records)
		res.Memory = MemorySummary{Classes: map[string]ClassMem{"whisper": {PeakKB: int64(memMiB * 1024)}}}
		return res
	}
	// run 2 was disturbed by a neighbour (rtf 3.0): the median of the three medians is still 0.4, but one run broke the memory bar
	v := Evaluate([]*Result{mk(0.4, 300), mk(3.0, 300), mk(0.5, 520)}, nil)
	got := map[int]Gate{}
	for _, g := range v.Gates {
		if g.Lang == "en" {
			got[g.ID] = g
		}
	}
	if got[2].Value == nil || *got[2].Value != 0.5 || !got[2].Pass || got[2].Runs != 3 {
		t.Errorf("rtf gate = %+v, want the median 0.5 of the runs' medians", got[2])
	}
	if got[3].Pass || *got[3].Value != 520 {
		t.Errorf("memory gate = %+v, want the largest peak 520 to fail", got[3])
	}
}

func TestEvaluateCoresidencyGatesOnlyAFullyProvisionedRun(t *testing.T) {
	co := func(max, reloads int, peakMiB int64) *CoResult {
		return &CoResult{MaxActive: max, DeclaredRoles: CoreResidentRoles, ReloadsTotal: reloads, BudgetMiB: liteBudgetMiB,
			Models: map[string]string{"stt": "s", "tts": "t", "vad": "v"}, Memory: MemorySummary{RTotalPeakKB: peakMiB << 10}}
	}
	v := Evaluate(nil, []*CoResult{co(5, 0, 4000), co(2, 9, 4000)})
	if len(v.Coresidency) != 4 {
		t.Fatalf("got %d gates", len(v.Coresidency))
	}
	if !v.Coresidency[0].Pass || !v.Coresidency[1].Pass || v.Coresidency[0].Note != "" {
		t.Errorf("the fully provisioned run should pass its two gates: %+v", v.Coresidency[:2])
	}
	if v.Coresidency[2].Pass || !strings.HasPrefix(v.Coresidency[2].Note, "informational") {
		t.Errorf("the 2-slot run shows reloads and is informational, not gated: %+v", v.Coresidency[2])
	}
}
