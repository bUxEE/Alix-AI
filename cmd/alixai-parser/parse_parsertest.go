//go:build parsertest

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// This file exists only in a binary built with -tags parsertest, which the
// tests in internal/rag/parse build into a temporary directory. It adds
// fault-injection "formats" so the tests can make a real worker hang, spin,
// crash, exhaust memory, and report what it can see and do, and watch the host
// and the OS deal with it. It is never part of a production build (a test checks
// that the production worker answers these formats with an error).
//
// Each fault is a Request.Format starting with "x-"; the body is its argument.

func parse(req wire.Request, body []byte) (*formats.Output, error) {
	switch f := string(req.Format); {
	case f == "x-hang": // blocks without using CPU
		time.Sleep(time.Hour)
	case f == "x-spin": // burns CPU
		for {
		}
	case f == "x-crash": // a panic in a goroutine cannot be recovered by the worker
		go func() { panic("injected crash") }()
		time.Sleep(time.Hour)
	case f == "x-exit":
		os.Exit(7)
	case f == "x-alloc": // allocates and touches memory until the OS or the guard stops it
		var keep [][]byte
		for {
			b := make([]byte, 8<<20)
			for i := 0; i < len(b); i += 4096 {
				b[i] = 1
			}
			keep = append(keep, b)
		}
	case f == "x-touch": // allocates and touches the number of bytes in the body, then returns
		n, err := strconv.ParseInt(string(body), 10, 64)
		if err != nil || n < 0 || n > 1<<34 {
			return nil, &formats.Error{Reason: wire.ReasonBadRequest, Message: "x-touch wants a byte count"}
		}
		keep := make([][]byte, 0, n>>20+1)
		for left := n; left > 0; {
			b := make([]byte, min(left, 1<<20))
			for i := 0; i < len(b); i += 4096 {
				b[i] = 1
			}
			keep = append(keep, b)
			left -= int64(len(b))
		}
		runtime.KeepAlive(keep)
		return text(fmt.Sprintf("touched %d", n)), nil
	case f == "x-env": // reports its environment, arguments, directory and pid
		wd, _ := os.Getwd()
		return text(fmt.Sprintf("env=%q\nargs=%q\nwd=%q\npid=%d\ngoos=%s", os.Environ(), os.Args, wd, os.Getpid(), runtime.GOOS)), nil
	case f == "x-net": // tries a TCP connection to the address in the body
		c, err := net.DialTimeout("tcp", string(body), 2*time.Second)
		if err != nil {
			return text("dial failed: " + err.Error()), nil
		}
		c.Close()
		return text("dial succeeded"), nil
	case f == "x-file": // tries to read the file named in the body
		b, err := os.ReadFile(string(body))
		if err != nil {
			return text("read failed: " + err.Error()), nil
		}
		return text(fmt.Sprintf("read succeeded: %d bytes", len(b))), nil
	case f == "x-write": // tries to create the file named in the body
		err := os.WriteFile(string(body), []byte("x"), 0o600)
		if err != nil {
			return text("write failed: " + err.Error()), nil
		}
		os.Remove(string(body))
		return text("write succeeded"), nil
	case f == "x-spawn": // tries to start the program named in the body
		out, err := exec.Command(string(body)).CombinedOutput()
		if err != nil {
			return text("spawn failed: " + err.Error()), nil
		}
		return text("spawn succeeded: " + strings.TrimSpace(string(out))), nil
	case strings.HasPrefix(f, "x-"):
		return nil, &formats.Error{Reason: wire.ReasonBadRequest, Message: "unknown fault"}
	}
	if !req.Format.Valid() {
		return nil, &formats.Error{Reason: wire.ReasonBadRequest, Message: "unsupported format"}
	}
	return formats.Parse(req.Format, body, req.Limits)
}

func text(s string) *formats.Output {
	return &formats.Output{
		Text:   []byte(s),
		Blocks: []wire.Block{{Kind: wire.KindParagraph, Start: 0, End: len(s)}},
	}
}
