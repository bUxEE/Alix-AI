//go:build live

package googletts

import "testing"

// Live calls against a real Google credential are human-gated. Set the
// credential and run: go test -tags live ./adapters/external/tts/googletts -run Live
func TestLiveSkippedByDefault(t *testing.T) {
	t.Skip("live Google TTS: set credentials and remove this skip deliberately")
}
