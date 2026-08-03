package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// A mutating request with no token is refused, which is the whole point of the
// CSRF guard.
func TestPostWithoutTokenIsRefused(t *testing.T) {
	b := newBrowser(t, accountStore())

	resp, body := b.postRaw("/login", url.Values{
		"email":    {testUser.Email},
		"password": {"correct horse battery staple"},
	}, b.sameOriginHeaders("/login"))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// The overwhelmingly common cause is an expired page, so the message says
	// what to do rather than accusing anyone.
	if !strings.Contains(body, "expired") {
		t.Errorf("unhelpful rejection page:\n%s", body)
	}
	if strings.Contains(body, "Log out") {
		t.Error("the rejected request logged somebody in")
	}
}

// A valid token from another site is still a cross-site request.
func TestPostFromAnotherOriginIsRefused(t *testing.T) {
	b := newBrowser(t, accountStore())

	_, page := b.get("/login")

	headers := http.Header{"Origin": {"https://evil.example"}}
	resp, _ := b.postRaw("/login", url.Values{
		"email":      {testUser.Email},
		"password":   {"correct horse battery staple"},
		"csrf_token": {csrfToken(t, page)},
	}, headers)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a cross-origin post", resp.StatusCode)
	}
}

// A request that matched no route has nothing to protect, so it gets the answer
// it earned rather than a CSRF rejection.
func TestUnroutedPostGets405NotACSRFFailure(t *testing.T) {
	b := newBrowser(t, accountStore())

	resp, body := b.postRaw(pathHealthz, url.Values{}, nil)

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}
	if !strings.Contains(body, "Not that way") {
		t.Errorf("unexpected page:\n%s", body)
	}
}

func TestUnknownPathGetsTheApplicationsOwnPage(t *testing.T) {
	b := newBrowser(t, accountStore())

	resp, body := b.get("/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No such page") {
		t.Errorf("not our 404 page:\n%s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", ct)
	}
}

func TestHomeSendsVisitorsWhereTheyBelong(t *testing.T) {
	b := newBrowser(t, accountStore()).noFollow()

	resp, _ := b.get("/")
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("anonymous visitor sent to %q, want /login", got)
	}

	b.client.CheckRedirect = nil
	logIn(t, b)
	b.noFollow()

	resp, _ = b.get("/")
	if got := resp.Header.Get("Location"); got != "/projects" {
		t.Errorf("logged-in visitor sent to %q, want /projects", got)
	}
}

var assetPattern = regexp.MustCompile(`/static/css/app\.css\?v=([0-9a-f]{12})`)

// Asset URLs carry a hash of the embedded assets, which is what lets them be
// cached forever and still change when a deployment changes them.
func TestAssetsAreVersionedAndCacheable(t *testing.T) {
	b := newBrowser(t, accountStore())

	_, page := b.get("/login")
	m := assetPattern.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no versioned stylesheet link on the page:\n%s", page)
	}

	resp, body := b.get("/static/css/app.css?v=" + m[1])
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stylesheet status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "tailwind") && len(body) < 1000 {
		t.Errorf("the stylesheet looks empty (%d bytes)", len(body))
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want it immutable", got)
	}
}

// Static files are served without touching the session store; a page with two
// assets should not cost two extra round trips to the database.
func TestStaticFilesSkipTheSession(t *testing.T) {
	b := newBrowser(t, accountStore())
	logIn(t, b)

	resp, _ := b.get("/static/js/htmx.min.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Errorf("a static asset touched the session: %v", resp.Cookies())
	}
}

func TestStaticDirectoryListingIsRefused(t *testing.T) {
	b := newBrowser(t, accountStore())

	resp, _ := b.get("/static/css/")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a directory", resp.StatusCode)
	}
}

// Every page carries the token, so a form on any of them can post.
func TestEveryPageCarriesACSRFToken(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	for _, path := range []string{"/login", "/register", "/projects", "/projects/new", "/projects/checkout", "/projects/checkout/edit"} {
		t.Run(path, func(t *testing.T) {
			resp, body := b.get(path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if !csrfPattern.MatchString(body) {
				t.Errorf("no CSRF field on the page:\n%s", body)
			}
		})
	}
}
