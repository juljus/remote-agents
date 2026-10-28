package triggers

import (
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
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
	gateway := fields["gateway"]
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
