//go:build race

package harness

// raceEnabled tells the harness to build the server with -race, matching the
// test binary. There is no exported way to ask the runtime, so the build tag
// answers instead.
const raceEnabled = true
