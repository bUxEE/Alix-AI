package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteReports writes report.json and report.md under outDir.
func WriteReports(outDir string, rep *Report) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	jb, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.json"), jb, 0o644); err != nil {
		return err
	}
	md := renderMarkdown(rep)
	return os.WriteFile(filepath.Join(outDir, "report.md"), []byte(md), 0o644)
}

// WriteQualityReports writes quality report.json and report.md under outDir.
func WriteQualityReports(outDir string, rep *QualityReport) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	jb, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.json"), jb, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "report.md"), []byte(renderQualityMarkdown(rep)), 0o644)
}

func renderQualityMarkdown(rep *QualityReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# llmbench quality report (Italian)\n\n")
	fmt.Fprintf(&b, "- generated: %s\n", rep.GeneratedAt)
	fmt.Fprintf(&b, "- mode: `%s`\n", rep.Mode)
	fmt.Fprintf(&b, "- endpoint: `%s`\n", rep.Endpoint)
	fmt.Fprintf(&b, "- model: `%s`\n", rep.Model)
	fmt.Fprintf(&b, "- language: `%s`\n", rep.Language)
	s := rep.Summary
	fmt.Fprintf(&b, "- pass rate: **%d / %d** (%.0f%%)\n\n", s.Passed, s.N, s.PassRate*100)
	fmt.Fprintf(&b, "| metric | value |\n| --- | ---: |\n")
	if s.SchemaValidRate != nil {
		fmt.Fprintf(&b, "| JSON schema validity | %d / %d (%.0f%%) |\n", s.SchemaValid, s.SchemaCases, *s.SchemaValidRate*100)
	}
	if s.VerifierPassRate != nil {
		fmt.Fprintf(&b, "| Verifier pass rate | %d / %d (%.0f%%) |\n", s.VerifierPassed, s.VerifierCases, *s.VerifierPassRate*100)
		fmt.Fprintf(&b, "| Verifier replace rate | %d / %d (%.0f%%) |\n", s.VerifierReplaced, s.VerifierCases, *s.VerifierReplaceRate*100)
	}
	if s.FactAccuracyMean != nil {
		fmt.Fprintf(&b, "| Fact/number accuracy (mean) | %.2f |\n", *s.FactAccuracyMean)
	}
	if s.ItalianOKRate != nil {
		fmt.Fprintf(&b, "| Italian language OK | %d / %d (%.0f%%) |\n", s.ItalianOK, s.ItalianChecked, *s.ItalianOKRate*100)
	}
	if s.JudgeMean != nil {
		fmt.Fprintf(&b, "| LLM-as-judge mean (1–5) | %.2f |\n", *s.JudgeMean)
	}
	fmt.Fprintf(&b, "\n## Cases\n\n")
	fmt.Fprintf(&b, "| id | kind | ok | problems |\n| --- | --- | --- | --- |\n")
	for _, c := range rep.Cases {
		prob := strings.Join(c.Problems, "; ")
		if c.Err != "" {
			prob = c.Err
		}
		fmt.Fprintf(&b, "| %s | %s | %v | %s |\n", c.CaseID, c.Kind, c.OK, escapeMDCell(prob))
	}
	b.WriteString("\n## Notes\n\n")
	for _, n := range rep.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}

func escapeMDCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func renderMarkdown(rep *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# llmbench report\n\n")
	fmt.Fprintf(&b, "- generated: %s\n", rep.GeneratedAt)
	fmt.Fprintf(&b, "- endpoint: `%s`\n", rep.Endpoint)
	fmt.Fprintf(&b, "- model: `%s`\n", rep.Model)
	fmt.Fprintf(&b, "- target full-answer p95: %s\n", rep.TargetFull)
	fmt.Fprintf(&b, "- max concurrent users within target: **%d**\n\n", rep.MaxConcurrentWithinTarget)
	fmt.Fprintf(&b, "| concurrency | n | errors | TTFT p50 ms | TTFT p95 ms | full p50 ms | full p95 ms | tok/s/user | tok/s agg | within target |\n")
	fmt.Fprintf(&b, "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, a := range rep.Aggregates {
		fmt.Fprintf(&b, "| %d | %d | %d | %.0f | %.0f | %.0f | %.0f | %.1f | %.1f | %v |\n",
			a.Concurrency, a.N, a.Errors, a.TTFT_P50_ms, a.TTFT_P95_ms, a.Full_P50_ms, a.Full_P95_ms,
			a.TokPerSecPerUser, a.TokPerSecAggregate, a.WithinTarget)
	}
	b.WriteString("\n## Notes\n\n")
	for _, n := range rep.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}
