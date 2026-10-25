package files

import (
	"context"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Aliases of the generated FileRef vocabulary, so a caller names one type.
type (
	// Treatment is how a file is treated (schemas/conversation/file-ref).
	Treatment = conversation.FileRefTreatment
	// Permission is what a principal may do with a file.
	Permission = conversation.FileRefPermissionsItem
	// Origin says where a file came from.
	Origin = conversation.FileRefOrigin
)

// The treatments of master plan 14.1: "ephemeral turn context, an explicitly
// registered RAG source, or an application object".
const (
	TreatmentEphemeral = conversation.FileRefTreatmentEphemeralContext
	TreatmentRAGSource = conversation.FileRefTreatmentRAGSource
	TreatmentObject    = conversation.FileRefTreatmentApplicationObject
)

// The permissions a file can carry.
const (
	PermRead     = conversation.FileRefPermissionsItemRead
	PermPreview  = conversation.FileRefPermissionsItemPreview
	PermDownload = conversation.FileRefPermissionsItemDownload
)

// The origins of a file.
const (
	OriginUpload      = conversation.FileRefOriginUpload
	OriginGenerated   = conversation.FileRefOriginGenerated
	OriginApplication = conversation.FileRefOriginApplication
)

// Visibility is who inside the file's workspace can resolve it.
type Visibility string

const (
	// VisibilityOwner: only the principal that stored the file.
	VisibilityOwner Visibility = "owner"
	// VisibilityWorkspace: every principal of the workspace (never of another
	// workspace, tenant or application). Only the owner can delete it.
	VisibilityWorkspace Visibility = "workspace"
)

func (v Visibility) valid() bool { return v == VisibilityOwner || v == VisibilityWorkspace }

// Rule is the host's policy for one treatment, resolved: every field has its
// final value.
type Rule struct {
	// AllowedMIME lists exact media types and the families image/* and text/*.
	AllowedMIME []string
	// MaxBytes caps one file.
	MaxBytes int64
	// TTL is how long a reference resolves.
	TTL time.Duration
	// Permissions are what the principal may do with the file.
	Permissions []Permission
	// Visibility is who may resolve it.
	Visibility Visibility
}

// Policy is the host application's declared attachment policy: which
// treatments it accepts and under what limits. A treatment with no rule is
// refused (fail closed); nothing here is chosen by the client or the model.
type Policy struct {
	// Default is the treatment of an upload that names none. Empty means the
	// upload must name one.
	Default Treatment
	// Rules holds the rule of each accepted treatment.
	Rules map[Treatment]Rule
}

// PolicySource tells the service the attachment policy a host declared for the
// application of a request. The composition root supplies one built from the
// application package or the settings; ConfigPolicy is the one built from the
// "files" section.
type PolicySource interface {
	AttachmentPolicy(ctx context.Context, scope contracts.Scope) (Policy, error)
}

// PolicyFunc adapts a function to a PolicySource.
type PolicyFunc func(ctx context.Context, scope contracts.Scope) (Policy, error)

// AttachmentPolicy implements PolicySource.
func (f PolicyFunc) AttachmentPolicy(ctx context.Context, scope contracts.Scope) (Policy, error) {
	return f(ctx, scope)
}

// ConfigPolicy is the PolicySource of the "files" configuration section: the
// same declared policy for every application. cfg is read on every call, so a
// reloaded section takes effect.
func ConfigPolicy(cfg func() Config) PolicySource {
	return PolicyFunc(func(context.Context, contracts.Scope) (Policy, error) {
		return policyOf(cfg()), nil
	})
}

// policyOf turns a configuration into a Policy. Disabled rules are absent.
func policyOf(c Config) Policy {
	p := Policy{Rules: map[Treatment]Rule{}}
	for _, e := range []struct {
		t Treatment
		r RuleConfig
	}{{TreatmentEphemeral, c.Ephemeral}, {TreatmentRAGSource, c.RAGSource}, {TreatmentObject, c.Object}} {
		if !e.r.Enabled {
			continue
		}
		perms := make([]Permission, 0, len(e.r.Permissions))
		for _, s := range e.r.Permissions {
			perms = append(perms, Permission(s))
		}
		p.Rules[e.t] = Rule{
			AllowedMIME: append([]string(nil), e.r.AllowedMIME...),
			MaxBytes:    e.r.MaxBytes,
			TTL:         e.r.TTL,
			Permissions: perms,
			Visibility:  Visibility(e.r.Visibility),
		}
	}
	// The default is the first treatment the host enabled, in the order of
	// narrowest to widest: ephemeral context needs the least trust.
	for _, t := range []Treatment{TreatmentEphemeral, TreatmentObject, TreatmentRAGSource} {
		if _, ok := p.Rules[t]; ok {
			p.Default = t
			break
		}
	}
	return p
}

// resolve fills the rule's unset fields from the treatment's defaults and
// clamps it to the deployment's limits. A rule that cannot accept anything, or
// carries a value outside the vocabulary, is refused: fail closed.
func (r Rule) resolve(t Treatment, cfg Config) (Rule, error) {
	out := r
	out.AllowedMIME = append([]string(nil), r.AllowedMIME...)
	if len(out.AllowedMIME) == 0 {
		return Rule{}, contracts.Newf(contracts.ErrPermissionDenied, "the attachment policy for %s allows no file type", t)
	}
	for _, m := range out.AllowedMIME {
		if !knownPattern(m) {
			return Rule{}, contracts.Newf(contracts.ErrInternal, "the attachment policy for %s names a type the core cannot verify", t)
		}
	}
	if out.MaxBytes <= 0 || out.MaxBytes > cfg.MaxFileBytes {
		out.MaxBytes = cfg.MaxFileBytes
	}
	if out.TTL <= 0 {
		out.TTL = defaultTTL(t)
	}
	if out.TTL > cfg.MaxTTL {
		out.TTL = cfg.MaxTTL
	}
	if len(out.Permissions) == 0 {
		out.Permissions = defaultPermissions(t)
	}
	out.Permissions = normalizePermissions(out.Permissions)
	if len(out.Permissions) == 0 {
		return Rule{}, contracts.Newf(contracts.ErrInternal, "the attachment policy for %s names a permission that does not exist", t)
	}
	if out.Visibility == "" {
		out.Visibility = VisibilityOwner
	}
	if !out.Visibility.valid() {
		return Rule{}, contracts.Newf(contracts.ErrInternal, "the attachment policy for %s names a visibility that does not exist", t)
	}
	return out, nil
}

func defaultTTL(t Treatment) time.Duration {
	switch t {
	case TreatmentRAGSource:
		return DefaultRAGSourceTTL
	case TreatmentObject:
		return DefaultObjectTTL
	}
	return DefaultEphemeralTTL
}

func defaultPermissions(t Treatment) []Permission {
	switch t {
	case TreatmentRAGSource:
		return []Permission{PermRead}
	case TreatmentObject:
		return []Permission{PermRead, PermPreview, PermDownload}
	}
	return []Permission{PermRead, PermPreview}
}

// normalizePermissions drops duplicates and returns nil when any entry is not a
// permission, in the order read, preview, download.
func normalizePermissions(in []Permission) []Permission {
	have := map[Permission]bool{}
	for _, p := range in {
		if !p.Valid() {
			return nil
		}
		have[p] = true
	}
	var out []Permission
	for _, p := range []Permission{PermRead, PermPreview, PermDownload} {
		if have[p] {
			out = append(out, p)
		}
	}
	return out
}

// allows reports whether the rule accepts the media type.
func (r Rule) allows(mime string) bool {
	for _, p := range r.AllowedMIME {
		p = lowerTrim(p)
		if p == mime || (strings.HasSuffix(p, "/*") && strings.HasPrefix(mime, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}
