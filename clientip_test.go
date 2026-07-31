package middlemonitor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func requestWith(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/api/orders", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// The whole point of recording the address is telling a scanner apart from real
// traffic. Behind a proxy the socket only shows the proxy, so a deployment that
// reads RemoteAddr labels every request with its own reverse proxy and the
// signal is lost.
func TestResolveClientIP_PrefersForwardedOverSocket(t *testing.T) {
	cfg := NewConfig("http://localhost:1", "svc", "tok")
	cfg.ClientIP = ClientIPFull

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"cloudflare wins over the chain", map[string]string{"CF-Connecting-IP": "203.0.113.7", "X-Forwarded-For": "198.51.100.4"}, "203.0.113.7"},
		{"leftmost entry of the chain is the client", map[string]string{"X-Forwarded-For": "203.0.113.7, 198.51.100.4, 127.0.0.1"}, "203.0.113.7"},
		{"x-real-ip when nothing else is set", map[string]string{"X-Real-IP": "203.0.113.7"}, "203.0.113.7"},
		{"socket address when no proxy is in front", nil, "192.0.2.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveClientIP(cfg, requestWith("192.0.2.1:54321", tc.headers)); got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// The default must not store an address that identifies a person: only the
// network it came from, which is what makes two hits recognisably the same scan.
func TestResolveClientIP_AnonymizedByDefault(t *testing.T) {
	cfg := NewConfig("http://localhost:1", "svc", "tok")
	if cfg.ClientIP != ClientIPAnonymized {
		t.Fatalf("default mode must be anonymized, got %q", cfg.ClientIP)
	}

	if got := resolveClientIP(cfg, requestWith("192.0.2.1:1", map[string]string{"X-Forwarded-For": "203.0.113.42"})); got != "203.0.113.0" {
		t.Errorf("want the host part dropped, got %q", got)
	}
	if got := resolveClientIP(cfg, requestWith("192.0.2.1:1", map[string]string{"X-Forwarded-For": "2001:db8:1234:5678::1"})); got != "2001:db8:1234::" {
		t.Errorf("want the IPv6 address reduced to its /48, got %q", got)
	}
}

// Off means nothing is recorded — a deployment that turns collection off must
// not leak the address through the socket fallback.
func TestResolveClientIP_OffRecordsNothing(t *testing.T) {
	cfg := NewConfig("http://localhost:1", "svc", "tok")
	cfg.ClientIP = ClientIPOff

	if got := resolveClientIP(cfg, requestWith("192.0.2.1:1", map[string]string{"X-Forwarded-For": "203.0.113.42"})); got != "" {
		t.Errorf("want no address, got %q", got)
	}
}

// A forwarded header is caller-controlled: anything that is not an address is
// dropped rather than stored, so the attribute never carries injected text.
func TestResolveClientIP_UnparseableIsDropped(t *testing.T) {
	cfg := NewConfig("http://localhost:1", "svc", "tok")
	cfg.ClientIP = ClientIPFull

	if got := resolveClientIP(cfg, requestWith("192.0.2.1:1", map[string]string{"X-Forwarded-For": "not-an-ip"})); got != "" {
		t.Errorf("want no address, got %q", got)
	}
}
