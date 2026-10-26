//go:build live

package openai

import "testing"

// Live calls against a real OpenAI key are human-gated. Set OPENAI_API_KEY and
// run: go test -tags live ./adapters/external/tts/openai -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live OpenAI TTS: set credentials and remove this skip deliberately")
}
