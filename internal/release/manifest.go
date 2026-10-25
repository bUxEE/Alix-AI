package release

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/trust"
)

// The release manifest is one JSON file that lists what a release consists of and carries detached Ed25519
// signatures over it (master plan 16.3, SEC-064, SEC-090, SEC-091):
//
//	format      alixai.release-manifest.v1
//	signed      kind, application_id, version, commit, created_at, hash_algorithm, and
//	            artifacts  the release files: binaries, archives, SBOMs, checksums, each with its SHA-256 and size
//	            images     container images by digest: the ones this release builds and the third-party ones the stack pins
//	            backends   the inference backends the stack installs, by digest, with their licence
//	            models     every model registry entry: the pinned file, its SHA-256, and its licence with the
//	                       redistribution notes
//	signatures  the same record internal/trust uses for packages, over trust.CanonicalJSON(signed)
//
// The signature covers `signed` and nothing else, so the purpose and the application are inside it (kind,
// application_id), as trust.SignPayload requires. There is no float and no null anywhere: the canonical JSON of
// internal/trust refuses both.
const (
	// Format is the format string of a release manifest.
	Format = "alixai.release-manifest.v1"
	// Kind is inside the signed part of a release manifest: the purpose the signature is for.
	Kind = "release-manifest"
	// HashAlgorithm is the only hash a manifest uses.
	HashAlgorithm = "sha256"

	// Kinds of artifact.
	KindBinary    = "binary"
	KindArchive   = "archive"
	KindSBOM      = "sbom"
	KindChecksums = "checksums"

	// Sources of an image: built by this release, or a third-party image the stack pins.
	SourceBuilt      = "built"
	SourceThirdParty = "third-party"

	// DefaultSignatureValidity is how long a release signature stays valid. Verifying a release after that needs
	// a fresh signature from the key holder (an Ed25519 signature with no expiry is refused by internal/trust).
	DefaultSignatureValidity = 366 * 24 * time.Hour

	// TimeLayout is the layout of created_at: a UTC instant to the second.
	TimeLayout = "2006-01-02T15:04:05Z"
)

// Manifest is a release manifest: the signed statement and its signatures.
type Manifest struct {
	Format     string            `json:"format"`
	Signed     Signed            `json:"signed"`
	Signatures []trust.Signature `json:"signatures"`
}

// Signed is the part of the manifest the signatures cover.
type Signed struct {
	Kind          string     `json:"kind"`
	ApplicationID string     `json:"application_id"`
	Version       string     `json:"version"`
	Commit        string     `json:"commit"`
	CreatedAt     string     `json:"created_at"`
	HashAlgorithm string     `json:"hash_algorithm"`
	Artifacts     []Artifact `json:"artifacts"`
	Images        []Image    `json:"images"`
	Backends      []Backend  `json:"backends"`
	Models        []Model    `json:"models"`
}

// Artifact is one release file.
type Artifact struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Image is a container image pinned by digest.
type Image struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	Reference string `json:"reference"` // repository and optional tag, without the digest
	Digest    string `json:"digest"`
}

// SortKey is the order images are listed in.
func (i Image) SortKey() string {
	return i.Source + "\x00" + i.Name + "\x00" + i.Reference + "\x00" + i.Digest
}

// Backend is an inference backend pinned by digest.
type Backend struct {
	Runtime   string   `json:"runtime"`
	Name      string   `json:"name"`
	Alias     string   `json:"alias"`
	Version   string   `json:"version"`
	Licence   string   `json:"licence"`
	Reference string   `json:"reference"`
	Digest    string   `json:"digest"`
	Mirrors   []string `json:"mirrors"` // full references, each pinned by the same digest
}

// SortKey is the order backends are listed in.
func (b Backend) SortKey() string { return b.Runtime + "\x00" + b.Name + "\x00" + b.Version }

// Model is a model registry entry with its pinned file and licence.
type Model struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Version  string        `json:"version"`
	Status   string        `json:"status"`
	Upstream ModelUpstream `json:"upstream"`
	Artifact ModelArtifact `json:"artifact"`
	Licence  ModelLicence  `json:"licence"`
	Backend  ModelBackend  `json:"backend"`
}

// ModelUpstream says where the model comes from.
type ModelUpstream struct {
	RepoURL  string `json:"repo_url"`
	Revision string `json:"revision"`
}

// ModelArtifact is the pinned model file.
type ModelArtifact struct {
	Format      string `json:"format"`
	RepoURL     string `json:"repo_url"`
	Filename    string `json:"filename"`
	Revision    string `json:"revision"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	QuantizedBy string `json:"quantized_by"`
	FirstParty  bool   `json:"first_party"`
}

// ModelLicence is the licence record and the redistribution notes.
type ModelLicence struct {
	SPDX                string `json:"spdx"`
	URL                 string `json:"url"`
	RedistributionNotes string `json:"redistribution_notes"`
}

// ModelBackend names the runtime a model is qualified for and the runtime versions it was qualified with.
type ModelBackend struct {
	Runtime                string   `json:"runtime"`
	Name                   string   `json:"name"`
	AllowedRuntimeVersions []string `json:"allowed_runtime_versions"`
}

var (
	// The version has no leading zeros in a number: 01.2.3 is not a semantic version.
	versionRE      = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	commitRE       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	artifactNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestRE       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// Validate checks the signed part against the rules of the format. Every problem found is reported.
func (s Signed) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if s.Kind != Kind {
		bad("kind is %q, want %q", s.Kind, Kind)
	}
	if s.ApplicationID != trust.PlatformApplicationID {
		bad("application_id is %q, want %q", s.ApplicationID, trust.PlatformApplicationID)
	}
	if !versionRE.MatchString(s.Version) {
		bad("version %q is not a semantic version", s.Version)
	}
	if !commitRE.MatchString(s.Commit) {
		bad("commit %q is not a 40-character lower-case hex commit id", s.Commit)
	}
	if t, err := time.Parse(TimeLayout, s.CreatedAt); err != nil || t.Format(TimeLayout) != s.CreatedAt {
		bad("created_at %q is not a UTC timestamp like 2026-10-03T12:00:00Z", s.CreatedAt)
	}
	if s.HashAlgorithm != HashAlgorithm {
		bad("hash_algorithm is %q, want %q", s.HashAlgorithm, HashAlgorithm)
	}

	// A list is a list: null is not an empty one, so a signed null and a signed [] are never two spellings of
	// one statement.
	if s.Images == nil {
		bad("images is null: want a list, possibly empty")
	}
	if s.Backends == nil {
		bad("backends is null: want a list, possibly empty")
	}
	if len(s.Artifacts) == 0 {
		bad("artifacts is empty: a release lists at least one file")
	}
	prev := ""
	for i, a := range s.Artifacts {
		if !artifactNameRE.MatchString(a.Name) {
			bad("artifacts[%d]: name %q is not a plain file name", i, a.Name)
		}
		if i > 0 && a.Name <= prev {
			bad("artifacts is not sorted strictly ascending by name at %q", a.Name)
		}
		prev = a.Name
		switch a.Kind {
		case KindBinary:
			if blank(a.OS) || blank(a.Arch) {
				bad("artifacts[%d] (%s): a binary names its os and arch", i, a.Name)
			}
		case KindArchive, KindSBOM, KindChecksums:
			if a.OS != "" || a.Arch != "" {
				bad("artifacts[%d] (%s): only a binary carries os and arch", i, a.Name)
			}
		default:
			bad("artifacts[%d] (%s): kind %q is unknown", i, a.Name, a.Kind)
		}
		if !sha256HexRE.MatchString(a.SHA256) {
			bad("artifacts[%d] (%s): sha256 is not 64 lower-case hex characters", i, a.Name)
		}
		if a.Size < 0 {
			bad("artifacts[%d] (%s): negative size", i, a.Name)
		}
	}

	prev = ""
	for i, im := range s.Images {
		if im.Source != SourceBuilt && im.Source != SourceThirdParty {
			bad("images[%d]: source %q is unknown", i, im.Source)
		}
		if blank(im.Name) || blank(im.Reference) {
			bad("images[%d]: name and reference are required", i)
		}
		if !digestRE.MatchString(im.Digest) {
			bad("images[%d] (%s): digest is not sha256:<64 lower-case hex>", i, im.Name)
		}
		if k := im.SortKey(); i > 0 && k <= prev {
			bad("images is not sorted strictly ascending at %q", im.Name)
		} else {
			prev = k
		}
	}

	prev = ""
	for i, b := range s.Backends {
		if blank(b.Runtime) || blank(b.Name) || blank(b.Version) || blank(b.Licence) || blank(b.Reference) {
			bad("backends[%d] (%s): runtime, name, version, licence and reference are required", i, b.Name)
		}
		if !digestRE.MatchString(b.Digest) {
			bad("backends[%d] (%s): digest is not sha256:<64 lower-case hex>", i, b.Name)
		}
		if b.Mirrors == nil {
			bad("backends[%d] (%s): mirrors is null: want a list, possibly empty", i, b.Name)
		}
		for _, m := range b.Mirrors {
			if r, err := ParseImageRef(m); err != nil || r.Digest != b.Digest {
				bad("backends[%d] (%s): mirror %q is not pinned by the backend's digest", i, b.Name, m)
			}
		}
		if k := b.SortKey(); i > 0 && k <= prev {
			bad("backends is not sorted strictly ascending at %q", b.Name)
		} else {
			prev = k
		}
	}

	if len(s.Models) == 0 {
		bad("models is empty: a release lists the registry's models and their licences")
	}
	prev = ""
	for i, m := range s.Models {
		if blank(m.ID) || blank(m.Name) || blank(m.Version) || blank(m.Status) {
			bad("models[%d] (%s): id, name, version and status are required", i, m.ID)
		}
		if i > 0 && m.ID <= prev {
			bad("models is not sorted strictly ascending by id at %q", m.ID)
		}
		prev = m.ID
		if !sha256HexRE.MatchString(m.Artifact.SHA256) || m.Artifact.SizeBytes <= 0 || blank(m.Artifact.Filename) || blank(m.Artifact.RepoURL) || blank(m.Artifact.Revision) {
			bad("models[%d] (%s): the artifact is not fully pinned (repo_url, filename, revision, size_bytes, sha256)", i, m.ID)
		}
		if blank(m.Licence.SPDX) || blank(m.Licence.URL) || blank(m.Licence.RedistributionNotes) {
			bad("models[%d] (%s): the licence record is incomplete (spdx, url and redistribution_notes are required)", i, m.ID)
		}
		if blank(m.Backend.Runtime) || blank(m.Backend.Name) {
			bad("models[%d] (%s): the backend is not named", i, m.ID)
		}
		if m.Backend.AllowedRuntimeVersions == nil {
			bad("models[%d] (%s): allowed_runtime_versions is null: want a list, possibly empty", i, m.ID)
		}
	}
	return errors.Join(errs...)
}

// ImageRef is a container image reference pinned by digest: repository, optional tag, digest.
type ImageRef struct {
	Repo   string
	Tag    string
	Digest string // sha256:<hex>
}

var imageRefRE = regexp.MustCompile(`^([^@\s]+)@(sha256:[0-9a-f]{64})$`)

// ParseImageRef parses repo[:tag]@sha256:digest. A reference with no digest is refused: an image that is not
// pinned by content cannot be recorded in a release (SEC-064).
func ParseImageRef(ref string) (ImageRef, error) {
	m := imageRefRE.FindStringSubmatch(ref)
	if m == nil {
		return ImageRef{}, fmt.Errorf("image reference %q is not pinned by a sha256 digest (want repo[:tag]@sha256:<64 hex>)", ref)
	}
	repo, tag := m[1], ""
	if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
		repo, tag = repo[:i], repo[i+1:]
	}
	if repo == "" {
		return ImageRef{}, fmt.Errorf("image reference %q has no repository", ref)
	}
	return ImageRef{Repo: repo, Tag: tag, Digest: m[2]}, nil
}

func (r ImageRef) String() string {
	if r.Tag != "" {
		return r.Repo + ":" + r.Tag + "@" + r.Digest
	}
	return r.Repo + "@" + r.Digest
}
