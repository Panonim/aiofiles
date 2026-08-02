package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("parse %q: %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

func request(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	// The topology this is written against: an operator's reverse proxy on
	// 10.0.0.0/8 in front of the bundled nginx on loopback.
	trust := New(prefixes(t, "10.0.0.0/8"))

	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{"no proxy at all", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"an untrusted peer proves nothing", "203.0.113.7:5000",
			map[string]string{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.7"},
		{"bundled nginx only", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "203.0.113.7"}, "203.0.113.7"},
		// The client's forgery sits to the left of what the proxies appended.
		{"forged prefix", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.7"}, "203.0.113.7"},
		// External proxy -> nginx -> app: nginx appends the external proxy,
		// which is trusted, so the walk continues one hop left.
		{"two hops", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.5"}, "203.0.113.7"},
		{"forged prefix behind two hops", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.7, 10.0.0.5"}, "203.0.113.7"},
		// A client that fills the header with trusted-looking addresses only
		// moves the answer within its own forgeries; the appended hop still wins.
		{"forged trusted addresses do not shift the walk", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "10.0.0.9, 10.0.0.8, 203.0.113.7"}, "203.0.113.7"},
		{"garbage entries are skipped", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "not-an-ip, 203.0.113.7"}, "203.0.113.7"},
		{"all garbage falls back to X-Real-IP", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "junk", "X-Real-IP": "203.0.113.9"}, "203.0.113.9"},
		{"nothing forwarded falls back to the peer", "127.0.0.1:1", nil, "127.0.0.1"},
		{"ipv6 client", "127.0.0.1:1",
			map[string]string{"X-Forwarded-For": "2001:db8::1, 10.0.0.5"}, "2001:db8::1"},
		{"ipv4-mapped peer is unmapped before matching", "[::ffff:127.0.0.1]:1",
			map[string]string{"X-Forwarded-For": "203.0.113.7"}, "203.0.113.7"},
		{"unparseable remote address", "not-an-address", nil, "not-an-address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := trust.ClientIP(request(tc.remoteAddr, tc.headers)); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// Splitting a forgery across repeated headers must not hide the entry the proxy
// appended to the last one.
func TestClientIPRepeatedForwardedForHeaders(t *testing.T) {
	r := request("127.0.0.1:1", nil)
	r.Header.Add("X-Forwarded-For", "1.2.3.4")
	r.Header.Add("X-Forwarded-For", "5.6.7.8, 203.0.113.7")
	if got := New(nil).ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want %q", got, "203.0.113.7")
	}
}

// A trusted proxy is the only thing that can make a plain-HTTP hop look like an
// HTTPS session, because that is what decides the Secure cookie attribute.
func TestIsHTTPS(t *testing.T) {
	trust := New(prefixes(t, "10.0.0.0/8"))
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       bool
	}{
		{"plain http", "127.0.0.1:1", nil, false},
		{"forwarded https from the proxy", "127.0.0.1:1",
			map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"chained value takes the first entry", "127.0.0.1:1",
			map[string]string{"X-Forwarded-Proto": "https, http"}, true},
		{"case insensitive", "10.0.0.5:1",
			map[string]string{"X-Forwarded-Proto": "HTTPS"}, true},
		{"claimed by an untrusted client", "203.0.113.7:5000",
			map[string]string{"X-Forwarded-Proto": "https"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := trust.IsHTTPS(request(tc.remoteAddr, tc.headers)); got != tc.want {
				t.Errorf("IsHTTPS = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostname(t *testing.T) {
	trust := New(nil)
	tests := []struct {
		name       string
		remoteAddr string
		host       string
		forwarded  string
		want       string
	}{
		{"host header", "127.0.0.1:1", "aio.example.com", "", "aio.example.com"},
		{"port is stripped", "127.0.0.1:1", "aio.example.com:8443", "", "aio.example.com"},
		{"lowercased", "127.0.0.1:1", "AIO.Example.COM", "", "aio.example.com"},
		{"ip literal", "127.0.0.1:1", "192.168.16.35:19882", "", "192.168.16.35"},
		{"ipv6 literal keeps its brackets", "127.0.0.1:1", "[fd00::1]:19882", "", "[fd00::1]"},
		{"forwarded host from a trusted proxy wins", "127.0.0.1:1",
			"127.0.0.1:1144", "aio.example.com", "aio.example.com"},
		{"forwarded host from an untrusted peer is ignored", "203.0.113.7:5000",
			"192.168.16.35:19882", "aio.example.com", "192.168.16.35"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := request(tc.remoteAddr, nil)
			r.Host = tc.host
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Host", tc.forwarded)
			}
			if got := trust.Hostname(r); got != tc.want {
				t.Errorf("Hostname = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOrigin(t *testing.T) {
	r := request("127.0.0.1:1", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "aio.example.com",
	})
	scheme, host := New(nil).Origin(r)
	if scheme != "https" || host != "aio.example.com" {
		t.Errorf("Origin = %q, %q, want https, aio.example.com", scheme, host)
	}
}
