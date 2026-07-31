package middlemonitor

import (
	"net"
	"net/http"
	"strings"
)

// ClientIPMode controls what the HTTP middlewares record as the caller's address.
type ClientIPMode string

const (
	// ClientIPAnonymized keeps the network the caller comes from and drops the
	// host part: enough to recognise a scanner, not enough to single out a person.
	ClientIPAnonymized ClientIPMode = "anonymized"

	// ClientIPFull records the address as received.
	ClientIPFull ClientIPMode = "full"

	// ClientIPOff records no address at all.
	ClientIPOff ClientIPMode = "off"
)

// forwardedHeaders are read in order, before falling back to the socket address.
// Behind a proxy (Caddy, nginx, Cloudflare) the socket only ever shows the proxy.
var forwardedHeaders = []string{"CF-Connecting-IP", "True-Client-IP", "X-Forwarded-For", "X-Real-IP"}

// resolveClientIP returns the caller address to record for this request, already
// reduced to what ClientIP allows. Empty means nothing is recorded — an
// unparseable address is dropped rather than stored raw.
func resolveClientIP(cfg *Config, r *http.Request) string {
	if cfg == nil || r == nil || cfg.ClientIP == ClientIPOff {
		return ""
	}

	raw := forwardedClientIP(r.Header)
	if raw == "" {
		raw = r.RemoteAddr
		if host, _, err := net.SplitHostPort(raw); err == nil {
			raw = host
		}
	}

	// An IPv6 socket address arrives bracketed once the port is stripped by hand.
	raw = strings.Trim(strings.TrimSpace(raw), "[]")

	ip := net.ParseIP(raw)
	if ip == nil {
		return ""
	}
	if cfg.ClientIP == ClientIPFull {
		return ip.String()
	}
	return anonymizeIP(ip)
}

// forwardedClientIP returns the first address a proxy put in the request, or "".
func forwardedClientIP(header http.Header) string {
	for _, name := range forwardedHeaders {
		v := strings.TrimSpace(header.Get(name))
		if v == "" {
			continue
		}
		// X-Forwarded-For is a chain: the client is the leftmost entry.
		if i := strings.Index(v, ","); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if v != "" {
			return v
		}
	}
	return ""
}

// anonymizeIP zeroes the host part: the last octet of an IPv4 address, the last
// 80 bits of an IPv6 one (the /48 a subscriber is assigned).
func anonymizeIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String()
	}
	return ip.Mask(net.CIDRMask(48, 128)).String()
}
