//go:build windows

package resources

import "golang.org/x/sys/windows"

// osFreeBytes is the space available to this process on the volume that holds
// path (GetDiskFreeSpaceEx's "free bytes available to the caller", which
// honours disk quotas).
func osFreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
