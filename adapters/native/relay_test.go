package native

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// startRelay is a process that does not hold the application key and sits between
// the adapter and the real bridge, as another local user could on a loopback port
// the package declares. It forwards the handshake frame by frame, and may rewrite
// a later frame: mutate gets the direction ("c2s", "s2c"), the 1-based number of
// the frame in that direction and its bytes. It returns the address to dial.
func startRelay(t *testing.T, target string, mutate func(dir string, n int, frame []byte) []byte) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	forward := func(dir string, from, to net.Conn) {
		defer wg.Done()
		defer from.Close()
		defer to.Close()
		for n := 1; ; n++ {
			var hdr [4]byte
			if _, err := io.ReadFull(from, hdr[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(hdr[:]))
			if _, err := io.ReadFull(from, body); err != nil {
				return
			}
			if mutate != nil {
				body = mutate(dir, n, body)
			}
			binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
			if _, err := to.Write(append(hdr[:], body...)); err != nil {
				return
			}
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			wg.Add(2)
			go forward("c2s", c, up)
			go forward("s2c", up, c)
		}
	}()
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	return l.Addr().String()
}

// SEC-038: the proof of the handshake authenticates the two ends, and everything
// after it is bound to that session by a MAC under a key derived from the proof's
// transcript. A relay that holds no key can pass the handshake through, but it
// cannot rewrite the identity of a request (the host would serve a tenant the
// caller never had) or the content of a reply (the adapter would take forged data
// for a result), and it cannot make either side accept a frame it altered.
func TestRelayCannotAlterAnAuthenticatedSession(t *testing.T) {
	read := func(w adapters.AppAdapter) (actions.DeterministicResult, error) {
		return w.Read(context.Background(), testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "item_1"}})
	}
	setup := func(t *testing.T, mutate func(string, int, []byte) []byte) (*realBridge, adapters.AppAdapter) {
		t.Helper()
		b := startBridge(t, NetworkTCP)
		b.app.Seed("ten_a", "wks_a", "item_1", "Widget")
		addr := startRelay(t, b.addr, mutate)
		return b, wrapped(t, newAdapter(t, fixtureConfig(t, NetworkTCP, addr)))
	}

	t.Run("a relay that alters nothing passes the session through", func(t *testing.T) {
		b, w := setup(t, nil)
		res, err := read(w)
		if err != nil || res.Status != actions.DeterministicResultStatusOk || b.handled.Load() != 1 {
			t.Fatalf("a plain relay: %+v, %v, %d calls handled", res, err, b.handled.Load())
		}
	})

	t.Run("a rewritten request never reaches a handler", func(t *testing.T) {
		b, w := setup(t, func(dir string, n int, frame []byte) []byte {
			if dir == "c2s" && n == 3 {
				return bytes.Replace(frame, []byte("ten_a"), []byte("ten_b"), 1) // the same length
			}
			return frame
		})
		if _, err := read(w); err == nil {
			t.Error("a request whose tenant was rewritten in flight was answered")
		}
		if b.handled.Load() != 0 {
			for _, c := range b.app.Calls() {
				t.Logf("the host served tenant %q", c.Identity.TenantID)
			}
			t.Fatal("a handler ran for a request that was rewritten in flight")
		}
	})

	t.Run("a rewritten reply is not a result", func(t *testing.T) {
		_, w := setup(t, func(dir string, n int, frame []byte) []byte {
			if dir == "s2c" && n == 3 {
				return bytes.Replace(frame, []byte("Widget"), []byte("Hacked"), 1) // valid JSON, forged value
			}
			return frame
		})
		res, err := read(w)
		if err == nil {
			t.Fatalf("a reply that was rewritten in flight was taken for a result: %+v", res.Values)
		}
		if code(err) != contracts.ErrSchemaInvalid || errReason(err) != ReasonProtocol {
			t.Errorf("a forged reply: %v, want schema_invalid (%s)", err, ReasonProtocol)
		}
	})

	t.Run("a commit whose reply was rewritten is uncertain", func(t *testing.T) {
		_, w := setup(t, func(dir string, n int, frame []byte) []byte {
			if dir == "s2c" && n == 3 {
				frame[10] ^= 0x01 // one bit of the payload
				return frame
			}
			return frame
		})
		if _, err := w.Commit(context.Background(), testScope(), validCommit(t)); code(err) != contracts.ErrUncertainOutcome {
			t.Errorf("a commit whose reply was forged: %v, want uncertain_outcome", err)
		}
	})

	t.Run("a bridge that was relayed to by a peer without the key is not believed", func(t *testing.T) {
		// A forged confirmation of the handshake: the relay answers session.auth
		// itself, so the bridge never saw a proof, and says it did.
		_, w := setup(t, func(dir string, n int, frame []byte) []byte {
			if dir == "s2c" && n == 2 {
				return []byte(`{"jsonrpc":"2.0","id":2,"result":{"authenticated":true}}`)
			}
			return frame
		})
		if _, err := read(w); err != nil && errReason(err) != ReasonPeerAuthFailed {
			t.Errorf("a forged confirmation: %v, want %s", err, ReasonPeerAuthFailed)
		} else if err == nil {
			t.Error("a forged confirmation of the handshake was believed")
		}
	})
}
