package repro

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/models"
)

// Check is the outcome of one verified input. Err is nil when it holds.
type Check struct {
	Name   string
	Detail string
	Err    error
}

// OK reports whether the check passed.
func (c Check) OK() bool { return c.Err == nil }

// VerifyOptions say where the inputs are.
type VerifyOptions struct {
	// Root is the repository root; every manifest path is relative to it.
	Root string
	// Registry is the model registry the manifest's model pins are compared with.
	Registry *models.Registry
	// ModelsDir, when set, is a directory that may hold the model files; every
	// file that is there is hashed. A file that is not there is skipped unless
	// RequireModels is set (LocalAI downloads and verifies it at its first start).
	ModelsDir     string
	RequireModels bool
}

// Verify checks every pinned input of the manifest against the repository
// without running a model: corpus checksums, model hashes against the registry
// and the model definitions, the runtime image digest, the profile limits and
// every published result (hash, and that it was measured on exactly the pinned
// inputs). It fails closed: anything it cannot read or does not recognise is a
// failed check.
func Verify(ctx context.Context, m *Manifest, o VerifyOptions) []Check {
	var out []Check
	add := func(name, detail string, err error) { out = append(out, Check{Name: name, Detail: detail, Err: err}) }

	for _, c := range m.Corpora {
		detail, err := verifyCorpus(o.Root, c)
		add("corpus "+c.ID, detail, err)
	}
	detail, err := verifyRuntime(o.Root, m)
	add("runtime", detail, err)
	for _, name := range sortedKeys(m.Profiles) {
		detail, err := verifyProfile(o.Root, m.Profiles[name])
		add("profile "+name, detail, err)
	}
	for _, md := range m.Models {
		detail, err := verifyModel(ctx, o, md)
		add("model "+md.ID, detail, err)
	}
	for _, r := range m.Runs {
		detail, err := verifyRun(o.Root, m, r)
		add("run "+r.ID, detail, err)
	}
	return out
}

// Failed counts the checks that did not hold.
func Failed(cs []Check) int {
	n := 0
	for _, c := range cs {
		if !c.OK() {
			n++
		}
	}
	return n
}

// safePath joins a manifest path to the root and refuses an absolute path or one
// that leaves the root.
func safePath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("path %q must be relative to the repository root", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the repository root", rel)
	}
	return filepath.Join(root, clean), nil
}

// FileSHA256 returns the lower-case hex SHA-256 of a regular file.
func FileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readPinned(root, rel, wantSHA string) ([]byte, string, error) {
	p, err := safePath(root, rel)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		return nil, "", fmt.Errorf("%s: sha256 is %s, the manifest pins %s", rel, got, wantSHA)
	}
	return data, rel, nil
}

func verifyCorpus(root string, c Corpus) (string, error) {
	data, _, err := readPinned(root, c.Path, c.SHA256)
	if err != nil {
		return "", err
	}
	lines := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines++
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if lines != c.Cases {
		return "", fmt.Errorf("%s: %d cases, the manifest pins %d", c.Path, lines, c.Cases)
	}
	return fmt.Sprintf("%s sha256 %.12s…, %d cases", c.Path, c.SHA256, lines), nil
}

var (
	imageLine  = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)`)
	memLimitRe = regexp.MustCompile(`(?m)^\s*mem_limit:\s*(\S+)`)
	cpusRe     = regexp.MustCompile(`(?m)^\s*cpus:\s*(\S+)`)
)

func verifyRuntime(root string, m *Manifest) (string, error) {
	rt := m.Runtime
	p, err := safePath(root, rt.Compose)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	mm := imageLine.FindAllSubmatch(data, -1)
	if len(mm) != 1 {
		return "", fmt.Errorf("%s: want exactly one image line, found %d", rt.Compose, len(mm))
	}
	if got := string(mm[0][1]); got != rt.Image {
		return "", fmt.Errorf("%s: image is %s, the manifest pins %s", rt.Compose, got, rt.Image)
	}
	if _, _, err := readPinned(root, rt.BackendGallery.Path, rt.BackendGallery.SHA256); err != nil {
		return "", err
	}
	op, err := safePath(root, rt.Override)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(op); err != nil || st.IsDir() {
		return "", fmt.Errorf("%s: compose override missing", rt.Override)
	}
	return fmt.Sprintf("image %s, backend %s (gallery sha256 %.12s…)", rt.Image, rt.Backend, rt.BackendGallery.SHA256), nil
}

func verifyProfile(root string, p Profile) (string, error) {
	path, err := safePath(root, p.Compose)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mem := memLimitRe.FindSubmatch(data)
	cpus := cpusRe.FindSubmatch(data)
	if mem == nil || cpus == nil {
		return "", fmt.Errorf("%s: mem_limit or cpus not found", p.Compose)
	}
	if string(mem[1]) != p.MemLimit || string(cpus[1]) != fmt.Sprint(p.CPUs) {
		return "", fmt.Errorf("%s: limits are %s / %s CPUs, the manifest pins %s / %d", p.Compose, mem[1], cpus[1], p.MemLimit, p.CPUs)
	}
	return fmt.Sprintf("%s mem %s, %d CPUs", p.Compose, p.MemLimit, p.CPUs), nil
}

// modelDefinition is the part of a LocalAI model definition that downloads files.
type modelDefinition struct {
	DownloadFiles []struct {
		Filename string `yaml:"filename"`
		URI      string `yaml:"uri"`
		SHA256   string `yaml:"sha256"`
	} `yaml:"download_files"`
}

func verifyModel(ctx context.Context, o VerifyOptions, md Model) (string, error) {
	var errs []error
	if o.Registry == nil {
		return "", errors.New("no model registry")
	}
	e, ok := o.Registry.Get(md.ID)
	switch {
	case !ok:
		errs = append(errs, fmt.Errorf("not in the model registry"))
	case e.Artifact.Filename == nil || *e.Artifact.Filename != md.Filename:
		errs = append(errs, fmt.Errorf("registry filename differs from the manifest's %s", md.Filename))
	case e.Artifact.SHA256 == nil || *e.Artifact.SHA256 != md.SHA256:
		errs = append(errs, fmt.Errorf("registry sha256 differs from the manifest's %.12s…", md.SHA256))
	case e.Artifact.SizeBytes == nil || *e.Artifact.SizeBytes != md.SizeBytes:
		errs = append(errs, fmt.Errorf("registry size differs from the manifest's %d", md.SizeBytes))
	case e.Quantization.Scheme != md.Quantization:
		errs = append(errs, fmt.Errorf("registry quantization %s differs from the manifest's %s", e.Quantization.Scheme, md.Quantization))
	}
	if err := verifyDefinition(o.Root, md); err != nil {
		errs = append(errs, err)
	}
	detail := fmt.Sprintf("%s %d bytes sha256 %.12s…", md.Filename, md.SizeBytes, md.SHA256)
	if o.ModelsDir != "" {
		file := filepath.Join(o.ModelsDir, md.Filename)
		switch sum, size, err := models.HashFile(ctx, file); {
		case errors.Is(err, os.ErrNotExist):
			if o.RequireModels {
				errs = append(errs, fmt.Errorf("%s is not in %s", md.Filename, o.ModelsDir))
			} else {
				detail += " (file not downloaded yet: not hashed)"
			}
		case err != nil:
			errs = append(errs, err)
		case sum != md.SHA256 || size != md.SizeBytes:
			errs = append(errs, fmt.Errorf("%s: file is %d bytes with sha256 %s, the manifest pins %d bytes %s", file, size, sum, md.SizeBytes, md.SHA256))
		default:
			detail += " (file hashed: match)"
		}
	}
	return detail, errors.Join(errs...)
}

func verifyDefinition(root string, md Model) error {
	p, err := safePath(root, md.Definition)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var docs []modelDefinition
	if err := yaml.Unmarshal(data, &docs); err != nil {
		return fmt.Errorf("%s: %w", md.Definition, err)
	}
	for _, d := range docs {
		for _, f := range d.DownloadFiles {
			if f.Filename == md.Filename {
				if f.SHA256 != md.SHA256 {
					return fmt.Errorf("%s: sha256 of %s is %.12s…, the manifest pins %.12s…", md.Definition, f.Filename, f.SHA256, md.SHA256)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("%s: no download_files entry for %s", md.Definition, md.Filename)
}

func verifyRun(root string, m *Manifest, r Run) (string, error) {
	if _, _, err := readPinned(root, r.Result, r.ResultSHA256); err != nil {
		return "", err
	}
	p, _ := safePath(root, r.Result)
	res, err := LoadResult(p)
	if err != nil {
		return "", err
	}
	md, _ := m.Model(r.Model)
	c, _ := m.Corpus(r.Corpus)
	b := m.Bench
	var errs []error
	want := func(field string, got, exp any) {
		if got != exp {
			errs = append(errs, fmt.Errorf("%s is %v, the manifest pins %v", field, got, exp))
		}
	}
	if res.Synthetic {
		errs = append(errs, errors.New("synthetic result: it measures the harness, not a model"))
	}
	if !res.Emulated {
		errs = append(errs, errors.New("emulated is false: no result is certified evidence"))
	}
	want("profile", res.Profile, r.Profile)
	want("provider", res.Provider.Name, b.Provider)
	want("model", res.Model.RegistryID, r.Model)
	want("model artifact sha256", res.Model.ArtifactSHA256, md.SHA256)
	want("quantization", res.Model.Quantization, md.Quantization)
	want("corpus id", res.Corpus.ID, c.ID)
	want("corpus sha256", res.Corpus.SHA256, c.SHA256)
	want("corpus cases run", res.Corpus.CasesRun, c.Cases)
	want("prompt id", res.Config.Prompt.ID, b.PromptID)
	want("prompt sha256", res.Config.Prompt.SHA256, b.PromptSHA256)
	want("role", res.Config.Role, b.Role)
	want("constraint", res.Config.Constraint, b.Constraint)
	want("max_toks", res.Config.MaxToks, b.MaxToks)
	want("context", res.Context.ConfiguredToks, b.ContextToks)
	want("percentile method", res.Config.PercentileMethod, b.PercentileMethod)
	want("runtime version", res.Backend.RuntimeVersion, m.Runtime.Version)
	want("backend", res.Backend.Name, m.Runtime.Backend)
	if res.Config.Limit != nil {
		errs = append(errs, fmt.Errorf("limit is %d: the published run must cover the whole corpus", *res.Config.Limit))
	}
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s config %s, %s, %s, ctx %d, sha256 %.12s…", r.Result, r.Config, r.Profile, md.Quantization, res.Context.ConfiguredToks, r.ResultSHA256), nil
}
