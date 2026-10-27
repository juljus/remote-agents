package box

import (
	"strings"
	"testing"
)

const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 444143615 1 0000000000000000 100 0 0 10 0
   1: 00000000:2232 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1F91 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 2 1 0000000000000000 100 0 0 10 0
   3: 0B0A000A:1F92 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3 1 0000000000000000 100 0 0 10 0
`

const procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F93 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1F94 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5 1 0000000000000000 100 0 0 10 0
   2: 5C117AFD0000E0A1000000004DED2D22:1F95 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 6 1 0000000000000000 100 0 0 10 0
`

func TestListeningOnLocalhost(t *testing.T) {
	for port, want := range map[uint16]bool{
		8080: true,  // 127.0.0.1
		8754: true,  // 0.0.0.0
		8081: false, // an established connection, not a listener
		8082: false, // a non-loopback address
		8083: true,  // ::1
		8084: true,  // ::
		8085: false, // a global IPv6 address
		9999: false,
	} {
		got, err := ListeningOnLocalhost(port, strings.NewReader(procTCP), strings.NewReader(procTCP6))
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		if got != want {
			t.Fatalf("port %d: listening = %v, want %v", port, got, want)
		}
	}
}

func TestListeningRejectsUnreadableTables(t *testing.T) {
	bad := "  sl  local_address rem_address   st\n   0: 0100007F 00000000:0000 0A\n"
	if _, err := ListeningOnLocalhost(8080, strings.NewReader(bad), strings.NewReader(procTCP6)); err == nil {
		t.Fatal("expected an error for a malformed table")
	}
	if _, err := ListeningOnLocalhost(8080, strings.NewReader(""), strings.NewReader(procTCP6)); err == nil || err.Error() != "socket table is empty" {
		t.Fatalf("empty table: %v", err)
	}
}
