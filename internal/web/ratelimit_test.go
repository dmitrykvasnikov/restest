package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/metrics"
	"github.com/dmitrykvasnikov/restest/internal/ratelimit"
)

// countingMetrics is the Metrics interface with a tally instead of a registry,
// so a test can check that a refusal was counted without a scrape.
type countingMetrics struct {
	requests     int
	mockOutcomes map[string]int
	limited      map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{mockOutcomes: map[string]int{}, limited: map[string]int{}}
}

func (m *countingMetrics) ObserveRequest(string, string, int, time.Duration) { m.requests++ }
func (m *countingMetrics) ObserveMockOutcome(outcome string)                 { m.mockOutcomes[outcome]++ }
func (m *countingMetrics) ObserveRateLimited(scope string)                   { m.limited[scope]++ }
func (m *countingMetrics) Handler() http.Handler                             { return http.NotFoundHandler() }

// A rate of one per second still allows a burst, so the test has to spend it
// before it can see a refusal — which is the behaviour, not an inconvenience.
func TestMockTrafficIsRefusedOverThePerAddressLimit(t *testing.T) {
	counts := newCountingMetrics()
	b := withMockOptions(t, func(o *Options) {
		o.RateLimitIP = 1
		o.RateLimitProject = 0
		o.Metrics = counts
	}, mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	// The burst first: every one of these is served.
	for i := range ratelimit.MinBurst {
		resp, body := b.get("/m/shop/users")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d of the burst: status = %d, want 200: %s", i, resp.StatusCode, body)
		}
	}

	resp, body := b.get("/m/shop/users")

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}

	// The refusal is JSON in the mock server's own shape, because that is what
	// the client has been parsing all along.
	var refusal mockError
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatalf("the refusal is not a mock error: %v (%s)", err, body)
	}
	if !strings.Contains(refusal.Error, "1 mock requests a second") {
		t.Errorf("the refusal does not say what the limit is: %q", refusal.Error)
	}
	if refusal.Project != "shop" || refusal.Path != "/users" {
		t.Errorf("the refusal names %q %q, want shop /users", refusal.Project, refusal.Path)
	}

	if counts.limited[metrics.ScopeMockIP] != 1 {
		t.Errorf("refusals counted = %d, want 1", counts.limited[metrics.ScopeMockIP])
	}
}

func TestMockTrafficIsRefusedOverThePerProjectLimit(t *testing.T) {
	counts := newCountingMetrics()
	b := withMockOptions(t, func(o *Options) {
		// The address limit off, so the only thing that can refuse is the
		// project's own.
		o.RateLimitIP = 0
		o.RateLimitProject = 1
		o.Metrics = counts
	}, mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	for range ratelimit.MinBurst {
		b.get("/m/shop/users")
	}

	resp, body := b.get("/m/shop/users")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "this project") {
		t.Errorf("the refusal does not name the project limit: %s", body)
	}
	if counts.limited[metrics.ScopeMockProject] != 1 {
		t.Errorf("refusals counted under the project scope = %d, want 1",
			counts.limited[metrics.ScopeMockProject])
	}
}

// A slug nothing answers to must not become a key in the limiter's own table,
// or inventing slugs would be a way to fill it.
func TestAnUnknownProjectIsNotCounted(t *testing.T) {
	b := withMockOptions(t, func(o *Options) {
		o.RateLimitIP = 0
		o.RateLimitProject = 100
	}, mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	for range 5 {
		if resp, _ := b.get("/m/no-such-project/users"); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	}

	if got := b.srv.mockProjectLimiter.Len(); got != 0 {
		t.Errorf("the project limiter holds %d buckets, want 0 for a project that does not exist", got)
	}

	// And a real one is counted, so the check above is not passing because
	// nothing is ever counted.
	b.get("/m/shop/users")
	if got := b.srv.mockProjectLimiter.Len(); got != 1 {
		t.Errorf("the project limiter holds %d buckets after a real request, want 1", got)
	}
}

// The limiter sheds load, and a limiter that still writes a row for every
// request it refused sheds much less of it.
func TestARefusedRequestIsNotRecorded(t *testing.T) {
	log := newFakeLog()
	b := logBrowserWith(t, log, 1024, func(o *Options) { o.RateLimitIP = 1 }, echoEndpoint)

	for range ratelimit.MinBurst {
		b.get("/m/" + logSlug + echoEndpoint.path)
	}
	served := len(log.recorded())
	if served == 0 {
		t.Fatal("nothing was recorded at all, so this test could not fail")
	}

	for range 5 {
		resp, _ := b.get("/m/" + logSlug + echoEndpoint.path)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", resp.StatusCode)
		}
	}

	if got := len(log.recorded()); got != served {
		t.Errorf("the log grew by %d entries over five refused requests, want 0", got-served)
	}
}

// Off is off. An instance that sets a limit to zero gets no limiter at all,
// rather than one with a very large bucket.
func TestARateLimitOfZeroServesEverything(t *testing.T) {
	b := withMockOptions(t, func(o *Options) {
		o.RateLimitIP = 0
		o.RateLimitProject = 0
	}, mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	for i := range ratelimit.MinBurst * 3 {
		if resp, _ := b.get("/m/shop/users"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
}

// The API is the credentialed surface, and before M7 it was bounded by nothing
// at all. The limit is applied in requireAPIUser, so every route has it.
func TestTheManagementAPIIsRefusedOverItsLimit(t *testing.T) {
	counts := newCountingMetrics()
	store := stubStore{}
	b := newBrowserWith(t, store, func(o *Options) {
		o.RateLimitAPI = 1
		o.Metrics = counts
	})

	// No credential at all: the request is about to be a 401, and the limiter
	// counts it under the address — which is what stops a flood of guesses from
	// being free.
	var last *http.Response
	for range ratelimit.MinBurst + 1 {
		resp, _ := b.get("/api/v1/")
		last = resp
	}

	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", last.StatusCode)
	}
	if got := last.Header.Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}
	if counts.limited[metrics.ScopeAPI] != 1 {
		t.Errorf("API refusals counted = %d, want 1", counts.limited[metrics.ScopeAPI])
	}
}

// The token itself is never a map key: what is kept is the same SHA-256 the
// database stores. A limiter holds its keys for a minute after the request that
// carried them, and a secret should not be one of them.
func TestTheAPILimiterKeysOnTheTokenHashNotTheToken(t *testing.T) {
	const secret = "rst_pretend-this-is-a-real-token"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/", nil)

	key := apiLimitKey(req, secret, true)

	if strings.Contains(key, secret) {
		t.Errorf("the key holds the token itself: %q", key)
	}
	if want := "token:" + string(core.HashToken(secret)); key != want {
		t.Errorf("key = %q, want the hash the database stores", key)
	}
	// Two presentations of the same token share a bucket, and two different
	// tokens do not — which is the whole of what "per credential" means.
	if apiLimitKey(req, secret, true) != key {
		t.Error("the same token produced two different keys")
	}
	if apiLimitKey(req, secret+"x", true) == key {
		t.Error("two different tokens produced the same key")
	}
}

func TestTheAPILimiterKeysASessionOnItsUser(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/", nil)
	req.RemoteAddr = "203.0.113.9:44321"

	// Anonymous: nothing but the address to count it under.
	if got, want := apiLimitKey(req, "", false), "addr:203.0.113.9"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}

	// Logged in: the account, so that two people behind one office address do
	// not spend each other's allowance.
	withUser := req.WithContext(context.WithValue(req.Context(), userContextKey{}, testUser))
	if got, want := apiLimitKey(withUser, "", false), "user:"+testUser.ID.String(); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

// The interface is deliberately not limited: it is behind a session, every
// mutating request costs a form submission, and a limit there is mostly a way
// to lock somebody out of their own project while they are looking at it.
func TestTheInterfaceIsNotRateLimited(t *testing.T) {
	b := newBrowserWith(t, stubStore{}, func(o *Options) {
		o.RateLimitIP = 1
		o.RateLimitProject = 1
		o.RateLimitAPI = 1
	})

	for i := range ratelimit.MinBurst * 2 {
		if resp, _ := b.get("/login"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d to /login: status = %d, want 200", i, resp.StatusCode)
		}
	}
}
