//go:build live

package openai

import "testing"

// Live calls against a real OpenAI key are human-gated. Set OPENAI_API_KEY and
// run: go test -tags live ./adapters/external/stt/openai -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live OpenAI STT: set credentials and remove this skip deliberately")
}
