package fs

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"path"
	"strings"
	"syscall"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// keyKind is what a key names, found without following anything.
type keyKind int

const (
	kindMissing   keyKind = iota // nothing there, or a file where a directory is needed
	kindRegular                  // a regular file
	kindDir                      // a directory
	kindSymlink                  // a symbolic link, at the key or on the way to it
	kindIrregular                // a junction or other reparse point, device, pipe or socket
)

// lstatKey inspects every component of key below the root without following
// any of them, through the os.Root so the kernel confines each step. It
// stops at the first symbolic link or irregular entry, so nothing beyond a
// link is ever touched. key must already have passed Paths.ValidKey.
//
// at is the path of the entry the answer is about: the key itself for a
// regular file or directory, the first link or irregular component otherwise.
//
// kindMissing is reported only when the lookup itself said "does not exist"
// (or a file stands where a directory is needed); any other failure is an
// error, never a "missing".
func (h *rootHandle) lstatKey(key string) (info iofs.FileInfo, kind keyKind, at string, err error) {
	parts := strings.Split(key, "/")
	for i := range parts {
		name := strings.Join(parts[:i+1], "/")
		fi, err := h.r.Lstat(name)
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				return nil, kindMissing, "", nil
			}
			return nil, kindMissing, "", itemError(key, err)
		}
		mode, last := fi.Mode(), i == len(parts)-1
		switch {
		case mode&iofs.ModeSymlink != 0:
			return fi, kindSymlink, name, nil
		case mode&iofs.ModeIrregular != 0:
			return fi, kindIrregular, name, nil
		case !last && !mode.IsDir():
			return nil, kindMissing, "", nil
		case last && mode.IsDir():
			return fi, kindDir, name, nil
		case last && !mode.IsRegular():
			return fi, kindIrregular, name, nil
		case last:
			return fi, kindRegular, name, nil
		}
	}
	return nil, kindMissing, "", nil
}

// itemError maps a failure to read one item to a typed error that carries the
// key and not the host path.
func itemError(key string, err error) error {
	switch {
	case strings.Contains(err.Error(), "path escapes"):
		return fmt.Errorf("%w: %q", wrapEscape(err), key)
	case errors.Is(err, iofs.ErrNotExist):
		return fmt.Errorf("%w: %q", source.ErrNotFound, key)
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, fmt.Sprintf("item %q cannot be read", key))
}

func wrapEscape(cause error) error {
	return contracts.Wrap(contracts.ErrScopeViolation, cause, source.ErrEscapesRoot.Message)
}

// refuse is the error for a key that names a link or other irregular entry.
func refuse(key string, kind keyKind) error {
	what := "a symbolic link"
	if kind == kindIrregular {
		what = "a junction, reparse point, device, pipe or socket"
	}
	return fmt.Errorf("%w: %q is or passes through %s, which is never followed", source.ErrEscapesRoot, key, what)
}

// classifyLink says why a symbolic link at linkKey is not read: ReasonSymlink
// when its target stays inside the root (links are still not followed), and
// ReasonLinkEscape when the target leaves the root or cannot be understood.
// It works on the target string alone, so it never touches anything outside
// the root, and it is pure: the Windows forms are tested on every platform.
func classifyLink(paths source.Paths, canon, linkKey, target string) source.Reason {
	if paths.Windows {
		target = strings.ReplaceAll(target, `\`, "/")
	}
	if target == "" || strings.ContainsRune(target, 0) {
		return source.ReasonLinkEscape
	}
	if rooted(paths, target) {
		if _, ok := paths.Rel(canon, target); ok {
			return source.ReasonSymlink
		}
		return source.ReasonLinkEscape
	}
	joined := path.Join(path.Dir(linkKey), target)
	if joined == ".." || strings.HasPrefix(joined, "../") || path.IsAbs(joined) {
		return source.ReasonLinkEscape
	}
	return source.ReasonSymlink
}

// rooted reports whether a link target is an absolute spelling on this flavour
// (a leading slash always counts: on Windows a rooted path escapes to the
// current drive).
func rooted(paths source.Paths, target string) bool {
	if strings.HasPrefix(target, "/") {
		return true
	}
	return paths.Windows && len(target) >= 2 && target[1] == ':'
}

// linkReason reads a link's target through the root (no following) and
// classifies it. A link that cannot be read is an escape: it is not trusted.
func (h *rootHandle) linkReason(paths source.Paths, key string) source.Reason {
	target, err := h.r.Readlink(key)
	if err != nil {
		return source.ReasonLinkEscape
	}
	return classifyLink(paths, h.canon, key, target)
}

// canonicalKey is the defence in depth for a key about to be statted or
// opened: the path's canonical form (EvalSymlinks) must lie inside the root by
// a separator-aware, case-folding prefix check, and must spell the same name
// as the key (case aside on Windows). An alias, a link the component walk
// somehow missed, or a Windows 8.3 short name fails here. It returns the key
// in its on-disk spelling.
func (p *Provider) canonicalKey(h *rootHandle, key string) (string, error) {
	resolved, err := p.evalSymlinks(p.paths.Join(h.canon, key))
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", fmt.Errorf("%w: %q", source.ErrNotFound, key)
		}
		return "", contracts.Wrap(contracts.ErrUnavailable, err, fmt.Sprintf("item %q cannot be resolved", key))
	}
	rel, ok := p.paths.Rel(h.canon, resolved)
	if !ok {
		return "", fmt.Errorf("%w: %q resolves outside the root", source.ErrEscapesRoot, key)
	}
	if p.paths.Fold(rel) != p.paths.Fold(key) {
		return "", fmt.Errorf("%w: %q is not the canonical name of the item", source.ErrEscapesRoot, key)
	}
	return rel, nil
}
