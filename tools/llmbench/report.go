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
