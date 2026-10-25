// Command ledger runs the synthetic ledger application: the native bridge AlixAI
// reaches over loopback, the contracts document API and, optionally, the signed
// event delivery to AlixAI.
//
//	LEDGER_BRIDGE_ADDR      bridge listen address, loopback only        (default 127.0.0.1:7421)
//	LEDGER_KEY_ID           application key id on the wire              (default k1)
//	LEDGER_KEY_REF          env:NAME or file:/path of the 32+ byte application key (required)
//	LEDGER_DOCS_ADDR        contracts document API address, loopback    (default: not served)
//	LEDGER_DOCS_TOKEN_REF   env:/file: reference of its bearer credential (required with LEDGER_DOCS_ADDR)
//	LEDGER_EVENT_URL        AlixAI event intake URL, loopback http      (default: events stay in the outbox)
//	LEDGER_EVENT_KEY_ID     id of the registered events key
//	LEDGER_EVENT_KEY_REF    env:/file: reference of the base64 Ed25519 seed (32 bytes)
//	LEDGER_TEST_SUPPORT=1   declare the audit's provisioning operations
//	LEDGER_DEMO_SEED=1      load a small synthetic organisation
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/buxee/alix-ai/examples/reference-apps/ledger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ledger:", err)
		os.Exit(1)
	}
}

func getenv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("an address is host:port")
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("an address must be loopback")
	}
	return nil
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	keyID := getenv("LEDGER_KEY_ID", "k1")
	key, err := ledger.ReadSecretRef(os.Getenv("LEDGER_KEY_REF"))
	if err != nil {
		return fmt.Errorf("the application key: %w", err)
	}
	cfg := ledger.Config{TestSupport: os.Getenv("LEDGER_TEST_SUPPORT") == "1"}
	if url := os.Getenv("LEDGER_EVENT_URL"); url != "" {
		seedB64, err := ledger.ReadSecretRef(os.Getenv("LEDGER_EVENT_KEY_REF"))
		if err != nil {
			return fmt.Errorf("the events key: %w", err)
		}
		seed, err := base64.StdEncoding.DecodeString(string(seedB64))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("the events key must be a base64 Ed25519 seed of 32 bytes")
		}
		sink, err := ledger.NewHTTPSink(url, ledger.Signer{KeyID: os.Getenv("LEDGER_EVENT_KEY_ID"), Key: ed25519.NewKeyFromSeed(seed)})
		if err != nil {
			return err
		}
		cfg.Events = sink
	}
	app := ledger.New(cfg)
	if os.Getenv("LEDGER_DEMO_SEED") == "1" {
		if err := app.SeedDemo(); err != nil {
			return err
		}
	}

	bridgeAddr := getenv("LEDGER_BRIDGE_ADDR", "127.0.0.1:7421")
	if err := loopbackOnly(bridgeAddr); err != nil {
		return err
	}
	srv, err := app.NewBridge(ledger.BridgeConfig{Keys: map[string][]byte{keyID: key}, Logger: log})
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", bridgeAddr)
	if err != nil {
		return err
	}
	rt := ledger.Runtime{Listen: l.Addr().String(), KeyID: keyID}

	var docs *http.Server
	if addr := os.Getenv("LEDGER_DOCS_ADDR"); addr != "" {
		if err := loopbackOnly(addr); err != nil {
			return err
		}
		token, err := ledger.ReadSecretRef(os.Getenv("LEDGER_DOCS_TOKEN_REF"))
		if err != nil {
			return fmt.Errorf("the document API credential: %w", err)
		}
		h, err := app.DocumentAPI(string(token))
		if err != nil {
			return err
		}
		dl, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		rt.DocsListen = dl.Addr().String()
		docs = &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = docs.Serve(dl) }()
	}
	app.SetRuntime(rt)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		if docs != nil {
			_ = docs.Close()
		}
	}()
	log.Info("listening", slog.String("bridge", rt.Listen), slog.String("docs", rt.DocsListen))
	return srv.Serve(l)
}
