package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The gates of docs/evidence/m4-voice/protocol.md, fixed before any measurement. They are constants here so a run cannot quietly change
// them; changing one is an edit to the protocol (and an amendment there), not to a result.
const (
	gateWERSTT     = 0.15 // STT word error rate on FLEURS, per language
	gateRTFSTT     = 1.0  // STT real-time factor, median, strictly below
	gateMemSTTMiB  = 500
	gateWERTTS     = 0.10 // TTS round-trip word error rate, per voice
	gateTTFAMS     = 1500 // TTS time to first audio, median, at most
	gateRTFTTS     = 1.0  // TTS real-time factor, median, strictly below
	gateMemTTSMiB  = 300
	gateVADRecall  = 0.95 // share of FLEURS clips in which the VAD finds speech
	gateMemVADMiB  = 150
	judgeModelName = "whisper-small"
)

// Gate is one line of the verdict.
type Gate struct {
	ID        int      `json:"id"`
	Role      string   `json:"role"`
	Candidate string   `json:"candidate"`
	Lang      string   `json:"lang"`
	Metric    string   `json:"metric"`
	Value     *float64 `json:"value"`
	Bar       float64  `json:"bar"`
	Op        string   `json:"op"`
	Pass      bool     `json:"pass"`
	// Runs is how many cold-start runs the figure is read from (protocol Amendment 1).
	Runs int    `json:"runs"`
	Note string `json:"note,omitempty"`
}

// LangOutcome is what the gates say for one language.
type LangOutcome struct {
	Lang    string `json:"lang"`
	STT     string `json:"stt"`
	TTS     string `json:"tts"`
	VAD     string `json:"vad"`
	LiteOK  bool   `json:"lite_voice"`
	Reasons string `json:"reasons"`
}

// Verdict is the result of applying the protocol.
type Verdict struct {
	ProtocolCommit *string       `json:"protocol_commit"`
	Gates          []Gate        `json:"gates"`
	Coresidency    []Gate        `json:"coresidency"`
	Languages      []LangOutcome `json:"languages"`
}

func modelBytes(r *Result) int64 {
	var n int64
	for _, f := range r.Model.Files {
		n += f.SizeBytes
	}
	return n
}

func findGroup(r *Result, set, lang string) *Group {
	for i := range r.Summary {
		if r.Summary[i].Set == set && r.Summary[i].Lang == lang {
			return &r.Summary[i]
		}
	}
	return nil
}

// sumGroups adds the clip counts of a set over both languages.
func sumGroups(r *Result, set string) (clips, ok, speech, nonEmpty int) {
	for _, g := range r.Summary {
		if g.Set == set {
			clips += g.Clips
			ok += g.OK
			speech += g.SpeechClips
			nonEmpty += g.NonEmpty
		}
	}
	return
}

func classMiB(r *Result, class string) (float64, bool) {
	c, ok := r.Memory.Classes[class]
	if !ok || c.PeakKB == 0 {
		return 0, false
	}
	return float64(c.PeakKB) / 1024, true
}

// gate builds a line; a missing measurement fails closed.
func gate(id int, role, cand, lang, metric string, v *float64, bar float64, op string) Gate {
	g := Gate{ID: id, Role: role, Candidate: cand, Lang: lang, Metric: metric, Value: v, Bar: bar, Op: op}
	switch {
	case v == nil:
		g.Note = "not measured: fails closed"
	case op == "<=":
		g.Pass = *v <= bar
	case op == "<":
		g.Pass = *v < bar
	case op == ">=":
		g.Pass = *v >= bar
	case op == "==":
		g.Pass = *v == bar
	}
	return g
}

// runsOf returns the results of one candidate: the same phase, model and language configuration. The protocol runs every candidate three times
// from a cold start (Amendment 1) because the development machine is shared; the gates read the runs together.
func runsOf(rs []*Result, phase, model, lang string) []*Result {
	var out []*Result
	for _, r := range rs {
		if r.Phase == phase && r.Model.Name == model && (lang == "" || r.Config.Language == lang) {
			out = append(out, r)
		}
	}
	return out
}

// worst returns the largest of f over the runs (the least favourable), or nil when any run lacks the figure.
func worst(runs []*Result, f func(*Result) *float64) *float64 {
	var w *float64
	for _, r := range runs {
		v := f(r)
		if v == nil {
			return nil
		}
		if w == nil || *v > *w {
			w = v
		}
	}
	return w
}

// worstLow is worst for a figure where lower is worse (recall).
func worstLow(runs []*Result, f func(*Result) *float64) *float64 {
	var w *float64
	for _, r := range runs {
		v := f(r)
		if v == nil {
			return nil
		}
		if w == nil || *v < *w {
			w = v
		}
	}
	return w
}

// medianOfRuns returns the median of f over the runs, or nil when any run lacks the figure.
func medianOfRuns(runs []*Result, f func(*Result) *float64) *float64 {
	var xs []float64
	for _, r := range runs {
		v := f(r)
		if v == nil {
			return nil
		}
		xs = append(xs, *v)
	}
	if len(xs) == 0 {
		return nil
	}
	m := medianF(xs)
	return &m
}

func groupFig(set, lang string, pick func(*Group) *float64) func(*Result) *float64 {
	return func(r *Result) *float64 {
		if g := findGroup(r, set, lang); g != nil {
			return pick(g)
		}
		return nil
	}
}

// memFig reads the peak of the worker's process class. The class is the backend's directory name; a result recorded before a backend had a
// class of its own ("other-backend", the kokoros runs) is read through that legacy name when the named class is absent.
func memFig(class string) func(*Result) *float64 {
	return func(r *Result) *float64 {
		if m, ok := classMiB(r, class); ok {
			return &m
		}
		if m, ok := classMiB(r, "other-backend"); ok && class != "whisper" && class != "piper" {
			return &m
		}
		return nil
	}
}

func uniqueModels(rs []*Result, phase string) []string {
	size := map[string]int64{}
	for _, r := range rs {
		if r.Phase == phase {
			size[r.Model.Name] = modelBytes(r)
		}
	}
	names := make([]string, 0, len(size))
	for n := range size {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if size[names[i]] != size[names[j]] {
			return size[names[i]] < size[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

func setRuns(gs []Gate, n int) {
	for i := range gs {
		gs[i].Runs = n
	}
}

// Evaluate applies the protocol to a set of results. Candidates are tried smallest first (model file bytes); the winner of a role in a
// language is the first one that passes every gate of the role there. A figure that was not measured fails its gate (fail closed).
func Evaluate(results []*Result, co []*CoResult) Verdict {
	var v Verdict
	failed := map[string][]string{}
	judge := func(role, lang string, gs []Gate, name string, winner map[string]string) {
		all := true
		for _, g := range gs {
			all = all && g.Pass
			if !g.Pass {
				failed[role+"/"+lang] = append(failed[role+"/"+lang], fmt.Sprintf("%s gate %d", name, g.ID))
			}
		}
		v.Gates = append(v.Gates, gs...)
		if all && winner[lang] == "" {
			winner[lang] = name
		}
	}
	p50RTF := func(g *Group) *float64 { return g.RTF.P50 }
	p50TTFA := func(g *Group) *float64 { return g.TTFAMS.P50 }
	werOf := func(g *Group) *float64 { return g.WER }
	sttWinner, ttsWinner, vadWinner := map[string]string{}, map[string]string{}, map[string]string{}

	for _, lang := range []string{"en", "it"} {
		for _, name := range uniqueModels(results, PhaseSTT) {
			runs := runsOf(results, PhaseSTT, name, "")
			var fl []*Result
			for _, r := range runs {
				if findGroup(r, "fleurs", lang) != nil {
					fl = append(fl, r)
				}
			}
			if len(fl) == 0 {
				continue // a judge-only result has no FLEURS clips of this language
			}
			gs := []Gate{
				gate(1, "stt", name, lang, "wer", worst(fl, groupFig("fleurs", lang, werOf)), gateWERSTT, "<="),
				gate(2, "stt", name, lang, "rtf_median", medianOfRuns(fl, groupFig("fleurs", lang, p50RTF)), gateRTFSTT, "<"),
				gate(3, "stt", name, lang, "worker_peak_rss_mib", worst(fl, memFig("whisper")), gateMemSTTMiB, "<="),
			}
			setRuns(gs, len(fl))
			judge("stt", lang, gs, name, sttWinner)
		}
		for _, name := range uniqueModels(results, PhaseTTS) {
			runs := runsOf(results, PhaseTTS, name, lang)
			if len(runs) == 0 {
				continue
			}
			var judged []*Result
			for _, j := range runsOf(results, PhaseSTT, judgeModelName, "") {
				if findGroup(j, "tts:"+name, lang) != nil {
					judged = append(judged, j)
				}
			}
			var rt *float64
			if len(judged) > 0 {
				rt = worst(judged, groupFig("tts:"+name, lang, werOf))
			}
			gs := []Gate{
				gate(4, "tts", name, lang, "round_trip_wer", rt, gateWERTTS, "<="),
				gate(5, "tts", name, lang, "ttfa_median_ms", medianOfRuns(runs, groupFig("tts", lang, p50TTFA)), gateTTFAMS, "<="),
				gate(6, "tts", name, lang, "rtf_median", medianOfRuns(runs, groupFig("tts", lang, p50RTF)), gateRTFTTS, "<"),
				gate(7, "tts", name, lang, "worker_peak_rss_mib", worst(runs, memFig(runs[0].Model.Backend)), gateMemTTSMiB, "<="),
			}
			setRuns(gs, len(runs))
			judge("tts", lang, gs, name, ttsWinner)
		}
	}
	for _, name := range uniqueModels(results, PhaseVAD) {
		runs := runsOf(results, PhaseVAD, name, "")
		speechIn := func(r *Result) *float64 {
			clips, ok, sp, _ := sumGroups(r, "nonspeech")
			if clips == 0 || ok != clips {
				return nil
			}
			x := float64(sp)
			return &x
		}
		recall := func(r *Result) *float64 {
			_, ok, sp, _ := sumGroups(r, "fleurs")
			if ok == 0 {
				return nil
			}
			x := float64(sp) / float64(ok)
			return &x
		}
		gs := []Gate{
			gate(8, "vad", name, "all", "speech_segments_in_nonspeech_clips", worst(runs, speechIn), 0, "=="),
			gate(9, "vad", name, "all", "fleurs_clips_with_speech", worstLow(runs, recall), gateVADRecall, ">="),
			gate(10, "vad", name, "all", "worker_peak_rss_mib", worst(runs, memFig("whisper")), gateMemVADMiB, "<="),
		}
		setRuns(gs, len(runs))
		judge("vad", "all", gs, name, vadWinner)
	}
	for _, c := range co {
		cand := fmt.Sprintf("%s+%s+%s", c.Models["stt"], c.Models["tts"], c.Models["vad"])
		rv, peak := float64(c.ReloadsTotal), float64(c.Memory.RTotalPeakKB)/1024
		gs := []Gate{
			gate(11, "coresidency", cand, "all", fmt.Sprintf("resident_role_reloads_at_max_active_%d", c.MaxActive), &rv, 0, "=="),
			gate(12, "coresidency", cand, "all", fmt.Sprintf("r_total_peak_mib_at_max_active_%d", c.MaxActive), &peak, float64(c.BudgetMiB), "<="),
		}
		setRuns(gs, 1)
		if c.MaxActive < c.DeclaredRoles {
			for i := range gs {
				gs[i].Note = fmt.Sprintf("informational: max_active_backends %d is below the %d declared resident roles, so R3 predicts eviction; not gated", c.MaxActive, c.DeclaredRoles)
			}
		}
		v.Coresidency = append(v.Coresidency, gs...)
	}
	for _, lang := range []string{"en", "it"} {
		o := LangOutcome{Lang: lang, STT: sttWinner[lang], TTS: ttsWinner[lang], VAD: vadWinner["all"]}
		o.LiteOK = o.STT != "" && o.TTS != "" && o.VAD != ""
		var why []string
		if o.STT == "" {
			why = append(why, "no STT candidate passes (failed: "+strings.Join(failed["stt/"+lang], ", ")+")")
		}
		if o.TTS == "" {
			why = append(why, "no TTS voice passes (failed: "+strings.Join(failed["tts/"+lang], ", ")+")")
		}
		if o.VAD == "" {
			why = append(why, "the VAD does not pass (failed: "+strings.Join(failed["vad/all"], ", ")+")")
		}
		o.Reasons = strings.Join(why, "; ")
		v.Languages = append(v.Languages, o)
	}
	return v
}

func walkFiles(dir, suffix string, visit func(path string) error) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(p, suffix) {
			return werr
		}
		return visit(p)
	})
}

func loadResults(dir string) ([]*Result, error) {
	var out []*Result
	err := walkFiles(dir, ".voice", func(p string) error {
		r, err := readResult(p)
		if err != nil {
			return err
		}
		if probs := verifyResult(r); len(probs) > 0 {
			return fmt.Errorf("%s does not verify: %s", p, strings.Join(probs, "; "))
		}
		out = append(out, r)
		return nil
	})
	if err == nil && len(out) == 0 {
		err = errors.New("no .voice results found")
	}
	return out, err
}

func loadCoResults(dir string) ([]*CoResult, error) {
	var out []*CoResult
	err := walkFiles(dir, ".coresident", func(p string) error {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var c CoResult
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, &c)
		return nil
	})
	return out, err
}

func markdownTable(v Verdict) string {
	var b strings.Builder
	b.WriteString("| Gate | Role | Candidate | Lang | Metric | Value | Bar | Runs | Pass |\n| --- | --- | --- | --- | --- | ---: | ---: | ---: | --- |\n")
	for _, g := range append(append([]Gate{}, v.Gates...), v.Coresidency...) {
		val := "n/a"
		if g.Value != nil {
			val = fmt.Sprintf("%.3f", *g.Value)
		}
		pass := "FAIL"
		if g.Pass {
			pass = "pass"
		}
		if strings.HasPrefix(g.Note, "informational") {
			pass += " (info)"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s %g | %d | %s |\n", g.ID, g.Role, g.Candidate, g.Lang, g.Metric, val, g.Op, g.Bar, g.Runs, pass)
	}
	return b.String()
}

func verdictCommand(args []string) error {
	fs := flag.NewFlagSet("verdict", flag.ContinueOnError)
	dir := fs.String("results", "benchmarks/results/m4", "directory of .voice and .coresident results")
	out := fs.String("out", "docs/evidence/m4-voice/verdict.json", "verdict file to write")
	md := fs.String("md", "", "also write the gate table as Markdown to this file")
	repo := fs.String("repo", ".", "repository root (for the protocol commit)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rs, err := loadResults(*dir)
	if err != nil {
		return err
	}
	co, err := loadCoResults(*dir)
	if err != nil {
		return err
	}
	v := Evaluate(rs, co)
	v.ProtocolCommit = protocolCommit(context.Background(), *repo, "docs/evidence/m4-voice/protocol.md")
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	table := markdownTable(v)
	if *md != "" {
		if err := os.WriteFile(*md, []byte(table), 0o644); err != nil {
			return err
		}
	}
	fmt.Print(table)
	for _, o := range v.Languages {
		fmt.Printf("%s: stt=%q tts=%q vad=%q lite_voice=%v %s\n", o.Lang, o.STT, o.TTS, o.VAD, o.LiteOK, o.Reasons)
	}
	return nil
}
