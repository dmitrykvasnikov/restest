package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Who a request is from.
//
// This is the question M0 deferred and M7 answers. The access log, the request
// inspector and the per-address rate limit all need one address per request,
// and until now that address was always the peer's — because believing
// X-Forwarded-For without knowing which proxies to trust lets any caller claim
// any address, which turns a rate limit into a formality and a log into
// fiction.
//
// The rule is the standard one, and it is worth stating precisely because the
// common wrong version — "take the first entry of X-Forwarded-For" — is the
// version an attacker writes the header for. A proxy *appends* the address it
// received the request from, so the rightmost entry is the only one written by
// something we chose to trust, and everything to its left was written by
// whatever came before it. Walking right to left through the trusted hops and
// stopping at the first address that is not one of ours yields the nearest
// address nobody in the chain could have forged.
//
// With no trusted proxies configured — the default — the header is not read at
// all.

// clientIPKey carries the resolved address down the middleware chain, so that
// the walk happens once per request rather than once per reader of it.
type clientIPKey struct{}

// headerForwardedFor is the de facto header. RFC 7239's `Forwarded` is the
// standardised one and is not read here: nothing in front of this application
// is configured to send it by default, and a header that is never populated is
// a parser that is never exercised.
const headerForwardedFor = "X-Forwarded-For"

// maxForwardedHops bounds the walk. A header with more entries than this is a
// caller padding it — a real chain is a proxy or two — and the walk stops
// rather than parsing a list somebody sized for the purpose.
const maxForwardedHops = 16

// ClientIP returns the address a request is attributed to. Outside the
// middleware chain it is "", which no caller here can reach: withClientIP runs
// above everything.
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

// withClientIP resolves the client address once and puts it in the context.
func (s *Server) withClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientIPKey{}, s.resolveClientIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveClientIP walks the forwarded chain as described above.
func (s *Server) resolveClientIP(r *http.Request) string {
	peer := peerIP(r)
	if len(s.trustedProxies) == 0 {
		return peer
	}

	addr, err := netip.ParseAddr(peer)
	if err != nil || !s.trusted(addr) {
		// The request did not come from a proxy we chose, so nothing it carries
		// about its own origin is worth reading.
		return peer
	}

	hops := forwardedHops(r.Header.Values(headerForwardedFor))
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(hops[i])
		if err != nil {
			// An entry we cannot read is an entry we cannot vouch for, and
			// everything to its left was written by whatever wrote it. The last
			// address that was trustworthy is as far as this can go.
			break
		}
		addr = hop
		if !s.trusted(addr) {
			break
		}
	}
	return addr.String()
}

// trusted reports whether an address is one of the proxies in front of us.
func (s *Server) trusted(addr netip.Addr) bool {
	// An IPv4 address arriving over a dual-stack listener is ::ffff:10.0.0.1,
	// which no IPv4 prefix contains. Unmapping makes 10.0.0.0/8 mean what its
	// author meant.
	addr = addr.Unmap()

	for _, prefix := range s.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedHops flattens the header into the addresses it names, in order.
// Repeated headers are one list, which is what net/http would have joined with
// commas anyway, and the whole thing is truncated to the hop limit.
func forwardedHops(values []string) []string {
	var hops []string
	for _, value := range values {
		for _, field := range strings.Split(value, ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			// Some proxies append a port, and IPv6 in that form arrives
			// bracketed. Either way the address is what matters.
			if ap, err := netip.ParseAddrPort(field); err == nil {
				field = ap.Addr().String()
			}
			hops = append(hops, strings.Trim(field, "[]"))
			if len(hops) >= maxForwardedHops {
				return hops
			}
		}
	}
	return hops
}

// peerIP is the address at the other end of the connection, with the port
// stripped. It is the truth about who is talking to this process, whoever they
// say they are talking on behalf of.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
