//go:build !linux

package main

import "errors"

const hostProcSupported = false

func readProcStatus(int) (string, error) {
	return "", errors.New("reading /proc is supported on Linux only")
}
