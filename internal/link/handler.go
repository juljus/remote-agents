// Package link is the laptop link daemon: one persistent ssh connection that
// answers the box shims over its stdio, and the login callback forwards they ask for.
package link

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"remote-agents/internal/control"
	"remote-agents/internal/linkproto"
)

const (
	MIMEText = "text/plain"
	MIMEPNG  = "image/png"
)

type Pasteboard interface {
	// Types lists the MIME types readable now, of MIMEPNG and MIMEText.
	Types() ([]string, error)
	// PNG returns nil when the clipboard holds no image.
	PNG() ([]byte, error)
	// Text fails when the clipboard holds no text.
	Text() (string, error)
	SetText(string) error
	SetPNG([]byte) error
}

type Opener interface {
	Open(ctx context.Context, url string) error
}

// MaxImage bounds a clipboard image so its base64 response stays within one link message.
const MaxImage = linkproto.MaxMessage / 2

// PortForwarder makes the laptop's localhost:port reach the box's localhost:port for a while.
type PortForwarder interface {
	Forward(ctx context.Context, port uint16) error
}

// Handler serves box requests.
type Handler struct {
	Pasteboard Pasteboard
	Opener     Opener
	Forwarder  PortForwarder
	Registry   *control.Registry
	Resolve    func(ctx context.Context, host string) ([]netip.Addr, error)
}

var _ linkproto.Handler = (*Handler)(nil)

// OpenURL treats a laptop-local url, and a loopback url in its query such as
// an OAuth redirect_uri, as a login on the box waiting for its callback: it
// forwards that port to the box first. A port some laptop program holds is
// refused, so these urls never reach a laptop service. Link-local and
// multicast hosts are refused. The browser resolves again, so DNS rebinding
// stays possible.
func (h *Handler) OpenURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("refusing to open %q: only http(s) urls are allowed", raw)
	}
	ports, err := h.callbackPorts(ctx, u)
	if err != nil {
		return fmt.Errorf("refusing to open %q: %w", raw, err)
	}
	for _, port := range ports {
		if err := h.Forwarder.Forward(ctx, port); err != nil {
			return fmt.Errorf("refusing to open %q: %w", raw, err)
		}
	}
	return h.Opener.Open(ctx, u.String())
}

// callbackPorts lists, sorted and once each, the laptop ports the url needs forwarded.
func (h *Handler) callbackPorts(ctx context.Context, u *url.URL) ([]uint16, error) {
	addrs, err := h.hostAddrs(ctx, u.Hostname())
	if err != nil {
		return nil, err
	}
	var ports []uint16
	for _, a := range addrs {
		a = a.Unmap()
		switch {
		case a == loopback4 || a == netip.IPv6Loopback():
			port, err := urlPort(u)
			if err != nil {
				return nil, err
			}
			if port < minCallbackPort {
				return nil, fmt.Errorf("port %d is privileged; login callbacks listen on %d and up", port, minCallbackPort)
			}
			ports = append(ports, port)
		case a.IsLoopback() || thisNetwork.Contains(a) || a.IsUnspecified():
			return nil, fmt.Errorf("%s is not a loopback address a login callback listens on", a)
		case a.IsLinkLocalUnicast() || a.IsMulticast():
			return nil, fmt.Errorf("%s is link-local or multicast", a)
		}
	}
	// A pair with a bad escape cannot carry a redirect the server could decode.
	query, _ := url.ParseQuery(u.RawQuery)
	for _, values := range query {
		for _, v := range values {
			port, ok := loopbackCallback(v)
			if ok {
				ports = append(ports, port)
			}
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

var loopback4 = netip.MustParseAddr("127.0.0.1")

// minCallbackPort keeps forwards off the ports of laptop system services.
const minCallbackPort = 1024

// thisNetwork (0.0.0.0/8) reaches the laptop itself on common stacks.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// loopbackCallback reads v as an http(s) url on a literal loopback host, the
// form RFC 8252 gives native-app redirect URIs, and returns its port.
func loopbackCallback(v string) (uint16, bool) {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return 0, false
	}
	addrs, ok := literalAddrs(u.Hostname())
	if !ok || len(addrs) != 1 || (addrs[0].Unmap() != loopback4 && addrs[0] != netip.IPv6Loopback()) {
		return 0, false
	}
	port, err := urlPort(u)
	if err != nil || port < minCallbackPort {
		return 0, false
	}
	return port, true
}

func urlPort(u *url.URL) (uint16, error) {
	if u.Port() == "" {
		if u.Scheme == "https" {
			return 443, nil
		}
		return 80, nil
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("invalid port %q", u.Port())
	}
	return uint16(port), nil
}

// hostAddrs reads host the way a browser does: numeric IPv4 shorthands
// (127.1, 2130706433, 0x7f000001) are addresses, everything else is resolved.
func (h *Handler) hostAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	if addrs, ok := literalAddrs(host); ok {
		return addrs, nil
	}
	addrs, err := h.Resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", host, err)
	}
	return addrs, nil
}

// literalAddrs reads the hosts a browser maps to an address without DNS.
func literalAddrs(host string) ([]netip.Addr, bool) {
	if a, ok := browserIPv4(host); ok {
		return []netip.Addr{a}, true
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, true
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return []netip.Addr{loopback4}, true
	}
	return nil, false
}

// browserIPv4 parses the WHATWG IPv4 host forms: one to four dot-separated
// decimal, octal (leading 0) or hex (0x) numbers, the last filling the remaining bytes.
func browserIPv4(host string) (netip.Addr, bool) {
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 0, 32)
		if p == "" || err != nil {
			return netip.Addr{}, false
		}
		nums[i] = n
	}
	var v uint64
	for i, n := range nums[:len(nums)-1] {
		if n > 255 {
			return netip.Addr{}, false
		}
		v |= n << (8 * (3 - i))
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(5-len(nums))) {
		return netip.Addr{}, false
	}
	v |= last
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

func (h *Handler) ClipboardTargets(context.Context) ([]string, error) {
	return h.Pasteboard.Types()
}

func (h *Handler) ClipboardRead(_ context.Context, mime string) ([]byte, error) {
	switch mime {
	case MIMEPNG:
		data, err := h.Pasteboard.PNG()
		if err != nil {
			return nil, err
		}
		if data == nil {
			return nil, errors.New("clipboard holds no image")
		}
		if len(data) > MaxImage {
			return nil, fmt.Errorf("clipboard image is %d MB, over the %d MB limit", len(data)>>20, MaxImage>>20)
		}
		return data, nil
	case MIMEText:
		text, err := h.Pasteboard.Text()
		if err != nil {
			return nil, err
		}
		return []byte(text), nil
	}
	return nil, fmt.Errorf("clipboard target %s not available", mime)
}

func (h *Handler) ClipboardWrite(_ context.Context, mime string, data []byte) error {
	switch mime {
	case MIMEPNG:
		return h.Pasteboard.SetPNG(data)
	case MIMEText:
		return h.Pasteboard.SetText(string(data))
	}
	return fmt.Errorf("cannot put %s on the clipboard", mime)
}

// Hook forwards the request to the wrapper registered for the session; the
// wrapper checks it against the hooks it injected and runs it inside its cmux terminal.
func (h *Handler) Hook(ctx context.Context, req linkproto.HookRequest) (linkproto.HookResult, error) {
	caller, err := h.Registry.Lookup(req.Session)
	if errors.Is(err, control.ErrNotRegistered) {
		return linkproto.HookResult{}, fmt.Errorf("%w: %s", linkproto.ErrNoTerminal, req.Session)
	}
	if err != nil {
		return linkproto.HookResult{}, err
	}
	resp, err := caller.Call(ctx, linkproto.Request{Hook: &req})
	if err != nil {
		return linkproto.HookResult{}, err
	}
	if err := resp.Err(); err != nil {
		return linkproto.HookResult{}, err
	}
	if resp.Hook == nil {
		return linkproto.HookResult{}, errors.New("wrapper answered a hook without a result")
	}
	return *resp.Hook, nil
}
