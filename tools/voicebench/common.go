package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// Case and clip statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Memory sources of the bench envelope.
const (
	MemSourceDocker = "docker-stats"
	MemSourceNone   = "none"
)

// clipRunes cuts s to at most n runes.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sha256File(path string) (string, int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	return sha256Hex(b), int64(len(b)), nil
}
