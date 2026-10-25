package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rootguard"
)

// ScanLimits bound everything the walker does. A zero field takes its default, so a caller
// that wants a tighter scan sets only what it cares about. Every cap fails closed: what is over
// a cap is recorded as skipped with a reason and is never read.
type ScanLimits struct {
	// MaxFiles is the number of files recorded; the walk stops there and reports it.
	MaxFiles int `json:"max_files"`
	// MaxDepth is the deepest directory level walked; the root is level 0.
	MaxDepth int `json:"max_depth"`
	// MaxFileBytes is the largest file that is read. A bigger file is recorded and not read.
	MaxFileBytes int64 `json:"max_file_bytes"`
	// MaxTotalBytes is the total of the bytes read across the scan.
	MaxTotalBytes int64 `json:"max_total_bytes"`
	// MaxDirEntries is the entries listed in one directory; the rest are skipped.
	MaxDirEntries int `json:"max_dir_entries"`
}

const (
	scanDefaultMaxFiles      = 4000
	scanDefaultMaxDepth      = 10
	scanDefaultMaxFileBytes  = 2 << 20
	scanDefaultMaxTotalBytes = 32 << 20
	scanDefaultMaxDirEntries = 2000
	scanMaxRelPath           = 512
)

func (l ScanLimits) scanWithDefaults() ScanLimits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = scanDefaultMaxFiles
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = scanDefaultMaxDepth
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = scanDefaultMaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = scanDefaultMaxTotalBytes
	}
	if l.MaxDirEntries <= 0 {
		l.MaxDirEntries = scanDefaultMaxDirEntries
	}
	return l
}

// Skip reasons. They are a closed vocabulary so no raw error text, and no secret, reaches a report.
const (
	ScanSkipDependencyDir = "dependency_or_vcs_directory"
	ScanSkipSymlink       = "symlink_or_junction_not_followed"
	ScanSkipSpecialFile   = "not_a_regular_file"
	ScanSkipTooLarge      = "file_over_size_cap"
	ScanSkipTotalCap      = "total_bytes_cap_reached"
	ScanSkipFileCap       = "file_count_cap_reached"
	ScanSkipDepthCap      = "depth_cap_reached"
	ScanSkipDirEntryCap   = "directory_entry_cap_reached"
	ScanSkipUnsafeName    = "unsafe_file_name"
	ScanSkipUnreadable    = "unreadable"
	ScanSkipChanged       = "changed_while_scanning"
	ScanSkipParseError    = "parse_error"
	ScanSkipTooDeep       = "structure_too_deeply_nested"
)

// ScanSkip is one thing the walker did not read, and why.
type ScanSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanFile is one file the walker recorded. Secret files carry no hash: they are listed, never opened.
type ScanFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Kind   string `json:"kind"`
}

// File kinds.
const (
	scanKindText   = "text"
	scanKindDoc    = "document"
	scanKindOther  = "other"
	scanKindSecret = "secret"
	scanKindBig    = "unread"
)

type scanFileRec struct {
	ScanFile
	content []byte // text files only
	binary  bool
	lines   int
}

// scanDirEntry is what the walker needs to know about a directory entry.
type scanDirEntry struct {
	Name string
	Mode fs.FileMode // type bits only
	Size int64
}

type scanReader interface {
	io.Reader
	io.Closer
	Stat() (fs.FileInfo, error)
}

// scanFS is the only way the scanner touches the disk. The real one is confined to the root with
// os.Root; tests wrap it to record every open and to fake entry types.
type scanFS interface {
	ReadDir(dir string, limit int) (entries []scanDirEntry, truncated bool, err error)
	Open(name string) (scanReader, error)
	Close() error
}

type scanRootFS struct{ root *os.Root }

func (r scanRootFS) Close() error { return r.root.Close() }

func (r scanRootFS) Open(name string) (scanReader, error) {
	return r.root.Open(filepath.FromSlash(name))
}

func (r scanRootFS) ReadDir(dir string, limit int) ([]scanDirEntry, bool, error) {
	f, err := r.root.Open(filepath.FromSlash(dir))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var out []scanDirEntry
	truncated := false
	for {
		batch, err := f.ReadDir(128)
		for _, e := range batch {
			if len(out) >= limit {
				truncated = true
				break
			}
			se := scanDirEntry{Name: e.Name(), Mode: e.Type() & fs.ModeType}
			if se.Mode == 0 {
				if info, ierr := e.Info(); ierr == nil {
					se.Size = info.Size()
				}
			}
			out = append(out, se)
		}
		if truncated || err != nil {
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, false, err
			}
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, truncated, nil
}

var scanSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".bzr": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "out": true, "target": true, "obj": true, ".next": true, ".nuxt": true,
	".turbo": true, ".cache": true, "__pycache__": true, ".venv": true, "venv": true, ".tox": true,
	".gradle": true, ".idea": true, "coverage": true, ".pnpm-store": true, ".worktrees": true,
}

// scanSecretFile reports whether a file name looks like it holds a secret. Such a file is
// recorded as present and never opened, hashed or quoted.
func scanSecretFile(name string) bool {
	n := strings.ToLower(name)
	switch {
	case n == ".env" || strings.HasPrefix(n, ".env.") || strings.HasSuffix(n, ".env"):
		return true
	case n == ".npmrc" || n == ".pypirc" || n == ".netrc" || n == ".htpasswd" || n == ".git-credentials" || n == ".pgpass":
		return true
	case n == "credentials" || n == "credentials.json" || n == "secrets.json" || n == "secrets.yaml" || n == "secrets.yml" || n == "secrets.toml":
		return true
	case strings.HasPrefix(n, "id_rsa") || strings.HasPrefix(n, "id_dsa") || strings.HasPrefix(n, "id_ecdsa") || strings.HasPrefix(n, "id_ed25519"):
		return true
	case strings.HasPrefix(n, "service-account") && strings.HasSuffix(n, ".json"):
		return true
	case strings.HasPrefix(n, "token") && filepath.Ext(n) != ".ts" && filepath.Ext(n) != ".js" && filepath.Ext(n) != ".py" &&
		filepath.Ext(n) != ".go" && filepath.Ext(n) != ".md":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".crt", ".cer", ".der", ".token", ".secret", ".secrets", ".kdbx", ".gpg", ".asc"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

var scanTextExt = map[string]bool{
	".md": true, ".markdown": true, ".mdx": true, ".rst": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true,
	".toml": true, ".ts": true, ".tsx": true, ".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".py": true, ".go": true,
	".java": true, ".kt": true, ".cs": true, ".rb": true, ".php": true, ".rs": true, ".sql": true, ".xml": true, ".gradle": true,
	".properties": true, ".ini": true, ".cfg": true, ".conf": true, ".gql": true, ".graphql": true, ".proto": true, ".html": true,
	".htm": true, ".sh": true, ".ps1": true, ".mod": true, ".csproj": true, ".kts": true, ".rego": true, ".csv": true,
}

var scanDocExt = map[string]bool{".pdf": true, ".docx": true, ".odt": true, ".pptx": true, ".xlsx": true, ".epub": true, ".doc": true}

func scanClassify(name string) string {
	n := strings.ToLower(name)
	if scanSecretFile(name) {
		return scanKindSecret
	}
	ext := filepath.Ext(n)
	switch {
	case scanTextExt[ext], n == "modelfile", n == "makefile", n == "pipfile", strings.HasPrefix(n, "dockerfile"), strings.HasPrefix(n, "requirements"):
		return scanKindText
	case scanDocExt[ext]:
		return scanKindDoc
	}
	return scanKindOther
}

// scanSafeName rejects names that cannot be a schema relative path segment.
func scanSafeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '\\' || r == ':' || r == '/' {
			return false
		}
	}
	return true
}

type scanWalkResult struct {
	files       []*scanFileRec
	skipped     []ScanSkip
	secretFiles []string
	truncated   bool
	totalBytes  int64
}

type scanWalker struct {
	fsys scanFS
	lim  ScanLimits
	res  scanWalkResult
}

func (w *scanWalker) skip(path, reason string) {
	w.res.skipped = append(w.res.skipped, ScanSkip{Path: path, Reason: reason})
}

func scanWalk(fsys scanFS, lim ScanLimits) *scanWalkResult {
	w := &scanWalker{fsys: fsys, lim: lim}
	w.dir("", 0)
	sort.Slice(w.res.files, func(i, j int) bool { return w.res.files[i].Path < w.res.files[j].Path })
	sort.Slice(w.res.skipped, func(i, j int) bool {
		if w.res.skipped[i].Path != w.res.skipped[j].Path {
			return w.res.skipped[i].Path < w.res.skipped[j].Path
		}
		return w.res.skipped[i].Reason < w.res.skipped[j].Reason
	})
	sort.Strings(w.res.secretFiles)
	return &w.res
}

func scanJoin(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (w *scanWalker) dir(rel string, depth int) {
	listing := rel
	if listing == "" {
		listing = "."
	}
	entries, truncated, err := w.fsys.ReadDir(listing, w.lim.MaxDirEntries)
	if err != nil {
		w.skip(scanDisplay(rel), ScanSkipUnreadable)
		return
	}
	if truncated {
		w.skip(scanDisplay(rel), ScanSkipDirEntryCap)
		w.res.truncated = true
	}
	for _, e := range entries {
		p := scanJoin(rel, e.Name)
		if !scanSafeName(e.Name) || len(p) > scanMaxRelPath {
			w.skip(scanSafePath(p), ScanSkipUnsafeName)
			continue
		}
		switch {
		case e.Mode&(fs.ModeSymlink|fs.ModeIrregular) != 0:
			// On Windows a junction reports ModeIrregular. Both are refused, so nothing
			// can lead out of the root.
			w.skip(p, ScanSkipSymlink)
		case e.Mode.IsDir():
			if scanSkipDirs[strings.ToLower(e.Name)] {
				w.skip(p, ScanSkipDependencyDir)
				continue
			}
			if depth+1 > w.lim.MaxDepth {
				w.skip(p, ScanSkipDepthCap)
				continue
			}
			w.dir(p, depth+1)
		case e.Mode != 0:
			w.skip(p, ScanSkipSpecialFile)
		default:
			w.file(p, e)
		}
	}
}

func scanDisplay(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// scanSafePath keeps a hostile name out of a report.
func scanSafePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError || r == '\\' || r == ':' {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (w *scanWalker) file(p string, e scanDirEntry) {
	if len(w.res.files) >= w.lim.MaxFiles {
		if !w.res.truncated || !w.capReported() {
			w.skip(p, ScanSkipFileCap)
		}
		w.res.truncated = true
		return
	}
	kind := scanClassify(e.Name)
	rec := &scanFileRec{ScanFile: ScanFile{Path: p, Size: e.Size, Kind: kind}}
	record := func(k, reason string) {
		rec.Kind = k
		if reason != "" {
			w.skip(p, reason)
		}
		w.res.files = append(w.res.files, rec)
	}
	switch kind {
	case scanKindSecret:
		// Present, never opened: no read, no hash, only the size the listing gave.
		w.res.secretFiles = append(w.res.secretFiles, p)
		record(scanKindSecret, "")
		return
	case scanKindOther:
		record(scanKindOther, "")
		return
	}
	if e.Size > w.lim.MaxFileBytes {
		record(scanKindBig, ScanSkipTooLarge)
		return
	}
	if w.res.totalBytes+e.Size > w.lim.MaxTotalBytes {
		record(scanKindBig, ScanSkipTotalCap)
		return
	}
	f, err := w.fsys.Open(p)
	if err != nil {
		record(scanKindBig, ScanSkipUnreadable)
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		record(scanKindBig, ScanSkipSpecialFile)
		return
	}
	h := sha256.New()
	var data []byte
	var n int64
	if kind == scanKindText {
		data, err = io.ReadAll(io.LimitReader(f, w.lim.MaxFileBytes+1))
		n = int64(len(data))
		h.Write(data)
	} else {
		n, err = io.Copy(h, io.LimitReader(f, w.lim.MaxFileBytes+1))
	}
	if err != nil {
		record(scanKindBig, ScanSkipUnreadable)
		return
	}
	if n > w.lim.MaxFileBytes {
		record(scanKindBig, ScanSkipChanged)
		return
	}
	w.res.totalBytes += n
	rec.Size = n
	rec.SHA256 = hex.EncodeToString(h.Sum(nil))
	if kind == scanKindText {
		head := data
		if len(head) > 4096 {
			head = head[:4096]
		}
		for _, b := range head {
			if b == 0 {
				rec.binary = true
				break
			}
		}
		if !rec.binary {
			rec.content = data
			rec.lines = ScanLineCount(data)
		}
	} else if n > 0 {
		rec.lines = 1
	}
	record(kind, "")
}

// capReported reports whether the file-count cap was already recorded.
func (w *scanWalker) capReported() bool {
	for _, s := range w.res.skipped {
		if s.Reason == ScanSkipFileCap {
			return true
		}
	}
	return false
}

// ScanRootRefusal explains why a directory may not be scanned, or returns "" when it may. It
// refuses a drive or filesystem root, the operating system's own directories and the home
// directory itself (internal/rootguard, which the RAG folder provider applies too).
func ScanRootRefusal(abs string) string { return rootguard.Refusal(abs) }

func scanRootRefusal(abs string, getenv func(string) string, goos string) string {
	return rootguard.RefusalWith(abs, getenv, goos)
}

// scanOpenRoot validates the directory the operator named and opens it confined to itself.
func scanOpenRoot(dir string) (scanFS, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("readiness scan: no directory named")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("readiness scan: cannot resolve the directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("readiness scan: cannot resolve the directory: %w", err)
	}
	if why := ScanRootRefusal(resolved); why != "" {
		return nil, fmt.Errorf("readiness scan: refused: %s", why)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return nil, errors.New("readiness scan: the path is not a directory")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("readiness scan: cannot open the directory: %w", err)
	}
	return scanRootFS{root: root}, nil
}
