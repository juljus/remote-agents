package triggers

import (
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/net/route"
)

func TestSystemNetworkReadsDefaultRoute(t *testing.T) {
	snap, err := SystemNetwork()
	if err != nil {
		t.Fatalf("SystemNetwork: %v", err)
	}
	if len(snap.Addrs) == 0 {
		t.Fatal("no interface addresses found")
	}
	t.Logf("fingerprint:\n%s", snap.Fingerprint())
}

// routeGetDefault is the default route of a family as `route -n get` reports
// it, in defaultRoutes' form, or "" when there is none.
func routeGetDefault(t *testing.T, family string) string {
	t.Helper()
	out, err := exec.Command("/sbin/route", "-n", "get", family, "default").CombinedOutput()
	if strings.Contains(string(out), "not in table") {
		return ""
	}
	if err != nil {
		t.Fatalf("route get %s default: %v: %s", family, err, out)
	}
	fields := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	iface, err := net.InterfaceByName(fields["interface"])
	if err != nil {
		t.Fatalf("interface %q: %v", fields["interface"], err)
	}
	// route prints a link-local gateway with its zone, as fe80::1%en0.
	gateway, _, _ := strings.Cut(fields["gateway"], "%")
	if gateway == "" {
		gateway = fmt.Sprintf("link%d", iface.Index)
	}
	return fmt.Sprintf("if%d via %s", iface.Index, gateway)
}

func TestDefaultRoutesMatchRouteGet(t *testing.T) {
	var want []string
	for _, family := range []string{"-inet", "-inet6"} {
		if r := routeGetDefault(t, family); r != "" {
			want = append(want, r)
		}
	}
	got, err := defaultRoutes()
	if err != nil {
		t.Fatalf("defaultRoutes: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("defaultRoutes = %q, route get says %q", got, want)
	}
}

func TestIsReplyMatchesTypePidAndSeq(t *testing.T) {
	msg := func(typ, pid, seq int) []byte {
		m := route.RouteMessage{Version: syscall.RTM_VERSION, Type: typ, ID: uintptr(pid), Seq: seq,
			Addrs: []route.Addr{syscall.RTAX_DST: &route.Inet4Addr{}}}
		b, err := m.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !isReply(msg(syscall.RTM_GET, 42, 7), 42, 7) {
		t.Fatal("the answer was not recognised")
	}
	for name, m := range map[string][]byte{
		"other type":    msg(syscall.RTM_ADD, 42, 7),
		"other process": msg(syscall.RTM_GET, 43, 7),
		"other request": msg(syscall.RTM_GET, 42, 8),
		"truncated":     msg(syscall.RTM_GET, 42, 7)[:20],
	} {
		if isReply(m, 42, 7) {
			t.Fatalf("%s taken for the answer", name)
		}
	}
}
