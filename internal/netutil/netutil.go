// Package netutil holds small networking helpers shared by the proxy and the
// admin API.
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the client address, honouring X-Forwarded-For / X-Real-IP
// only when the direct peer is a trusted proxy.
func ClientIP(r *http.Request, trusted []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipTrusted(peer, trusted) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Walk from the right, skipping trusted proxies.
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				break
			}
			if !ipTrusted(ip, trusted) || i == 0 {
				return ip.String()
			}
		}
	}
	if xr := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-Ip"))); xr != nil {
		return xr.String()
	}
	return host
}

func ipTrusted(ip net.IP, trusted []string) bool {
	for _, t := range trusted {
		if strings.Contains(t, "/") {
			if _, n, err := net.ParseCIDR(t); err == nil && n.Contains(ip) {
				return true
			}
		} else if p := net.ParseIP(t); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}

// truncateStr cuts s to at most n bytes without splitting a UTF-8 sequence.
