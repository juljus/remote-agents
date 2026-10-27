//go:build !race

package cloexec

// net.Listen leaked 13 of 3000 such connections on darwin; set
// RA_CLOEXEC_PLAIN=1 to measure it.
const leakProbes = 1000
