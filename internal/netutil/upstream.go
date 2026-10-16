package netutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// UpstreamPolicy restricts where the gateway may send requests (SSRF
// protection). Keys carry their own base URL, so without it an admin-API
// credential or a mistake could point the gateway at internal services.
type UpstreamPolicy struct {
	AllowedHosts  []string // host names or "*.suffix" patterns; empty = any host
	AllowInsecure bool     // allow http:// upstreams
	AllowPrivate  bool     // allow loopback, private, link-local and similar addresses
}

// ErrBlockedAddress is returned when a connection to a non-public address
// is refused.
var ErrBlockedAddress = errors.New("upstream address is not public (allow private upstreams in settings to use it)")

// CheckURL validates an upstream base URL against the policy.
func (p UpstreamPolicy) CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid upstream URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecure {
			return fmt.Errorf("upstream URL must use https (allow insecure upstreams in settings to use http)")
		}
	default:
		return fmt.Errorf("upstream URL must be http(s)")
	}
	if u.User != nil {
		return fmt.Errorf("upstream URL must not contain credentials")
	}
	host := strings.ToLower(u.Hostname())
	if !p.hostAllowed(host) {
		return fmt.Errorf("upstream host %q is not in the allowed hosts list", host)
	}
	if ip, err := netip.ParseAddr(host); err == nil && !p.AllowPrivate && !IsPublic(ip) {
		return ErrBlockedAddress
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		if !p.AllowPrivate {
			return ErrBlockedAddress
		}
	}
	return nil
}

func (p UpstreamPolicy) hostAllowed(host string) bool {
	if len(p.AllowedHosts) == 0 {
		return true
	}
	for _, pat := range p.AllowedHosts {
		pat = strings.ToLower(strings.TrimSpace(pat))
		switch {
		case pat == "*":
			return true
		case strings.HasPrefix(pat, "*."):
			if strings.HasSuffix(host, pat[1:]) && len(host) > len(pat)-1 {
				return true
			}
		case pat == host:
			return true
		}
	}
	return false
}

// IsPublic reports whether ip is a globally routable unicast address.
func IsPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
}

// GuardedDialer returns a DialContext that refuses non-public addresses
// after DNS resolution (so a public name resolving to an internal address is
// blocked too), unless allow(addr) reports true for the dialed "host:port".
func GuardedDialer(timeout func() time.Duration, allow func(addr string) bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: timeout(), KeepAlive: 30 * time.Second}
		if !allow(addr) {
			d.Control = func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip, err := netip.ParseAddr(host)
				if err != nil || !IsPublic(ip) {
					return ErrBlockedAddress
				}
				return nil
			}
		}
		return d.DialContext(ctx, network, addr)
	}
}
