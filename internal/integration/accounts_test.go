//go:build integration

package integration

import (
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// site is a running application plus a client that keeps cookies — as close to
// a browser as a test gets without one.
type site struct {
	t      *testing.T
	client *http.Client
	url    string
	pool   *pgxpool.Pool
}

func newSite(t *testing.T) *site {
	t.Helper()

	pool := migratedPool(t, startPostgres(t))
	_, srv := newApp(t, pool)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &site{t: t, client: &http.Client{Jar: jar}, url: ts.URL, pool: pool}
}

func (s *site) get(path string) (*http.Response, string) {
	s.t.Helper()

	resp, err := s.client.Get(s.url + path)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	return resp, readBody(s.t, resp)
}

// submit fills in a form the way a browser does: load the page, take the CSRF
// token out of it, post back with the headers a same-origin submission carries.
func (s *site) submit(formPath, action string, values url.Values) (*http.Response, string) {
	s.t.Helper()

	_, page := s.get(formPath)
	values.Set("csrf_token", csrfToken(s.t, page))

	req, err := http.NewRequest(http.MethodPost, s.url+action, strings.NewReader(values.Encode()))
	if err != nil {
		s.t.Fatalf("build POST %s: %v", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", s.url+formPath)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatalf("POST %s: %v", action, err)
	}
	return resp, readBody(s.t, resp)
}

// count is a one-number query, for asserting on what ended up in the database.
func (s *site) count(query string, args ...any) int {
	s.t.Helper()

	var n int
	if err := s.pool.QueryRow(s.t.Context(), query, args...).Scan(&n); err != nil {
		s.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close() //nolint:errcheck // test client response

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func csrfToken(t *testing.T, page string) string {
	t.Helper()

	m := csrfPattern.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no CSRF field in the page:\n%s", page)
	}
	// html/template escaped the token on the way out; a browser decodes the
	// entities before submitting, and so does this.
	return html.UnescapeString(m[1])
}

// TestTheMilestone is M1's "done when": a new user can register, log in and
// create a project. Everything it touches is real — Postgres, the session
// table, the CSRF guard, the templates.
func TestTheMilestone(t *testing.T) {
	s := newSite(t)

	// --- register -------------------------------------------------------
	resp, body := s.submit("/register", "/register", url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if got := resp.Request.URL.Path; got != "/projects" {
		t.Fatalf("register landed on %q, want /projects", got)
	}
	if n := s.count("select count(*) from users where email = 'sam@example.com'"); n != 1 {
		t.Fatalf("%d users in the table, want 1", n)
	}
	// Registering leaves a session behind, in Postgres, where logging out can
	// revoke it.
	if n := s.count("select count(*) from sessions"); n != 1 {
		t.Errorf("%d rows in sessions, want 1", n)
	}

	// --- create a project -----------------------------------------------
	resp, body = s.submit("/projects/new", "/projects", url.Values{
		"slug": {"checkout"},
		"name": {"Checkout API"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create project: status = %d:\n%s", resp.StatusCode, body)
	}
	if got := resp.Request.URL.Path; got != "/projects/checkout" {
		t.Fatalf("landed on %q, want the new project", got)
	}
	if n := s.count(`select count(*) from projects p
		join users u on u.id = p.owner_id
		where p.slug = 'checkout' and u.email = 'sam@example.com'`); n != 1 {
		t.Fatal("the project was not stored against the registered account")
	}
	if !strings.Contains(body, "http://restest.test/m/checkout/") {
		t.Errorf("the project page does not show its mock URL:\n%s", body)
	}

	// --- and it is on the list ------------------------------------------
	_, body = s.get("/projects")
	if !strings.Contains(body, "Checkout API") {
		t.Errorf("the project is not on the list:\n%s", body)
	}

	// --- log out ---------------------------------------------------------
	if _, body = s.submit("/projects", "/logout", url.Values{}); !strings.Contains(body, "logged out") {
		t.Errorf("no confirmation of the logout:\n%s", body)
	}
	// Server-side sessions mean logging out revokes access immediately, rather
	// than waiting for a token to expire (DESIGN.md §8).
	if n := s.count("select count(*) from sessions where data::text like '%user_id%'"); n != 0 {
		t.Errorf("%d sessions still name a user after logging out", n)
	}

	_, body = s.get("/projects")
	if strings.Contains(body, "Checkout API") {
		t.Error("the project list is readable after logging out")
	}

	// --- log back in ------------------------------------------------------
	resp, body = s.submit("/login", "/login", url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("log in: status = %d:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Checkout API") {
		t.Errorf("the project did not survive the session:\n%s", body)
	}
}

// A second account registering the same address is told so, rather than meeting
// a constraint violation as a 500.
func TestRegisterTwiceThroughTheForm(t *testing.T) {
	s := newSite(t)

	values := url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	}
	if resp, body := s.submit("/register", "/register", values); resp.StatusCode != http.StatusOK {
		t.Fatalf("first registration: %d:\n%s", resp.StatusCode, body)
	}

	// A fresh visitor, with no session of their own.
	second := newSiteSharing(t, s)
	resp, body := second.submit("/register", "/register", values)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "already registered") {
		t.Errorf("the page does not say the address is taken:\n%s", body)
	}
	if n := s.count("select count(*) from users"); n != 1 {
		t.Errorf("%d users in the table, want 1", n)
	}
}

// A slug another account already owns is refused, because slugs address mock
// traffic and mock traffic arrives without an account.
func TestSlugCollisionAcrossAccounts(t *testing.T) {
	s := newSite(t)

	if resp, body := s.submit("/register", "/register", url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %d:\n%s", resp.StatusCode, body)
	}
	if resp, body := s.submit("/projects/new", "/projects", url.Values{
		"slug": {"checkout"},
		"name": {"Checkout API"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("create project: %d:\n%s", resp.StatusCode, body)
	}

	other := newSiteSharing(t, s)
	if resp, body := other.submit("/register", "/register", url.Values{
		"email":    {"alex@example.com"},
		"password": {"correct horse battery staple"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("second register: %d:\n%s", resp.StatusCode, body)
	}

	resp, body := other.submit("/projects/new", "/projects", url.Values{
		"slug": {"checkout"},
		"name": {"My checkout"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "taken") {
		t.Errorf("the page does not say the slug is taken:\n%s", body)
	}

	// And the first account's project is untouched, and still invisible to the
	// second.
	if n := s.count("select count(*) from projects where slug = 'checkout'"); n != 1 {
		t.Errorf("%d projects with that slug, want 1", n)
	}
	if resp, _ := other.get("/projects/checkout"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another owner's project answers %d, want 404", resp.StatusCode)
	}
}

// A reserved slug is refused before the database is asked, so the message is a
// sentence beside the field rather than a constraint name.
func TestReservedSlugIsRefused(t *testing.T) {
	s := newSite(t)

	if resp, body := s.submit("/register", "/register", url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: %d:\n%s", resp.StatusCode, body)
	}

	for _, slug := range []string{"api", "admin", "demo", "healthz", "m", "static"} {
		t.Run(slug, func(t *testing.T) {
			resp, body := s.submit("/projects/new", "/projects", url.Values{
				"slug": {slug},
				"name": {"Reserved"},
			})
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", resp.StatusCode)
			}
			if !strings.Contains(body, "reserved") {
				t.Errorf("unexpected message:\n%s", body)
			}
		})
	}

	if n := s.count("select count(*) from projects"); n != 0 {
		t.Errorf("%d reserved slugs made it into the table", n)
	}
}

// newSiteSharing is a second visitor to the same running application: its own
// cookie jar, the same server and database.
func newSiteSharing(t *testing.T, s *site) *site {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &site{t: t, client: &http.Client{Jar: jar}, url: s.url, pool: s.pool}
}
