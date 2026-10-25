//go:build linux

package confine

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Landlock, by raw system calls so that no dependency is needed and the
// semantics are the ones this file states. The ruleset handles every access
// right the kernel knows and allows none: after it is installed the process
// cannot open, create, remove or rename anything on the filesystem, and (ABI
// 4+) cannot bind or connect a TCP socket. Descriptors already open (the
// protocol pipes) keep working. It is installed on every thread of the process
// (syscall.AllThreadsSyscall6, which exists only with cgo disabled, as this
// build is): a Landlock domain is per thread and the Go runtime already has
// several.
const (
	sysLandlockCreateRuleset = 444
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1

	fsExecute    = 1 << 0
	fsWriteFile  = 1 << 1
	fsReadFile   = 1 << 2
	fsReadDir    = 1 << 3
	fsRemoveDir  = 1 << 4
	fsRemoveFile = 1 << 5
	fsMakeChar   = 1 << 6
	fsMakeDir    = 1 << 7
	fsMakeReg    = 1 << 8
	fsMakeSock   = 1 << 9
	fsMakeFifo   = 1 << 10
	fsMakeBlock  = 1 << 11
	fsMakeSym    = 1 << 12
	fsRefer      = 1 << 13 // ABI 2
	fsTruncate   = 1 << 14 // ABI 3
	fsIoctlDev   = 1 << 15 // ABI 5

	netBindTCP    = 1 << 0 // ABI 4
	netConnectTCP = 1 << 1

	scopeAbstractUnix = 1 << 0 // ABI 6
	scopeSignal       = 1 << 1
)

type landlockAttr struct {
	handledFS  uint64
	handledNet uint64
	scoped     uint64
}

type landlockResult struct {
	abi int
	net bool
}

func applyLandlock() (landlockResult, error) {
	var res landlockResult
	v, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return res, fmt.Errorf("landlock is not available (%v)", errno)
	}
	res.abi = int(v)
	attr := landlockAttr{
		handledFS: fsExecute | fsWriteFile | fsReadFile | fsReadDir | fsRemoveDir | fsRemoveFile | fsMakeChar |
			fsMakeDir | fsMakeReg | fsMakeSock | fsMakeFifo | fsMakeBlock | fsMakeSym,
	}
	size := 8 // ABI 1 to 3: the filesystem field only
	if res.abi >= 2 {
		attr.handledFS |= fsRefer
	}
	if res.abi >= 3 {
		attr.handledFS |= fsTruncate
	}
	if res.abi >= 4 {
		attr.handledNet = netBindTCP | netConnectTCP
		res.net = true
		size = 16
	}
	if res.abi >= 5 {
		attr.handledFS |= fsIoctlDev
	}
	if res.abi >= 6 {
		attr.scoped = scopeAbstractUnix | scopeSignal
		size = 24
	}
	fd, _, errno := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), uintptr(size), 0)
	if errno != 0 {
		return res, fmt.Errorf("landlock_create_ruleset: %v", errno)
	}
	defer syscall.Close(int(fd))
	if _, _, errno := syscall.AllThreadsSyscall6(sysLandlockRestrictSelf, fd, 0, 0, 0, 0, 0); errno != 0 {
		return res, fmt.Errorf("landlock_restrict_self: %v", errno)
	}
	return res, nil
}
