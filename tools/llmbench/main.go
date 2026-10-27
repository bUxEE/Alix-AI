package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const usage = `usage: llmbench <command> [flags]

  run    replay the corpus against an OpenAI-compatible endpoint

One-command lab run (after Performance vLLM is up):

  go run ./tools/llmbench run \
    -endpoint http://127.0.0.1:8000/v1 \
    -key "$VLLM_API_KEY" \
    -model gpt-oss-20b \
    -out /tmp/llmbench-out

Do not run against real GPUs in CI (none assumed). Unit tests use a fake endpoint.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "llmbench: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "OpenAI-compatible base URL ending in /v1")
	key := fs.String("key", "", "bearer API key")
	model := fs.String("model", "gpt-oss-20b", "model id")
	corpusDir := fs.String("corpus", "", "directory of turn JSON (default: testdata/corpus next to this tool)")
	outDir := fs.String("out", "llmbench-out", "report output directory")
	target := fs.Duration("target", 8*time.Second, "p95 full-answer latency target")
	salt := fs.String("cache-salt", "", "optional cache_salt for every request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := *corpusDir
	if dir == "" {
		dir = defaultCorpusDir()
	}
	corpus, err := LoadCorpus(dir)
	if err != nil {
		return err
	}
	cfg := Config{
		Endpoint:      *endpoint,
		APIKey:        *key,
		Model:         *model,
		Corpus:        corpus,
		Concurrencies: []int{1, 4, 16, 64},
		TargetFull:    *target,
		CacheSalt:     *salt,
	}
	rep, err := Run(context.Background(), cfg)
	if err != nil {
		return err
	}
	if err := WriteReports(*outDir, rep); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", filepath.Join(*outDir, "report.json"), filepath.Join(*outDir, "report.md"))
	fmt.Printf("max concurrent within target (%s): %d\n", *target, rep.MaxConcurrentWithinTarget)
	return nil
}

func defaultCorpusDir() string {
	// Prefer source-tree path when running via go run.
	candidates := []string{
		"tools/llmbench/testdata/corpus",
		"testdata/corpus",
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "testdata/corpus"
}
