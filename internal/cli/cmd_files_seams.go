package cli

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/pkg/contracts"
	settingsgen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// The seams of the files module (M3-20), as the composition root fills them:
//
//   - the attachment policy: the host's declared policy of the "files" section, with the
//     size of an attachment held to the deployment's settings (attachPolicy);
//   - files.RAGSources, over the RAG source registry (attachRAGSources);
//   - files.VisionGate, over the model registry (attachVisionGate);
//   - the compaction, a service job of serve (attachFilesRegisterCompaction).
//
// Every one of them fails closed: what cannot be answered is a refusal, never a permission.

// ---- the attachment policy ----------------------------------------------------

// attachSettingsView is what the policy asks of the settings service
// (gateway.SettingsService has it): the settings in force for the caller, the three levels and
// the operator's ceilings added up.
type attachSettingsView interface {
	Effective(ctx context.Context, scope contracts.Scope) (settingsgen.EffectiveSettings, error)
}

// attachPolicy is the files.PolicySource serve gives the files service. The treatments, the
// types, the lifetimes and the permissions are the host's declared policy (the base, from the
// "files" section); the size of one attachment is held to the deployment's settings:
//
//   - the operator's ceiling, settings.max_attachment_mb, applies to every application and every
//     token, and is read for every upload;
//   - the settings in force for the caller (the deployment's document of the caller's
//     application, the tenant's and the user's, each of which can only narrow the ceiling) are
//     applied on top. A deployment document is per application, so the policy is asked for
//     each scope and never cached. The settings are read as the caller (Effective needs the
//     settings.user.read scope in the token): a policy switch that cannot be read is a
//     denial, so a token without that scope, a quarantined application or a store that cannot
//     be read refuses the upload, and a deployment or tenant that switched attachments off is
//     enforced for every token alike. Only a process with no settings service at all (a build
//     that does not provide the settings area) is held to the operator's ceiling alone;
//   - attachments that the settings switch off for the caller are an empty policy: every
//     treatment is refused.
//
// files.max_file_bytes still caps every rule (files.Rule.resolve), so the smaller of the two
// decides.
type attachPolicy struct {
	base files.PolicySource
	// ceilingMB is the operator's settings.max_attachment_mb.
	ceilingMB func() int
	// settings is the settings service, looked up when asked: the providers of serve run in
	// name order, so the files provider is built before the settings one.
	settings func() (attachSettingsView, bool)
}

var _ files.PolicySource = attachPolicy{}

// AttachmentPolicy implements files.PolicySource.
func (p attachPolicy) AttachmentPolicy(ctx context.Context, scope contracts.Scope) (files.Policy, error) {
	pol, err := p.base.AttachmentPolicy(ctx, scope)
	if err != nil {
		return files.Policy{}, err
	}
	none := files.Policy{Rules: map[files.Treatment]files.Rule{}}
	mb := int64(p.ceilingMB())
	if view, ok := p.settings(); ok {
		eff, err := view.Effective(ctx, scope)
		switch {
		case err == nil:
			if !eff.Attachments.Enabled {
				return none, nil
			}
			mb = min(mb, int64(eff.Attachments.MaxFileMb))
		default:
			return files.Policy{}, err
		}
	}
	if mb < 1 {
		return none, nil
	}
	limit := mb << 20
	out := files.Policy{Default: pol.Default, Rules: make(map[files.Treatment]files.Rule, len(pol.Rules))}
	for t, r := range pol.Rules {
		if r.MaxBytes <= 0 || r.MaxBytes > limit {
			r.MaxBytes = limit
		}
		out.Rules[t] = r
	}
	return out, nil
}

// attachFilesPolicy builds the policy of a serve process.
func attachFilesPolicy(d serveDeps) files.PolicySource {
	return attachPolicy{
		base:      files.ConfigPolicy(func() files.Config { return *files.ConfigSection() }),
		ceilingMB: func() int { return settings.CurrentConfig().MaxAttachmentMB },
		settings: func() (attachSettingsView, bool) {
			if d.Services == nil {
				return nil, false
			}
			return gateway.Lookup[gateway.SettingsService](d.Services)
		},
	}
}

// ---- RAG ------------------------------------------------------------------------

// attachRAGSources is files.RAGSources over the RAG source registry, and it does what the
// registry can: nothing. The registry (gateway.RAGSources, ragSources) registers a source by
// the name the application's signed package declares it under and by nothing else, and every
// source type a package can declare reads a folder, a mount, a host API or a package, never a
// file of the files module. So an uploaded file cannot be made a source here, and saying it
// was would be a retrieval the registry never serves: RegisterFile is a typed refusal
// (unavailable, which files turns into the upload being rolled back: the file does not stay
// as a source), and RemoveFile has nothing to withdraw, because nothing was ever registered.
// When a file-backed source type exists (a provider that reads through files.Service.Open
// under the scope it is handed), this is the one place that changes.
type attachRAGSources struct{}

var _ files.RAGSources = attachRAGSources{}

// RegisterFile implements files.RAGSources.
func (attachRAGSources) RegisterFile(context.Context, contracts.Scope, files.RAGFile) error {
	return contracts.New(contracts.ErrUnavailable, "no RAG source can be made of an uploaded file in this deployment").
		WithDetails("reason", "rag_file_source_unavailable")
}

// RemoveFile implements files.RAGSources. It is idempotent: a file that was never a source is
// already withdrawn.
func (attachRAGSources) RemoveFile(context.Context, files.Location, string) error { return nil }

// ---- vision ---------------------------------------------------------------------

// attachVisionProvider is what a model provider says to be installed as a vision-capable one.
// No provider of this build implements it, so the gate answers false until one does.
type attachVisionProvider interface {
	VisionCapable() bool
}

// attachVisionGate is files.VisionGate over the model registry: a vision-capable provider is
// installed when the controller role holds a provider that says it is one and the role is not
// down. The files module hands image bytes to a model only then; the dialogue engine never
// puts them in a prompt (a prompt message is text).
type attachVisionGate struct{ reg *providers.Registry }

var _ files.VisionGate = attachVisionGate{}

// VisionAvailable implements files.VisionGate.
func (g attachVisionGate) VisionAvailable(context.Context) bool {
	if g.reg == nil || !g.reg.Usable(providers.RoleController) {
		return false
	}
	p, ok := g.reg.Provider(providers.RoleController)
	if !ok {
		return false
	}
	v, ok := p.(attachVisionProvider)
	return ok && v.VisionCapable()
}

// ---- compaction -----------------------------------------------------------------

// attachFilesCompact is one compaction of the files service. A variable so a test can watch it.
var attachFilesCompact = func(ctx context.Context, svc *files.Service) error {
	_, err := svc.Compact(ctx)
	return err
}

// attachFilesRegisterCompaction makes the compaction a service job of serve: it starts with the
// other jobs, once everything is built, runs every files.compact_every, and is stopped, and
// waited for, when serve stops, before the store closes. Nothing starts here: a goroutine
// started during the wiring would run until a context nobody owns ends, and could still be
// inside a sweep when the store is closed under it.
func attachFilesRegisterCompaction(d serveDeps, svc *files.Service) error {
	if d.AddService == nil {
		return errors.New("files: serve gave no way to run the compaction job")
	}
	every := attachFilesCompactEvery()
	d.AddService(func(ctx context.Context) (stop func()) {
		jobs, cancel := context.WithCancel(ctx)
		wait := startServiceJobs(jobs, d.Log, serviceJob{name: "files.compact", every: every, run: func(ctx context.Context) error {
			return attachFilesCompact(ctx, svc)
		}})
		return func() {
			cancel()
			wait()
		}
	})
	return nil
}

// attachFilesCompactEvery is how often the job runs: files.compact_every, or its default.
func attachFilesCompactEvery() time.Duration {
	if every := files.ConfigSection().CompactEvery; every > 0 {
		return every
	}
	return files.DefaultCompactEvery
}
