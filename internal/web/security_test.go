package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

func TestInterfacePagesCarryTheStrictPolicy(t *testing.T) {
	b := newBrowser(t, stubStore{})

	resp, _ := b.get("/login")

	want := map[string]string{
		headerCSP:        appCSP,
		headerNoSniff:    "nosniff",
		headerFrameOpts:  "DENY",
		headerReferrer:   "same-origin",
		headerCOOP:       "same-origin",
		headerPermission: permissionsPolicy,
	}
	for name, value := range want {
		if got := resp.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

// The policy is affordable only because no template carries an inline script or
// an inline style. If one ever does, this is the test that should fail — before
// somebody reaches for 'unsafe-inline' to make the page work again.
func TestTheStrictPolicyNeedsNoUnsafeInline(t *testing.T) {
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "unsafe-hashes", "*"} {
		if strings.Contains(appCSP, forbidden) {
			t.Errorf("the policy contains %q: %s", forbidden, appCSP)
		}
	}
	// default-src 'none' is what makes the rest of it a list of what is allowed
	// rather than a list of what happens to be mentioned.
	if !strings.HasPrefix(appCSP, "default-src 'none'") {
		t.Errorf("the policy does not start from a default of none: %s", appCSP)
	}
}

// HSTS is a promise about a hostname, and making it over plain HTTP is a
// promise about somebody else's site as often as about ours.
func TestHSTSOnlyWhenReachedOverTLS(t *testing.T) {
	t.Run("plain http", func(t *testing.T) {
		b := newBrowser(t, stubStore{})
		resp, _ := b.get("/login")
		if got := resp.Header.Get(headerHSTS); got != "" {
			t.Errorf("Strict-Transport-Security = %q on an http instance, want none", got)
		}
	})

	t.Run("https", func(t *testing.T) {
		b := newBrowserWith(t, stubStore{}, func(o *Options) {
			o.BaseURL = "https://restest.test"
			o.Sessions = newSessionManager(true)
		})
		resp, _ := b.get("/login")
		if got := resp.Header.Get(headerHSTS); got != hstsValue {
			t.Errorf("Strict-Transport-Security = %q, want %q", got, hstsValue)
		}
	})
}

// A project's response body is written by whoever owns the project and served
// from this origin. Sandboxing it is what stops a mock endpoint from being a
// way to run script against the interface's own origin.
func TestMockResponsesAreSandboxed(t *testing.T) {
	b := withMocks(t, mockEndpoint{
		method: "GET", path: "/page", status: 200,
		body:    `<html><body><script>fetch('/projects')</script></body></html>`,
		headers: core.Headers{"Content-Type": "text/html; charset=utf-8"},
	})

	resp, _ := b.get("/m/shop/page")

	if got := resp.Header.Get(headerCSP); got != mockCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", got, mockCSP)
	}
	if got := resp.Header.Get(headerNoSniff); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	// The endpoint's own Content-Type still arrives: sandboxing decides what a
	// browser may do with the body, not what the body claims to be.
	if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the endpoint's own", got)
	}
}

// An endpoint's headers are applied inside the handler, so a project that names
// Content-Security-Policy would overwrite the sandbox for every other user of
// the instance — unless the header is written after the handler has had its
// turn, which is what mockHeaderWriter is for.
func TestAnEndpointCannotOverrideTheSandbox(t *testing.T) {
	b := withMocks(t, mockEndpoint{
		method: "GET", path: "/escape", status: 200, body: "<h1>hi</h1>",
		headers: core.Headers{
			headerCSP:       "default-src *",
			headerNoSniff:   "",
			headerFrameOpts: "ALLOWALL",
		},
	})

	resp, _ := b.get("/m/shop/escape")

	if got := resp.Header.Get(headerCSP); got != mockCSP {
		t.Errorf("an endpoint replaced the policy: Content-Security-Policy = %q, want %q", got, mockCSP)
	}
	if got := resp.Header.Get(headerNoSniff); got != "nosniff" {
		t.Errorf("an endpoint cleared X-Content-Type-Options: %q", got)
	}
	if got := resp.Header.Get(headerFrameOpts); got != "DENY" {
		t.Errorf("an endpoint replaced X-Frame-Options: %q", got)
	}
}

// A browser client fetching a mock from a page on another origin is exactly
// what a mock server is for, so the one header that would refuse it is the one
// header the mock set does not carry.
func TestMockResponsesDoNotBlockCrossOriginReads(t *testing.T) {
	b := withMocks(t, mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	resp, _ := b.get("/m/shop/users")

	if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != "" {
		t.Errorf("Cross-Origin-Resource-Policy = %q, want none on mock traffic", got)
	}
}

// A 404 from the mock server is still the mock server answering, and it carries
// a body somebody wrote a path into.
func TestAMockRefusalIsSandboxedToo(t *testing.T) {
	b := withMocks(t, mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	resp, _ := b.get("/m/shop/nothing-here")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get(headerCSP); got != mockCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", got, mockCSP)
	}
}

// Static assets are ours, and get the strict policy like every other page: they
// are served from the same origin and a stylesheet is not a special case.
func TestStaticAssetsCarryTheStrictPolicy(t *testing.T) {
	b := newBrowser(t, stubStore{})

	resp, _ := b.get("/static/css/app.css")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(headerCSP); got != appCSP {
		t.Errorf("Content-Security-Policy = %q, want the interface policy", got)
	}
}
