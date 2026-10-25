//go:build windows

package cli

import "golang.org/x/sys/windows"

// ragFreeBytes is the space available to this process on the volume that holds
// dir (GetDiskFreeSpaceEx's "free bytes available to the caller", which honours
// disk quotas), for the production disk-pressure hook.
func ragFreeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
