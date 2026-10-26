//go:build live

package mistral

import "testing"

// Live calls against a real Mistral key are human-gated. Set ALIXAI_LIVE_MISTRAL_KEY and
// run: go test -tags live ./adapters/external/tts/mistral -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live Mistral TTS: set credentials and remove this skip deliberately")
}
