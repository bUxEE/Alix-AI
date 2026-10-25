//go:build live

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external/openai"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// liveBuild is true: this binary can run against the real service.
const liveBuild = true

const (
	liveKeyEnv   = "ALIXAI_LIVE_OPENAI_KEY"
	liveModelEnv = "ALIXAI_LIVE_OPENAI_MODEL"
)

// liveSubject builds the subject of one check against the real OpenAI API. It
// is reached only after resolveMode saw --live, this build tag and
// ALIXAI_PROVIDERCONF_LIVE=1. The adapter talks to a loopback proxy, so the
// suite can still inject the faults a real service will not produce on demand
// (a stall, a quota failure, a malformed body, a cut stream): the proxy
// forwards the natural scenarios (text, structured output, tool call) to the
// real API with the operator's key and answers every fault from the recorded
// fixtures. Natural content cannot be forced, so checks assert structure.
//
// This path costs real money and is human-gated (task X-H02). It is compiled
// and vetted by the build but never exercised by a verify command.
func liveSubject(provider string, profile pv.ProviderProfile, env func(string) string) (suite.Subject, func(), error) {
	if provider != providerOpenAI {
		return suite.Subject{}, nil, fmt.Errorf("unknown provider %q", provider)
	}
	key := env(liveKeyEnv)
	if key == "" {
		return suite.Subject{}, nil, errors.New(liveKeyEnv + " is not set")
	}
	if m := env(liveModelEnv); m != "" {
		profile.ModelID = pv.ModelID(m)
	}
	up, err := newOpenAIUpstream()
	if err != nil {
		return suite.Subject{}, nil, err
	}
	hc := &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	up.forward = func(w http.ResponseWriter, r *http.Request, body []byte) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, openai.DefaultBaseURL+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "{}", http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		req.Host = ""
		resp, err := hc.Do(req)
		if err != nil {
			up.drop(w)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			if !strings.EqualFold(k, "Content-Length") {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(resp.StatusCode)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if fl != nil {
					fl.Flush()
				}
			}
			if err != nil {
				if err != io.EOF {
					up.drop(w)
				}
				return
			}
		}
	}
	ring := suite.NewKeyRingWith(key)
	logs := &lockedBuffer{}
	c, err := newOpenAIClient(up.URL, profile, ringKeys{ring}, logs)
	if err != nil {
		up.Close()
		return suite.Subject{}, nil, err
	}
	return suite.Subject{
		Name: "openai (live)", Generator: c, Caps: c, Runtime: c, Profile: profile, Upstream: up, Keys: ring,
		Logs: logs.String, Natural: true,
	}, up.Close, nil
}
