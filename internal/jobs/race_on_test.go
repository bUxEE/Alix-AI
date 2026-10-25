//go:build race

package jobs

// raceEnabled is true when the test binary is built with -race, whose memory
// and speed overhead makes the footprint measurement meaningless.
const raceEnabled = true
