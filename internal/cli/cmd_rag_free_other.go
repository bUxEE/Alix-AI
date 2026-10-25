//go:build !windows

package cli

import "golang.org/x/sys/unix"

// ragFreeBytes is the space available to an unprivileged process on the volume
// that holds dir (statfs f_bavail times f_bsize), for the production
// disk-pressure hook.
func ragFreeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
