package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/store"
)

// hardeningDataDirOtherBits are the group and world permission bits. A data
// directory or database file with any of them set can be read (or changed) by
// other users of the machine (SEC-081).
const hardeningDataDirOtherBits fs.FileMode = 0o077

// hardeningDataDirFindings returns the warnings for an EXISTING data directory
// on a platform with POSIX modes: one per object (the directory, the database
// file) that other users can access.
//
// The store creates new directories 0700 and files 0600, but never tightens an
// existing one, so a directory made by hand (or by an older install) stays
// loose. serve warns instead of refusing: refusing would strand an upgrade over
// data that is still intact, and the warning names the one-line fix.
func hardeningDataDirFindings(dir string, dirInfo, dbInfo fs.FileInfo) []string {
	var out []string
	if dirInfo != nil && dirInfo.IsDir() && dirInfo.Mode().Perm()&hardeningDataDirOtherBits != 0 {
		out = append(out, fmt.Sprintf("%s is accessible by other users (mode %04o); chmod 700 it", dir, dirInfo.Mode().Perm()))
	}
	if dbInfo != nil && dbInfo.Mode().IsRegular() && dbInfo.Mode().Perm()&hardeningDataDirOtherBits != 0 {
		out = append(out, fmt.Sprintf("the database file in %s is accessible by other users (mode %04o); chmod 600 it", dir, dbInfo.Mode().Perm()))
	}
	return out
}

// hardeningWindowsFindings is the same check on Windows, where mode bits mean
// nothing (Go reports 0777 for every directory) and access is the ACL: the
// directory and the database file must be owner only as internal/fsperm defines
// it (an ACL whose entries, inherited or not, grant only the service identity,
// LocalSystem and Administrators). The store makes a new data directory that
// way; one made by hand or by an older install inherits whatever its parent
// grants (an agent found Authenticated Users with modify rights under E:\).
func hardeningWindowsFindings(dir string) []string {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil // missing (the store creates it owner only) or unreadable (the store open reports it)
	}
	var out []string
	if err := fsperm.Check(dir, true); errors.Is(err, fsperm.ErrNotOwnerOnly) {
		out = append(out, fmt.Sprintf("the data directory is not owner only (%v): restrict it to the service account "+
			"(icacls %s /inheritance:r /grant:r <service-account>:(OI)(CI)F)", err, dir))
	}
	db := filepath.Join(dir, store.DefaultFileName)
	if st, err := os.Stat(db); err == nil && st.Mode().IsRegular() {
		if err := fsperm.Check(db, false); errors.Is(err, fsperm.ErrNotOwnerOnly) {
			out = append(out, fmt.Sprintf("the database file in %s is not owner only (%v): restrict it to the service account "+
				"(icacls %s /inheritance:r /grant:r <service-account>:F)", dir, err, db))
		}
	}
	return out
}

// hardeningCheckDataDir logs the findings for dir. A directory that does not
// exist yet is not a finding: the store creates it owner-only.
func hardeningCheckDataDir(log *slog.Logger, dir string) {
	hardeningWarnDataDir(log, runtime.GOOS, dir)
}

func hardeningWarnDataDir(log *slog.Logger, goos, dir string) {
	if dir == "" {
		return
	}
	if goos == "windows" {
		for _, msg := range hardeningWindowsFindings(dir) {
			log.Warn(msg)
		}
		return
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return // missing (the store creates it 0700) or unreadable (the store open reports it)
	}
	dbInfo, _ := os.Stat(filepath.Join(dir, store.DefaultFileName))
	for _, msg := range hardeningDataDirFindings(dir, dirInfo, dbInfo) {
		log.Warn(msg)
	}
}
