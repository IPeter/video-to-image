package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// maxForwardedHops bounds how many X-Forwarded-For entries are examined: a longer
// chain of our own proxies is not a real deployment, and the header is client input.
const maxForwardedHops = 32

// rateKeyIPv6Bits is the IPv6 prefix a client is rate limited by: subscribers and
// cloud VMs get at least a /64, so per-address buckets would be free to multiply.
const rateKeyIPv6Bits = 64

// ParseTrustedProxies parses V2I_TRUSTED_PROXIES entries: CIDRs ("10.42.0.0/16",
// "fd00::/8") or bare addresses (taken as /32 or /128). Blank entries are skipped;
// IPv4-mapped IPv6 forms are converted to IPv4 so they match unmapped peers.
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, raw := range list {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(v, "/") {
			pp, err := netip.ParsePrefix(v)
			if err != nil {
				return nil, fmt.Errorf("httpapi: trusted proxy %q: %w", v, err)
			}
			p = pp
		} else {
			a, err := netip.ParseAddr(v)
			if err != nil {
				return nil, fmt.Errorf("httpapi: trusted proxy %q: %w", v, err)
			}
			a = a.WithZone("")
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// clientAddr returns the caller's address. Forwarding headers are client input, so
// they are only honoured when the TCP peer is one of Deps.TrustedProxies:
//
//   - X-Forwarded-For is walked from the RIGHT (each proxy appends the address it
//     saw), skipping trusted proxies; the first untrusted address is the client. If
//     every entry is a trusted proxy the leftmost one is used, and an unparsable entry
//     stops the walk at the last trusted hop (what lies left of it is unknowable).
//   - X-Real-IP is used only when there is no X-Forwarded-For at all.
//   - Deps.ClientIPHeader (e.g. Cloudflare's CF-Connecting-IP), when configured and
//     parsable, takes precedence over both: behind a CDN the proxy chain in front of
//     the trusted ingress is the CDN's, and it states the visitor address directly.
//
// ok is false when RemoteAddr itself is not an IP (e.g. a unix socket).
func (s *Server) clientAddr(r *http.Request) (netip.Addr, bool) {
	peer, ok := parseAddr(r.RemoteAddr)
	if !ok || !s.trustedProxy(peer) {
		return peer, ok
	}
	if s.d.ClientIPHeader != "" {
		if a, ok := parseAddr(r.Header.Get(s.d.ClientIPHeader)); ok {
			return a, true
		}
	}
	if a, found := s.forwardedClient(r.Header.Values("X-Forwarded-For"), peer); found {
		return a, true
	}
	if a, ok := parseAddr(r.Header.Get("X-Real-IP")); ok {
		return a, true
	}
	return peer, true
}

// forwardedClient walks X-Forwarded-For values from the right (see clientAddr).
// found is false when the header holds no entry at all.
func (s *Server) forwardedClient(values []string, peer netip.Addr) (addr netip.Addr, found bool) {
	client := peer // the nearest hop known so far
	hops := 0
	for i := len(values) - 1; i >= 0; i-- {
		rest := values[i]
		for rest != "" {
			var entry string
			if j := strings.LastIndexByte(rest, ','); j >= 0 {
				entry, rest = rest[j+1:], rest[:j]
			} else {
				entry, rest = rest, ""
			}
			if strings.TrimSpace(entry) == "" {
				continue // "a,,b" or a trailing comma
			}
			found = true
			a, ok := parseAddr(entry)
			if !ok {
				return client, true
			}
			if !s.trustedProxy(a) {
				return a, true
			}
			client = a
			if hops++; hops >= maxForwardedHops {
				return client, true
			}
		}
	}
	return client, found
}

// trustedProxy reports whether a is inside Deps.TrustedProxies.
func (s *Server) trustedProxy(a netip.Addr) bool {
	for _, p := range s.d.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP is the canonical text form of clientAddr, or the raw RemoteAddr when that
// is not an IP.
func (s *Server) clientIP(r *http.Request) string {
	if a, ok := s.clientAddr(r); ok {
		return a.String()
	}
	return r.RemoteAddr
}

// rateKey is the in-memory key of every per-client limit: the IPv4 address, or the
// IPv6 /64 the address belongs to (one subscriber, however many addresses they
// rotate through). It is never persisted.
func (s *Server) rateKey(r *http.Request) string {
	a, ok := s.clientAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	if a.Is6() {
		return netip.PrefixFrom(a, rateKeyIPv6Bits).Masked().String()
	}
	return a.String()
}

// parseAddr accepts "ip", "ip:port" and "[ipv6]:port" and returns the address with
// IPv4-mapped IPv6 unmapped and any zone dropped, so one client maps to one key.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	norm := func(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }
	if a, err := netip.ParseAddr(s); err == nil {
		return norm(a), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return norm(ap.Addr()), true
	}
	if inner, ok := strings.CutPrefix(s, "["); ok {
		if inner, ok = strings.CutSuffix(inner, "]"); ok {
			if a, err := netip.ParseAddr(inner); err == nil {
				return norm(a), true
			}
		}
	}
	return netip.Addr{}, false
}

// ipHash is hex(HMAC-SHA256(key, client IP)), stored on the job so raw client IPs are
// never persisted. The key is Deps.IPHashSalt (or a random per-process key, see New):
// without it the small IPv4 space could simply be enumerated.
func (s *Server) ipHash(r *http.Request) string {
	m := hmac.New(sha256.New, s.ipKey)
	_, _ = m.Write([]byte(s.clientIP(r)))
	return hex.EncodeToString(m.Sum(nil))
}

// validInternalToken reports whether token equals Deps.InternalToken, in constant
// time. An unset InternalToken never matches.
func (s *Server) validInternalToken(token string) bool {
	if s.d.InternalToken == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.d.InternalToken)) == 1
}
