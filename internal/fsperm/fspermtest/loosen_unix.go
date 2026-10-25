//go:build !windows

package fspermtest

import "os"

func loosen(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st.IsDir() {
		mode = 0o755
	}
	return os.Chmod(path, mode)
}
