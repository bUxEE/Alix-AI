//go:build live

package elevenlabs

import "testing"

// Live calls against a real ElevenLabs key are human-gated. Set
// ELEVENLABS_API_KEY and run: go test -tags live ./adapters/external/tts/elevenlabs -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live ElevenLabs TTS: set credentials and remove this skip deliberately")
}
