package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dmitrykvasnikov/restest/internal/metrics"
	"github.com/dmitrykvasnikov/restest/internal/mock"
)

// An instance with metrics off answers 404 there, which is a truer answer than
// 200 with an empty body — a scraper is told there is nothing here rather than
// left to conclude the process serves no traffic.
func TestMetricsAreNotServedWhenTheyAreOff(t *testing.T) {
	b := newBrowser(t, stubStore{})

	resp, _ := b.get(pathMetrics)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 on an instance without metrics", resp.StatusCode)
	}
}

func TestMetricsAreServedWhenTheyAreOn(t *testing.T) {
	b := newBrowserWith(t, stubStore{}, func(o *Options) {
		o.Metrics = metrics.New("test")
	})

	resp, body := b.get(pathMetrics)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "restest_build_info") {
		t.Errorf("the scrape does not carry the build info: %s", firstLines(body))
	}
}

// The endpoint names every project's traffic volume and this process's memory
// layout. An instance that publishes it as it stands should be able to guard it
// without a proxy in front.
func TestTheMetricsTokenGuardsTheEndpoint(t *testing.T) {
	b := newBrowserWith(t, stubStore{}, func(o *Options) {
		o.Metrics = metrics.New("test")
		o.MetricsToken = "scrape-me"
	})

	t.Run("no token", func(t *testing.T) {
		resp, _ := b.get(pathMetrics)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get(headerWWWAuth); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
		}
	})

	t.Run("the wrong token", func(t *testing.T) {
		resp, _ := b.script("scrape-you").get(pathMetrics)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("the right token", func(t *testing.T) {
		resp, body := b.script("scrape-me").get(pathMetrics)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(body, "restest_http_requests_total") {
			t.Errorf("the scrape does not carry the request counter: %s", firstLines(body))
		}
	})
}

// The scrape must not be answered from the session, or a logged-in user's
// browser would be a way past the token — and the endpoint carries no CSRF
// token of its own either.
func TestTheMetricsTokenIsNotSatisfiedByASession(t *testing.T) {
	b := newBrowserWith(t, projectStore(), func(o *Options) {
		o.Metrics = metrics.New("test")
		o.MetricsToken = "scrape-me"
	})
	logIn(t, b)

	resp, _ := b.get(pathMetrics)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a logged-in browser with no metrics token", resp.StatusCode)
	}
}

// The mock server's four outcomes are what make a match rate computable, and
// they are counted where the matcher's decision is known and nowhere else.
func TestMockOutcomesAreCounted(t *testing.T) {
	counts := newCountingMetrics()
	b := withMockOptions(t, func(o *Options) { o.Metrics = counts },
		mockEndpoint{method: "GET", path: "/users", status: 200, body: `[]`})

	b.get("/m/shop/users")                       // matched
	b.get("/m/shop/nothing")                     // no_route
	b.do(http.MethodPost, "/m/shop/users", "{}") // wrong_method
	b.get("/m/nobody/users")                     // no_project

	want := map[string]int{"matched": 1, "no_route": 1, "wrong_method": 1, "no_project": 1}
	for outcome, n := range want {
		if counts.mockOutcomes[outcome] != n {
			t.Errorf("%s counted %d times, want %d (all: %v)",
				outcome, counts.mockOutcomes[outcome], n, counts.mockOutcomes)
		}
	}
}

func TestOutcomeLabelsCoverTheMatcher(t *testing.T) {
	cases := map[mock.Outcome]string{
		mock.Matched:     "matched",
		mock.NoProject:   "no_project",
		mock.NoRoute:     "no_route",
		mock.WrongMethod: "wrong_method",
		mock.Outcome(0):  "unknown",
	}
	for outcome, want := range cases {
		if got := mockOutcomeLabel(outcome); got != want {
			t.Errorf("mockOutcomeLabel(%d) = %q, want %q", outcome, got, want)
		}
	}
}

// The surface a request is counted under comes from the pattern it matched, so
// that the label set stays the four things with genuinely different shapes.
func TestSurfacesAreTakenFromTheMatchedPattern(t *testing.T) {
	cases := map[string]string{
		patternMock:            metrics.SurfaceMock,
		"GET " + pathAPIRoot:   metrics.SurfaceAPI,
		"POST " + pathAPIReset: metrics.SurfaceAPI,
		"GET " + pathHealthz:   metrics.SurfaceOps,
		"GET " + pathReadyz:    metrics.SurfaceOps,
		"GET " + pathStatic:    metrics.SurfaceOps,
		"GET " + pathMetrics:   metrics.SurfaceOps,
		"GET /projects":        metrics.SurfaceApp,
		patternCatchAll:        metrics.SurfaceApp,
		"":                     metrics.SurfaceOther,
	}
	for pattern, want := range cases {
		if got := surfaceOf(pattern); got != want {
			t.Errorf("surfaceOf(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// A verb is written by the caller, and every distinct label value is a time
// series this process then holds for as long as it runs.
func TestMethodLabelsAreBounded(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS", "HEAD"} {
		if got := methodLabel(method); got != method {
			t.Errorf("methodLabel(%q) = %q, want it unchanged", method, got)
		}
	}
	for _, method := range []string{"BREW", "", "get", strings.Repeat("X", 500)} {
		if got := methodLabel(method); got != "other" {
			t.Errorf("methodLabel(%q) = %q, want other", method, got)
		}
	}
}

// firstLines keeps a failure message from carrying a whole scrape.
func firstLines(body string) string {
	lines := strings.SplitN(body, "\n", 6)
	if len(lines) > 5 {
		lines = append(lines[:5], "…")
	}
	return strings.Join(lines, "\n")
}
