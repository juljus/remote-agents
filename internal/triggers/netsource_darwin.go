package triggers

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"

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

func defaultRoutes() ([]string, error) {
	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, fmt.Errorf("fetching routing table: %w", err)
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, fmt.Errorf("parsing routing table: %w", err)
	}
	var out []string
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&syscall.RTF_UP == 0 || rm.Flags&syscall.RTF_GATEWAY == 0 {
			continue
		}
		if len(rm.Addrs) <= syscall.RTAX_GATEWAY || !isUnspecified(rm.Addrs[syscall.RTAX_DST]) {
			continue
		}
		out = append(out, fmt.Sprintf("if%d via %s", rm.Index, addrString(rm.Addrs[syscall.RTAX_GATEWAY])))
	}
	return out, nil
}

func isUnspecified(a route.Addr) bool {
	switch v := a.(type) {
	case *route.Inet4Addr:
		return v.IP == [4]byte{}
	case *route.Inet6Addr:
		return v.IP == [16]byte{}
	}
	return false
}

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
