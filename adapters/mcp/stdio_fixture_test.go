package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters/fake"
)

// Environment of the stdio fixture process (see TestMain).
const (
	envFixture = "ALIXAI_MCP_FIXTURE"
	// envCrash names the tools whose call ends the process mid-call.
	envCrash = "ALIXAI_MCP_FIXTURE_CRASH"
	// envHang names the tools whose call never answers.
	envHang = "ALIXAI_MCP_FIXTURE_HANG"
	// envExit makes the process exit before it answers anything.
	envExit = "ALIXAI_MCP_FIXTURE_EXIT"
	// envFlood makes the process answer the first call with an endless line.
	envFlood = "ALIXAI_MCP_FIXTURE_FLOOD"
	// envProbe makes the describe tool report the one variable the parent set.
	envProbe = "ALIXAI_MCP_FIXTURE_ENVPROBE"
	// envDeaf names the tools whose call never answers and makes the process
	// stop reading its input.
	envDeaf = "ALIXAI_MCP_FIXTURE_DEAF"
)

// runStdioFixture is the body of the stdio fixture process: the test binary,
// re-executed, serving a fake application over its stdin and stdout.
func runStdioFixture() {
	if os.Getenv(envExit) != "" {
		os.Exit(4)
	}
	srv := fixture.New(fake.NewApp())
	for _, t := range strings.Split(os.Getenv(envCrash), ",") {
		if t != "" {
			srv.Handle(t, func(_ context.Context, _ json.RawMessage) fixture.Result { return fixture.Result{Crash: true} })
		}
	}
	for _, t := range strings.Split(os.Getenv(envHang), ",") {
		if t != "" {
			srv.Handle(t, func(_ context.Context, _ json.RawMessage) fixture.Result { return fixture.Result{Hang: true} })
		}
	}
	if os.Getenv(envProbe) != "" {
		srv.Handle(fixture.ToolDescribe, func(context.Context, json.RawMessage) fixture.Result {
			body := `{"contract_versions":[1],"application_version":"env:` + os.Getenv("ALIXAI_PARENT_SECRET") + `","capabilities":[{"capability_id":"items.get","operations":["read"]}]}`
			return fixture.FromBody([]byte(body), "")
		})
	}
	if names := os.Getenv(envDeaf); names != "" {
		deaf := make(chan struct{})
		var once sync.Once
		for _, t := range strings.Split(names, ",") {
			srv.Handle(t, func(_ context.Context, _ json.RawMessage) fixture.Result {
				once.Do(func() { close(deaf) })
				return fixture.Result{Hang: true}
			})
		}
		srv.ServeStream(deafStdio{deaf})
		return
	}
	if os.Getenv(envFlood) != "" {
		srv.Handle(fixture.ToolGet, func(_ context.Context, _ json.RawMessage) fixture.Result {
			// A message that never ends (a JSON string with no closing quote): the
			// adapter must cut it off.
			_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":99,"result":{"content":[{"type":"text","text":"`)
			chunk := []byte(strings.Repeat("x", 64<<10))
			for {
				if _, err := os.Stdout.Write(chunk); err != nil {
					os.Exit(5)
				}
			}
		})
	}
	srv.RunStdio()
}

// deafStdio is the process's input and output, except that nothing more is read
// once deaf is closed.
type deafStdio struct{ deaf chan struct{} }

func (d deafStdio) Read(p []byte) (int, error) {
	select {
	case <-d.deaf:
		for {
			time.Sleep(time.Hour)
		}
	default:
	}
	return os.Stdin.Read(p)
}

func (deafStdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

func (deafStdio) Close() error { return os.Stdout.Close() }
