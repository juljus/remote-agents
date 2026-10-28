package triggers

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/route"
)

// SystemNetwork reads interface addresses and default routes from the kernel.
func SystemNetwork() (Snapshot, error) {
	addrs, err := interfaceAddrs()
	if err != nil {
		return Snapshot{}, err
	}
	routes, err := defaultRoutes()
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Addrs: addrs, DefaultRoutes: routes}, nil
}

func interfaceAddrs() ([]IfaceAddr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}
	var out []IfaceAddr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("addresses of %s: %w", iface.Name, err)
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			out = append(out, IfaceAddr{Iface: iface.Name, Addr: addr.Unmap()})
		}
	}
	return out, nil
}

// defaultRoutes asks the kernel for the IPv4 and IPv6 default routes, as
// `route get default` does. Scoped per-interface defaults are left out: only
// the route new connections take matters. Dumping the whole routing table
// instead races its size, which fails with ENOMEM while routes churn after a
// wake.
func defaultRoutes() ([]string, error) {
	var out []string
	for _, zero := range []route.Addr{&route.Inet4Addr{}, &route.Inet6Addr{}} {
		r, found, err := defaultRoute(zero)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, r)
		}
	}
	return out, nil
}

var routeSeq atomic.Int32

// defaultRoute sends an RTM_GET for the default route of zero's family on a
// routing socket of its own and reads the kernel's answer.
func defaultRoute(zero route.Addr) (string, bool, error) {
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, syscall.AF_UNSPEC)
	if err != nil {
		return "", false, fmt.Errorf("opening routing socket: %w", err)
	}
	defer syscall.Close(fd)
	timeout := syscall.NsecToTimeval(routeReplyTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return "", false, fmt.Errorf("setting routing socket timeout: %w", err)
	}
	seq := int(routeSeq.Add(1))
	req := route.RouteMessage{
		Version: syscall.RTM_VERSION,
		Type:    syscall.RTM_GET,
		ID:      uintptr(os.Getpid()),
		Seq:     seq,
		Addrs:   []route.Addr{syscall.RTAX_DST: zero, syscall.RTAX_NETMASK: zero},
	}
	msg, err := req.Marshal()
	if err != nil {
		return "", false, fmt.Errorf("encoding route request: %w", err)
	}
	if _, err := syscall.Write(fd, msg); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("requesting default route: %w", err)
	}
	// The socket also receives every routing change on the system; skip to the reply.
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if errors.Is(err, syscall.EAGAIN) {
			return "", false, fmt.Errorf("no answer to the default route request within %s", routeReplyTimeout)
		}
		if err != nil {
			return "", false, fmt.Errorf("reading default route: %w", err)
		}
		// Byte 3 of a routing message is its type.
		if n < 4 || buf[3] != syscall.RTM_GET {
			continue
		}
		msgs, err := route.ParseRIB(route.RIBTypeRoute, buf[:n])
		if err != nil {
			return "", false, fmt.Errorf("parsing default route: %w", err)
		}
		for _, m := range msgs {
			rm, ok := m.(*route.RouteMessage)
			if !ok || rm.Type != syscall.RTM_GET || rm.ID != req.ID || rm.Seq != seq {
				continue
			}
			if rm.Err != nil {
				return "", false, fmt.Errorf("default route: %w", rm.Err)
			}
			if rm.Flags&syscall.RTF_UP == 0 || len(rm.Addrs) <= syscall.RTAX_GATEWAY || rm.Addrs[syscall.RTAX_GATEWAY] == nil {
				return "", false, nil
			}
			return fmt.Sprintf("if%d via %s", rm.Index, addrString(rm.Addrs[syscall.RTAX_GATEWAY])), true, nil
		}
	}
}

// routeReplyTimeout bounds the wait for the kernel's answer to an RTM_GET.
const routeReplyTimeout = 2 * time.Second

func addrString(a route.Addr) string {
	switch v := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(v.IP).String()
	case *route.Inet6Addr:
		return netip.AddrFrom16(v.IP).String()
	case *route.LinkAddr:
		return fmt.Sprintf("link%d", v.Index)
	}
	return fmt.Sprintf("%T", a)
}
