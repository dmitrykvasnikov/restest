package web

import (
	"context"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// errNotStubbed is what an unconfigured stub method returns, so a handler
// reaching for something the test did not set up fails loudly instead of
// quietly seeing a zero value.
var errNotStubbed = errors.New("stub: method not configured for this test")

// stubStore stands in for core.Store. The real implementation is exercised
// against real Postgres in internal/integration; these tests are about routing,
// sessions, CSRF and what the handlers render.
type stubStore struct {
	ping                  func(ctx context.Context) error
	registerUser          func(ctx context.Context, email, password string) (core.User, error)
	authenticate          func(ctx context.Context, email, password string) (core.User, error)
	userByID              func(ctx context.Context, id uuid.UUID) (core.User, error)
	createProject         func(ctx context.Context, ownerID uuid.UUID, slug, name string) (core.Project, error)
	projectsByOwner       func(ctx context.Context, ownerID uuid.UUID) ([]core.Project, error)
	projectByOwnerAndSlug func(ctx context.Context, ownerID uuid.UUID, slug string) (core.Project, error)
	updateProject         func(ctx context.Context, ownerID, id uuid.UUID, slug, name string) (core.Project, error)
	deleteProject         func(ctx context.Context, ownerID, id uuid.UUID) error
}

func (s stubStore) Ping(ctx context.Context) error {
	if s.ping == nil {
		return nil
	}
	return s.ping(ctx)
}

func (s stubStore) RegisterUser(ctx context.Context, email, password string) (core.User, error) {
	if s.registerUser == nil {
		return core.User{}, errNotStubbed
	}
	return s.registerUser(ctx, email, password)
}

func (s stubStore) Authenticate(ctx context.Context, email, password string) (core.User, error) {
	if s.authenticate == nil {
		return core.User{}, errNotStubbed
	}
	return s.authenticate(ctx, email, password)
}

func (s stubStore) UserByID(ctx context.Context, id uuid.UUID) (core.User, error) {
	if s.userByID == nil {
		return core.User{}, core.ErrNotFound
	}
	return s.userByID(ctx, id)
}

func (s stubStore) CreateProject(ctx context.Context, ownerID uuid.UUID, slug, name string) (core.Project, error) {
	if s.createProject == nil {
		return core.Project{}, errNotStubbed
	}
	return s.createProject(ctx, ownerID, slug, name)
}

func (s stubStore) ProjectsByOwner(ctx context.Context, ownerID uuid.UUID) ([]core.Project, error) {
	if s.projectsByOwner == nil {
		return nil, nil
	}
	return s.projectsByOwner(ctx, ownerID)
}

func (s stubStore) ProjectByOwnerAndSlug(ctx context.Context, ownerID uuid.UUID, slug string) (core.Project, error) {
	if s.projectByOwnerAndSlug == nil {
		return core.Project{}, core.ErrNotFound
	}
	return s.projectByOwnerAndSlug(ctx, ownerID, slug)
}

func (s stubStore) UpdateProject(ctx context.Context, ownerID, id uuid.UUID, slug, name string) (core.Project, error) {
	if s.updateProject == nil {
		return core.Project{}, errNotStubbed
	}
	return s.updateProject(ctx, ownerID, id, slug, name)
}

func (s stubStore) DeleteProject(ctx context.Context, ownerID, id uuid.UUID) error {
	if s.deleteProject == nil {
		return errNotStubbed
	}
	return s.deleteProject(ctx, ownerID, id)
}

// testUser is the account the stubs hand back, and testProjectID the project
// it owns. Fixed values, so a failure names something recognisable.
var (
	testUser = core.User{
		ID:    uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Email: "sam@example.com",
	}
	testProjectID = uuid.MustParse("22222222-2222-4222-8222-222222222222")
)

func testProject(slug, name string) core.Project {
	return core.Project{
		ID:      testProjectID,
		OwnerID: testUser.ID,
		Slug:    slug,
		Name:    name,
	}
}

// newServer builds a Server over an in-memory session store — the session table
// is exercised for real in the integration tests, and needing Docker to check a
// redirect would be a poor trade.
func newServer(t *testing.T, store Store) *Server {
	t.Helper()
	return newServerWith(t, store, nil)
}

// newServerWith lets a test adjust the options — the cookie policy, mostly.
func newServerWith(t *testing.T, store Store, tweak func(*Options)) *Server {
	t.Helper()

	opts := Options{
		Logger:   discardLogger(),
		Store:    store,
		Sessions: newSessionManager(false),
		BaseURL:  "http://restest.test",
	}
	if tweak != nil {
		tweak(&opts)
	}

	srv, err := New(opts)
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	return srv
}

// browser is a running server plus a client that keeps cookies, which is what
// makes a session and a CSRF token behave the way they do in a real browser.
type browser struct {
	t      *testing.T
	client *http.Client
	url    string
}

// newBrowser starts the server and returns a client that follows redirects and
// remembers cookies.
func newBrowser(t *testing.T, store Store) *browser {
	t.Helper()
	return newBrowserWith(t, store, nil)
}

func newBrowserWith(t *testing.T, store Store, tweak func(*Options)) *browser {
	t.Helper()

	srv := newServerWith(t, store, tweak)

	// A server configured for Secure cookies is served over TLS, because a jar
	// will not store a Secure cookie from a plain HTTP response — which is the
	// whole point of the attribute, and would make the test a fiction.
	ts := httptest.NewServer(srv.Handler())
	if srv.secure {
		ts.Close()
		ts = httptest.NewTLSServer(srv.Handler())
	}
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}

	client := ts.Client()
	client.Jar = jar
	return &browser{t: t, client: client, url: ts.URL}
}

// noFollow stops the client following redirects, for tests that care about the
// redirect itself rather than where it lands.
func (b *browser) noFollow() *browser {
	b.client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return b
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()

	resp, err := b.client.Get(b.url + path)
	if err != nil {
		b.t.Fatalf("GET %s: %v", path, err)
	}
	return resp, readBody(b.t, resp)
}

// post submits a form, fetching a CSRF token first the way a browser would by
// loading the page the form is on.
func (b *browser) post(formPath, action string, values url.Values) (*http.Response, string) {
	b.t.Helper()

	_, body := b.get(formPath)
	values.Set("csrf_token", csrfToken(b.t, body))
	return b.postRaw(action, values, b.sameOriginHeaders(formPath))
}

// sameOriginHeaders are what a browser attaches to a form submission back to
// the page it came from. nosurf wants one of them on every unsafe request, so a
// test client that omits them is not standing in for a browser at all.
func (b *browser) sameOriginHeaders(formPath string) http.Header {
	return http.Header{
		"Referer":        {b.url + formPath},
		"Sec-Fetch-Site": {"same-origin"},
	}
}

// postRaw submits without fetching a token, for tests about what happens when
// the token is missing or wrong.
func (b *browser) postRaw(action string, values url.Values, headers http.Header) (*http.Response, string) {
	b.t.Helper()

	req, err := http.NewRequest(http.MethodPost, b.url+action, strings.NewReader(values.Encode()))
	if err != nil {
		b.t.Fatalf("build POST %s: %v", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header[k] = v
	}

	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("POST %s: %v", action, err)
	}
	return resp, readBody(b.t, resp)
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

// csrfToken pulls the token out of a rendered form, undoing the HTML escaping
// that html/template applies to it — a browser decodes the entities before
// submitting, and so must a test that stands in for one.
func csrfToken(t *testing.T, body string) string {
	t.Helper()

	m := csrfPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF field in the page:\n%s", body)
	}
	return html.UnescapeString(m[1])
}
