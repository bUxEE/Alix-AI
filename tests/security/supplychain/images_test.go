package supplychainsec

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/tests/security/secutil"
)

// digestPinned is the only accepted way to name an image: any reference ending
// in an @sha256 digest of 64 lowercase hex characters. A tag is informational.
var digestPinned = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:\-]*@sha256:[0-9a-f]{64}$`)

// imageRef is one image reference found in a file, with where it was found.
type imageRef struct {
	File  string
	Where string
	Ref   string
}

// shellVar is `${NAME}`, `${NAME:-default}`, `${NAME-default}`, `${NAME:?msg}`
// or `$NAME`.
var shellVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// resolveVars substitutes variables from defaults, then from the inline
// `${X:-default}` value. A variable with no value anywhere (including the
// required form `${X:?msg}`) is left as `${X}` so the pin check refuses it: an
// image whose digest is chosen by the environment is not pinned.
func resolveVars(ref string, defaults map[string]string) string {
	return shellVar.ReplaceAllStringFunc(ref, func(m string) string {
		sub := shellVar.FindStringSubmatch(m)
		name, op, inline := sub[1], sub[2], sub[3]
		if name == "" {
			name = sub[4]
		}
		if v, ok := defaults[name]; ok && v != "" {
			return v
		}
		if op == ":-" || op == "-" {
			return inline
		}
		return "${" + name + "}"
	})
}

// pinProblem returns why ref is not pinned by digest, or "".
func pinProblem(ref string) string {
	ref = strings.Trim(strings.TrimSpace(ref), `"'`)
	switch {
	case ref == "":
		return "empty image reference"
	case strings.Contains(ref, "${") || strings.Contains(ref, "$"):
		return "the digest is chosen by an unresolved variable"
	case !strings.Contains(ref, "@"):
		return "no digest (name@sha256:<64 hex>); a tag can move"
	case !digestPinned.MatchString(ref):
		return "digest is not sha256 of 64 lowercase hex characters at the end of the reference"
	}
	return ""
}

// ---- compose ----

// composeImages reads the image of every service. A service with a `build:`
// section and no image is built locally from a Dockerfile, which is checked on
// its own; a service with neither is an override and names nothing.
func composeImages(file string, data []byte) ([]imageRef, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var out []imageRef
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		svc := doc.Services[n]
		img, ok := svc["image"].(string)
		if !ok {
			continue
		}
		if _, builds := svc["build"]; builds {
			// `image:` names the output of a local build, not something pulled.
			continue
		}
		out = append(out, imageRef{File: file, Where: "service " + n, Ref: resolveVars(img, nil)})
	}
	return out, nil
}

// ---- Quadlet ----

// quadletImages reads `Image=` of [Container] and [Image] sections. A value that
// names another Quadlet unit (`x.build`, `x.image`, `x.container`) is a local
// build, whose Dockerfile is checked on its own.
func quadletImages(file, text string) []imageRef {
	var out []imageRef
	section := ""
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[Container]" && section != "[Image]" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "Image" {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			continue // an empty Image= in a drop-in resets the value
		}
		switch path.Ext(val) {
		case ".build", ".image", ".container":
			continue
		}
		out = append(out, imageRef{File: file, Where: fmt.Sprintf("line %d", i+1), Ref: resolveVars(val, nil)})
	}
	return out
}

// ---- Dockerfile ----

// dockerfileImages reads every FROM of a Dockerfile, resolving ARG defaults.
// FROM scratch and FROM <an earlier stage> are not pulled images.
func dockerfileImages(file, text string) []imageRef {
	var lines []string
	var cur strings.Builder
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		t := strings.TrimRight(raw, " \t")
		if strings.HasSuffix(t, "\\") {
			cur.WriteString(strings.TrimSuffix(t, "\\") + " ")
			continue
		}
		cur.WriteString(t)
		lines = append(lines, cur.String())
		cur.Reset()
	}
	args := map[string]string{}
	stages := map[string]bool{}
	var out []imageRef
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "ARG":
			for _, a := range f[1:] {
				if k, v, ok := strings.Cut(a, "="); ok {
					args[k] = strings.Trim(v, `"'`)
				}
			}
		case "FROM":
			rest := f[1:]
			for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				continue
			}
			ref := resolveVars(rest[0], args)
			if ref != "scratch" && !stages[strings.ToLower(ref)] {
				out = append(out, imageRef{File: file, Where: fmt.Sprintf("FROM line %d", i+1), Ref: ref})
			}
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				stages[strings.ToLower(rest[2])] = true
			}
		}
	}
	return out
}

// ---- env files ----

// envImages reads `*IMAGE*=value` lines of an env example file.
func envImages(file, text string) []imageRef {
	var out []imageRef
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !strings.Contains(strings.ToUpper(key), "IMAGE") || strings.TrimSpace(val) == "" {
			continue
		}
		out = append(out, imageRef{File: file, Where: fmt.Sprintf("line %d (%s)", i+1, strings.TrimSpace(key)), Ref: strings.TrimSpace(val)})
	}
	return out
}

// ---- GitHub workflows ----

// workflowImages reads job containers, service containers and docker:// actions.
func workflowImages(file string, data []byte) ([]imageRef, error) {
	var doc struct {
		Jobs map[string]struct {
			Container any `yaml:"container"`
			Services  map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
			Steps []struct {
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var out []imageRef
	jobs := make([]string, 0, len(doc.Jobs))
	for n := range doc.Jobs {
		jobs = append(jobs, n)
	}
	sort.Strings(jobs)
	for _, n := range jobs {
		j := doc.Jobs[n]
		switch c := j.Container.(type) {
		case string:
			out = append(out, imageRef{File: file, Where: "job " + n + " container", Ref: c})
		case map[string]any:
			if s, ok := c["image"].(string); ok {
				out = append(out, imageRef{File: file, Where: "job " + n + " container", Ref: s})
			}
		}
		for sn, s := range j.Services {
			out = append(out, imageRef{File: file, Where: "job " + n + " service " + sn, Ref: s.Image})
		}
		for _, st := range j.Steps {
			if ref, ok := strings.CutPrefix(st.Uses, "docker://"); ok {
				out = append(out, imageRef{File: file, Where: "job " + n + " docker:// action", Ref: ref})
			}
		}
	}
	return out, nil
}

// ---- discovery ----

var (
	composeName = regexp.MustCompile(`(^|/)((docker|podman)-)?compose([.-][^/]*)?\.ya?ml$|\.compose\.ya?ml$`)
	envExample  = regexp.MustCompile(`(^|/)[^/]*\.env\.example$|(^|/)\.env\.[^/]*example$|(^|/)\.env\.example$`)
)

func isDockerfile(rel string) bool {
	base := path.Base(rel)
	return strings.HasPrefix(base, "Dockerfile") && !strings.HasSuffix(base, ".dockerignore") ||
		strings.HasPrefix(base, "Containerfile") || strings.HasSuffix(base, ".Dockerfile")
}

func isQuadlet(rel string) bool {
	switch path.Ext(rel) {
	case ".container", ".image":
		return true
	case ".conf":
		return strings.Contains(rel, ".container.d/") || strings.Contains(rel, ".image.d/")
	}
	return false
}

// kindCounts says how many files of each kind the scan read.
type kindCounts struct{ compose, quadlet, dockerfile, env, workflow int }

// scanImages applies every detector to the tracked files of the checkout.
func scanImages(t testing.TB) (refs []imageRef, counts kindCounts) {
	t.Helper()
	for _, rel := range secutil.RepoFiles(t) {
		var found []imageRef
		var err error
		switch {
		case composeName.MatchString(rel):
			counts.compose++
			found, err = composeImages(rel, secutil.ReadFile(t, rel))
		case isQuadlet(rel):
			counts.quadlet++
			found = quadletImages(rel, string(secutil.ReadFile(t, rel)))
		case isDockerfile(rel):
			counts.dockerfile++
			found = dockerfileImages(rel, string(secutil.ReadFile(t, rel)))
		case envExample.MatchString(rel):
			counts.env++
			found = envImages(rel, string(secutil.ReadFile(t, rel)))
		case strings.HasPrefix(rel, ".github/workflows/") && (path.Ext(rel) == ".yml" || path.Ext(rel) == ".yaml"):
			counts.workflow++
			found, err = workflowImages(rel, secutil.ReadFile(t, rel))
		}
		if err != nil {
			t.Errorf("cannot parse: %v", err)
		}
		refs = append(refs, found...)
	}
	return refs, counts
}

// SEC-064
//
// Every image reference in every compose, Quadlet, Dockerfile, env example and
// workflow file is pinned by a sha256 digest, so a replaced LocalAI image, a
// moved tag or an unapproved backend image cannot be substituted silently.
func TestSecAllImagesPinnedByDigest(t *testing.T) {
	refs, counts := scanImages(t)
	if counts.compose == 0 || counts.quadlet == 0 || counts.dockerfile == 0 {
		t.Fatalf("the scan did not see every kind of container file: %+v", counts)
	}
	if len(refs) == 0 {
		t.Fatal("no image reference found anywhere: the scan is not seeing images")
	}
	pulled := 0
	for _, r := range refs {
		if p := pinProblem(r.Ref); p != "" {
			t.Errorf("%s (%s): image %q: %s", r.File, r.Where, r.Ref, p)
			continue
		}
		pulled++
	}
	if pulled == 0 {
		t.Fatal("no pinned image was found: the scan is not seeing the real images")
	}
	t.Logf("%d image references pinned by digest in %+v files", pulled, counts)
}

// SEC-064
//
// The detectors are proven against planted violations: each must flag its
// unpinned reference and each clean variant must pass, in compose, Quadlet,
// Dockerfile, env and workflow text alike. Without this a detector that cannot
// see its violation would let the real scan pass by finding nothing.
func TestSecImageDetectorsSeePlantedViolations(t *testing.T) {
	const digest = "@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b"
	flagged := func(refs []imageRef) int {
		n := 0
		for _, r := range refs {
			if pinProblem(r.Ref) != "" {
				n++
			}
		}
		return n
	}

	type tc struct {
		name string
		refs func(t *testing.T) []imageRef
		want int // refs that must be flagged
		all  int // refs the detector must have found
	}
	compose := func(doc string) func(*testing.T) []imageRef {
		return func(t *testing.T) []imageRef {
			refs, err := composeImages("compose.yaml", []byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			return refs
		}
	}
	tests := []tc{
		{"compose tag only", compose("services:\n  a:\n    image: redis:7\n"), 1, 1},
		{"compose no tag", compose("services:\n  a:\n    image: redis\n"), 1, 1},
		{"compose short digest", compose("services:\n  a:\n    image: redis@sha256:abc\n"), 1, 1},
		{"compose upper-case digest", compose("services:\n  a:\n    image: redis" + strings.ToUpper(digest) + "\n"), 1, 1},
		{"compose sha1 digest", compose("services:\n  a:\n    image: redis@sha1:0632c21ddbe440bb71bc7aa744796c227a516586\n"), 1, 1},
		{"compose digest then junk", compose("services:\n  a:\n    image: redis" + digest + ":x\n"), 1, 1},
		{"compose unresolved variable", compose("services:\n  a:\n    image: ${IMG}\n"), 1, 1},
		{"compose required variable", compose("services:\n  a:\n    image: ${IMG:?set it}\n"), 1, 1},
		{"compose variable default unpinned", compose("services:\n  a:\n    image: ${IMG:-redis:7}\n"), 1, 1},
		{"compose clean", compose("services:\n  a:\n    image: docker.io/localai/localai:v4.10.0" + digest + "\n"), 0, 1},
		{"compose variable default pinned", compose("services:\n  a:\n    image: ${IMG:-redis" + digest + "}\n"), 0, 1},
		{"compose local build is not pulled", compose("services:\n  a:\n    image: alixai:local\n    build: .\n"), 0, 0},
		{"compose override names no image", compose("services:\n  a:\n    mem_limit: 1g\n"), 0, 0},
		{"quadlet tag", func(*testing.T) []imageRef {
			return quadletImages("x.container", "[Container]\nImage=docker.io/localai/localai:v4\n")
		}, 1, 1},
		{"quadlet image section", func(*testing.T) []imageRef {
			return quadletImages("x.image", "[Image]\nImage=docker.io/library/redis:7\n")
		}, 1, 1},
		{"quadlet drop-in", func(*testing.T) []imageRef {
			return quadletImages("x.container.d/10.conf", "[Container]\nImage=redis\n")
		}, 1, 1},
		{"quadlet clean", func(*testing.T) []imageRef {
			return quadletImages("x.container", "[Container]\nImage=docker.io/localai/localai:v4.10.0"+digest+"\n")
		}, 0, 1},
		{"quadlet unit reference is a local build", func(*testing.T) []imageRef {
			return quadletImages("x.container", "[Container]\nImage=alixai.build\n")
		}, 0, 0},
		{"quadlet other sections ignored", func(*testing.T) []imageRef {
			return quadletImages("x.pod", "[Pod]\nImage=redis\n")
		}, 0, 0},
		{"dockerfile tag", func(*testing.T) []imageRef { return dockerfileImages("Dockerfile", "FROM alpine:3.20\n") }, 1, 1},
		{"dockerfile arg default unpinned", func(*testing.T) []imageRef {
			return dockerfileImages("Dockerfile", "ARG BASE=golang:1.27\nFROM ${BASE} AS build\n")
		}, 1, 1},
		{"dockerfile arg without default", func(*testing.T) []imageRef {
			return dockerfileImages("Dockerfile", "ARG BASE\nFROM ${BASE}\n")
		}, 1, 1},
		{"dockerfile platform flag", func(*testing.T) []imageRef {
			return dockerfileImages("Dockerfile", "FROM --platform=linux/amd64 alpine:3.20 AS x\n")
		}, 1, 1},
		{"dockerfile clean with stages", func(*testing.T) []imageRef {
			return dockerfileImages("Dockerfile", "ARG B=golang:1.27"+digest+"\nFROM ${B} AS build\nFROM scratch\nFROM build AS final\n")
		}, 0, 1},
		{"env example", func(*testing.T) []imageRef {
			return envImages(".env.example", "ALIXAI_IMAGE=docker.io/x/y:latest\nOTHER=1\n# LOCALAI_IMAGE=z\n")
		}, 1, 1},
		{"env example clean", func(*testing.T) []imageRef {
			return envImages(".env.example", "ALIXAI_IMAGE=docker.io/x/y:1"+digest+"\n")
		}, 0, 1},
		{"workflow container", func(t *testing.T) []imageRef {
			refs, err := workflowImages("w.yml", []byte("jobs:\n  a:\n    container: golang:1.27\n    services:\n      db:\n        image: postgres:16\n    steps:\n      - uses: docker://alpine:3.20\n"))
			if err != nil {
				t.Fatal(err)
			}
			return refs
		}, 3, 3},
		{"workflow container clean", func(t *testing.T) []imageRef {
			refs, err := workflowImages("w.yml", []byte("jobs:\n  a:\n    container:\n      image: golang:1.27"+digest+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			return refs
		}, 0, 1},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			refs := c.refs(t)
			if len(refs) != c.all {
				t.Fatalf("detector found %d references %v, want %d", len(refs), refs, c.all)
			}
			if got := flagged(refs); got != c.want {
				t.Fatalf("flagged %d of %v, want %d", got, refs, c.want)
			}
		})
	}

	// The file-kind matchers must recognise the shipped layouts.
	for rel, want := range map[string]bool{
		"deploy/container/stack/compose.yaml":                                  true,
		"deploy/container/stack/compose.lite.yaml":                             true,
		"docker-compose.yml":                                                   true,
		"deploy/container/stack/config/alixai.yaml":                            false,
		"deploy/container/quadlet/alixai.container":                            true,
		"deploy/container/quadlet/profiles/lite/x.container.d/10-profile.conf": true,
		"deploy/container/quadlet/alixai.env.example":                          false,
	} {
		got := composeName.MatchString(rel) || isQuadlet(rel)
		if got != want {
			t.Errorf("file classification of %s = %v, want %v", rel, got, want)
		}
	}
	for rel, want := range map[string]bool{
		"deploy/container/alixai/Dockerfile":              true,
		"deploy/container/alixai/Dockerfile.dockerignore": false,
		"x/Containerfile":  true,
		"x/app.Dockerfile": true,
		"x/main.go":        false,
	} {
		if isDockerfile(rel) != want {
			t.Errorf("isDockerfile(%s) = %v, want %v", rel, !want, want)
		}
	}
	for rel, want := range map[string]bool{
		"deploy/container/stack/.env.example":         true,
		"deploy/container/quadlet/alixai.env.example": true,
		"deploy/container/stack/compose.yaml":         false,
	} {
		if envExample.MatchString(rel) != want {
			t.Errorf("envExample(%s) = %v, want %v", rel, !want, want)
		}
	}
}
