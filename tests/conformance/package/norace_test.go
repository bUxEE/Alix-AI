//go:build !race

package conformancepkg

// raceDetector is true when the test binary is built with -race.
const raceDetector = false
