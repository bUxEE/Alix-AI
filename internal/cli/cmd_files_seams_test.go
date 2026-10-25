package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/settings"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	settingsgen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// The files seams of serve (M3-20): the attachment size held to the deployment's settings, the RAG
// and vision seams that answer no until something can say yes, and the compaction as a job of serve.

const (
	attachMiB = 1 << 20
	attachApp = "app_files"
)

func attachScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_files", ApplicationID: attachApp, TenantID: "ten_a", WorkspaceID: "wsp_1", PrincipalID: "usr_1",
		Roles: []string{"member"}, CapabilityScopes: []string{"files.use"}, SessionID: "ses_1",
	}
}

// attachRig is provideFiles run over a throwaway store, the way serve runs it.
type attachRig struct {
	deps   serveDeps
	svc    gateway.FilesService
	models gateway.AttachModelFiles
	starts []func(context.Context) func()
}

func attachNewRig(t *testing.T, before ...func()) *attachRig {
	t.Helper()
	oldFiles, oldSettings := *files.ConfigSection(), *settings.ConfigSection()
	t.Cleanup(func() { *files.ConfigSection(), *settings.ConfigSection() = oldFiles, oldSettings })
	for _, f := range before {
		f()
	}
	r := &attachRig{}
	r.deps = serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: clock.Real{}, DB: storetest.Open(t, nil), DataDir: t.TempDir(),
		Services:   gateway.NewServices(),
		AddService: func(start func(context.Context) func()) { r.starts = append(r.starts, start) },
	}
	if err := provideFiles(context.Background(), r.deps); err != nil {
		t.Fatalf("provideFiles: %v", err)
	}
	var ok bool
	if r.svc, ok = gateway.Lookup[gateway.FilesService](r.deps.Services); !ok {
		t.Fatal("the files service was not provided")
	}
	if r.models, ok = gateway.Lookup[gateway.AttachModelFiles](r.deps.Services); !ok {
		t.Fatal("the turn's view of the files service was not provided")
	}
	return r
}

func (r *attachRig) put(t *testing.T, size int, mods ...func(*files.PutInput)) error {
	t.Helper()
	in := files.PutInput{Name: "notes.txt", DeclaredMIME: "text/plain", Content: bytes.NewReader(bytes.Repeat([]byte("a"), size))}
	for _, m := range mods {
		m(&in)
	}
	_, err := r.svc.Put(context.Background(), attachScope(), in)
	return err
}

func (r *attachRig) wantStored(t *testing.T, what string, size int) {
	t.Helper()
	if err := r.put(t, size); err != nil {
		t.Errorf("%s: %d bytes refused: %v", what, size, err)
	}
}

func (r *attachRig) wantTooLarge(t *testing.T, what string, size int) {
	t.Helper()
	err := r.put(t, size)
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed || ce.Details["reason"] != "too_large" {
		t.Errorf("%s: %d bytes = %v, want validation_failed too_large", what, size, err)
	}
}

func attachSetCeiling(mb int) {
	cfg := settings.DefaultConfig()
	cfg.MaxAttachmentMB = mb
	*settings.ConfigSection() = cfg
}

// The size of an attachment is the deployment's: the operator's settings.max_attachment_mb, read
// for every upload, and narrowed by the settings in force for the caller; files.max_file_bytes
// still caps it from above.
func TestFilesAttachmentCeilingComesFromSettings(t *testing.T) {
	r := attachNewRig(t)

	t.Run("the operator's ceiling", func(t *testing.T) {
		attachSetCeiling(8)
		r.wantStored(t, "ceiling 8 MB", 7*attachMiB)
		r.wantTooLarge(t, "ceiling 8 MB", 9*attachMiB) // within files.max_file_bytes (10 MiB): only the ceiling refuses it
		attachSetCeiling(4)                            // a reloaded section takes effect at once
		r.wantStored(t, "ceiling 4 MB", 3*attachMiB)
		r.wantTooLarge(t, "ceiling 4 MB", 5*attachMiB)
		attachSetCeiling(25) // above files.max_file_bytes: the smaller of the two decides
		r.wantStored(t, "ceiling 25 MB", 9*attachMiB)
		r.wantTooLarge(t, "ceiling 25 MB", 11*attachMiB)
	})

	t.Run("the settings in force for the caller narrow it, found when asked", func(t *testing.T) {
		attachSetCeiling(8)
		// The settings provider runs after the files one: it is provided now, and found now.
		fake := &attachFakeSettings{eff: settingsgen.EffectiveSettings{Attachments: settingsgen.EffectiveSettingsAttachments{
			Allowed: true, Enabled: true, MaxFileMb: 2,
		}}}
		if err := gateway.Provide[gateway.SettingsService](r.deps.Services, fake); err != nil {
			t.Fatal(err)
		}
		r.wantStored(t, "deployment 2 MB", 1*attachMiB)
		r.wantTooLarge(t, "deployment 2 MB", 3*attachMiB)
		if fake.calls.Load() == 0 {
			t.Error("the settings were never asked")
		}

		// Attachments switched off for the caller: every treatment is refused.
		fake.set(settingsgen.EffectiveSettings{Attachments: settingsgen.EffectiveSettingsAttachments{Allowed: true, MaxFileMb: 20}})
		if err := r.put(t, 10); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("an upload with attachments switched off = %v, want permission_denied", err)
		}

		// Settings that cannot be read as the caller are a denial, not a guess: a token with no
		// settings scope cannot be shown to have attachments switched on.
		fake.fail(contracts.New(contracts.ErrPermissionDenied, "no settings scope"))
		if err := r.put(t, 10); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("an upload with a token that may not read settings = %v, want permission_denied", err)
		}

		// And so is any other failure of the settings.
		fake.fail(contracts.New(contracts.ErrQuarantined, "no active package"))
		if err := r.put(t, 10); contracts.CodeOf(err) != contracts.ErrQuarantined {
			t.Errorf("an upload while the settings fail = %v, want quarantined", err)
		}
	})
}

// attachFakeSettings is a gateway.SettingsService that answers Effective only.
type attachFakeSettings struct {
	gateway.SettingsService
	mu    sync.Mutex
	eff   settingsgen.EffectiveSettings
	err   error
	calls atomic.Int64
}

func (f *attachFakeSettings) set(e settingsgen.EffectiveSettings) {
	f.mu.Lock()
	f.eff, f.err = e, nil
	f.mu.Unlock()
}

func (f *attachFakeSettings) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *attachFakeSettings) Effective(context.Context, contracts.Scope) (settingsgen.EffectiveSettings, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eff, f.err
}

// RAG: an uploaded file cannot be made a source by this registry, and the seam says so; nothing
// stays behind as a source, and withdrawing what was never registered is not an error.
func TestFilesRAGSeamRefusesWhatTheRegistryCannotDo(t *testing.T) {
	r := attachNewRig(t)
	cfg := files.DefaultConfig()
	cfg.RAGSource.Enabled = true
	*files.ConfigSection() = cfg
	attachSetCeiling(8)

	err := r.put(t, 100, func(in *files.PutInput) { in.Treatment = files.TreatmentRAGSource })
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable {
		t.Fatalf("a RAG source upload = %v, want unavailable", err)
	}
	var live int
	if e := r.deps.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM files WHERE tombstoned_at IS NULL`).Scan(&live); e != nil || live != 0 {
		t.Errorf("%d live files after a refused source (%v)", live, e)
	}
	if err := (attachRAGSources{}).RemoveFile(context.Background(), files.Location{}, "fil_x"); err != nil {
		t.Errorf("withdrawing a file that was never a source = %v", err)
	}
}

type attachVisionModel struct {
	*pfake.Provider
	vision bool
}

func (m attachVisionModel) VisionCapable() bool { return m.vision }

// Vision: only a controller that says it sees, and is not down, opens the gate.
func TestFilesVisionGateFollowsTheModelRegistry(t *testing.T) {
	ctx := context.Background()
	if (attachVisionGate{}).VisionAvailable(ctx) {
		t.Error("a gate with no registry is open")
	}
	clk := clock.NewFake(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	gate := func(p any) (attachVisionGate, *providers.Registry) {
		reg := providers.NewRegistry(clk)
		if p != nil {
			if err := reg.Register(providers.RoleController, p); err != nil {
				t.Fatal(err)
			}
		}
		return attachVisionGate{reg: reg}, reg
	}
	if g, _ := gate(nil); g.VisionAvailable(ctx) {
		t.Error("a gate with no controller is open")
	}
	if g, _ := gate(pfake.New(pfake.WithClock(clk))); g.VisionAvailable(ctx) {
		t.Error("a text-only controller opened the gate")
	}
	if g, _ := gate(attachVisionModel{Provider: pfake.New(pfake.WithClock(clk)), vision: false}); g.VisionAvailable(ctx) {
		t.Error("a controller that says it does not see opened the gate")
	}
	g, reg := gate(attachVisionModel{Provider: pfake.New(pfake.WithClock(clk)), vision: true})
	if !g.VisionAvailable(ctx) {
		t.Error("a vision-capable controller did not open the gate")
	}
	if err := reg.SetHealth(providers.RoleController, providers.HealthUnavailable, "down"); err != nil {
		t.Fatal(err)
	}
	if g.VisionAvailable(ctx) {
		t.Error("a controller that is down opened the gate")
	}
}

// The provider registers the compaction as a job of serve and starts nothing itself; the job runs
// while serve runs and is gone, finished, when serve's stop function returns.
func TestFilesProviderRegistersCompactionAsAServiceJob(t *testing.T) {
	var runs, inFlight atomic.Int64
	old := attachFilesCompact
	attachFilesCompact = func(context.Context, *files.Service) error {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		runs.Add(1)
		time.Sleep(10 * time.Millisecond) // a sweep that does not watch its context
		return nil
	}
	t.Cleanup(func() { attachFilesCompact = old })
	r := attachNewRig(t, func() { files.ConfigSection().CompactEvery = 10 * time.Millisecond })
	if len(r.starts) != 1 {
		t.Fatalf("the provider registered %d services, want one job", len(r.starts))
	}
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("the compaction ran before serve started its jobs")
	}
	stop := r.starts[0](context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runs.Load() < 3 {
		t.Fatalf("the job ran %d times", runs.Load())
	}
	stop()
	if inFlight.Load() != 0 {
		t.Fatal("stop returned with a compaction still running")
	}
	n := runs.Load()
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != n {
		t.Fatal("the job kept running after it was stopped")
	}

	// serve that gives no way to run a job is a failure to start, not a files area that never compacts.
	svc, err := files.New(files.Options{DB: r.deps.DB, DataDir: r.deps.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachFilesRegisterCompaction(serveDeps{Log: r.deps.Log}, svc); err == nil {
		t.Error("a compaction with nowhere to run was accepted")
	}
}

// In a real serve: the compaction runs on the period of files.compact_every, and `serve` does not
// return while a sweep is in flight nor leave one running behind it.
func TestFilesCompactionStopsWithServe(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "files:\n  compact_every: 20ms\n")
	t.Cleanup(func() { *files.ConfigSection() = files.DefaultConfig() })

	var runs, inFlight atomic.Int64
	old := attachFilesCompact
	attachFilesCompact = func(context.Context, *files.Service) error {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		runs.Add(1)
		time.Sleep(15 * time.Millisecond) // a sweep that does not watch its context: serve has to wait for it
		return nil
	}
	t.Cleanup(func() { attachFilesCompact = old })

	var out, errOut syncBuf
	env := serveEnv(t, &out, &errOut)
	files.RegisterSection(env.Config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, env, []string{"-config", cfg}) }()

	deadline := time.Now().Add(30 * time.Second)
	for runs.Load() < 3 && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("serve stopped before the job ran: %v\nstderr:\n%s", err, errOut.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if runs.Load() < 3 {
		t.Fatalf("the compaction ran %d times while serve ran\nstderr:\n%s", runs.Load(), errOut.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("serve returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	if inFlight.Load() != 0 {
		t.Fatal("serve returned with a compaction still running")
	}
	n := runs.Load()
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != n {
		t.Fatal("a compaction ran after serve had stopped")
	}
	if !strings.Contains(out.String(), "listening on") {
		t.Errorf("serve never listened:\n%s", out.String())
	}
}
