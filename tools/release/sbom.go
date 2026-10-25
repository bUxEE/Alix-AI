package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/release"
)

// The SBOM is CycloneDX 1.6 JSON. Its three schema files are vendored from github.com/CycloneDX/specification at
// commit 1ce97b2a7b8cf2429da248560d2aa671c6bce74a (Apache-2.0) and compiled in, so a document can be validated
// offline. bom-1.6 refers to the other two by relative URL.
const (
	cdxSpecVersion = "1.6"
	cdxBaseURL     = "http://cyclonedx.org/schema/"
	cdxSchemaID    = cdxBaseURL + "bom-1.6.schema.json"
)

//go:embed schemas/*.schema.json
var cdxSchemaFiles embed.FS

var (
	cdxOnce     sync.Once
	cdxCompiled *jsonschema.Schema
	cdxErr      error
)

func compiledCycloneDX() (*jsonschema.Schema, error) {
	cdxOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		for _, name := range []string{"bom-1.6.schema.json", "spdx.schema.json", "jsf-0.82.schema.json"} {
			raw, err := cdxSchemaFiles.ReadFile("schemas/" + name)
			if err != nil {
				cdxErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				cdxErr = fmt.Errorf("embedded schema %s: %w", name, err)
				return
			}
			if err := c.AddResource(cdxBaseURL+name, doc); err != nil {
				cdxErr = fmt.Errorf("embedded schema %s: %w", name, err)
				return
			}
		}
		if cdxCompiled, cdxErr = c.Compile(cdxSchemaID); cdxErr != nil {
			cdxErr = fmt.Errorf("embedded CycloneDX schema does not compile: %w", cdxErr)
		}
	})
	return cdxCompiled, cdxErr
}

// validateCycloneDX checks raw against the CycloneDX 1.6 JSON schema.
func validateCycloneDX(raw []byte) error {
	sch, err := compiledCycloneDX()
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if err := sch.Validate(doc); err != nil {
		return fmt.Errorf("not a valid CycloneDX %s document: %w", cdxSpecVersion, err)
	}
	// The schema types specVersion as any string; this tool writes, and so checks for, exactly one version.
	if m, ok := doc.(map[string]any); !ok || m["specVersion"] != cdxSpecVersion {
		return fmt.Errorf("specVersion is not %q", cdxSpecVersion)
	}
	return nil
}

// The document model: only the CycloneDX properties this tool writes, in the order they are written.

type bom struct {
	BOMFormat    string          `json:"bomFormat"`
	SpecVersion  string          `json:"specVersion"`
	SerialNumber string          `json:"serialNumber"`
	Version      int             `json:"version"`
	Metadata     bomMetadata     `json:"metadata"`
	Components   []bomComponent  `json:"components"`
	Dependencies []bomDependency `json:"dependencies"`
}

type bomMetadata struct {
	Timestamp string       `json:"timestamp,omitempty"`
	Tools     bomTools     `json:"tools"`
	Component bomComponent `json:"component"`
}

type bomTools struct {
	Components []bomComponent `json:"components"`
}

type bomComponent struct {
	Type       string        `json:"type"`
	BOMRef     string        `json:"bom-ref,omitempty"`
	Name       string        `json:"name"`
	Version    string        `json:"version,omitempty"`
	Scope      string        `json:"scope,omitempty"`
	Hashes     []bomHash     `json:"hashes,omitempty"`
	Licenses   []bomLicense  `json:"licenses,omitempty"`
	PURL       string        `json:"purl,omitempty"`
	Properties []bomProperty `json:"properties,omitempty"`
}

type bomHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type bomLicense struct {
	License struct {
		ID string `json:"id"`
	} `json:"license"`
}

type bomProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type bomDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// sbomOptions are the inputs that are not read from a binary.
type sbomOptions struct {
	// Timestamp, when not empty, is metadata.timestamp (RFC 3339). It is left out by default, so the same inputs
	// always give the same bytes: a reproducible build has a reproducible SBOM.
	Timestamp string
}

func (o sbomOptions) validate() error {
	if o.Timestamp != "" {
		if _, err := time.Parse(time.RFC3339, o.Timestamp); err != nil {
			return fmt.Errorf("timestamp %q is not RFC 3339", o.Timestamp)
		}
	}
	return nil
}

// binaryInfo is a built binary: its hash and the module list its build recorded.
type binaryInfo struct {
	Name   string
	SHA256 string
	Size   int64
	Info   *buildinfo.BuildInfo
}

// readBinary hashes the executable at path and reads the build information Go embeds in it (ELF, PE and Mach-O,
// whatever the host). A file that is not a Go binary is an error.
func readBinary(name, path string) (binaryInfo, error) {
	sum, size, err := release.HashFile(path)
	if err != nil {
		return binaryInfo{}, err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return binaryInfo{}, fmt.Errorf("%s has no Go build information: %w", path, err)
	}
	return binaryInfo{Name: name, SHA256: sum, Size: size, Info: info}, nil
}

// purlEscape percent-encodes every byte of s outside the unreserved set.
func purlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._~-", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// golangPURL is pkg:golang/<namespace>/<name>@<version> for a module path.
func golangPURL(modulePath, version string) string {
	ns, name := "", modulePath
	if i := strings.LastIndexByte(modulePath, '/'); i >= 0 {
		ns, name = modulePath[:i], modulePath[i+1:]
	}
	var b strings.Builder
	b.WriteString("pkg:golang/")
	if ns != "" {
		for _, seg := range strings.Split(ns, "/") {
			b.WriteString(purlEscape(seg))
			b.WriteByte('/')
		}
	}
	b.WriteString(purlEscape(name))
	if version != "" {
		b.WriteString("@" + purlEscape(version))
	}
	return b.String()
}

// moduleComponents lists the modules and the standard library linked into a binary. A replaced module is listed
// as what was actually built, with the original path in a property.
func moduleComponents(info *buildinfo.BuildInfo) []bomComponent {
	var out []bomComponent
	for _, dep := range info.Deps {
		mod := dep
		var props []bomProperty
		if dep.Replace != nil {
			mod = dep.Replace
			props = append(props, bomProperty{"alixai:go.replaces", dep.Path})
		}
		version := mod.Version
		if version == "(devel)" {
			version = ""
		}
		purl := golangPURL(mod.Path, version)
		if mod.Sum != "" {
			// go.sum's h1: value is a hash of the module's file tree, not of a file, so it is a property and
			// not a CycloneDX hash.
			props = append(props, bomProperty{"alixai:go.sum", mod.Sum})
		}
		out = append(out, bomComponent{Type: "library", BOMRef: purl, Name: mod.Path, Version: version, Scope: "required", PURL: purl, Properties: props})
	}
	goVer := strings.TrimPrefix(info.GoVersion, "go")
	std := "pkg:golang/std@" + purlEscape(goVer)
	out = append(out, bomComponent{Type: "library", BOMRef: std, Name: "Go standard library", Version: goVer, Scope: "required", PURL: std})
	return out
}

func apache2() []bomLicense {
	var l bomLicense
	l.License.ID = "Apache-2.0"
	return []bomLicense{l}
}

// binaryComponent describes one binary. The licence is the project's own (LICENSE); the licences of the modules
// linked into it are not recorded here, they are gated by the CI licence allow-list.
func binaryComponent(b binaryInfo, version string) bomComponent {
	props := []bomProperty{{"alixai:go.path", b.Info.Path}, {"alixai:go.version", b.Info.GoVersion}}
	for _, s := range b.Info.Settings {
		props = append(props, bomProperty{"alixai:go.build." + s.Key, s.Value})
	}
	return bomComponent{
		Type: "application", BOMRef: "bin/" + b.Name, Name: b.Name, Version: version,
		Hashes: []bomHash{{"SHA-256", b.SHA256}}, Licenses: apache2(), Properties: props,
	}
}

// bomBuilder collects components (deduplicated by bom-ref) and the dependency edges between them.
type bomBuilder struct {
	comps map[string]bomComponent
	edges map[string]map[string]bool
}

func newBOMBuilder() *bomBuilder {
	return &bomBuilder{comps: map[string]bomComponent{}, edges: map[string]map[string]bool{}}
}

// add records c and returns its bom-ref.
func (b *bomBuilder) add(c bomComponent) string {
	if _, ok := b.comps[c.BOMRef]; !ok {
		b.comps[c.BOMRef] = c
	}
	if b.edges[c.BOMRef] == nil {
		b.edges[c.BOMRef] = map[string]bool{}
	}
	return c.BOMRef
}

func (b *bomBuilder) link(from string, to ...string) {
	if b.edges[from] == nil {
		b.edges[from] = map[string]bool{}
	}
	for _, t := range to {
		b.edges[from][t] = true
	}
}

// addModules adds the modules and standard library of a binary and links the binary to them.
func (b *bomBuilder) addModules(binRef string, info *buildinfo.BuildInfo) {
	for _, m := range moduleComponents(info) {
		b.link(binRef, b.add(m))
	}
}

// finish assembles the document around subject (which is not repeated in components). Components and
// dependencies are sorted, and the serial number is derived from the content, so the same inputs always give the
// same bytes.
func (b *bomBuilder) finish(subject bomComponent, opts sbomOptions) *bom {
	b.add(subject)
	doc := &bom{BOMFormat: "CycloneDX", SpecVersion: cdxSpecVersion, Version: 1}
	doc.Metadata = bomMetadata{
		Timestamp: opts.Timestamp,
		Tools:     bomTools{Components: []bomComponent{{Type: "application", Name: "alixai-release"}}},
		Component: subject,
	}
	refs := make([]string, 0, len(b.comps))
	for ref := range b.comps {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	doc.Components = []bomComponent{}
	doc.Dependencies = []bomDependency{}
	for _, ref := range refs {
		if ref != subject.BOMRef {
			doc.Components = append(doc.Components, b.comps[ref])
		}
		dep := bomDependency{Ref: ref}
		for to := range b.edges[ref] {
			dep.DependsOn = append(dep.DependsOn, to)
		}
		sort.Strings(dep.DependsOn)
		doc.Dependencies = append(doc.Dependencies, dep)
	}
	raw, _ := json.Marshal(doc)
	h := sha256.Sum256(append([]byte("alixai-sbom-v1\n"), raw...))
	u := h[:16]
	u[6] = u[6]&0x0f | 0x50 // version 5 layout, a name-based UUID
	u[8] = u[8]&0x3f | 0x80
	doc.SerialNumber = fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	return doc
}

// binarySBOM is the SBOM of one binary: the binary with its hash, the modules and the Go standard library it was
// linked from.
func binarySBOM(b binaryInfo, version string, opts sbomOptions) (*bom, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	bb := newBOMBuilder()
	subject := binaryComponent(b, version)
	bb.add(subject)
	bb.addModules(subject.BOMRef, b.Info)
	return bb.finish(subject, opts), nil
}

// ociPURL is pkg:oci/<name>[@<digest>]?repository_url=<repo>[&tag=<tag>].
func ociPURL(repo, tag, digest string) string {
	name := repo[strings.LastIndexByte(repo, '/')+1:]
	var b strings.Builder
	b.WriteString("pkg:oci/" + purlEscape(name))
	if digest != "" {
		b.WriteString("@" + purlEscape(digest))
	}
	b.WriteString("?repository_url=" + strings.ReplaceAll(purlEscape(repo), "%2F", "/"))
	if tag != "" {
		b.WriteString("&tag=" + purlEscape(tag))
	}
	return b.String()
}

// imageComponent describes a base image by digest.
func imageComponent(r release.ImageRef, scope string) bomComponent {
	purl := ociPURL(r.Repo, r.Tag, r.Digest)
	version := r.Tag
	if version == "" {
		version = r.Digest
	}
	return bomComponent{
		Type: "container", BOMRef: purl, Name: r.Repo, Version: version, Scope: scope, PURL: purl,
		Hashes: []bomHash{{"SHA-256", strings.TrimPrefix(r.Digest, "sha256:")}},
	}
}

// dockerfileImages are the two base images a Dockerfile pins through ARG.
type dockerfileImages struct {
	Builder release.ImageRef // GO_IMAGE: the build stage, not part of the final image
	Runtime release.ImageRef // RUNTIME_IMAGE: the base of the final image
}

var dockerfileArgRE = regexp.MustCompile(`^ARG\s+(GO_IMAGE|RUNTIME_IMAGE)=(\S+)\s*$`)

// parseDockerfileImages reads the digest-pinned build and runtime base images from the ARG lines of
// deploy/container/alixai/Dockerfile. Both are required and both must carry a digest.
func parseDockerfileImages(r io.Reader) (dockerfileImages, error) {
	var out dockerfileImages
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		m := dockerfileArgRE.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r"))
		if m == nil {
			continue
		}
		ref, err := release.ParseImageRef(m[2])
		if err != nil {
			return out, fmt.Errorf("Dockerfile ARG %s: %w", m[1], err)
		}
		seen[m[1]] = true
		if m[1] == "GO_IMAGE" {
			out.Builder = ref
		} else {
			out.Runtime = ref
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	for _, arg := range []string{"GO_IMAGE", "RUNTIME_IMAGE"} {
		if !seen[arg] {
			return out, fmt.Errorf("Dockerfile has no ARG %s=<image>@sha256:<digest> line", arg)
		}
	}
	return out, nil
}

// imageSpec names the image being described.
type imageSpec struct {
	Name      string
	Reference string // repository the image is pushed to, without tag or digest; may be empty
	Digest    string // sha256:<hex> once the image is built and pushed; may be empty
	Version   string
}

// imageSBOM is the SBOM of the service image as it is composed: the base image by digest, the binaries the
// Dockerfile copies in with their hashes, and the modules linked into them. It is derived from the Dockerfile
// and the binaries, not from a scan of the image's file system.
func imageSBOM(img imageSpec, bases dockerfileImages, bins []binaryInfo, opts sbomOptions) (*bom, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if img.Name == "" {
		return nil, fmt.Errorf("image name is required")
	}
	if img.Digest != "" && !digestRE.MatchString(img.Digest) {
		return nil, fmt.Errorf("image digest %q is not sha256:<64 hex>", img.Digest)
	}
	if len(bins) == 0 {
		return nil, fmt.Errorf("an image SBOM needs at least one binary")
	}
	subject := bomComponent{
		Type: "container", BOMRef: "image/" + img.Name, Name: img.Name, Version: img.Version, Licenses: apache2(),
	}
	if img.Digest != "" {
		subject.Hashes = []bomHash{{"SHA-256", strings.TrimPrefix(img.Digest, "sha256:")}}
	}
	if img.Reference != "" {
		subject.PURL = ociPURL(img.Reference, img.Version, img.Digest)
	}

	bb := newBOMBuilder()
	bb.add(subject)
	bb.link(subject.BOMRef, bb.add(imageComponent(bases.Runtime, "required")))
	bb.add(imageComponent(bases.Builder, "excluded")) // the build stage: recorded, not part of the image
	for _, bin := range bins {
		c := binaryComponent(bin, img.Version)
		bb.link(subject.BOMRef, bb.add(c))
		bb.addModules(c.BOMRef, bin.Info)
	}
	return bb.finish(subject, opts), nil
}

func cmdSBOM(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		fmt.Fprintln(stderr, "usage: release sbom binary -in FILE -version V [-name N] [-out FILE]\n       release sbom image -name N -version V -binary NAME=FILE... [-reference REPO] [-digest sha256:HEX] [-out FILE]")
		if len(args) == 0 {
			return usagef("sbom needs a mode: binary or image")
		}
		return nil
	}
	switch args[0] {
	case "binary":
		return sbomBinaryCmd(args[1:], stdout, stderr)
	case "image":
		return sbomImageCmd(args[1:], stdout, stderr)
	}
	return usagef("unknown sbom mode %q (want binary or image)", args[0])
}

// finishSBOM validates the document and writes it. An SBOM that does not validate is never written.
func finishSBOM(doc *bom, out string, stdout io.Writer) error {
	raw, err := marshalIndent(doc)
	if err != nil {
		return err
	}
	if err := validateCycloneDX(raw); err != nil {
		return fmt.Errorf("generated SBOM is invalid: %w", err)
	}
	return writeOutput(out, raw, stdout)
}

func sbomBinaryCmd(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sbom binary", stderr)
	in := fs.String("in", "", "the built binary")
	name := fs.String("name", "", "component name (default: the file name without .exe)")
	version := fs.String("version", "", "release version")
	out := fs.String("out", "", "output file (default: standard output)")
	ts := fs.String("timestamp", "", "metadata.timestamp, RFC 3339 (default: none, so the output is reproducible)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag("in", *in); err != nil {
		return err
	}
	if err := requireFlag("version", *version); err != nil {
		return err
	}
	if *name == "" {
		*name = strings.TrimSuffix(filepath.Base(*in), ".exe")
	}
	bin, err := readBinary(*name, *in)
	if err != nil {
		return err
	}
	doc, err := binarySBOM(bin, *version, sbomOptions{Timestamp: *ts})
	if err != nil {
		return err
	}
	return finishSBOM(doc, *out, stdout)
}

func sbomImageCmd(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sbom image", stderr)
	root := fs.String("C", "", "module root (default: the directory with go.mod above the working directory)")
	dockerfile := fs.String("dockerfile", "", "the image's Dockerfile (default: deploy/container/alixai/Dockerfile under the module root)")
	name := fs.String("name", "alixai", "image name")
	ref := fs.String("reference", "", "repository the image is pushed to, without tag or digest")
	digest := fs.String("digest", "", "image digest, sha256:<hex>, once it is pushed")
	version := fs.String("version", "", "release version (the image tag)")
	var bins multiFlag
	fs.Var(&bins, "binary", "NAME=FILE: a binary the image carries (repeatable, at least one)")
	out := fs.String("out", "", "output file (default: standard output)")
	ts := fs.String("timestamp", "", "metadata.timestamp, RFC 3339 (default: none)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag("version", *version); err != nil {
		return err
	}
	if *dockerfile == "" {
		modRoot, err := moduleRootFlag(*root)
		if err != nil {
			return err
		}
		*dockerfile = filepath.Join(modRoot, "deploy", "container", "alixai", "Dockerfile")
	}
	f, err := os.Open(*dockerfile)
	if err != nil {
		return err
	}
	defer f.Close()
	bases, err := parseDockerfileImages(f)
	if err != nil {
		return err
	}
	var infos []binaryInfo
	for _, kv := range bins {
		n, p, ok := strings.Cut(kv, "=")
		if !ok || n == "" || p == "" {
			return usagef("-binary %q is not NAME=FILE", kv)
		}
		bin, err := readBinary(n, p)
		if err != nil {
			return err
		}
		infos = append(infos, bin)
	}
	doc, err := imageSBOM(imageSpec{Name: *name, Reference: *ref, Digest: *digest, Version: *version}, bases, infos, sbomOptions{Timestamp: *ts})
	if err != nil {
		return err
	}
	return finishSBOM(doc, *out, stdout)
}
