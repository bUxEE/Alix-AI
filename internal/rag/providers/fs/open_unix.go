//go:build unix

package fs

import "syscall"

// openFlags opens an item without blocking: a FIFO swapped in for the file
// that was checked would otherwise hold the open until a writer came. What
// opens is then refused anyway, because it is not the file that was checked.
const openFlags = syscall.O_RDONLY | syscall.O_NONBLOCK
