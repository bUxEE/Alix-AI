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

  run       replay the speed corpus against an OpenAI-compatible endpoint
  quality   Italian quality evaluation (minutes / governance / company data)

Speed (latency / concurrency):

  go run ./tools/llmbench run \
    -endpoint http://127.0.0.1:8000/v1 \
    -key "$VLLM_API_KEY" \
    -model gpt-oss-20b \
    -out /tmp/llmbench-out

Italian quality (schema / verifier / facts / language; no invented latency):

  go run ./tools/llmbench quality \
    -endpoint http://127.0.0.1:8000/v1 \
    -key "$VLLM_API_KEY" \
    -model gpt-oss-20b \
    -out /tmp/llmbench-quality

  # equivalent:
  go run ./tools/llmbench run -mode quality -endpoint … -key … -out …

Optional LLM-as-judge (off by default): -judge-endpoint URL [-judge-key] [-judge-model]

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
	case "quality":
		if err := qualityCmd(os.Args[2:]); err != nil {
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
	mode := fs.String("mode", "speed", "speed | quality")
	endpoint := fs.String("endpoint", "", "OpenAI-compatible base URL ending in /v1")
	key := fs.String("key", "", "bearer API key")
	model := fs.String("model", "gpt-oss-20b", "model id")
	corpusDir := fs.String("corpus", "", "corpus directory (speed: testdata/corpus; quality: testdata/quality/it)")
	outDir := fs.String("out", "llmbench-out", "report output directory")
	target := fs.Duration("target", 8*time.Second, "p95 full-answer latency target (speed mode)")
	salt := fs.String("cache-salt", "", "optional cache_salt for every request (speed mode)")
	schemas := fs.String("schemas", "", "schemas/ directory (quality mode)")
	judgeEndpoint := fs.String("judge-endpoint", "", "optional OpenAI-compatible judge URL (quality; off by default)")
	judgeKey := fs.String("judge-key", "", "judge API key (defaults to -key)")
	judgeModel := fs.String("judge-model", "", "judge model id (defaults to -model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *mode {
	case "quality":
		return runQuality(*endpoint, *key, *model, *corpusDir, *outDir, *schemas, *judgeEndpoint, *judgeKey, *judgeModel)
	case "speed", "":
		return runSpeed(*endpoint, *key, *model, *corpusDir, *outDir, *target, *salt)
	default:
		return fmt.Errorf("unknown -mode %q (want speed|quality)", *mode)
	}
}

func qualityCmd(args []string) error {
	fs := flag.NewFlagSet("quality", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "OpenAI-compatible base URL ending in /v1")
	key := fs.String("key", "", "bearer API key")
	model := fs.String("model", "gpt-oss-20b", "model id")
	corpusDir := fs.String("corpus", "", "directory of Italian quality JSON (default: testdata/quality/it)")
	outDir := fs.String("out", "llmbench-quality-out", "report output directory")
	schemas := fs.String("schemas", "", "schemas/ directory (default: walk up to repo schemas/)")
	judgeEndpoint := fs.String("judge-endpoint", "", "optional OpenAI-compatible judge URL (off by default)")
	judgeKey := fs.String("judge-key", "", "judge API key (defaults to -key)")
	judgeModel := fs.String("judge-model", "", "judge model id (defaults to -model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runQuality(*endpoint, *key, *model, *corpusDir, *outDir, *schemas, *judgeEndpoint, *judgeKey, *judgeModel)
}

func runSpeed(endpoint, key, model, corpusDir, outDir string, target time.Duration, salt string) error {
	dir := corpusDir
	if dir == "" {
		dir = defaultCorpusDir()
	}
	corpus, err := LoadCorpus(dir)
	if err != nil {
		return err
	}
	cfg := Config{
		Endpoint:      endpoint,
		APIKey:        key,
		Model:         model,
		Corpus:        corpus,
		Concurrencies: []int{1, 4, 16, 64},
		TargetFull:    target,
		CacheSalt:     salt,
	}
	rep, err := Run(context.Background(), cfg)
	if err != nil {
		return err
	}
	if err := WriteReports(outDir, rep); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", filepath.Join(outDir, "report.json"), filepath.Join(outDir, "report.md"))
	fmt.Printf("max concurrent within target (%s): %d\n", target, rep.MaxConcurrentWithinTarget)
	return nil
}

func runQuality(endpoint, key, model, corpusDir, outDir, schemas, judgeEndpoint, judgeKey, judgeModel string) error {
	dir := corpusDir
	if dir == "" {
		dir = defaultQualityCorpusDir()
	}
	corpus, err := LoadQualityCorpus(dir)
	if err != nil {
		return err
	}
	sch, err := LoadQualitySchemas(schemas)
	if err != nil {
		return err
	}
	cfg := QualityConfig{
		Endpoint:      endpoint,
		APIKey:        key,
		Model:         model,
		Corpus:        corpus,
		Schemas:       sch,
		JudgeEndpoint: judgeEndpoint,
		JudgeAPIKey:   judgeKey,
		JudgeModel:    judgeModel,
	}
	rep, err := RunQuality(context.Background(), cfg)
	if err != nil {
		return err
	}
	if err := WriteQualityReports(outDir, rep); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s\n", filepath.Join(outDir, "report.json"), filepath.Join(outDir, "report.md"))
	fmt.Printf("Italian quality pass rate: %d / %d (%.0f%%)\n", rep.Summary.Passed, rep.Summary.N, rep.Summary.PassRate*100)
	return nil
}

func defaultCorpusDir() string {
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

func defaultQualityCorpusDir() string {
	candidates := []string{
		"tools/llmbench/testdata/quality/it",
		"testdata/quality/it",
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "testdata/quality/it"
}
