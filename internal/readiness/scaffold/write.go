package scaffold

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ScaffoldWrite verifies the report's findings, plans the draft and writes it into OUTDIR, which must not exist. It writes nothing
// else anywhere. On a failure half way it leaves what it wrote (it never deletes anything) and says so.
func ScaffoldWrite(outDir string, in ScaffoldInput) (*ScaffoldResult, error) {
	// Refuse before any work is done.
	target, err := scaffoldCheckTarget(outDir, in.ScannedRoot)
	if err != nil {
		return nil, err
	}
	v, err := scaffoldVerify(in)
	if err != nil {
		return nil, err
	}
	plan := v.plan(in)
	files := scaffoldRender(plan)
	if err := scaffoldCreate(target, files); err != nil {
		return nil, err
	}
	res := &ScaffoldResult{OutDir: target, FindingsUsed: v.used, FindingsDropped: v.drop}
	for _, f := range files {
		res.Files = append(res.Files, f.Path)
	}
	if !plan.placeholder {
		for _, c := range plan.caps {
			res.Capabilities = append(res.Capabilities, c.ID)
		}
	}
	for _, r := range plan.rag {
		res.RAGSources = append(res.RAGSources, r.ID)
	}
	for _, c := range plan.candidates {
		res.Candidates = append(res.Candidates, c.Name)
	}
	return res, nil
}

// scaffoldCheckTarget resolves OUTDIR and refuses anything that already exists, whatever its type (a directory, a file, a symlink,
// a junction), and any place inside the scanned directory. It returns the absolute path to create.
func scaffoldCheckTarget(outDir, scannedRoot string) (string, error) {
	if strings.TrimSpace(outDir) == "" || strings.ContainsRune(outDir, 0) {
		return "", errors.New("readiness scaffold: the output directory is not a usable path")
	}
	abs, err := filepath.Abs(outDir)
	if err != nil {
		return "", fmt.Errorf("readiness scaffold: the output directory cannot be resolved: %w", err)
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err == nil {
		return "", fmt.Errorf("readiness scaffold: %s already exists; the scaffold never overwrites, name a directory that does not exist", abs)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("readiness scaffold: %s cannot be examined: %w", abs, err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("readiness scaffold: the parent of the output directory cannot be resolved: %w", err)
	}
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return "", errors.New("readiness scaffold: the parent of the output directory is not a directory")
	}
	base := filepath.Base(abs)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "", errors.New("readiness scaffold: the output directory is not a usable path")
	}
	target := filepath.Join(parent, base)
	if scannedRoot != "" {
		root, err := filepath.EvalSymlinks(scannedRoot)
		if err != nil {
			return "", fmt.Errorf("readiness scaffold: the scanned directory cannot be resolved: %w", err)
		}
		if scaffoldInside(root, target) {
			return "", errors.New("readiness scaffold: the output directory is inside the scanned directory; the scan never writes there")
		}
	}
	return target, nil
}

// scaffoldInside reports whether p is root or below it.
func scaffoldInside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// scaffoldSafeRel accepts only the fixed relative names this package generates.
func scaffoldSafeRel(p string) bool {
	if !scaffoldSafePath(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return false
		}
	}
	return true
}

// scaffoldCreate makes target exclusively, then creates every file exclusively through an os.Root so no name can leave it.
func scaffoldCreate(target string, files []scaffoldOut) error {
	for _, f := range files {
		if !scaffoldSafeRel(f.Path) {
			return fmt.Errorf("readiness scaffold: internal error: %q is not a safe file name", f.Path)
		}
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("readiness scaffold: the output directory cannot be created (it is never overwritten): %w", err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return fmt.Errorf("readiness scaffold: %s was created but cannot be opened; it is left as it is: %w", target, err)
	}
	defer root.Close()
	made := map[string]bool{}
	for _, f := range files {
		if dir := path.Dir(f.Path); dir != "." && !made[dir] {
			if err := root.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("readiness scaffold: writing stopped at %s (what was written is left in %s): %w", dir, target, err)
			}
			made[dir] = true
		}
		h, err := root.OpenFile(filepath.FromSlash(f.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("readiness scaffold: writing stopped at %s (what was written is left in %s): %w", f.Path, target, err)
		}
		_, werr := h.WriteString(f.Content)
		cerr := h.Close()
		if werr != nil || cerr != nil {
			return fmt.Errorf("readiness scaffold: writing stopped at %s (what was written is left in %s): %w", f.Path, target, errors.Join(werr, cerr))
		}
	}
	return nil
}

// ScaffoldCheck is the refusal ScaffoldWrite makes before it does any work: OUTDIR exists (whatever it is), its parent is missing, or it
// lies inside the scanned directory. The command line calls it first so a scan is not made for a draft that cannot be written.
func ScaffoldCheck(outDir, scannedRoot string) error {
	_, err := scaffoldCheckTarget(outDir, scannedRoot)
	return err
}
