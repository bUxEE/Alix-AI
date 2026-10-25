package main

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/trust"
)

// The release manifest itself (its types, the rules of its format, signing and verifying it) lives in
// internal/release, which the upgrade imports as well: there is one verifier. This file only builds the
// unsigned manifest from the release's inputs.

// ---------------------------------------------------------------------------------------------------------------
// Building

// artifactInput is one file the release ships.
type artifactInput struct {
	Kind string
	Path string
}

// manifestInputs is everything the manifest is built from.
type manifestInputs struct {
	Version   string
	Commit    string
	CreatedAt time.Time
	Artifacts []artifactInput
	// BuiltImages are the images this release builds, name=repo[:tag]@sha256:digest.
	BuiltImages []string
	// ComposeFiles are the compose files whose pinned third-party images are listed.
	ComposeFiles []string
	// QuadletFiles are Quadlet .container files whose pinned registry images are listed too. An Image= that names a
	// .build or .image unit is built locally and has nothing to pin.
	QuadletFiles []string
	// BackendGallery is the pinned LocalAI backend catalogue.
	BackendGallery string
	Registry       *models.Registry
}

// buildReleaseManifest assembles the unsigned manifest. Every input that is missing, unpinned or malformed is an
// error: a manifest that quietly lists less than the release contains would be a false statement.
func buildReleaseManifest(in manifestInputs) (*release.Manifest, error) {
	if in.Registry == nil {
		return nil, errors.New("no model registry")
	}
	s := release.Signed{
		Kind:          release.Kind,
		ApplicationID: trust.PlatformApplicationID,
		Version:       in.Version,
		Commit:        in.Commit,
		CreatedAt:     in.CreatedAt.UTC().Truncate(time.Second).Format(release.TimeLayout),
		HashAlgorithm: release.HashAlgorithm,
		Artifacts:     []release.Artifact{},
		Images:        []release.Image{},
		Backends:      []release.Backend{},
		Models:        []release.Model{},
	}
	for _, a := range in.Artifacts {
		e, err := artifactFromFile(a)
		if err != nil {
			return nil, err
		}
		s.Artifacts = append(s.Artifacts, e)
	}
	for _, kv := range in.BuiltImages {
		name, ref, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("built image %q is not name=repo[:tag]@sha256:<digest>", kv)
		}
		r, err := release.ParseImageRef(ref)
		if err != nil {
			return nil, err
		}
		repo := r.Repo
		if r.Tag != "" {
			repo += ":" + r.Tag
		}
		s.Images = append(s.Images, release.Image{Name: name, Source: release.SourceBuilt, Reference: repo, Digest: r.Digest})
	}
	third, err := composeImages(in.ComposeFiles)
	if err != nil {
		return nil, err
	}
	quadlet, err := quadletImages(in.QuadletFiles)
	if err != nil {
		return nil, err
	}
	s.Images = append(s.Images, dedupeImages(append(third, quadlet...))...)
	if in.BackendGallery != "" {
		if s.Backends, err = galleryBackends(in.BackendGallery); err != nil {
			return nil, err
		}
	}
	if s.Models, err = registryModels(in.Registry); err != nil {
		return nil, err
	}

	sort.Slice(s.Artifacts, func(i, j int) bool { return s.Artifacts[i].Name < s.Artifacts[j].Name })
	sort.Slice(s.Images, func(i, j int) bool { return s.Images[i].SortKey() < s.Images[j].SortKey() })
	sort.Slice(s.Backends, func(i, j int) bool { return s.Backends[i].SortKey() < s.Backends[j].SortKey() })
	sort.Slice(s.Models, func(i, j int) bool { return s.Models[i].ID < s.Models[j].ID })

	m := &release.Manifest{Format: release.Format, Signed: s, Signatures: []trust.Signature{}}
	if err := m.Signed.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// artifactFromFile hashes one release file. A binary also records the target it was built for, read from the
// build information inside it, never from its name.
func artifactFromFile(a artifactInput) (release.Artifact, error) {
	sum, size, err := release.HashFile(a.Path)
	if err != nil {
		return release.Artifact{}, err
	}
	e := release.Artifact{Name: filepath.Base(a.Path), Kind: a.Kind, SHA256: sum, Size: size}
	switch a.Kind {
	case release.KindBinary:
		info, err := buildinfo.ReadFile(a.Path)
		if err != nil {
			return release.Artifact{}, fmt.Errorf("%s is listed as a binary but has no Go build information: %w", a.Path, err)
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "GOOS":
				e.OS = s.Value
			case "GOARCH":
				e.Arch = s.Value
			}
		}
		if e.OS == "" || e.Arch == "" {
			return release.Artifact{}, fmt.Errorf("%s: the build information names no GOOS and GOARCH", a.Path)
		}
	case release.KindArchive, release.KindSBOM, release.KindChecksums:
	default:
		return release.Artifact{}, fmt.Errorf("artifact kind %q is not one of binary, archive, sbom, checksums", a.Kind)
	}
	return e, nil
}

// composeImages lists the third-party images the compose files pin. A service that is built from source has no
// image to list. An image that is not pinned by digest fails the whole manifest (SEC-064).
func composeImages(files []string) ([]release.Image, error) {
	var out []release.Image
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Services map[string]struct {
				Image string `yaml:"image"`
				Build any    `yaml:"build"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for svc, s := range doc.Services {
			if s.Image == "" || s.Build != nil {
				continue
			}
			r, err := release.ParseImageRef(s.Image)
			if err != nil {
				return nil, fmt.Errorf("%s service %s: %w", f, svc, err)
			}
			out = append(out, thirdPartyImage(r))
		}
	}
	return out, nil
}

// galleryBackends reads the pinned backend catalogue.
func galleryBackends(path string) ([]release.Backend, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []struct {
		Name    string   `yaml:"name"`
		Alias   string   `yaml:"alias"`
		Version string   `yaml:"version"`
		License string   `yaml:"license"`
		URI     string   `yaml:"uri"`
		Mirrors []string `yaml:"mirrors"`
	}
	if err := yaml.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := []release.Backend{}
	for _, it := range items {
		r, err := release.ParseImageRef(it.URI)
		if err != nil {
			return nil, fmt.Errorf("%s backend %q: %w", path, it.Name, err)
		}
		e := release.Backend{Runtime: "localai", Name: it.Name, Alias: it.Alias, Version: it.Version, Licence: it.License, Reference: r.Repo, Digest: r.Digest, Mirrors: []string{}}
		for _, m := range it.Mirrors {
			mr, err := release.ParseImageRef(m)
			if err != nil {
				return nil, fmt.Errorf("%s backend %q mirror: %w", path, it.Name, err)
			}
			if mr.Digest != r.Digest {
				return nil, fmt.Errorf("%s backend %q: mirror %s has a different digest from the backend", path, it.Name, m)
			}
			e.Mirrors = append(e.Mirrors, m)
		}
		sort.Strings(e.Mirrors)
		out = append(out, e)
	}
	return out, nil
}

// registryModels lists every registry entry with its pinned file and licence. An entry with no pinned hash, size,
// file name, repository or revision, or with an empty licence record, fails the manifest: the release could not
// state what it lets an operator fetch.
func registryModels(reg *models.Registry) ([]release.Model, error) {
	out := []release.Model{}
	for _, e := range reg.Entries() {
		a := e.Artifact
		if a.SHA256 == nil || a.SizeBytes == nil || a.Filename == nil || a.RepoURL == nil || a.Revision == nil {
			return nil, fmt.Errorf("model %s has no pinned artifact (repo_url, filename, revision, size_bytes and sha256 are all required in a release)", e.ID)
		}
		m := release.Model{
			ID: e.ID, Name: e.Name, Version: e.Version, Status: string(e.Status),
			Upstream: release.ModelUpstream{RepoURL: e.Upstream.RepoURL},
			Artifact: release.ModelArtifact{
				Format: a.Format, RepoURL: *a.RepoURL, Filename: *a.Filename, Revision: *a.Revision,
				SizeBytes: *a.SizeBytes, SHA256: *a.SHA256, QuantizedBy: a.QuantizedBy, FirstParty: a.FirstParty,
			},
			Licence: release.ModelLicence{SPDX: e.Licence.SPDX, URL: e.Licence.URL, RedistributionNotes: e.Licence.RedistributionNotes},
			Backend: release.ModelBackend{Runtime: e.Backend.Runtime, Name: e.Backend.Name, AllowedRuntimeVersions: append([]string{}, e.Backend.AllowedRuntimeVersions...)},
		}
		if e.Upstream.Revision != nil {
			m.Upstream.Revision = *e.Upstream.Revision
		}
		out = append(out, m)
	}
	return out, nil
}

var quadletImageRE = regexp.MustCompile(`(?m)^Image=(\S+)[ \t]*\r?$`)

// quadletImages lists the registry images that Quadlet .container files pin. Every such image must carry a digest
// (SEC-064 names compose and Quadlet files alike); an Image= naming a .build or .image unit is skipped.
func quadletImages(files []string) ([]release.Image, error) {
	var out []release.Image
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range quadletImageRE.FindAllStringSubmatch(string(raw), -1) {
			if strings.HasSuffix(m[1], ".build") || strings.HasSuffix(m[1], ".image") {
				continue
			}
			r, err := release.ParseImageRef(m[1])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			out = append(out, thirdPartyImage(r))
		}
	}
	return out, nil
}

func thirdPartyImage(r release.ImageRef) release.Image {
	repo := r.Repo
	if r.Tag != "" {
		repo += ":" + r.Tag
	}
	return release.Image{Name: r.Repo[strings.LastIndexByte(r.Repo, '/')+1:], Source: release.SourceThirdParty, Reference: repo, Digest: r.Digest}
}

// dedupeImages drops repeated entries: the same image is pinned in a compose file and in a Quadlet unit.
func dedupeImages(in []release.Image) []release.Image {
	seen := map[string]bool{}
	var out []release.Image
	for _, im := range in {
		if k := im.SortKey(); !seen[k] {
			seen[k] = true
			out = append(out, im)
		}
	}
	return out
}
