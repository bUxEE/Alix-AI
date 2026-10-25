package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Language packs and the voice profile (master plan 13, 16.3; M4-01, M4-05).
//
// This file is the composition root of the voice profile: it is the one place
// that imports internal/voice/profile (which also registers the `voice` section
// of the configuration, so a `voice:` YAML key is accepted from the moment this
// package is linked), supplies it with the pack store, and calls profile.Load.
//
//   - `alixai voice list|install|activate|deactivate` manage the packs of the
//     data directory. Install copies the operator's already downloaded files and
//     checks every size and SHA-256 against the manifest compiled into the
//     binary; nothing is ever downloaded, and no pack outside that set can be
//     installed. Install needs the operator's explicit approval of the pack,
//     activation is a separate explicit step, and at most one pack is active.
//   - `alixai serve` loads the profile only when `voice.enabled` is true. Disabled
//     (the default), the provider below does no I/O at all. When voice is wanted
//     and cannot come up, serve logs it and runs text-only (master plan 18): voice
//     is optional and a text-only deployment must keep serving.
//
// Activation by the CLI is persisted, not live: the CLI is another process than serve, so
// `alixai voice activate` takes effect at the next start of `alixai serve`. Once the
// profile is loaded, serve builds the voice session service over it (cmd_voice_session.go,
// M4-08): a pack changed while serve runs goes through that service's registry, which writes
// the activation marker too, so voice.active_pack, the marker and the live pack agree.

func init() {
	Register(Command{Name: "voice list", Summary: "list the approved language packs, which are installed and which is active", Run: voiceRunList})
	Register(Command{Name: "voice install", Summary: "install an approved language pack from files you downloaded; hashes are verified and you must approve it explicitly", Run: voiceRunInstall})
	Register(Command{Name: "voice activate", Summary: "activate an installed language pack (one at a time; takes effect at the next start of serve)", Run: voiceRunActivate})
	Register(Command{Name: "voice deactivate", Summary: "deactivate the active language pack", Run: voiceRunDeactivate})
	registerServeProvider("voice", voiceServeProvide)
}

const (
	voiceListUsage       = "alixai voice list [-config FILE] [-data-dir DIR]"
	voiceInstallUsage    = "alixai voice install [-config FILE] [-data-dir DIR] [-from DIR] [-approve PACK] PACK"
	voiceActivateUsage   = "alixai voice activate [-config FILE] [-data-dir DIR] PACK"
	voiceDeactivateUsage = "alixai voice deactivate [-config FILE] [-data-dir DIR]"

	// voicePackDirName is the directory of the pack store inside the data directory.
	voicePackDirName = "voice-packs"
	// voiceCloseTimeout bounds the unload of the active pack at shutdown.
	voiceCloseTimeout = 10 * time.Second
)

// voiceShippedManifests returns the approved manifests. It is the manifest set
// embedded in the binary; a test replaces it with a small fixture.
var voiceShippedManifests = packs.Shipped

// voiceProviderFactory connects the voice providers for profile.Load. No voice
// runtime adapter is linked into the binary yet (the LocalAI STT, TTS and VAD
// adapters are M4-02), so the default refuses: a deployment that enables voice
// before that is wired runs text-only and says why. The task that wires the
// adapter assigns this variable from the init of its own cmd_*.go file.
var voiceProviderFactory profile.ProviderFactory = func(context.Context) (profile.Providers, error) {
	return profile.Providers{}, contracts.New(contracts.ErrUnavailable, "voice: no voice runtime adapter is built into this binary")
}

func voiceEmit(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// voiceFlags are the flags every voice subcommand takes.
type voiceFlags struct {
	env     *Env
	config  *string
	dataDir *string
}

func voiceNewFlags(env *Env, fs *flag.FlagSet) voiceFlags {
	return voiceFlags{env: env, config: env.ConfigFlag(fs), dataDir: fs.String("data-dir", "", "data directory (default: the data_dir of the configuration)")}
}

// open loads the configuration, resolves the data directory (the -data-dir
// override, else the configuration's) and opens the pack store below it. The
// configuration is always loaded: `voice list` reports its voice section.
func (f voiceFlags) open() (*packs.Store, error) {
	if err := f.env.Config.LoadFrom(*f.config, f.env.LookupEnv); err != nil {
		return nil, err
	}
	dir := *f.dataDir
	if dir == "" {
		dir = f.env.Config.Core().DataDir.String()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return voiceOpenStore(filepath.Join(abs, voicePackDirName))
}

// voiceOpenStore opens the pack store at dir over the approved manifests, after
// checking them against the model registry.
func voiceOpenStore(dir string) (*packs.Store, error) {
	manifests, err := voiceShippedManifests()
	if err != nil {
		return nil, err
	}
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		return nil, err
	}
	if err := manifests.CheckRegistry(reg); err != nil {
		return nil, err
	}
	return packs.NewStore(dir, manifests, clock.Real{})
}

func voiceRoleNames(roles []providers.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

type voicePackOut struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"`
	Language  string   `json:"language"`
	Status    string   `json:"status,omitempty"`
	Roles     []string `json:"roles"`
	DiskBytes int64    `json:"disk_bytes,omitempty"`
	// Approved: this binary ships the pack's manifest, so it can be installed.
	Approved  bool `json:"approved"`
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	// Stale: installed from other pins (files, roles or version) than this binary approves; it
	// will not activate until it is installed again.
	Stale       bool   `json:"stale,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
}

type voiceListOut struct {
	Directory string `json:"directory"`
	Active    string `json:"active"`
	// VoiceEnabled and ConfigActivePack are the voice section of the configuration,
	// for the operator who wonders why serve does or does not load voice.
	VoiceEnabled     bool           `json:"voice_enabled"`
	ConfigActivePack string         `json:"config_active_pack"`
	Packs            []voicePackOut `json:"packs"`
}

// voiceRunList lists the approved and the installed packs from the manifests and
// the install records. It hashes nothing.
func voiceRunList(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("voice list")
	f := voiceNewFlags(env, fs)
	if err := env.ParseFlags(fs, voiceListUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), voiceListUsage)
	}
	st, err := f.open()
	if err != nil {
		return err
	}
	installed, err := st.Installed(ctx)
	if err != nil {
		return err
	}
	active, _, err := st.ActiveID()
	if err != nil {
		return err
	}
	byID := map[string]*voicePackOut{}
	var order []string
	for _, m := range st.Manifests().All() {
		byID[m.ID] = &voicePackOut{ID: m.ID, Name: m.Name, Language: m.Language, Status: m.Status, Roles: voiceRoleNames(m.Roles()), DiskBytes: m.DiskBytes(), Approved: true}
		order = append(order, m.ID)
	}
	for _, in := range installed {
		o, ok := byID[in.Pack.ID]
		if !ok {
			// Installed, but this binary approves no such pack: listed so the operator sees it.
			o = &voicePackOut{ID: in.Pack.ID, Language: in.Pack.Language, Roles: voiceRoleNames(in.Pack.Roles)}
			byID[o.ID] = o
			order = append(order, o.ID)
			o.Stale = true
		} else if m, _ := st.Manifests().Lookup(o.ID); m.Digest != in.ManifestSHA256 {
			o.Stale = true
		}
		o.Installed = true
		o.InstalledAt = in.InstalledAt.Format(time.RFC3339)
	}
	if o, ok := byID[active]; ok {
		o.Active = true
	}
	cfg := profile.CurrentConfig()
	out := voiceListOut{Directory: st.Root(), Active: active, VoiceEnabled: cfg.Enabled, ConfigActivePack: cfg.ActivePack, Packs: make([]voicePackOut, 0, len(order))}
	for _, id := range order {
		out.Packs = append(out.Packs, *byID[id])
	}
	return voiceEmit(env.Stdout, out)
}

type voiceInstallPlanFile struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type voiceInstallPlanModel struct {
	Role         string                 `json:"role"`
	Name         string                 `json:"name"`
	Engine       string                 `json:"engine"`
	Licence      string                 `json:"licence"`
	LicenceURL   string                 `json:"licence_url"`
	LicenceNotes string                 `json:"licence_notes"`
	Files        []voiceInstallPlanFile `json:"files"`
}

type voiceInstallPlanOut struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Language  string                  `json:"language"`
	Status    string                  `json:"status"`
	DiskBytes int64                   `json:"disk_bytes"`
	Models    []voiceInstallPlanModel `json:"models"`
}

func voicePlanOut(p packs.Plan) voiceInstallPlanOut {
	out := voiceInstallPlanOut{ID: p.ID, Name: p.Name, Language: p.Language, Status: p.Status, DiskBytes: p.DiskBytes, Models: []voiceInstallPlanModel{}}
	for _, m := range p.Models {
		pm := voiceInstallPlanModel{Role: string(m.Role), Name: m.Name, Engine: m.Engine, Licence: m.Licence.SPDX, LicenceURL: m.Licence.URL, LicenceNotes: m.Licence.RedistributionNotes}
		for _, f := range m.Files {
			pm.Files = append(pm.Files, voiceInstallPlanFile{Name: f.Name, URL: f.URL, SizeBytes: f.SizeBytes, SHA256: f.SHA256})
		}
		out.Models = append(out.Models, pm)
	}
	return out
}

// voiceRunInstall installs an approved pack from a directory of files the
// operator downloaded (the plan lists where). Without -approve naming the pack
// it prints the plan (models, licences, sizes, hashes) and refuses; nothing is
// read or written. With it, every file is verified while it is copied, and a
// single mismatch installs nothing. It does not activate the pack.
func voiceRunInstall(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("voice install")
	f := voiceNewFlags(env, fs)
	from := fs.String("from", "", "directory holding the pack's model files, downloaded from the urls in the plan")
	approve := fs.String("approve", "", "the id of the pack, to approve its models and licences explicitly")
	if err := env.ParseFlags(fs, voiceInstallUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one pack id (usage: %s)", voiceInstallUsage)
	}
	id := fs.Arg(0)
	if *approve != "" && *from == "" {
		return Usagef("-approve needs -from, the directory holding the downloaded files (usage: %s)", voiceInstallUsage)
	}
	st, err := f.open()
	if err != nil {
		return err
	}
	res, err := st.Install(ctx, packs.InstallRequest{PackID: id, From: *from, Approval: packs.Approval{PackID: *approve}})
	if contracts.HasCode(err, contracts.ErrConfirmationRequired) {
		if jerr := voiceEmit(env.Stdout, map[string]any{
			"installed": false,
			"reason":    err.Error(),
			"plan":      voicePlanOut(res.Plan),
			"next":      "download the files above, then: alixai voice install -from DIR -approve " + id + " " + id,
		}); jerr != nil {
			return jerr
		}
		return err
	}
	if err != nil {
		return err
	}
	return voiceEmit(env.Stdout, map[string]any{
		"installed": true, "replaced": res.Replaced, "id": id, "directory": res.Dir, "disk_bytes": res.Plan.DiskBytes,
		"active": false, "next": "alixai voice activate " + id,
	})
}

// voiceRunActivate activates an installed pack: its files are verified again,
// and a second pack is refused with conflict while another is active.
func voiceRunActivate(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("voice activate")
	f := voiceNewFlags(env, fs)
	if err := env.ParseFlags(fs, voiceActivateUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one pack id (usage: %s)", voiceActivateUsage)
	}
	st, err := f.open()
	if err != nil {
		return err
	}
	pack, err := st.Activate(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return voiceEmit(env.Stdout, map[string]any{
		"activated": pack.ID, "language": pack.Language, "roles": voiceRoleNames(pack.Roles), "verified": true,
		"takes_effect": "at the next start of alixai serve, when voice.enabled is true",
	})
}

// voiceRunDeactivate clears the active pack. With none active it does nothing.
func voiceRunDeactivate(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("voice deactivate")
	f := voiceNewFlags(env, fs)
	if err := env.ParseFlags(fs, voiceDeactivateUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), voiceDeactivateUsage)
	}
	st, err := f.open()
	if err != nil {
		return err
	}
	prev, _, _ := st.ActiveID()
	if err := st.Deactivate(ctx); err != nil {
		return err
	}
	return voiceEmit(env.Stdout, map[string]any{
		"deactivated": prev, "takes_effect": "at the next start of alixai serve",
	})
}

// voiceServeProvide loads the voice profile for `alixai serve`. Voice is
// optional: with voice.enabled false (the default) this does nothing and touches
// nothing, and when voice is wanted but cannot come up it logs why and returns
// nil, so the deployment runs text-only (master plan 18) instead of not starting.
func voiceServeProvide(ctx context.Context, d serveDeps) error {
	cfg := profile.CurrentConfig()
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	deps := profile.Deps{Config: cfg, Registry: d.Providers, Factory: voiceProviderFactory}
	var st *packs.Store
	if cfg.Enabled {
		var err error
		st, err = voiceOpenStore(filepath.Join(d.DataDir, voicePackDirName))
		if err != nil {
			log.Warn("voice profile not loaded: running text-only", slog.String("reason", err.Error()))
			return nil
		}
		// The activation is the operator's, recorded by `alixai voice activate`; the
		// configuration may name one too. Two different answers are an error, not a
		// silent pick of one of them.
		recorded, _, err := st.ActiveID()
		if err != nil {
			log.Warn("voice profile not loaded: running text-only", slog.String("reason", err.Error()))
			return nil
		}
		switch {
		case cfg.ActivePack != "" && recorded != "" && cfg.ActivePack != recorded:
			log.Warn("voice profile not loaded: running text-only",
				slog.String("reason", "voice.active_pack and the pack recorded by 'alixai voice activate' differ"),
				slog.String("config_pack", cfg.ActivePack), slog.String("recorded_pack", recorded))
			return nil
		case cfg.ActivePack == "":
			deps.Config.ActivePack = recorded
		}
		deps.Catalog, deps.Loader = st.Catalog(), st.Loader()
	}
	p, status, err := profile.Load(ctx, deps)
	if err != nil {
		log.Warn("voice profile not loaded: running text-only", slog.String("reason", err.Error()))
		return nil
	}
	if p == nil {
		if cfg.Enabled {
			log.Info("voice profile absent", slog.String("why", string(status.Absent)))
		}
		return nil
	}
	// The pin is the configuration's own choice, before the recorded activation filled a gap in it.
	return voiceSessionWire(ctx, d, st, p, status, cfg.ActivePack, log)
}
