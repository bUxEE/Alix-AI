//go:build !windows

package resources

import "golang.org/x/sys/unix"

// osFreeBytes is the space available to an unprivileged process on the volume
// that holds path (statfs f_bavail times f_bsize).
func osFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
