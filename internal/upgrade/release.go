package upgrade

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The release manifest is written and signed by tools/release (task M5-03,
// docs/operations/release.md section 1) and verified by internal/release: the
// same package the tool signs and verifies with, so there is one decision about
// whether a manifest is genuine. This file only reduces a verified manifest to
// what an upgrade needs.
//
// Verification fails closed (see internal/release): after the signature (a key
// registered for the platform application, over the exact bytes of the signed
// part) every pin the upgrade relies on must be present and well-formed, so a
// release that lists an image, backend or model without a digest is refused even
// though it is signed.
const (
	// ReleaseFormat is the format string of a release manifest.
	ReleaseFormat = release.Format
	// ReleaseKind is inside the signed part of a release manifest.
	ReleaseKind = release.Kind
)

// Release is a verified release manifest, reduced to what an upgrade needs.
type Release struct {
	Version   string
	Commit    string
	CreatedAt time.Time
	Artifacts []ArtifactPin
	Backends  []BackendPin
	Models    []ModelPin
	// Accepted is the signature that authenticated the manifest.
	Accepted trust.Accepted

	// manifest is the verified manifest the pins above were read from; the
	// artifact files are checked against it.
	manifest *release.Manifest
}

// ArtifactPin is a release file with its hash.
type ArtifactPin struct {
	Name, Kind, OS, Arch, SHA256 string
	Size                         int64
}

// BackendPin is an inference backend the release was qualified with.
type BackendPin struct {
	Runtime, Name, Version, Digest string
}

// ModelPin is a registry model and the runtime versions qualified for it.
type ModelPin struct {
	ID, Runtime, Backend   string
	AllowedRuntimeVersions []string
}

// ReleaseVerifier verifies release manifests with internal/release.
type ReleaseVerifier struct {
	// Keys are the keys trusted for the platform application: a trust.Store
	// (which refuses the published TEST-ONLY keys unless opened with
	// AllowTestOnlyKeys) or release.StaticKeys (same rule, AllowTestOnly).
	// release.Verify narrows them to release keys (trust.PurposeRelease) and
	// adds the built-in release key (release.PinnedKeyID).
	Keys trust.KeyLookup
	// Clock is read for signature expiry. Nil means the wall clock.
	Clock clock.Clock
}

// VerifyRelease verifies raw (release.Verify) and returns the release.
func (v ReleaseVerifier) VerifyRelease(ctx context.Context, raw []byte) (*Release, error) {
	m, acc, err := release.Verify(ctx, raw, v.Keys, v.Clock)
	if err != nil {
		return nil, err
	}
	return reduceRelease(m, acc)
}

// reduceRelease reduces a verified manifest. The manifest was parsed and
// validated by release.Verify, so every pin is already known to be present.
func reduceRelease(m *release.Manifest, acc trust.Accepted) (*Release, error) {
	s := m.Signed
	created, err := time.Parse(release.TimeLayout, s.CreatedAt)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "release manifest: created_at is not a UTC timestamp")
	}
	rel := &Release{Version: s.Version, Commit: s.Commit, CreatedAt: created, Accepted: acc, manifest: m}
	for _, a := range s.Artifacts {
		rel.Artifacts = append(rel.Artifacts, ArtifactPin{Name: a.Name, Kind: a.Kind, OS: a.OS, Arch: a.Arch, SHA256: a.SHA256, Size: a.Size})
	}
	for _, b := range s.Backends {
		rel.Backends = append(rel.Backends, BackendPin{Runtime: b.Runtime, Name: b.Name, Version: b.Version, Digest: b.Digest})
	}
	for _, md := range s.Models {
		rel.Models = append(rel.Models, ModelPin{ID: md.ID, Runtime: md.Backend.Runtime, Backend: md.Backend.Name,
			AllowedRuntimeVersions: append([]string{}, md.Backend.AllowedRuntimeVersions...)})
	}
	return rel, nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// MatchesBinary requires the release to be the one of the binary that applies
// it (security finding L2): its version is the binary's (version, as
// internal/version reports it), or the binary's own file (executable) is one of
// the release's binary artifacts, by SHA-256 and size. Without it a binary
// could apply a newer release's manifest, record that version, and then refuse
// the release in between as a downgrade, while the migrations that ran were its
// own. The error is contracts.ErrConflict.
func (r *Release) MatchesBinary(version, executable string) error {
	if sameVersion(r.Version, version) {
		return nil
	}
	if executable != "" {
		if sum, size, err := release.HashFile(executable); err == nil {
			for _, a := range r.Artifacts {
				if a.Kind == release.KindBinary && a.SHA256 == sum && a.Size == size {
					return nil
				}
			}
		}
	}
	return contracts.Newf(contracts.ErrConflict,
		"release %s is not the release of this binary (version %s, and its file is not one of the release's binaries): run the upgrade with the binary of the release it applies", r.Version, version)
}

// VerifyArtifacts re-hashes the files in dir against the release: every listed
// file must be there with the listed size and SHA-256 and no other regular file
// may be (an unlisted file is unsigned content).
func (r *Release) VerifyArtifacts(dir string) error {
	if r.manifest == nil {
		return contracts.New(contracts.ErrInternal, "release was not produced by VerifyRelease: its files cannot be checked")
	}
	return r.manifest.VerifyFiles(dir, "")
}

// RuntimeSet is the inference runtime an installation uses: the runtime (for
// example "localai") with its version and the versions or digests of the
// backends inside it. The upgrade never imports a runtime's code; a Runtime
// implementation reports and switches this set.
type RuntimeSet struct {
	Runtime  string            `json:"runtime"`
	Version  string            `json:"version"`
	Backends map[string]string `json:"backends,omitempty"` // backend name -> version or image digest
}

// Qualify reports whether the release was qualified for rs (master plan 16.4
// b15, SEC-072): the runtime version must be in backend.allowed_runtime_versions
// of every model of that runtime the installation uses (modelIDs, or all of the
// release's models of the runtime when none are named), and every backend must
// be one the release pins, by version or digest. Case and a leading "v" are
// cosmetic in versions, as in the adapter. The error is contracts.ErrUnavailable:
// an unqualified combination does not serve.
func (r *Release) Qualify(rs RuntimeSet, modelIDs ...string) error {
	unq := func(format string, a ...any) error {
		return contracts.Newf(contracts.ErrUnavailable, "runtime is not qualified by release "+r.Version+": "+format, a...)
	}
	if blank(rs.Runtime) || blank(rs.Version) {
		return contracts.New(contracts.ErrValidationFailed, "runtime set names no runtime or version")
	}
	var models []ModelPin
	if len(modelIDs) == 0 {
		for _, m := range r.Models {
			if m.Runtime == rs.Runtime {
				models = append(models, m)
			}
		}
	} else {
		for _, id := range modelIDs {
			found := false
			for _, m := range r.Models {
				if m.ID == id {
					models, found = append(models, m), true
				}
			}
			if !found {
				return unq("model %q is not in the release", id)
			}
		}
	}
	if len(models) == 0 {
		return unq("the release lists no model for runtime %q", rs.Runtime)
	}
	for _, m := range models {
		if m.Runtime != rs.Runtime {
			return unq("model %s is for runtime %q, not %q", m.ID, m.Runtime, rs.Runtime)
		}
		ok := false
		for _, v := range m.AllowedRuntimeVersions {
			if sameVersion(v, rs.Version) {
				ok = true
			}
		}
		if !ok {
			return unq("%s version %q is not among the versions qualified for model %s (%s)", rs.Runtime, rs.Version, m.ID, strings.Join(m.AllowedRuntimeVersions, ", "))
		}
	}
	for name, v := range rs.Backends {
		ok := false
		for _, b := range r.Backends {
			if b.Runtime == rs.Runtime && b.Name == name && (sameVersion(b.Version, v) || b.Digest == v) {
				ok = true
			}
		}
		if !ok {
			return unq("backend %s %q is not one the release pins", name, v)
		}
	}
	return nil
}

func sameVersion(a, b string) bool { return normVersion(a) == normVersion(b) && normVersion(a) != "" }

func normVersion(v string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "v")
}

// compareVersions orders two semantic versions by precedence: -1, 0 or 1.
// Build metadata is ignored; a pre-release sorts before its release.
func compareVersions(a, b string) int {
	pa, pb := splitVersion(a), splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa.core[i] != pb.core[i] {
			if pa.core[i] < pb.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	}
	return comparePre(pa.pre, pb.pre)
}

type parsedVersion struct {
	core [3]int
	pre  string
}

func splitVersion(v string) parsedVersion {
	v, _, _ = strings.Cut(v, "+")
	core, pre, _ := strings.Cut(v, "-")
	var p parsedVersion
	for i, s := range strings.SplitN(core, ".", 3) {
		n, _ := strconv.Atoi(s)
		p.core[i] = n
	}
	p.pre = pre
	return p
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		case x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}
