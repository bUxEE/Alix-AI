// Package fs is the source provider for local folders and mounted HDD, SSD and
// NAS paths that the host can see (master plan 12.1, 16.2, 19). It implements
// source.Provider for the source types local_folder, mounted_volume and
// exported_dataset.
//
// # Confinement
//
// A source has one explicit root, and the provider lists and reads only inside
// it. Four layers enforce that, each of which denies on its own:
//
//  1. The declared root is canonicalised once per call: normalised (Windows
//     verbatim \\?\ and UNC forms included), resolved with
//     filepath.EvalSymlinks, normalised again and required to be a directory.
//     A folder root that is a whole volume or share is refused (the package
//     author may not point AlixAI at an entire disk); a mount root is the
//     operator's explicit choice.
//  2. An item key is hostile input. It must be a clean relative slash path;
//     absolute, drive, UNC, "..", backslash, alternate-data-stream and
//     Windows-reserved forms never reach the filesystem (source.Paths.ValidKey),
//     and the include and exclude rules are applied to the key itself.
//  3. Every component of the path below the root is inspected without
//     following it. A symbolic link, a Windows junction or other reparse
//     point, a device, a pipe or a socket anywhere in the path denies the item.
//     Links are never followed, even when they lead inside the root; a link is
//     reported as skipped, and one that leads outside as symlink_escape. For
//     Stat and Open the canonical form of the path (EvalSymlinks plus a
//     separator-aware, case-folding prefix check) must also equal the key, so
//     an alias such as an 8.3 short name cannot name an excluded file.
//  4. All metadata and content access goes through an os.Root opened on the
//     canonical root, which makes the kernel refuse any path that leaves it,
//     including one swapped for a link between the checks and the read.
//
// # What is read, and when
//
// An item larger than limits.max_file_bytes is rejected from the directory
// entry's size before the file is opened, and the read is bounded again in case
// the file grew. The MIME type is sniffed from the first bytes (source.Classify),
// never taken from the extension, and an item whose name contradicts its bytes
// is skipped. Change detection compares size and modification time with the
// caller's previous record and hashes (SHA-256) only when they differ or the
// record is too fresh to trust (the racy window); a file whose time changed
// but whose bytes did not is reported unchanged.
//
// # Failing closed
//
// An unreachable root is contracts.ErrUnavailable and Health unavailable, never
// an empty result. A scan that cannot finish returns no changes at all. An
// unreadable directory or file is a Failure and keeps its previous records; only
// an item whose place was inspected and found empty becomes a tombstone.
//
// Known limitation: an unmounted NAS or removable drive can leave an empty
// directory at the mount point. That is indistinguishable from a reachable,
// empty root, and a scan of it tombstones what the source held. The provider
// reports exactly what it sees; a caller that wants a guard (for example, a
// refusal to apply a scan that deletes most of a large source without an
// operator confirming it) applies it to the ChangeSet before acting on Deleted.
package fs

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rootguard"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// DefaultRacyWindow is how close to a record's observation time its
// modification time may be before the record stops being trusted for the
// size-and-time shortcut. It covers the two-second granularity of FAT and the
// coarse timestamps of some network shares.
const DefaultRacyWindow = 2 * time.Second

// Config configures a Provider. The zero value is usable.
type Config struct {
	// Clock supplies the time stamped on tombstones and probes. Nil means
	// clock.Real.
	Clock clock.Clock
	// CaseInsensitive forces (true) or forbids (false) case-folding of include
	// and exclude rules. Nil means fold on Windows and macOS, and for mount
	// roots everywhere (a mounted share is usually case-insensitive), so a
	// directory called "Private" cannot slip past the exclude "private/**".
	CaseInsensitive *bool
	// RacyWindow overrides DefaultRacyWindow.
	RacyWindow time.Duration
	// Protected are directories of AlixAI itself: its data directory and the
	// directory of its executable. A folder root that is one of them, lies
	// inside one or contains one is refused, so a package cannot point
	// ingestion at the core's own store, keys or binaries. Each is made
	// absolute and resolved through links where it exists.
	Protected []string
}

// Provider is the folder and mounted-volume source provider. It is stateless
// and safe for concurrent use.
type Provider struct {
	clk   clock.Clock
	paths source.Paths
	fold  *bool
	racy  time.Duration
	// protected are Config.Protected, absolute and normalised.
	protected []string

	// evalSymlinks is filepath.EvalSymlinks; tests replace it to simulate a
	// resolver that disagrees with the filesystem without needing symlink
	// privileges.
	evalSymlinks func(string) (string, error)
	// onOpen, when set, sees the canonical absolute path of every file whose
	// content the provider is about to open, before it opens it.
	onOpen func(abs string)
	// wrapFS, when set, wraps the file system a scan walks, so a test can make
	// a directory listing fail without needing OS permissions.
	wrapFS func(iofs.FS) iofs.FS
}

var _ source.Provider = (*Provider)(nil)

// New returns a Provider.
func New(cfg Config) *Provider {
	p := &Provider{
		clk:          cfg.Clock,
		paths:        source.HostPaths(),
		fold:         cfg.CaseInsensitive,
		racy:         cfg.RacyWindow,
		evalSymlinks: filepath.EvalSymlinks,
	}
	if p.clk == nil {
		p.clk = clock.Real{}
	}
	if p.racy <= 0 {
		p.racy = DefaultRacyWindow
	}
	for _, d := range cfg.Protected {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		}
		// A directory that is refused as a root anyway (a volume, the home directory, a system directory) is not kept:
		// "inside it" would be every folder of the disk or of the user, as when the executable sits at /alixai.
		if n, err := p.paths.Normalize(abs); err == nil && !p.paths.IsVolumeRoot(n) && rootguard.Refusal(n) == "" {
			p.protected = append(p.protected, n)
		}
	}
	return p
}

// refuseFolder is the refusal of a folder root, on its declared and on its
// canonical form: a whole volume or share, a directory the readiness scanner
// would refuse too (a system directory, the home directory itself, a drive
// root), and AlixAI's own data and install directories, below or above.
func (p *Provider) refuseFolder(path string) error {
	if p.paths.IsVolumeRoot(path) {
		return contracts.New(contracts.ErrValidationFailed, "a folder source may not be a whole volume or share")
	}
	if why := rootguard.Refusal(path); why != "" {
		return contracts.New(contracts.ErrValidationFailed, "a folder source may not be this directory: "+why)
	}
	for _, d := range p.protected {
		if p.paths.Within(d, path) || p.paths.Within(path, d) {
			return contracts.New(contracts.ErrValidationFailed, "a folder source may not be, lie inside or contain AlixAI's own data or install directory")
		}
	}
	return nil
}

// ResolveRoot returns the canonical host path a folder or mount root resolves
// to, after every refusal openRoot applies, without opening it. Registration
// calls it so that the operator sees where a source really points before
// anything is ingested, and a refused folder is refused then, not at the first
// scan.
func (p *Provider) ResolveRoot(def source.Definition) (string, error) {
	declared, err := p.paths.Normalize(def.Root.Path)
	if err != nil {
		return "", unavailable("the root is not an absolute path on this host", err)
	}
	if def.Root.Kind == source.RootFolder {
		if err := p.refuseFolder(declared); err != nil {
			return "", err
		}
	}
	canon, err := p.canonicalDir(declared)
	if err != nil {
		return "", err
	}
	if def.Root.Kind == source.RootFolder {
		if err := p.refuseFolder(canon); err != nil {
			return "", err
		}
	}
	return canon, nil
}

// Kinds implements source.Provider.
func (p *Provider) Kinds() []ap.RAGSourceType {
	return []ap.RAGSourceType{ap.RAGSourceTypeLocalFolder, ap.RAGSourceTypeMountedVolume, ap.RAGSourceTypeExportedDataset}
}

// begin checks what every call needs: a live context, a scope the source's
// binding admits and a definition this provider can serve.
func (p *Provider) begin(ctx context.Context, scope contracts.Scope, def source.Definition) error {
	if err := ctx.Err(); err != nil {
		return interrupted(err)
	}
	if err := def.CheckScope(scope); err != nil {
		return err
	}
	if def.Root.Kind != source.RootFolder && def.Root.Kind != source.RootMount {
		return contracts.Newf(contracts.ErrValidationFailed, "the folder provider serves folder and mount roots, not %q", def.Root.Kind)
	}
	return nil
}

func (p *Provider) matcher(def source.Definition) (*source.Matcher, error) {
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin" || def.Root.Kind == source.RootMount
	if p.fold != nil {
		fold = *p.fold
	}
	return source.NewMatcher(def.Include, def.Exclude, fold)
}

func interrupted(err error) error {
	return contracts.Wrap(contracts.ErrTimeout, err, "the operation was interrupted")
}

func unavailable(msg string, cause error) *contracts.Error {
	return contracts.Wrap(contracts.ErrUnavailable, cause, msg)
}

// rootHandle is an opened, canonical root.
type rootHandle struct {
	// canon is the canonical absolute host path of the (sub-)root; every path
	// the provider hands to a hook or compares is derived from it.
	canon string
	r     *os.Root
}

func (h *rootHandle) close() {
	if h != nil && h.r != nil {
		_ = h.r.Close()
	}
}

// openRoot canonicalises the declared root and opens it. A root that cannot be
// reached for any reason is contracts.ErrUnavailable; a folder root that is a
// whole volume, a system or home directory, or AlixAI's own data or install
// directory is contracts.ErrValidationFailed.
func (p *Provider) openRoot(def source.Definition) (*rootHandle, error) {
	canon, err := p.ResolveRoot(def)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(canon)
	if err != nil {
		return nil, unavailable("the root cannot be opened", err)
	}
	h := &rootHandle{canon: canon, r: r}
	if sub := def.Root.SubPath; sub != "" {
		// The mount's sub-path is resolved as an item below the mount, so it
		// cannot leave the mount through a link or a "..".
		if err := p.paths.ValidKey(sub); err != nil {
			h.close()
			return nil, err
		}
		sr, err := r.OpenRoot(sub)
		if err != nil {
			h.close()
			if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				return nil, unavailable("the source's folder inside the mount does not exist", err)
			}
			return nil, escapeOrUnavailable(err, "the source's folder inside the mount cannot be opened")
		}
		_ = r.Close()
		h = &rootHandle{canon: p.paths.Join(canon, sub), r: sr}
	}
	// A root that opens but cannot be listed is not reachable.
	if err := h.listable(); err != nil {
		h.close()
		return nil, unavailable("the root cannot be listed", err)
	}
	return h, nil
}

// listable reads one entry of the root's directory listing.
func (h *rootHandle) listable() error {
	f, err := h.r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// canonicalDir resolves a normalised absolute path to the canonical directory
// it names: symbolic links resolved, the result normalised again (EvalSymlinks
// keeps a \\?\ prefix it was given) and required to be a directory.
func (p *Provider) canonicalDir(declared string) (string, error) {
	resolved, err := p.evalSymlinks(declared)
	if err != nil {
		return "", unavailable("the root cannot be reached", err)
	}
	canon, err := p.paths.Normalize(resolved)
	if err != nil {
		return "", unavailable("the root resolves to a path this host cannot use", err)
	}
	fi, err := os.Stat(canon)
	if err != nil {
		return "", unavailable("the root cannot be reached", err)
	}
	if !fi.IsDir() {
		return "", unavailable("the root is not a directory", nil)
	}
	return canon, nil
}

func escapeOrUnavailable(err error, msg string) error {
	if strings.Contains(err.Error(), "path escapes") {
		return contracts.Wrap(contracts.ErrScopeViolation, err, "path is outside the declared source root")
	}
	return unavailable(msg, err)
}

// Probe implements source.Provider.
func (p *Provider) Probe(ctx context.Context, scope contracts.Scope, def source.Definition) (source.Health, error) {
	if err := p.begin(ctx, scope, def); err != nil {
		return source.Health{}, err
	}
	h, err := p.openRoot(def)
	if err != nil {
		var ce *contracts.Error
		if errors.As(err, &ce) && ce.Code == contracts.ErrUnavailable {
			return source.Health{Availability: source.Unavailable, Err: ce}, nil
		}
		return source.Health{}, err
	}
	h.close()
	return source.Health{Availability: source.Available, CheckedAt: p.clk.Now()}, nil
}
