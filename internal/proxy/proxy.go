// Package proxy decides how much of a request's X-Forwarded-* headers may be
// believed, and answers the two questions the rest of the app asks about a
// request that arrived through one or more reverse proxies: who the client
// really is, and what name and scheme it used.
//
// Forwarding headers are plain request headers: anything the client sends
// arrives intact unless a proxy overwrites or appends to it. They are therefore
// only meaningful when the immediate peer is a proxy we put there ourselves,
// which is what Trust encodes.
package proxy

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Loopback is trusted unconditionally: the bundled nginx reaches the Go process
// over 127.0.0.1, so without this every request in the shipped image would look
// like it came straight from nginx and the real client would be invisible.
var loopback = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// Trust is the set of addresses whose forwarding headers are believed. The zero
// value is not usable; build one with New.
type Trust struct {
	prefixes []netip.Prefix
}

// New returns a Trust covering loopback plus extra, which is
// config.TrustedProxies (TRUSTED_PROXIES). A nil extra is fine and yields the
// shipped default: believe the bundled nginx, nobody else.
func New(extra []netip.Prefix) *Trust {
	p := make([]netip.Prefix, 0, len(loopback)+len(extra))
	p = append(p, loopback...)
	p = append(p, extra...)
	return &Trust{prefixes: p}
}

// FromProxy reports whether the immediate peer is a trusted proxy, i.e. whether
// this request's X-Forwarded-* headers mean anything at all.
func (t *Trust) FromProxy(r *http.Request) bool {
	return t.trusted(peerIP(r))
}

func (t *Trust) trusted(ip string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	// An IPv4 client on a dual-stack listener shows up as ::ffff:127.0.0.1;
	// unmapping keeps a 127.0.0.0/8 prefix matching it.
	addr = addr.Unmap()
	for _, p := range t.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP is the address the rate limiter counts against: the last hop in the
// X-Forwarded-For chain that is not one of our own proxies.
//
// The chain is written left to right, each proxy appending the peer it saw, so
// the rightmost entry is the only one the nearest proxy wrote and everything to
// its left is hearsay from whoever came before. Walking right to left and
// stopping at the first untrusted address therefore lands on the real client
// however many trusted hops sit in front of it, and cannot be pushed further
// left by a client that pre-loads the header with forgeries.
func (t *Trust) ClientIP(r *http.Request) string {
	peer := peerIP(r)
	if !t.trusted(peer) {
		return peer // no proxy of ours wrote anything; headers prove nothing
	}

	hops := forwardedFor(r)
	for i := len(hops) - 1; i >= 0; i-- {
		// Garbage entries are skipped rather than returned: a client can put
		// anything in the header, and the entry our own proxy appended is
		// always further right and always parses.
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			continue
		}
		if !t.trusted(addr.String()) {
			return addr.Unmap().String()
		}
	}
	// Every hop was one of ours (a proxy chain talking to itself, or no chain
	// at all). X-Real-IP is a single value nginx overwrites, so it is the next
	// best thing before falling back to the socket.
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	return peer
}

// IsHTTPS reports whether the *client's* connection was TLS, which is what the
// Secure cookie attribute has to key on: the hop into the app is plain HTTP
// even when the browser is on HTTPS.
func (t *Trust) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !t.FromProxy(r) {
		return false
	}
	return strings.EqualFold(firstValue(r.Header.Get("X-Forwarded-Proto")), "https")
}

// Hostname is the name the client asked for, lowercased and without a port.
// X-Forwarded-Host wins over Host when a trusted proxy set it; a proxy that
// rewrites Host to its upstream is otherwise indistinguishable from a client
// lying about it.
func (t *Trust) Hostname(r *http.Request) string {
	host := r.Host
	if t.FromProxy(r) {
		if v := firstValue(r.Header.Get("X-Forwarded-Host")); v != "" {
			host = v
		}
	}
	return hostname(host)
}

// Origin reconstructs the origin the browser sees. The bundled nginx forwards
// $host, which drops the port, and it listens on a different port than the one
// published to the browser - so the port is simply not knowable here and only
// the scheme and hostname are returned.
func (t *Trust) Origin(r *http.Request) (scheme, host string) {
	scheme = "http"
	if t.IsHTTPS(r) {
		scheme = "https"
	}
	return scheme, t.Hostname(r)
}

// hostname strips the port and lowercases what is left. url.URL does the
// bracket handling for IPv6 literals, but keeps the brackets off the result, so
// they are put back: "[::1]" is the form that appears in Host headers and in
// ALLOWED_HOSTS.
func hostname(host string) string {
	h := (&url.URL{Host: host}).Hostname()
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return strings.ToLower(h)
}

// forwardedFor flattens every X-Forwarded-For header, in order, into one list
// of hops. Repeating the header is equivalent to one comma-joined value, and a
// client that splits its forgeries across several of them must not be able to
// hide the entry the proxy appended to the last one.
func forwardedFor(r *http.Request) []string {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				hops = append(hops, s)
			}
		}
	}
	return hops
}

// A chain of proxies appends to these headers; the first entry is the client's.
func firstValue(header string) string {
	first, _, _ := strings.Cut(header, ",")
	return strings.TrimSpace(first)
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
