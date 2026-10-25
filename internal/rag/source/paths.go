package source

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// ErrEscapesRoot is what a provider returns when a request names, or resolves
// to, something outside the declared root: a "..", an absolute or drive or UNC
// name, a symbolic link or reparse point, an alias of another name. Match it
// with errors.Is; the concrete error carries the offending relative key, never
// a host path.
var ErrEscapesRoot = contracts.New(contracts.ErrScopeViolation, "path is outside the declared source root")

// ErrNotFound is returned for an item that does not exist in a reachable
// root. Match it with errors.Is(err, source.ErrNotFound) or fs.ErrNotExist.
// It is the only outcome a caller may read as "gone"; an unreachable root is
// contracts.ErrUnavailable and never ErrNotFound.
var ErrNotFound = contracts.Wrap(contracts.ErrStale, fs.ErrNotExist, "source item no longer exists")

// ErrNotAdmitted is returned for an item that exists but that the include and
// exclude rules, the size limit or the accepted formats of the source do not
// admit. It is distinct from ErrNotFound: the item is there, it is not for
// AlixAI to read.
var ErrNotAdmitted = contracts.New(contracts.ErrPermissionDenied, "the source rules do not admit this item")

// maxKeyBytes bounds an item key. Real path limits are lower on every
// supported host; the bound exists so hostile input cannot be arbitrarily long.
const maxKeyBytes = 4096

// Paths holds the path rules of one host flavour. The zero value is the POSIX
// flavour. Windows selects the Win32/NTFS rules: case-insensitive comparison,
// drive, UNC and verbatim (\\?\) prefixes, reserved device names, alternate
// data streams and the trailing dot and space that Windows silently strips.
//
// The methods are pure string functions, so both flavours are tested on every
// platform; a provider uses HostPaths.
type Paths struct {
	Windows bool
}

// HostPaths returns the flavour of the platform the process runs on.
func HostPaths() Paths { return Paths{Windows: runtime.GOOS == "windows"} }

// Fold returns s in the form used to compare two names on this flavour:
// lower-cased on Windows (NTFS and SMB compare names case-insensitively),
// unchanged elsewhere.
func (p Paths) Fold(s string) string {
	if p.Windows {
		return strings.ToLower(s)
	}
	return s
}

// Normalize returns the canonical spelling of an absolute host path, or an
// error if the path is not an absolute path on this flavour, contains a ".."
// segment, a NUL or a control character, or names a Windows device namespace.
//
// On Windows the verbatim prefixes are removed: \\?\C:\x and \??\C:\x become
// C:\x, and \\?\UNC\srv\share\x becomes \\srv\share\x. Device paths (\\.\),
// volume GUID paths and any other verbatim form are refused, as are
// drive-relative (C:x) and rooted (\x) names. Separators become backslashes,
// "." segments and repeated separators are dropped, and the result has no
// trailing separator except for a drive root (C:\). Every other flavour needs
// a leading "/" and gets the same cleaning with "/" as the separator.
func (p Paths) Normalize(path string) (string, error) {
	vol, segs, err := p.parse(path)
	if err != nil {
		return "", err
	}
	if !p.Windows {
		return "/" + strings.Join(segs, "/"), nil
	}
	if len(segs) == 0 {
		if strings.HasPrefix(vol, `\\`) {
			return vol, nil // a share root has no trailing separator
		}
		return vol + `\`, nil
	}
	return vol + `\` + strings.Join(segs, `\`), nil
}

// parse splits an absolute path into its volume ("" on POSIX, "C:" or
// "\\srv\share" on Windows) and its cleaned segments.
func (p Paths) parse(path string) (vol string, segs []string, err error) {
	if path == "" {
		return "", nil, errors.New("empty path")
	}
	if strings.IndexFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", nil, errors.New("path contains a control character")
	}
	if !utf8.ValidString(path) {
		return "", nil, errors.New("path is not valid UTF-8")
	}
	sep, rest := "/", path
	if p.Windows {
		sep = `\`
		s := strings.ReplaceAll(path, "/", `\`)
		switch {
		case hasPrefixFold(s, `\\?\UNC\`):
			s = `\\` + s[len(`\\?\UNC\`):]
		case hasPrefixFold(s, `\??\UNC\`):
			s = `\\` + s[len(`\??\UNC\`):]
		case strings.HasPrefix(s, `\\?\`):
			if s = s[len(`\\?\`):]; !isDriveAbs(s) {
				return "", nil, errors.New("unsupported verbatim path")
			}
		case strings.HasPrefix(s, `\??\`):
			if s = s[len(`\??\`):]; !isDriveAbs(s) {
				return "", nil, errors.New("unsupported NT path")
			}
		case strings.HasPrefix(s, `\\.\`):
			return "", nil, errors.New("device namespace paths are not allowed")
		}
		switch {
		case isDriveAbs(s):
			vol, rest = strings.ToUpper(s[:1])+":", s[2:]
		case strings.HasPrefix(s, `\\`):
			parts := strings.SplitN(s[2:], `\`, 3)
			if len(parts) < 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == "?" {
				return "", nil, errors.New("UNC path needs a server and a share")
			}
			vol, rest = `\\`+parts[0]+`\`+parts[1], ""
			if len(parts) == 3 {
				rest = `\` + parts[2]
			}
		default:
			return "", nil, errors.New("path is not absolute")
		}
	} else if path[0] != '/' {
		return "", nil, errors.New("path is not absolute")
	}
	for _, seg := range strings.Split(rest, sep) {
		switch seg {
		case "", ".":
		case "..":
			return "", nil, errors.New(`path contains a ".." segment`)
		default:
			segs = append(segs, seg)
		}
	}
	return vol, segs, nil
}

func isDriveAbs(s string) bool {
	if len(s) < 3 || s[1] != ':' || (s[2] != '\\' && s[2] != '/') {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// Within reports whether path lies inside root or is root itself. Both must be
// absolute; they are normalised first, and a path that cannot be normalised is
// never within anything. The comparison respects the flavour's case rules and
// works on whole segments, so C:\root2 is not under C:\root and c:\ROOT\x is
// under C:\root on Windows.
func (p Paths) Within(root, path string) bool {
	_, ok := p.Rel(root, path)
	return ok
}

// Rel returns path relative to root as a slash-separated key ("" for root
// itself) and true when path is inside root. It reports false for anything
// else, including inputs that cannot be normalised. Segments are compared one
// by one (folded on Windows), so the result never depends on how case folding
// changes the length of a string.
func (p Paths) Rel(root, path string) (string, bool) {
	rv, rsegs, err := p.parse(root)
	if err != nil {
		return "", false
	}
	qv, qsegs, err := p.parse(path)
	if err != nil {
		return "", false
	}
	if p.Fold(rv) != p.Fold(qv) || len(qsegs) < len(rsegs) {
		return "", false
	}
	for i := range rsegs {
		if p.Fold(rsegs[i]) != p.Fold(qsegs[i]) {
			return "", false
		}
	}
	return strings.Join(qsegs[len(rsegs):], "/"), true
}

// IsVolumeRoot reports whether an absolute path is a whole volume or share:
// "/" on POSIX, a drive root (C:\) or a UNC share root (\\srv\share) on
// Windows. A source declared by a package may not be one: AlixAI never crawls
// an entire machine or share (master plan 12.1).
func (p Paths) IsVolumeRoot(path string) bool {
	_, segs, err := p.parse(path)
	return err == nil && len(segs) == 0
}

// Join returns root joined with an already validated key in host spelling. It
// does no checking of its own; run ValidKey first.
func (p Paths) Join(root, key string) string {
	if key == "" {
		return root
	}
	if p.Windows {
		return strings.TrimRight(root, `\`) + `\` + strings.ReplaceAll(key, "/", `\`)
	}
	return strings.TrimRight(root, "/") + "/" + key
}

// reservedWindowsNames are the device names Windows resolves in every
// directory, with or without an extension.
var reservedWindowsNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true, "conin$": true, "conout$": true, "clock$": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"com\u00b9": true, "com\u00b2": true, "com\u00b3": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	"lpt\u00b9": true, "lpt\u00b2": true, "lpt\u00b3": true,
}

// ValidKey checks an item key: a relative, slash-separated path made of plain
// names. It returns an error wrapping ErrEscapesRoot for anything else.
//
// On every flavour a key is refused when it is empty or longer than 4096
// bytes, is not valid UTF-8, has a NUL or control character, a backslash, a
// leading or trailing slash, or an empty, "." or ".." segment. On Windows it is
// also refused when a segment has a colon (a drive letter or an alternate data
// stream such as a.txt::$DATA), ends in a dot or a space (Windows drops them,
// so "private./x" would open "private/x" past an exclude rule), contains
// <>"|?*, is longer than 255 characters, or is a reserved device name (CON,
// NUL, COM1, ... with or without an extension). Refusing a name here is what
// stops \\?\C:\x, \\srv\share\x, C:\x, ..\x and /etc/passwd from ever reaching
// the filesystem.
func (p Paths) ValidKey(key string) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: unsafe item name (%s)", ErrEscapesRoot, why)
	}
	switch {
	case key == "":
		return bad("empty")
	case len(key) > maxKeyBytes:
		return bad("too long")
	case !utf8.ValidString(key):
		return bad("not UTF-8")
	case strings.IndexFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return bad("control character")
	case strings.ContainsRune(key, '\\'):
		return bad("backslash")
	case key[0] == '/':
		return bad("absolute")
	case key == "." || !fs.ValidPath(key):
		return bad("not a clean relative path")
	}
	if !p.Windows {
		return nil
	}
	for _, seg := range strings.Split(key, "/") {
		switch {
		case strings.ContainsRune(seg, ':'):
			return bad("colon: drive letter or alternate data stream")
		case strings.ContainsAny(seg, `<>"|?*`):
			return bad("reserved character")
		case strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " "):
			return bad("trailing dot or space")
		case utf8.RuneCountInString(seg) > 255:
			return bad("name too long")
		}
		stem := seg
		if i := strings.IndexByte(stem, '.'); i >= 0 {
			stem = stem[:i]
		}
		if reservedWindowsNames[strings.ToLower(strings.TrimRight(stem, " "))] {
			return bad("reserved device name")
		}
	}
	return nil
}
