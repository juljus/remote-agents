package session

import (
	"bytes"
	"regexp"
	"strconv"
)

// Agents often enable the kitty keyboard protocol or xterm modifyOtherKeys, and
// those modes stay on in the local terminal while the link is down, so Ctrl+C
// may arrive encoded instead of as 0x03.
var (
	kittyKey        = regexp.MustCompile(`\x1b\[(\d+)(?::\d*)*;(\d+)(?::(\d+))?u`)
	modifyOtherKeys = regexp.MustCompile(`\x1b\[27;(\d+);(\d+)~`)
)

const (
	keyC         = 99
	modCtrl      = 4
	modLockBits  = 64 | 128
	eventRelease = 3
)

func ctrlOnly(modsField string) bool {
	mods, err := strconv.Atoi(modsField)
	if err != nil || mods < 1 {
		return false
	}
	return (mods-1)&^modLockBits == modCtrl
}

func isInterrupt(buf []byte) bool {
	if bytes.IndexByte(buf, 0x03) >= 0 {
		return true
	}
	for _, m := range kittyKey.FindAllSubmatch(buf, -1) {
		if string(m[1]) != strconv.Itoa(keyC) || !ctrlOnly(string(m[2])) {
			continue
		}
		if len(m[3]) > 0 && string(m[3]) == strconv.Itoa(eventRelease) {
			continue
		}
		return true
	}
	for _, m := range modifyOtherKeys.FindAllSubmatch(buf, -1) {
		if ctrlOnly(string(m[1])) && string(m[2]) == strconv.Itoa(keyC) {
			return true
		}
	}
	return false
}
