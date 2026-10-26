package apppkg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// DevCoreVersion is the AlixAI core version assumed when the binary is not
// stamped with a release version (a development build). It is the current
// release line as a SemVer prerelease so it sorts inside [1.0.0, 2.0.0) like a
// stamped 1.1.0 binary, and below the stamped release.
const DevCoreVersion = version.ReleaseLine + "-dev"

// Authenticity says how far the signature of a loaded package was checked.
type Authenticity string

const (
	// AuthVerified: the signature manifest verified against a trust anchor and
	// the content matches it exactly.
	AuthVerified Authenticity = "verified"
	// AuthIntegrityOnly: no trust anchor was supplied, so the content was
	// compared to the digests the package lists, but nobody vouched for the
	// list. It is never enough to install a package.
	AuthIntegrityOnly Authenticity = "integrity_only"
	// AuthFailed: the package is unsigned, tampered or not signed by an
	// acceptable key.
	AuthFailed Authenticity = "failed"
)

// Options configure Load.
type Options struct {
	// Verifier checks the package signature against a trust store. When nil the
	// loader still checks that the content matches the digests the package
	// lists, reports the missing trust anchor as a warning and marks the
	// result AuthIntegrityOnly; an Installer refuses such a result.
	Verifier *trust.Verifier
	// CoreVersion is the installed AlixAI core version, a semantic version.
	// The package's compatibility range is checked against it. Required: a
	// loader that does not know the core cannot say a package is compatible.
	CoreVersion string
	// Limits bound the package. Zero fields take DefaultLimits.
	Limits Limits
}

// Result is the outcome of Load: findings, and a typed Package only when
// nothing critical was found.
type Result struct {
	// Findings lists everything the checks found, most severe first, in a
	// deterministic order. It is never nil.
	Findings []contracts.Finding
	// Package is the typed, validated package. It is nil whenever any finding
	// is critical: a package with a critical finding is never handed to code
	// that builds capabilities from it.
	Package *Package
	// Authenticity says how far the signature was checked.
	Authenticity Authenticity
	// Signer is the signature that authenticated the package, when verified.
	Signer *trust.Accepted
	// ManifestHash is the lower-case hex SHA-256 of the canonical JSON of the
	// signed integrity manifest. It identifies exactly this content (every
	// file, by digest, plus the application and package version) and is the
	// content address under which the package is installed. It is empty when
	// the content did not match its manifest.
	ManifestHash string
	// ApplicationID and PackageVersion are what alixai.yaml declares, when it
	// could be read. They are not trusted unless Authenticity is AuthVerified.
	ApplicationID  string
	PackageVersion string
	// Files and Bytes describe the snapshot.
	Files int
	Bytes int64

	snap *Snapshot
}

// OK reports whether the package loaded with no critical finding.
func (r *Result) OK() bool { return r != nil && r.Package != nil }

// Critical reports whether any finding is critical.
func (r *Result) Critical() bool { return r == nil || contracts.HasCritical(r.Findings) }

// Load reads a package from src, checks it and returns the findings.
//
// Load is deterministic: no model, no network, no clock except the injected
// verifier's, no file outside src. The same bytes always give the same
// findings in the same order. An error is returned only when the checks could
// not run at all (a source that cannot be read, a cancelled context, a
// misconfiguration); every defect of the package itself is a finding.
//
// Order of work:
//
//  1. read the source into an immutable Snapshot (links, hidden or unsafe
//     names, duplicates and limits are refused here);
//  2. parse and validate alixai.yaml, refuse the reserved platform
//     application ID;
//  3. verify the signature and the content from the Snapshot; the very same
//     bytes are parsed below;
//  4. parse and schema-validate every other file as plain YAML;
//  5. check that the files agree with each other (section 14);
//  6. compile the policies with internal/policy, the code the runtime uses,
//     so loader and runtime cannot disagree about the references.
func Load(ctx context.Context, src Source, opts Options) (*Result, error) {
	if src == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "no package source")
	}
	lim := opts.Limits.withDefaults()
	snap, probs, err := src.snapshot(ctx, lim)
	if err != nil {
		return nil, err
	}
	c := newCollector(lim.MaxFindings)
	if snap == nil {
		for _, p := range probs {
			c.add(p.check, "", p.path, "%s", p.msg)
		}
		return &Result{Findings: c.finalize(), Authenticity: AuthFailed}, nil
	}
	return loadSnapshot(ctx, snap, opts, lim, c)
}

// LoadSnapshot is Load for a Snapshot that is already in memory.
func loadSnapshot(ctx context.Context, snap *Snapshot, opts Options, lim Limits, c *collector) (*Result, error) {
	res := &Result{Authenticity: AuthFailed, Files: snap.Len(), Bytes: snap.Size(), snap: snap}
	finish := func() (*Result, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res.Findings = c.finalize()
		if res.Critical() {
			res.Package = nil
		}
		return res, nil
	}
	md := &model{snap: snap, c: c, lim: lim, opts: opts}

	// 2. the manifest
	mb, ok := snap.bytes(trust.ManifestFile)
	if !ok {
		c.add(chkManifestMissing, "", trust.ManifestFile, "the package has no %s at its root", trust.ManifestFile)
		return finish()
	}
	if !md.parseFile(trust.ManifestFile, mb, kindManifest, &md.m) {
		if md.fatal != nil {
			return nil, md.fatal
		}
		return finish()
	}
	res.ApplicationID = string(md.m.Application.ID)
	res.PackageVersion = string(md.m.Package.Version)
	if string(md.m.Application.ID) == trust.PlatformApplicationID {
		c.add(chkReservedApp, "", trust.ManifestFile,
			"application.id %s is reserved for AlixAI's own signing keys; a host application can never use it", q(trust.PlatformApplicationID))
		return finish()
	}

	// 3. signature, then content
	md.authenticate(ctx, res)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 4 and 5. every other file, then the cross-checks
	md.compat()
	md.loadFiles()
	if md.fatal != nil {
		return nil, md.fatal
	}
	md.crossCheck()

	// 6. policies, compiled by the runtime's own code
	pkg := md.buildPackage()
	if !c.critical() && pkg != nil {
		pkg.Verification = res.Signer
		pkg.Authenticity = res.Authenticity
		pkg.ManifestHash = res.ManifestHash
		res.Package = pkg
	}
	return finish()
}

// authenticate verifies the signature manifest and that the snapshot matches
// it, and fills the authenticity fields of res.
func (md *model) authenticate(ctx context.Context, res *Result) {
	opts := trust.PackageOptions{
		ApplicationID:   string(md.m.Application.ID),
		PackageVersion:  string(md.m.Package.Version),
		PublisherKeyIDs: identifiers(md.m.Integrity.PublisherKeyIDs),
	}
	if md.opts.Verifier != nil {
		pv, err := md.opts.Verifier.VerifyPackage(ctx, md.snap, opts)
		if err != nil {
			md.reportSignatureError(err)
			return
		}
		acc := pv.Accepted
		res.Signer = &acc
		res.Authenticity = AuthVerified
		res.ManifestHash = manifestHash(pv.Manifest.Signed)
		return
	}

	sm, err := trust.ReadSignatureManifest(md.snap)
	if err != nil {
		md.reportSignatureError(err)
		return
	}
	if sm.Signed.ApplicationID != opts.ApplicationID || sm.Signed.PackageVersion != opts.PackageVersion {
		md.c.add(chkSigBinding, "", trust.SignatureManifestPath,
			"the signature is for application %s version %s, the manifest declares %s version %s",
			q(sm.Signed.ApplicationID), q(sm.Signed.PackageVersion), q(opts.ApplicationID), q(opts.PackageVersion))
		return
	}
	built, err := trust.BuildSigned(md.snap, trust.PackageIdentity{ApplicationID: opts.ApplicationID, PackageVersion: opts.PackageVersion}, time.Unix(0, 0))
	if err != nil {
		md.reportSignatureError(fmt.Errorf("%w: %w", trust.ErrUnsafePackage, err))
		return
	}
	if diffs := diffFileLists(sm.Signed.Files, built.Files); len(diffs) > 0 {
		md.reportTamper(diffs, len(diffs))
		return
	}
	res.Authenticity = AuthIntegrityOnly
	res.ManifestHash = manifestHash(sm.Signed)
	md.c.add(chkSigNotChecked, "", trust.SignatureManifestPath,
		"the content matches the digests the package lists, but no trust anchor was supplied, so who signed it is not established")
}

func manifestHash(s trust.Signed) string {
	canon, err := s.Canonical()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

func identifiers[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// diffFileLists compares the signed list with the actual list.
func diffFileLists(signed, actual []trust.FileEntry) []trust.Difference {
	want := make(map[string]trust.FileEntry, len(signed))
	for _, f := range signed {
		want[f.Path] = f
	}
	var diffs []trust.Difference
	got := make(map[string]bool, len(actual))
	for _, f := range actual {
		got[f.Path] = true
		w, ok := want[f.Path]
		switch {
		case !ok:
			diffs = append(diffs, trust.Difference{Path: f.Path, Kind: trust.DiffAdded})
		case w.SHA256 != f.SHA256 || w.Size != f.Size:
			diffs = append(diffs, trust.Difference{Path: f.Path, Kind: trust.DiffModified})
		}
	}
	for _, f := range signed {
		if !got[f.Path] {
			diffs = append(diffs, trust.Difference{Path: f.Path, Kind: trust.DiffMissing})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs
}

// reportSignatureError turns an error of internal/trust into one finding.
func (md *model) reportSignatureError(err error) {
	var te *trust.TamperError
	var ve *trust.VerifyError
	switch {
	case errors.As(err, &te):
		md.reportTamper(te.Differences, te.Total)
	case errors.Is(err, trust.ErrUnsigned):
		md.c.add(chkSigMissing, "", trust.SignatureManifestPath, "the package has no %s: it is unsigned", trust.SignatureManifestPath)
	case errors.Is(err, trust.ErrMalformedManifest):
		md.c.add(chkSigMalformed, "", trust.SignatureManifestPath, "the signature manifest is malformed or does not match its schema")
	case errors.Is(err, trust.ErrBindingMismatch):
		md.c.add(chkSigBinding, "", trust.SignatureManifestPath, "the signature is bound to another application or package version than the manifest declares")
	case errors.As(err, &ve):
		reasons := make([]string, 0, len(ve.Failures))
		for _, f := range ve.Failures {
			reasons = append(reasons, fmt.Sprintf("signature %d (key %s): %s", f.Index, q(f.KeyID), f.Reason))
		}
		md.c.add(chkSigInvalid, "", trust.SignatureManifestPath, "no acceptable signature: %s", strings.Join(reasons, "; "))
	case errors.Is(err, trust.ErrUnsafePackage):
		md.c.add(chkSigTampered, "", "", "the package cannot be listed against its signed manifest: it holds an unsafe entry")
	case contracts.HasCode(err, contracts.ErrUnavailable), contracts.HasCode(err, contracts.ErrInternal):
		md.c.add(chkSigUnverifiable, "", trust.SignatureManifestPath, "the signature could not be established: the trust store or the package could not be read")
	default:
		md.c.add(chkSigInvalid, "", trust.SignatureManifestPath, "the signature could not be verified")
	}
}

func (md *model) reportTamper(diffs []trust.Difference, total int) {
	parts := make([]string, 0, 5)
	for i, d := range diffs {
		if i == 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s", d.Kind, q(d.Path)))
	}
	more := ""
	if total > len(parts) {
		more = fmt.Sprintf(" and %d more", total-len(parts))
	}
	path := ""
	if len(diffs) > 0 {
		path = diffs[0].Path
	}
	md.c.add(chkSigTampered, "", path, "the package content differs from what was signed (%d difference(s)): %s%s", total, strings.Join(parts, ", "), more)
}

// compat checks the compatibility range against the installed core.
func (md *model) compat() {
	rng := md.m.Compatibility.AlixAICore
	lo, okLo := parseSemver(string(rng.Min))
	hi, okHi := parseSemver(string(rng.MaxExclusive))
	if !okLo || !okHi {
		md.c.add(chkCompatRange, "", trust.ManifestFile, "compatibility.alixai_core bounds are not semantic versions")
		return
	}
	if compareSemver(lo, hi) >= 0 {
		md.c.add(chkCompatRange, "", trust.ManifestFile,
			"compatibility.alixai_core.min %s is not lower than max_exclusive %s", q(string(rng.Min)), q(string(rng.MaxExclusive)))
		return
	}
	core, ok := parseSemver(md.opts.CoreVersion)
	if !ok {
		md.c.add(chkCoreVersion, "", "", "the installed core version %s is not a semantic version", q(md.opts.CoreVersion))
		return
	}
	if compareSemver(core, lo) < 0 || compareSemver(core, hi) >= 0 {
		md.c.add(chkCompatCore, "", trust.ManifestFile,
			"core version %s is outside the package's compatibility range [%s, %s)", q(md.opts.CoreVersion), q(string(rng.Min)), q(string(rng.MaxExclusive)))
	}
}

// parseFile parses one YAML file, validates it against its schema and decodes
// it strictly into v. It reports findings itself and returns whether v is
// usable.
func (md *model) parseFile(path string, data []byte, k schemaKind, v any) bool {
	js, ok := md.toJSON(path, data, k)
	if !ok {
		return false
	}
	if err := decodeStrict(js, v); err != nil {
		md.c.add(k.check, "", path, "the file validates against the schema but cannot be decoded into the typed package: %s", clipMsg(err.Error()))
		return false
	}
	return true
}

// toJSON parses and schema-validates one file and returns its JSON form.
func (md *model) toJSON(path string, data []byte, k schemaKind) ([]byte, bool) {
	tree, err := parseYAML(data, md.lim)
	if err != nil {
		md.c.add(chkYAML, "", path, "%s", err.Error())
		return nil, false
	}
	js, err := toJSON(tree)
	if err != nil {
		md.c.add(chkYAML, "", path, "the file cannot be represented as JSON: %s", clipMsg(err.Error()))
		return nil, false
	}
	viol, err := validateSchema(k, js)
	if err != nil {
		md.fatal = err
		return nil, false
	}
	if len(viol) > 0 {
		md.c.add(k.check, "", path, "does not match the %s schema: %s", k.name, strings.Join(viol, "; "))
		return nil, false
	}
	return js, true
}

// decodeStrict decodes one JSON value into v, rejecting unknown members and
// trailing data.
func decodeStrict(js []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func clipMsg(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// buildPackage builds the typed package and compiles its policies with the
// runtime's own code. It returns nil when anything needed for that is missing;
// the findings already say why.
func (md *model) buildPackage() *Package {
	if md.c.critical() {
		return nil
	}
	roles := make([]string, 0, len(md.m.Tenancy.Roles))
	for r := range md.m.Tenancy.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)

	ids := md.capIDs()
	decls := make([]policy.CapabilityDecl, 0, len(ids))
	for _, id := range ids {
		d, err := capabilityDecl(md.caps[id].c)
		if err != nil {
			md.c.add(chkPolicyInvalid, id, md.caps[id].path, "the capability cannot be handed to the policy gateway: %s", clipMsg(err.Error()))
			return nil
		}
		decls = append(decls, d)
	}
	var hosts []*policy.HostPolicy
	for _, p := range md.policies {
		hosts = append(hosts, p.p)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: string(md.m.Application.ID),
		Version:       string(md.m.Package.Version),
		Status:        policy.StatusQuarantined,
		Roles:         roles,
		Capabilities:  decls,
		Policies:      hosts,
	})
	if err != nil {
		md.c.add(chkPolicyInvalid, "", "policies", "the policy gateway refuses the package: %s", clipMsg(err.Error()))
		return nil
	}

	pkg := &Package{
		gate:         pp,
		Manifest:     md.m,
		Capabilities: map[string]ap.Capability{},
		Files:        md.snap.Paths(),
		Sources:      map[string]ap.RAGSource{},
		Terms:        map[string]ap.TerminologyTerm{},
		Contracts:    map[ContractKey]ap.ConversationContract{},
	}
	for id, e := range md.caps {
		pkg.Capabilities[id] = e.c
	}
	for k, e := range md.contracts {
		pkg.Contracts[ContractKey{Capability: k.cap, Language: k.lang}] = e.c
	}
	for id, e := range md.sources {
		pkg.Sources[id] = e.s
	}
	for id, e := range md.terms {
		pkg.Terms[id] = e.t
	}
	for _, p := range md.policies {
		pkg.Policies = append(pkg.Policies, p.p)
	}
	if md.docs != nil {
		pkg.Documentation = *md.docs
	}
	for _, s := range md.suites {
		pkg.Tests = append(pkg.Tests, s.t)
	}
	return pkg
}

// capabilityDecl converts a typed capability to the part the policy gateway
// decides on. The risk level goes through contracts.ParseRiskLevel, never a
// cast, so an out-of-range value is an error rather than a wrapped integer.
func capabilityDecl(c ap.Capability) (policy.CapabilityDecl, error) {
	risk, err := contracts.ParseRiskLevel(int(c.RiskLevel))
	if err != nil {
		return policy.CapabilityDecl{}, err
	}
	d := policy.CapabilityDecl{
		ID:           string(c.ID),
		Audience:     policy.Audience(c.Audience),
		EffectClass:  policy.EffectClass(c.EffectClass),
		RiskLevel:    risk,
		Confirmation: policy.ConfirmationMode(c.Confirmation.Mode),
	}
	for _, f := range c.Fields {
		d.Fields = append(d.Fields, policy.FieldDecl{
			Name:      string(f.Name),
			Required:  f.Required,
			Sensitive: f.Sensitive != nil && *f.Sensitive,
		})
	}
	return d, nil
}
