package parse

import (
	"bufio"
	"encoding/json"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// hostileMain is a stand-in worker that does what a compromised or buggy worker
// might: it answers the handshake correctly (so the host proceeds) and then
// misbehaves in the way the mode names. The host must treat every byte from a
// worker as untrusted.
func hostileMain(mode string) int {
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	send := func(t wire.FrameType, v any) {
		b, _ := json.Marshal(v)
		wire.WriteFrame(out, t, b)
		out.Flush()
	}
	if mode == "silent" {
		time.Sleep(time.Hour)
		return 0
	}
	_, payload, err := wire.ReadFrame(in, 1<<16)
	if err != nil {
		return 5
	}
	var init wire.Init
	json.Unmarshal(payload, &init)

	hello := wire.Hello{
		Protocol:  wire.ProtocolVersion,
		Component: formats.Component(),
		GoOS:      runtime.GOOS,
		GoArch:    runtime.GOARCH,
		Sandbox:   wire.Sandbox{Applied: wire.RequiredMechanisms(runtime.GOOS), Observed: honestObserved(init)},
	}
	switch mode {
	case "badversion":
		hello.Component.Version = "9.9.9+deadbeef"
	case "wrongparser":
		hello.Component.Name = "evil"
	case "nomech":
		hello.Sandbox.Applied = nil
	case "wrongobs":
		for k, v := range hello.Sandbox.Observed {
			hello.Sandbox.Observed[k] = v + 1
		}
	case "wrongas": // the address-space limit is not what the arithmetic says
		hello.Sandbox.Observed[wire.RlimitAS] += 1 << 30
	case "wrongos":
		hello.GoOS = "plan9"
	case "badprotocol":
		hello.Protocol = 99
	}
	send(wire.FrameHello, hello)
	switch mode {
	case "badversion", "wrongparser", "nomech", "wrongobs", "wrongas", "wrongos", "badprotocol":
		time.Sleep(time.Hour) // the host must not wait for this
		return 0
	}

	// read the request and the body
	if _, _, err := wire.ReadFrame(in, 1<<20); err != nil {
		return 5
	}
	if _, _, err := wire.ReadFrame(in, 1<<30); err != nil {
		return 5
	}
	good := wire.Result{Protocol: wire.ProtocolVersion, Component: formats.Component(), Format: wire.FormatTXT, TextSize: 5,
		Blocks: []wire.Block{{Kind: wire.KindParagraph, Start: 0, End: 5}}}
	switch mode {
	case "hugeframe": // declares 3 GiB
		out.Write([]byte{byte(wire.FrameResult), 0xC0, 0, 0, 0})
		out.Flush()
	case "outoforder":
		wire.WriteFrame(out, wire.FrameText, []byte("hello"))
		out.Flush()
	case "badjson":
		wire.WriteFrame(out, wire.FrameResult, []byte("{not json"))
		out.Flush()
	case "spanspastend":
		bad := good
		bad.Blocks = []wire.Block{{Kind: wire.KindParagraph, Start: 0, End: 500}}
		send(wire.FrameResult, bad)
		wire.WriteFrame(out, wire.FrameText, []byte("hello"))
		out.Flush()
	case "badutf8":
		wire.WriteFrame(out, wire.FrameResult, mustJSON(good))
		wire.WriteFrame(out, wire.FrameText, []byte{0xff, 0xfe, 0xfd, 'a', 'b'})
		out.Flush()
	case "splitrune":
		bad := good
		bad.TextSize = 6
		bad.Blocks = []wire.Block{{Kind: wire.KindParagraph, Start: 0, End: 2}}
		wire.WriteFrame(out, wire.FrameResult, mustJSON(bad))
		wire.WriteFrame(out, wire.FrameText, []byte("é€")[:3]) // 'é' then half of '€'... valid UTF-8 up to a rune boundary
		out.Flush()
	case "bighidden":
		bad := good
		bad.Blocks = []wire.Block{{Kind: wire.KindParagraph, Start: 0, End: 5, Hidden: true}} // hidden without a reason
		wire.WriteFrame(out, wire.FrameResult, mustJSON(bad))
		wire.WriteFrame(out, wire.FrameText, []byte("hello"))
		out.Flush()
	case "textmismatch":
		wire.WriteFrame(out, wire.FrameResult, mustJSON(good))
		wire.WriteFrame(out, wire.FrameText, []byte("hello world")) // declared 5
		out.Flush()
	case "truncated":
		wire.WriteFrame(out, wire.FrameResult, mustJSON(good))
		out.Write([]byte{byte(wire.FrameText), 0, 0, 0, 5, 'h', 'e'}) // dies mid-frame
		out.Flush()
		return 0
	case "wrongparserresult":
		bad := good
		bad.Component.Version = "9.9.9"
		wire.WriteFrame(out, wire.FrameResult, mustJSON(bad))
		wire.WriteFrame(out, wire.FrameText, []byte("hello"))
		out.Flush()
	case "textover":
		bad := good
		bad.TextSize = 1 << 40
		wire.WriteFrame(out, wire.FrameResult, mustJSON(bad))
		out.Flush()
	case "unknownfailure":
		send(wire.FrameError, wire.Failure{Reason: "something_new", Message: "x"})
		time.Sleep(time.Hour)
	case "valid": // control: a correct answer is accepted
		wire.WriteFrame(out, wire.FrameResult, mustJSON(good))
		wire.WriteFrame(out, wire.FrameText, []byte("hello"))
		out.Flush()
		return 0
	}
	time.Sleep(time.Hour)
	return 0
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// honestObserved is the Observed map a worker would report for these limits.
func honestObserved(init wire.Init) map[string]int64 {
	switch runtime.GOOS {
	case "windows":
		return map[string]int64{"job_process_memory_bytes": init.MemoryBytes, "job_active_process_limit": 1}
	}
	cpu := (init.CPUMillis + 999) / 1000
	obs := map[string]int64{wire.RlimitCPU: cpu, wire.RlimitFsize: 0}
	if runtime.GOOS == "linux" {
		const baseline = 1200 << 20
		obs[wire.VMBaseline] = baseline
		obs[wire.RlimitAS] = baseline + init.MemoryBytes + wire.ASSlack
	}
	return obs
}

// The host treats everything from the worker as untrusted: a worker that lies in
// its handshake is never sent a document, and a worker that sends a malformed or
// inconsistent result is refused and killed.
func TestHostRejectsHostileWorker(t *testing.T) {
	cases := []struct {
		mode   string
		code   contracts.ErrCode
		reason string
	}{
		{"badversion", contracts.ErrUnavailable, "sandbox_unavailable"},
		{"wrongparser", contracts.ErrUnavailable, "sandbox_unavailable"},
		{"badprotocol", contracts.ErrUnavailable, "sandbox_unavailable"},
		{"nomech", contracts.ErrUnavailable, "sandbox_unavailable"},
		{"wrongobs", contracts.ErrUnavailable, "sandbox_unavailable"},
		{"wrongos", contracts.ErrUnavailable, "sandbox_unavailable"},
		{"hugeframe", contracts.ErrInternal, "protocol"},
		{"outoforder", contracts.ErrInternal, "protocol"},
		{"badjson", contracts.ErrInternal, "protocol"},
		{"spanspastend", contracts.ErrInternal, "protocol"},
		{"badutf8", contracts.ErrInternal, "protocol"},
		{"bighidden", contracts.ErrInternal, "protocol"},
		{"textmismatch", contracts.ErrInternal, "protocol"},
		{"wrongparserresult", contracts.ErrInternal, "protocol"},
		{"textover", contracts.ErrInternal, "protocol"},
		{"truncated", contracts.ErrInternal, "worker_crashed"},
		{"unknownfailure", contracts.ErrInternal, "protocol"},
	}
	if runtime.GOOS == "linux" { // only Linux has an address-space limit to check
		cases = append(cases, struct {
			mode   string
			code   contracts.ErrCode
			reason string
		}{"wrongas", contracts.ErrUnavailable, "sandbox_unavailable"})
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			pids := &pidRecorder{}
			cfg := testConfig(testExecutable(t))
			cfg.StartupTimeout = 30 * time.Second
			cfg.WallTimeout = 20 * time.Second
			r, err := NewRunner(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.args = []string{"-hostile=" + c.mode}
			r.onStart = pids.add
			start := time.Now()
			_, err = r.Parse(ctx(t), wire.FormatTXT, []byte("hello"))
			if code(err) != c.code || reasonOf(err) != c.reason {
				t.Fatalf("err = %v (code %s reason %q), want %s/%s", err, code(err), reasonOf(err), c.code, c.reason)
			}
			if el := time.Since(start); el > 15*time.Second {
				t.Errorf("took %s: the host waited on a hostile worker", el)
			}
			waitGone(t, pids.last(t))
		})
	}

	t.Run("a correct answer from the stand-in is accepted", func(t *testing.T) {
		r, err := NewRunner(testConfig(testExecutable(t)))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.args = []string{"-hostile=valid"}
		res, err := r.Parse(ctx(t), wire.FormatTXT, []byte("hello"))
		if err != nil || res.Text != "hello" {
			t.Fatalf("control case: %v %+v", err, res)
		}
	})
}

// A stale worker next to a newer service is refused at the handshake.
func TestWorkerVersionRecordedAndChecked(t *testing.T) {
	r := newTestRunner(t)
	res := mustParse(t, r, wire.FormatTXT, []byte("hello"))
	if res.Parser != Component() {
		t.Errorf("parser = %+v, want %+v", res.Parser, Component())
	}
	if res.Parser.Name != "parser" || res.Parser.Version == "" {
		t.Errorf("component = %+v", res.Parser)
	}
	// every result carries it; a worker reporting another version is refused
	// (hostile_test: badversion, wrongparserresult)
}

var _ = contracts.ErrInternal
