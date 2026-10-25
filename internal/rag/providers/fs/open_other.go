//go:build !unix

package fs

import "os"

// openFlags opens an item for reading (see open_unix.go).
const openFlags = os.O_RDONLY
