// Command alixai-parser is the sandboxed document parser: a separate,
// disposable process that turns one untrusted document into extracted text and
// structure and nothing else. It is started by the AlixAI core (internal/rag/
// parse), never by a person, and speaks the framed protocol of
// internal/rag/parse/wire on stdin and stdout.
//
// Properties, each checked by a test in internal/rag/parse:
//
//   - It has no network code: neither it nor anything it imports links net,
//     net/http, crypto/tls or os/exec (TestParserHasNoNetwork). On Linux the
//     kernel also refuses sockets (seccomp) and TCP (Landlock).
//   - It takes no arguments and reads no environment; the host starts it with
//     an empty environment, so it inherits no secret. Its working directory is
//     an empty directory the host made for it.
//   - It confines itself, and reports what it did in its Hello, before it reads
//     a document (internal/rag/parse/confine). It parses nothing until the host
//     has verified that report.
//   - It never writes to stdout except protocol frames, and never to a file.
//   - It exits on its own at the wall-time limit and, where the OS has no memory
//     limit, when its memory passes the budget.
//
// Exit codes: 0 the host closed stdin; 2 a Go crash or panic; 3 the wall-time
// limit; 4 the memory budget; 5 a protocol or I/O error; 6 a mandatory
// confinement could not be installed (the worker did not parse anything).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/confine"
	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

const (
	exitDeadline = 3
	exitMemory   = 4
	exitProtocol = 5
	exitConfine  = 6

	// graceAfterWall is how long past the wall limit the worker waits for the
	// host to kill it before it exits by itself.
	graceAfterWall = 2 * time.Second
)

func main() {
	os.Exit(serve(os.Stdin, os.Stdout))
}

func serve(in io.Reader, out io.Writer) int {
	r := bufio.NewReaderSize(in, 64<<10)
	w := bufio.NewWriterSize(out, 64<<10)

	t, payload, err := wire.ReadFrame(r, 1<<16)
	if err != nil || t != wire.FrameInit {
		return exitProtocol
	}
	var init wire.Init
	if json.Unmarshal(payload, &init) != nil || init.Protocol != wire.ProtocolVersion {
		return exitProtocol
	}

	threads := init.Threads
	if threads < 1 || threads > 8 {
		threads = 1
	}
	runtime.GOMAXPROCS(threads)
	if init.MemoryBytes > 0 {
		// A soft limit: the collector works harder as the heap nears it. The
		// hard limits are the OS's and the guard's.
		debug.SetMemoryLimit(init.MemoryBytes / 10 * 8)
	}

	sb, err := confine.Apply(confine.Options{MemoryBytes: init.MemoryBytes, CPUMillis: init.CPUMillis})
	if err != nil {
		fmt.Fprintln(os.Stderr, "alixai-parser: confinement failed:", err)
		return exitConfine
	}
	for _, need := range wire.RequiredMechanisms(runtime.GOOS) {
		if !contains(sb.Applied, need) {
			fmt.Fprintln(os.Stderr, "alixai-parser: required confinement missing:", need)
			return exitConfine
		}
	}
	if init.SelfGuard {
		confine.StartMemoryGuard(init.MemoryBytes, func() { os.Exit(exitMemory) })
	}

	hello := wire.Hello{
		Protocol:  wire.ProtocolVersion,
		Component: formats.Component(),
		GoOS:      runtime.GOOS,
		GoArch:    runtime.GOARCH,
		Sandbox:   sb,
	}
	if err := writeJSON(w, wire.FrameHello, hello); err != nil {
		return exitProtocol
	}

	wall := time.Duration(init.WallMillis) * time.Millisecond
	for {
		t, payload, err := wire.ReadFrame(r, 1<<20)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0 // the host closed stdin: it has what it needs
			}
			return exitProtocol
		}
		if t != wire.FrameRequest {
			return exitProtocol
		}
		var req wire.Request
		if json.Unmarshal(payload, &req) != nil {
			return exitProtocol
		}
		var guard *time.Timer
		if wall > 0 {
			guard = time.AfterFunc(wall+graceAfterWall, func() { os.Exit(exitDeadline) })
		}
		code := handle(r, w, req)
		if guard != nil {
			guard.Stop()
		}
		if code != 0 {
			return code
		}
	}
}

// handle serves one Request/Body pair. A returned non-zero code ends the
// process; a failed parse is a Failure frame and the worker lives on.
func handle(r *bufio.Reader, w *bufio.Writer, req wire.Request) int {
	t, n, err := wire.ReadHeader(r, max(req.Limits.MaxInputBytes, 0))
	if err != nil || t != wire.FrameBody || n != req.Size {
		return exitProtocol
	}
	body, err := wire.ReadPayload(r, n)
	if err != nil {
		return exitProtocol
	}
	if req.Protocol != wire.ProtocolVersion || !validLimits(req.Limits) {
		return fail(w, wire.ReasonBadRequest, "the request is malformed")
	}
	start := time.Now()
	out, err := parse(req, body)
	if err != nil {
		var fe *formats.Error
		if errors.As(err, &fe) {
			return fail(w, fe.Reason, fe.Message)
		}
		return fail(w, wire.ReasonInternal, "the parser failed")
	}
	res := wire.Result{
		Protocol:   wire.ProtocolVersion,
		Component:  formats.Component(),
		Format:     req.Format,
		TextSize:   int64(len(out.Text)),
		PageStarts: out.PageStarts,
		Blocks:     out.Blocks,
		Warnings:   out.Warnings,
		NeedsOCR:   out.NeedsOCR,
		Stats: wire.Stats{
			InputBytes:    int64(len(body)),
			OutputBytes:   int64(len(out.Text)),
			ExpandedBytes: out.Expanded,
			Pages:         out.Pages,
			Blocks:        len(out.Blocks),
			ParseNanos:    time.Since(start).Nanoseconds(),
		},
	}
	if res.Blocks == nil {
		res.Blocks = []wire.Block{}
	}
	for _, b := range out.Blocks {
		if b.Hidden {
			res.Stats.HiddenBlocks++
		}
	}
	// A self-check: the same validation the host and the chunker apply.
	if err := res.Validate(out.Text, req.Limits.MaxBlocks); err != nil {
		return fail(w, wire.ReasonInternal, "the parser produced an inconsistent result")
	}
	if err := writeJSON(w, wire.FrameResult, res); err != nil {
		return exitProtocol
	}
	if err := wire.WriteFrame(w, wire.FrameText, out.Text); err != nil || w.Flush() != nil {
		return exitProtocol
	}
	return 0
}

func validLimits(l wire.Limits) bool {
	return l.MaxInputBytes > 0 && l.MaxOutputBytes > 0 && l.MaxExpandedBytes > 0 && l.MaxExpansionRatio > 0 &&
		l.MaxEntries > 0 && l.MaxDepth > 0 && l.MaxDepth <= 1000 && l.MaxPages > 0 && l.MaxBlocks > 0
}

func fail(w *bufio.Writer, reason, message string) int {
	if err := writeJSON(w, wire.FrameError, wire.Failure{Reason: reason, Message: message}); err != nil {
		return exitProtocol
	}
	return 0
}

func writeJSON(w *bufio.Writer, t wire.FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := wire.WriteFrame(w, t, b); err != nil {
		return err
	}
	return w.Flush()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
