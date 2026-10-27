//go:build race

package cloexec

// Each probe is about 15 times slower under the race detector.
const leakProbes = 300
