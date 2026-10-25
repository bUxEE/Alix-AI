package native

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func readItem(a adapters.AppAdapter, ctx context.Context) error {
	_, err := a.Read(ctx, testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "item_1"}})
	return err
}

func errReason(err error) string { return reasonOf(err) }

func code(err error) contracts.ErrCode {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		return ce.Code
	}
	return ""
}

// impostor is a process at the bridge address that does not know the application
// key. It answers the adapter's hello as a bridge would, with a proof it made up,
// and records every frame the adapter sends it.
type impostor struct {
	l      net.Listener
	mu     sync.Mutex
	frames []string // the methods received, in order
	after  int      // bytes received after the hello was answered
}

func startImpostor(t *testing.T, network string, mode string) (*impostor, string) {
	t.Helper()
	addr := wireAddr(t, network)
	l, err := listen(t, network, addr)
	if err != nil {
		t.Fatal(err)
	}
	im := &impostor{l: l}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(3 * time.Second))
				b, err := readFrame(c, 1<<20)
				if err != nil {
					return
				}
				var r struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				_ = json.Unmarshal(b, &r)
				im.mu.Lock()
				im.frames = append(im.frames, r.Method)
				im.mu.Unlock()
				sn := make([]byte, nonceLen)
				_, _ = rand.Read(sn)
				made := make([]byte, 32) // a proof of nothing
				_, _ = rand.Read(made)
				reply := `{"jsonrpc":"2.0","id":1,"result":{"version":1,"nonce":"` + b64.EncodeToString(sn) + `","proof":"` + b64.EncodeToString(made) + `"}}`
				if mode == "error" {
					reply = `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"authentication failed"}}`
				}
				_ = rawFrame(c, []byte(reply))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					im.mu.Lock()
					im.after += n
					im.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	if network == NetworkUnix {
		return im, addr
	}
	return im, l.Addr().String()
}

// SEC-038: a local process that is not the application cannot impersonate it, and
// a process that is not AlixAI cannot call it. Both ends prove they hold the
// application key before any identity, field or prepared action is sent or any
// operation is served; a bridge that cannot prove it is refused with nothing sent,
// also for a Commit; and no key, proof or nonce reaches a log or an error.
func TestPeerAuthRequired(t *testing.T) {
	for _, network := range []string{NetworkUnix, NetworkTCP} {
		t.Run(network, func(t *testing.T) {
			t.Run("a bridge with another key is refused and sees nothing", func(t *testing.T) {
				logs := &logBuffer{}
				b := startBridge(t, network)
				cfg := fixtureConfig(t, network, b.addr)
				cfg.Endpoints[0].Key = NewSecret(bytes.Repeat([]byte{7}, 48))
				cfg.Logger = logs.logger()
				w := wrapped(t, newAdapter(t, cfg))
				b.app.Seed("ten_a", "wks_a", "item_1", "Widget")

				err := readItem(w, context.Background())
				if code(err) != contracts.ErrUnavailable || errReason(err) != ReasonPeerAuthFailed {
					t.Fatalf("read against a bridge with another key: %v, want unavailable (%s)", err, ReasonPeerAuthFailed)
				}
				_, err = w.Commit(context.Background(), testScope(), validCommit(t))
				if code(err) != contracts.ErrUnavailable || errReason(err) != ReasonPeerAuthFailed {
					t.Errorf("a commit that was never sent is %v, want unavailable (%s), not uncertain", err, ReasonPeerAuthFailed)
				}
				if b.handled.Load() != 0 || len(b.app.Calls()) != 0 {
					t.Errorf("a handler ran for a peer that did not authenticate")
				}
				if out := logs.String(); !strings.Contains(out, "did not authenticate") || strings.Contains(out, testSecret) {
					t.Errorf("the refusal is logged without the key: %q", out)
				}
			})

			for _, mode := range []string{"proof", "error"} {
				t.Run("an impostor ("+mode+") receives nothing of the call", func(t *testing.T) {
					im, addr := startImpostor(t, network, mode)
					w := wrapped(t, newAdapter(t, fixtureConfig(t, network, addr)))
					err := readItem(w, context.Background())
					if code(err) != contracts.ErrUnavailable || errReason(err) != ReasonPeerAuthFailed {
						t.Fatalf("read against an impostor: %v, want unavailable (%s)", err, ReasonPeerAuthFailed)
					}
					time.Sleep(50 * time.Millisecond)
					im.mu.Lock()
					defer im.mu.Unlock()
					if len(im.frames) != 1 || im.frames[0] != methodHello {
						t.Errorf("the impostor received %v, want the hello alone", im.frames)
					}
					if im.after != 0 {
						t.Errorf("the adapter sent %d bytes after a proof that did not verify: identity, fields or a proof of its own", im.after)
					}
				})
			}

			t.Run("a peer that is not AlixAI is refused before anything is served", func(t *testing.T) {
				b := startBridge(t, network)
				c := b.rawConn(t)
				params, _ := json.Marshal(map[string]any{"identity": map[string]any{
					"application_id": testApp, "tenant_id": "ten_a", "workspace_id": "wks_a", "principal_id": "usr_alice", "roles": []string{"member"}, "session_id": "ses_alice",
				}})
				req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "items_get", "params": json.RawMessage(params)})
				if err := rawFrame(c, req); err != nil {
					t.Fatal(err)
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				body, err := readFrame(c, 1<<20)
				if err != nil || !strings.Contains(string(body), `"code":-32001`) {
					t.Fatalf("a call with no handshake got %q, %v; want an authentication failure", body, err)
				}
				if b.handled.Load() != 0 {
					t.Error("a handler ran for a peer that did not authenticate")
				}
			})
		})
	}

	t.Run("proofs are bound to their role and transcript", func(t *testing.T) {
		cn, sn := bytes.Repeat([]byte{1}, nonceLen), bytes.Repeat([]byte{2}, nonceLen)
		tr := transcript(testApp, testEP, testKeyID, cn, sn)
		server, client := proof(testKey, labelServer, tr...), proof(testKey, labelClient, tr...)
		if bytes.Equal(server, client) {
			t.Fatal("the proof of one role verifies as the other: a bridge could reflect the adapter's own proof")
		}
		for name, other := range map[string][][]byte{
			"another application":  transcript("app_other", testEP, testKeyID, cn, sn),
			"another endpoint":     transcript(testApp, "other-bridge", testKeyID, cn, sn),
			"another key id":       transcript(testApp, testEP, "k2", cn, sn),
			"another client nonce": transcript(testApp, testEP, testKeyID, sn, sn),
			"another server nonce": transcript(testApp, testEP, testKeyID, cn, cn),
		} {
			if bytes.Equal(server, proof(testKey, labelServer, other...)) {
				t.Errorf("a proof carries over to %s", name)
			}
		}
		if bytes.Equal(server, proof(bytes.Repeat([]byte{9}, 40), labelServer, tr...)) {
			t.Error("a proof does not depend on the key")
		}
	})

	t.Run("the operating system's account of the bridge is honoured where it exists", func(t *testing.T) {
		b := startBridge(t, NetworkUnix)
		b.app.Seed("ten_a", "wks_a", "item_1", "Widget")
		cfg := fixtureConfig(t, NetworkUnix, b.addr)
		var asked bool
		cfg.Endpoints[0].PeerCheck = func(PeerCreds) error { asked = true; return errors.New("not the user the operator named") }
		w := wrapped(t, newAdapter(t, cfg))
		err := readItem(w, context.Background())
		if runtime.GOOS == "linux" {
			if !asked || code(err) != contracts.ErrUnavailable || errReason(err) != ReasonPeerRefused || b.handled.Load() != 0 {
				t.Errorf("peer credentials: asked=%v, %v, %d calls handled; want a refusal before anything is sent", asked, err, b.handled.Load())
			}
			return
		}
		// Where the system gives none, the key is the control and the call works.
		if asked || err != nil {
			t.Errorf("without peer credentials: asked=%v, %v; want the key to be enough", asked, err)
		}
	})
}

// SEC-038: frames above the limit are refused. A length is checked before any of
// the body is read, in both directions and in the handshake; a frame at the limit
// is accepted and one byte over is not; and an oversize request is refused before
// anything is sent.
func TestFramingLimits(t *testing.T) {
	t.Run("readFrame", func(t *testing.T) {
		frame := func(n uint32, body []byte) *bytes.Reader {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], n)
			return bytes.NewReader(append(hdr[:], body...))
		}
		if b, err := readFrame(frame(3, []byte("abc")), 3); err != nil || string(b) != "abc" {
			t.Errorf("a frame at the limit: %q, %v", b, err)
		}
		if _, err := readFrame(frame(4, []byte("abcd")), 3); !errors.Is(err, errFrameTooLarge) {
			t.Errorf("a frame over the limit: %v", err)
		}
		// The body is not read: a reader that fails on any read past the header is
		// not asked for it.
		hdrOnly := io.MultiReader(frame(1<<30, nil), failingReader{})
		if _, err := readFrame(hdrOnly, 1<<20); !errors.Is(err, errFrameTooLarge) {
			t.Errorf("a 1 GiB length: %v, want it refused from the header alone", err)
		}
		if _, err := readFrame(frame(0xFFFFFFFF, nil), 1<<20); !errors.Is(err, errFrameTooLarge) {
			t.Errorf("a 4 GiB length: %v", err)
		}
		if _, err := readFrame(frame(0, nil), 3); !errors.Is(err, errFrameEmpty) {
			t.Errorf("an empty frame: %v", err)
		}
		if _, err := readFrame(frame(5, []byte("ab")), 8); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("a truncated frame: %v", err)
		}
		if err := writeFrame(io.Discard, make([]byte, 9), 8); !errors.Is(err, errFrameTooLarge) {
			t.Errorf("an oversize frame was written: %v", err)
		}
	})

	for _, network := range []string{NetworkUnix, NetworkTCP} {
		t.Run(network, func(t *testing.T) {
			newSubject := func(t *testing.T, script func(net.Conn, []byte, int, string) bool, mutate ...func(*Config)) (*host, adapters.AppAdapter) {
				t.Helper()
				app := fake.NewApp()
				app.Seed("ten_a", "wks_a", "item_1", "Widget")
				h := startHost(t, network, app)
				h.script = script
				cfg := fixtureConfig(t, network, h.addr)
				for _, m := range mutate {
					m(&cfg)
				}
				return h, wrapped(t, newAdapter(t, cfg))
			}
			// maxPayload is the longest reply the adapter reads; its seal makes the frame macLen longer.
			const maxPayload = testSize + envelopeSlack
			header := func(n uint32) []byte {
				var hdr [4]byte
				binary.BigEndian.PutUint32(hdr[:], n)
				return hdr[:]
			}
			// stall sends the bytes given and then nothing, until the adapter hangs up.
			stall := func(raw []byte) func(net.Conn, []byte, int, string) bool {
				return func(c net.Conn, _ []byte, _ int, _ string) bool {
					_, _ = c.Write(raw)
					_, _ = io.Copy(io.Discard, c)
					return true
				}
			}

			t.Run("a reply longer than the limit is refused from its header", func(t *testing.T) {
				for _, n := range []uint32{maxPayload + macLen + 1, 1 << 24, 0xFFFFFFFF} {
					_, w := newSubject(t, stall(header(n))) // the body never comes
					start := time.Now()
					err := readItem(w, context.Background())
					if code(err) != contracts.ErrSchemaInvalid || errReason(err) != adapters.ReasonResponseTooLarge {
						t.Errorf("a frame of %d bytes: %v, want schema_invalid (%s)", n, err, adapters.ReasonResponseTooLarge)
					}
					if d := time.Since(start); d > 2*time.Second {
						t.Errorf("a frame of %d bytes took %s to refuse: the adapter waited for its body", n, d)
					}
				}
			})

			t.Run("a commit whose reply is too long is uncertain", func(t *testing.T) {
				_, w := newSubject(t, stall(header(maxPayload+macLen+1)))
				_, err := w.Commit(context.Background(), testScope(), validCommit(t))
				if code(err) != contracts.ErrUncertainOutcome || errReason(err) != adapters.ReasonResponseTooLarge {
					t.Errorf("commit: %v, want uncertain_outcome caused by %s", err, adapters.ReasonResponseTooLarge)
				}
			})

			t.Run("an empty frame and a truncated frame are not replies", func(t *testing.T) {
				_, w := newSubject(t, stall(header(0)))
				if err := readItem(w, context.Background()); code(err) != contracts.ErrSchemaInvalid || errReason(err) != ReasonProtocol {
					t.Errorf("an empty frame: %v, want schema_invalid (%s)", err, ReasonProtocol)
				}
				trunc := func(c net.Conn, _ []byte, _ int, _ string) bool {
					_, _ = c.Write(append(header(100), "{\"jsonrpc\""...))
					return true
				}
				_, w = newSubject(t, trunc)
				if err := readItem(w, context.Background()); err == nil {
					t.Errorf("a truncated frame was accepted: %v", err)
				}
				_, w = newSubject(t, trunc)
				if _, err := w.Commit(context.Background(), testScope(), validCommit(t)); code(err) != contracts.ErrUncertainOutcome {
					t.Errorf("a commit whose reply was cut off: %v, want uncertain_outcome", err)
				}
			})

			t.Run("a stalled header is cut off by the deadline", func(t *testing.T) {
				_, w := newSubject(t, stall([]byte{0, 0}))
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				start := time.Now()
				err := readItem(w, ctx)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
					t.Errorf("a stalled header: %v after %s", err, time.Since(start))
				}
			})

			t.Run("a frame at the limit is accepted and one byte over is not", func(t *testing.T) {
				const base = `{"jsonrpc":"2.0","id":3,"result":`
				pad := func(n int) func(net.Conn, []byte, int, string) bool {
					return func(c net.Conn, ks []byte, _ int, _ string) bool {
						// The result is small; the whitespace after it fills the frame.
						res := `{"status":"rejected","identifiers":[],"values":[],"errors":[{"code":"validation_failed","message":"no such item","field":"item_id"}],"allowed_next_actions":[]}`
						body := base + res + "}"
						body += strings.Repeat(" ", n-len(body))
						_ = writeSealed(c, ks, labelReply, []byte(body), 1<<20)
						return true
					}
				}
				_, w := newSubject(t, pad(maxPayload))
				if err := readItem(w, context.Background()); err != nil {
					t.Errorf("a frame of exactly %d bytes: %v", maxPayload, err)
				}
				_, w = newSubject(t, pad(maxPayload+1))
				if err := readItem(w, context.Background()); errReason(err) != adapters.ReasonResponseTooLarge {
					t.Errorf("a frame of %d bytes: %v, want %s", maxPayload+1, err, adapters.ReasonResponseTooLarge)
				}
			})

			t.Run("the handshake is bounded too", func(t *testing.T) {
				h, w := newSubject(t, nil)
				h.down()
				l, err := listen(t, network, h.addr)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				go func() {
					c, err := l.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					_, _ = readFrame(c, 1<<20)
					_, _ = c.Write(header(1 << 20)) // a handshake frame larger than any legitimate one
					_, _ = io.Copy(io.Discard, c)
				}()
				start := time.Now()
				err = readItem(w, context.Background())
				if code(err) != contracts.ErrUnavailable || errReason(err) != ReasonPeerAuthFailed || time.Since(start) > 2*time.Second {
					t.Errorf("an oversize handshake frame: %v after %s, want unavailable (%s) at once", err, time.Since(start), ReasonPeerAuthFailed)
				}
			})

			t.Run("a request longer than the limit is refused before anything is sent", func(t *testing.T) {
				h, w := newSubject(t, nil, func(c *Config) { c.MaxRequestBytes = 1024 })
				err := func() error {
					_, err := w.Read(context.Background(), testScope(), adapters.ReadRequest{
						CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": strings.Repeat("x", 2048)},
					})
					return err
				}()
				if code(err) != contracts.ErrValidationFailed || errReason(err) != ReasonRequestTooLarge || len(h.app.Calls()) != 0 {
					t.Errorf("an oversize request: %v, %d calls reached the host", err, len(h.app.Calls()))
				}
			})
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the body was read") }

// SEC-038, SEC-081: the socket the adapter connects to must be reachable by its
// owner only, in a directory nobody else can write to; the bridge creates it so,
// and the adapter refuses one that is not, before connecting.
func TestSocketPermissionsRestricted(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		const sock = fs.ModeSocket
		tests := []struct {
			name      string
			goos      string
			file, dir fs.FileMode
			ok        bool
		}{
			{"owner only", "linux", sock | 0o600, 0o700, true},
			{"group-readable directory", "linux", sock | 0o600, 0o750, true},
			{"group-accessible socket", "linux", sock | 0o660, 0o700, false},
			{"world-accessible socket", "darwin", sock | 0o666, 0o700, false},
			{"world-writable directory", "linux", sock | 0o600, 0o777, false},
			{"group-writable directory", "linux", sock | 0o600, 0o770, false},
			{"sticky shared directory", "linux", sock | 0o600, 0o777 | fs.ModeSticky, true},
			{"not a socket", "linux", 0o600, 0o700, false},
			{"windows modes carry no bits", "windows", sock | 0o666, 0o777, true},
		}
		for _, tc := range tests {
			if err := socketPermsOK(tc.file, tc.dir, tc.goos); (err == nil) != tc.ok {
				t.Errorf("%s: %v", tc.name, err)
			}
		}
	})

	t.Run("the bridge creates an owner-only socket and the adapter refuses a loose one", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits do not apply; on Windows the application key is the control")
		}
		b := startBridge(t, NetworkUnix)
		b.app.Seed("ten_a", "wks_a", "item_1", "Widget")
		dir := filepath.Dir(b.addr)
		if fi, err := os.Stat(b.addr); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("the bridge made a socket with mode %v (%v), want 0600", fi.Mode().Perm(), err)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("the bridge made a directory with mode %v (%v), want 0700", fi.Mode().Perm(), err)
		}
		w := wrapped(t, newAdapter(t, fixtureConfig(t, NetworkUnix, b.addr)))
		if err := readItem(w, context.Background()); err != nil {
			t.Fatalf("an owner-only socket: %v", err)
		}
		handled := b.handled.Load()

		for name, loosen := range map[string]func() (restore func()){
			"group and others can use the socket": func() func() {
				_ = os.Chmod(b.addr, 0o666)
				return func() { _ = os.Chmod(b.addr, 0o600) }
			},
			"others can replace the socket": func() func() {
				_ = os.Chmod(dir, 0o777)
				return func() { _ = os.Chmod(dir, 0o700) }
			},
		} {
			restore := loosen()
			err := readItem(w, context.Background())
			restore()
			if code(err) != contracts.ErrUnavailable || errReason(err) != ReasonSocketPermissions {
				t.Errorf("%s: %v, want unavailable (%s)", name, err, ReasonSocketPermissions)
			}
			if b.handled.Load() != handled {
				t.Errorf("%s: the adapter connected anyway", name)
			}
		}
		if err := readItem(w, context.Background()); err != nil {
			t.Errorf("after the permissions were restored: %v", err)
		}
	})

	t.Run("a path that is not a socket is not connected to", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits do not apply")
		}
		p := filepath.Join(shortDir(t), "plain")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		w := wrapped(t, newAdapter(t, fixtureConfig(t, NetworkUnix, p)))
		if err := readItem(w, context.Background()); errReason(err) != ReasonSocketPermissions {
			t.Errorf("a regular file: %v, want %s", err, ReasonSocketPermissions)
		}
	})
}
