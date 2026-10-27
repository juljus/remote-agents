package box

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// tcpListen is the LISTEN state in /proc/net/tcp.
const tcpListen = "0A"

// ListeningOnLocalhost reports whether a socket in the /proc/net/tcp and
// tcp6 tables listens on port where localhost reaches it: a loopback or
// wildcard address. Reading the tables, unlike connecting, never takes a
// request from a login server that answers only one.
func ListeningOnLocalhost(port uint16, tcp, tcp6 io.Reader) (bool, error) {
	for _, table := range []io.Reader{tcp, tcp6} {
		found, err := listensIn(table, port)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func listensIn(table io.Reader, port uint16) (bool, error) {
	scanner := bufio.NewScanner(table)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("reading socket table: %w", err)
		}
		return false, errors.New("socket table is empty")
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			return false, fmt.Errorf("socket table line %q: too few fields", scanner.Text())
		}
		if fields[3] != tcpListen {
			continue
		}
		addr, p, err := parseProcAddr(fields[1])
		if err != nil {
			return false, fmt.Errorf("socket table line %q: %w", scanner.Text(), err)
		}
		addr = addr.Unmap()
		if p == port && (addr.IsLoopback() || addr.IsUnspecified()) {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("reading socket table: %w", err)
	}
	return false, nil
}

// parseProcAddr reads "0100007F:1F90": the address as 32-bit words in host
// (little-endian) byte order, then the port in hex.
func parseProcAddr(s string) (netip.Addr, uint16, error) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return netip.Addr{}, 0, fmt.Errorf("address %q has no port", s)
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("port in %q: %w", s, err)
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.Addr{}, 0, fmt.Errorf("address %q is not 4 or 16 hex bytes", hexAddr)
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	addr, _ := netip.AddrFromSlice(raw)
	return addr, uint16(port), nil
}
