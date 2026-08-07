package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// resolveFrom runs the resolver over one synthetic request.
func resolveFrom(t *testing.T, trusted []string, peer string, forwarded ...string) string {
	t.Helper()

	prefixes := make([]netip.Prefix, 0, len(trusted))
	for _, c := range trusted {
		prefixes = append(prefixes, netip.MustParsePrefix(c))
	}

	s := &Server{trustedProxies: prefixes}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	for _, value := range forwarded {
		req.Header.Add(headerForwardedFor, value)
	}
	return s.resolveClientIP(req)
}

// The default. Believing the header without knowing who is in front of us lets
// any caller claim any address, which makes the per-address rate limit a
// formality and the request log a work of fiction.
func TestForwardedForIsIgnoredWithoutATrustedProxy(t *testing.T) {
	got := resolveFrom(t, nil, "203.0.113.9:44321", "10.0.0.1, 198.51.100.4")
	if got != "203.0.113.9" {
		t.Errorf("client address = %q, want the peer's 203.0.113.9", got)
	}
}

func TestForwardedForIsIgnoredFromAnUntrustedPeer(t *testing.T) {
	// The proxy is trusted, but this request did not come through it.
	got := resolveFrom(t, []string{"10.0.0.0/8"}, "203.0.113.9:44321", "198.51.100.4")
	if got != "203.0.113.9" {
		t.Errorf("client address = %q, want the peer's 203.0.113.9", got)
	}
}

func TestTheClientIsTheRightmostAddressNoTrustedHopWrote(t *testing.T) {
	cases := []struct {
		name      string
		trusted   []string
		peer      string
		forwarded []string
		want      string
	}{
		{
			name: "one proxy", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"198.51.100.4"},
			want: "198.51.100.4",
		},
		{
			name: "two proxies, both ours", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"198.51.100.4, 10.0.0.9"},
			want: "198.51.100.4",
		},
		{
			// The client wrote the first two entries before the request ever
			// reached us. Taking the leftmost — the common wrong version — would
			// hand the caller whatever address they chose.
			name: "a forged prefix", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"1.1.1.1, 2.2.2.2, 198.51.100.4"},
			want: "198.51.100.4",
		},
		{
			name: "repeated headers are one chain", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"198.51.100.4", "10.0.0.9"},
			want: "198.51.100.4",
		},
		{
			// Nothing left that is not ours: the leftmost entry is as far back
			// as the chain goes.
			name: "every hop is ours", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"10.0.0.2, 10.0.0.9"},
			want: "10.0.0.2",
		},
		{
			name: "no header at all", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234",
			want: "10.0.0.5",
		},
		{
			name: "a hop with a port", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"198.51.100.4:51000"},
			want: "198.51.100.4",
		},
		{
			name: "an IPv6 client", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"2001:db8::42"},
			want: "2001:db8::42",
		},
		{
			name: "a bracketed IPv6 client with a port", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"[2001:db8::42]:51000"},
			want: "2001:db8::42",
		},
		{
			// An IPv4 peer arriving over a dual-stack listener. Without
			// unmapping, 10.0.0.0/8 would not contain it and the proxy would
			// not be recognised as one.
			name: "a mapped IPv4 peer", trusted: []string{"10.0.0.0/8"},
			peer: "[::ffff:10.0.0.5]:1234", forwarded: []string{"198.51.100.4"},
			want: "198.51.100.4",
		},
		{
			// An entry we cannot parse is an entry nothing vouches for, and
			// everything left of it was written by whatever wrote it.
			name: "an unreadable hop", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{"198.51.100.4, unknown, 10.0.0.9"},
			want: "10.0.0.9",
		},
		{
			name: "an empty header", trusted: []string{"10.0.0.0/8"},
			peer: "10.0.0.5:1234", forwarded: []string{""},
			want: "10.0.0.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveFrom(t, tc.trusted, tc.peer, tc.forwarded...); got != tc.want {
				t.Errorf("client address = %q, want %q", got, tc.want)
			}
		})
	}
}

// A header padded with thousands of entries is somebody making the walk the
// expensive part of the request.
func TestAForwardedChainIsBounded(t *testing.T) {
	var header string
	for range 5000 {
		header += "10.0.0.9, "
	}
	header += "198.51.100.4"

	got := resolveFrom(t, []string{"10.0.0.0/8"}, "10.0.0.5:1234", header)
	// The walk stops at the hop limit, so the client the padding was hiding is
	// never reached — which is the safe answer, not the flattering one.
	if got != "10.0.0.9" {
		t.Errorf("client address = %q, want a trusted hop rather than the far end of a padded chain", got)
	}
}

// The address the rest of the application sees is the resolved one, and it is
// resolved once: the access log, the inspector and the limiters must agree, and
// three separate walks are three chances to disagree.
func TestTheResolvedAddressReachesTheHandlers(t *testing.T) {
	s := &Server{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}

	var seen string
	handler := s.withClientIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = remoteIP(r)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set(headerForwardedFor, "198.51.100.4")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "198.51.100.4" {
		t.Errorf("the handler saw %q, want 198.51.100.4", seen)
	}
}

// Outside the middleware chain — which is where the handler tests run their
// own middleware — the peer address is the answer, not an empty string.
func TestRemoteIPFallsBackToThePeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:44321"

	if got := remoteIP(req); got != "203.0.113.9" {
		t.Errorf("remoteIP = %q, want 203.0.113.9", got)
	}
}
