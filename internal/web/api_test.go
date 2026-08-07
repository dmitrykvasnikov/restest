package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// apiStore is a store that can answer everything /api/v1/ asks of it: an
// account, a project with one collection and one endpoint, and writes that
// report what they were given.
func apiStore() stubStore {
	store := collectionStore()
	store.tokens = newFakeTokens()

	store.endpointsByProject = func(context.Context, uuid.UUID, uuid.UUID) ([]core.Endpoint, error) {
		return []core.Endpoint{testEndpoint()}, nil
	}
	store.endpointByOwner = func(_ context.Context, _ uuid.UUID, id uuid.UUID) (core.Endpoint, error) {
		if id != testEndpointID {
			return core.Endpoint{}, core.ErrNotFound
		}
		return testEndpoint(), nil
	}
	store.createProject = func(_ context.Context, _ uuid.UUID, slug, name string, _ []string) (core.Project, error) {
		return testProject(slug, name), nil
	}
	store.updateProject = func(_ context.Context, _, _ uuid.UUID, slug, name string) (core.Project, error) {
		return testProject(slug, name), nil
	}
	store.deleteProject = func(context.Context, uuid.UUID, uuid.UUID) error { return nil }
	store.resetCollection = func(context.Context, uuid.UUID) (int, error) { return 4, nil }
	return store
}

// tokenFor mints a token through the interface, which is the only place that
// mints one, and hands it back as a script would hold it.
func tokenFor(t *testing.T, b *browser) *script {
	t.Helper()
	logIn(t, b)
	return b.script(mintToken(t, b, "ci"))
}

// The cheapest thing a script can do with a fresh token is find out whether it
// works, and be told which account it resolved to.
func TestAPIIndexNamesTheAccountAndHowItWasProved(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.get(pathAPIPrefix)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}

	var index apiIndex
	decodeJSONBody(t, body, &index)
	if index.User != testUser.Email {
		t.Errorf("user = %q, want %q", index.User, testUser.Email)
	}
	if index.AuthenticatedBy != "token" {
		t.Errorf("authenticated_by = %q, want token", index.AuthenticatedBy)
	}
	if len(index.Routes) == 0 {
		t.Error("the index lists no routes")
	}
}

func TestAPIRefusesAnUnknownToken(t *testing.T) {
	b := newBrowser(t, apiStore())
	logIn(t, b)

	resp, body := b.script("rst_" + strings.Repeat("A", 43)).get(pathAPIPrefix)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get(headerWWWAuth); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	// One answer for unknown, revoked and expired alike: saying which would
	// tell a caller holding a guess how close it was.
	if strings.Contains(body, "expired") && strings.Contains(body, "unknown") {
		t.Errorf("the refusal distinguishes the reasons: %s", body)
	}
}

func TestAPIRefusesAnAnonymousCaller(t *testing.T) {
	b := newBrowser(t, apiStore())

	resp, body := b.script("").get(pathAPIProjects)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON — a script cannot read a login page", ct)
	}
}

// The milestone in one test: a mutating call with a token and no CSRF token, no
// cookie and no form, is accepted.
func TestATokenNeedsNoCSRFToken(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPost, pathAPIProjects, `{"slug":"checkout","name":"Checkout API"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", resp.StatusCode, body)
	}

	var project apiProject
	decodeJSONBody(t, body, &project)
	if project.Slug != "checkout" {
		t.Errorf("slug = %q, want checkout", project.Slug)
	}
	if !strings.HasSuffix(project.MockURL, "/m/checkout/") {
		t.Errorf("mock_url = %q, want it to point at the mock root", project.MockURL)
	}
	if got := resp.Header.Get("Location"); got != pathAPIProjects+"/checkout" {
		t.Errorf("Location = %q", got)
	}
}

// The exemption is for a request the token authenticated, not for one that
// merely mentions a token. A logged-in browser sending a bad bearer must be
// refused rather than quietly falling back to its cookie — that fallback would
// be a CSRF hole with an Authorization header taped over it.
func TestABadBearerNeverFallsBackToTheSession(t *testing.T) {
	b := newBrowser(t, apiStore())
	logIn(t, b)

	resp, body := b.raw(http.MethodPost, pathAPIProjects, http.Header{
		"Authorization": {"Bearer rst_" + strings.Repeat("B", 43)},
		"Content-Type":  {"application/json"},
	}, `{"slug":"sneaky","name":"Sneaky"}`)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", resp.StatusCode, body)
	}
}

// And the cookie on its own is still guarded, exactly as it was before M6.
func TestTheSessionCookieStillNeedsACSRFTokenOnTheAPI(t *testing.T) {
	b := newBrowser(t, apiStore())
	logIn(t, b)

	resp, body := b.raw(http.MethodPost, pathAPIProjects, http.Header{
		"Content-Type": {"application/json"},
	}, `{"slug":"sneaky","name":"Sneaky"}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from the CSRF guard:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Bearer") {
		t.Errorf("the refusal does not say what to send instead: %s", body)
	}
}

// The reset route is the one M3 shipped and could not script. Same URL, same
// handler, now reachable with a token.
func TestResetIsScriptableWithAToken(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPost, resetPath("checkout", "users"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}

	var result resetResult
	decodeJSONBody(t, body, &result)
	if result.Collection != "users" || result.Documents != 4 {
		t.Errorf("result = %+v, want users with 4 documents", result)
	}
}

func TestAPIListsEndpointsWithTheirCollectionNames(t *testing.T) {
	store := apiStore()
	// One collection endpoint, so that the binding has a name to render.
	store.endpointsByProject = func(context.Context, uuid.UUID, uuid.UUID) ([]core.Endpoint, error) {
		return []core.Endpoint{{
			ID:           testEndpointID,
			ProjectID:    testProjectID,
			Method:       core.MethodAny,
			Path:         "/users",
			Kind:         core.KindCollection,
			IsEnabled:    true,
			CollectionID: testCollectionID,
		}}, nil
	}

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	resp, body := caller.get(pathAPIProjects + "/checkout/endpoints")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}

	var list apiList[apiEndpoint]
	decodeJSONBody(t, body, &list)
	if list.Count != 1 {
		t.Fatalf("count = %d, want 1", list.Count)
	}
	// The name, not the uuid: it is what the caller created the collection
	// under and what it would use to name it again.
	if list.Items[0].Collection != "users" {
		t.Errorf("collection = %q, want users", list.Items[0].Collection)
	}
}

// An endpoint is bound to a collection by name, and a name that is not there is
// a rejection with the field on it rather than a foreign key error.
func TestAPIEndpointNamingAnUnknownCollection(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPost, pathAPIProjects+"/checkout/endpoints",
		`{"path":"/orders","collection":"orders"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}

	var refusal errorBody
	decodeJSONBody(t, body, &refusal)
	if refusal.Fields["collection"] == "" {
		t.Errorf("no message against the collection field: %s", body)
	}
}

// A misspelt field is refused rather than dropped: a caller who wrote `slugg`
// would otherwise get a project with a name it did not choose and no idea why.
func TestAPIRefusesAnUnknownField(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPost, pathAPIProjects, `{"slugg":"checkout","name":"Checkout"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "slugg") {
		t.Errorf("the refusal does not name the field: %s", body)
	}
}

// Validation is core's, not the API's: the same rules the forms enforce, in the
// same words, reported per field. The stub defers to core.Validate rather than
// inventing an answer, so this is about what the API does with a rejection and
// not about a second, laxer copy of the rules.
func TestAPIRejectsWhatTheFormWouldReject(t *testing.T) {
	store := apiStore()
	store.createCollection = func(_ context.Context, _, _ uuid.UUID, in core.CollectionInput) (core.Collection, error) {
		if err := in.Validate(); err != nil {
			return core.Collection{}, err
		}
		return testCollection(), nil
	}

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPost, pathAPIProjects+"/checkout/collections",
		`{"name":"Not A Name","seed":{"not":"an array"}}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}

	var refusal errorBody
	decodeJSONBody(t, body, &refusal)
	if refusal.Fields["name"] == "" || refusal.Fields["seed"] == "" {
		t.Errorf("both fields should carry a message: %s", body)
	}
	if refusal.Error == "" {
		t.Errorf("a refusal with no sentence in it: %s", body)
	}
}

// A project belonging to somebody else is the same 404 as one that never
// existed, on this side of the application too.
func TestAPIHidesAnotherAccountsProject(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.get(pathAPIProjects + "/somebody-elses")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", resp.StatusCode, body)
	}
}

func TestAPIUnknownRouteAnswersJSON(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.get(pathAPIPrefix + "widgets")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

// "This address answers POST, not GET" is exactly the mistake a script makes,
// and it should be told in a form it can read.
func TestAPIMethodMismatchAnswersJSONWithAllow(t *testing.T) {
	b := newBrowser(t, apiStore())
	caller := tokenFor(t, b)

	resp, body := caller.get(resetPath("checkout", "users"))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405:\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

// The log joins the API in the same milestone: what the inspector shows in a
// browser, a CI job can read from a script.
func TestAPIServesTheRequestLog(t *testing.T) {
	store := apiStore()
	store.exchanges = newFakeLog()
	store.exchanges.Record(core.Exchange{
		ProjectID:      testProjectID,
		Matched:        true,
		Method:         http.MethodGet,
		Path:           "/users",
		StatusCode:     200,
		RequestHeaders: core.HeaderSet{"Accept": {"application/json"}},
		ResponseBody:   []byte(`{"ok":true}`),
	})

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	resp, body := caller.get(pathAPIProjects + "/checkout/log")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}

	var page apiLogPage
	decodeJSONBody(t, body, &page)
	if page.Count != 1 {
		t.Fatalf("count = %d, want 1:\n%s", page.Count, body)
	}
	if page.Items[0].Path != "/users" {
		t.Errorf("path = %q, want /users", page.Items[0].Path)
	}

	// And the detail carries what the list left out.
	resp, body = caller.get(pathAPIProjects + "/checkout/log/" + page.Items[0].ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d, want 200:\n%s", resp.StatusCode, body)
	}

	var detail apiExchangeDetail
	decodeJSONBody(t, body, &detail)
	if detail.Response.Text == nil || *detail.Response.Text != `{"ok":true}` {
		t.Errorf("response body = %v, want the recorded bytes", detail.Response.Text)
	}
	if len(detail.RequestHeaders["Accept"]) != 1 {
		t.Errorf("request headers = %v, want the Accept that was sent", detail.RequestHeaders)
	}
}

// A body that is not UTF-8 is described rather than mangled: a JSON string
// cannot carry arbitrary bytes, and replacement characters would be a body that
// never existed.
func TestAPILogDescribesABinaryBody(t *testing.T) {
	store := apiStore()
	store.exchanges = newFakeLog()
	store.exchanges.Record(core.Exchange{
		ProjectID:   testProjectID,
		Method:      http.MethodPost,
		Path:        "/upload",
		RequestBody: []byte{0xff, 0xfe, 0x00},
	})

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	_, body := caller.get(pathAPIProjects + "/checkout/log")
	var page apiLogPage
	decodeJSONBody(t, body, &page)

	_, body = caller.get(pathAPIProjects + "/checkout/log/" + page.Items[0].ID.String())
	var detail apiExchangeDetail
	decodeJSONBody(t, body, &detail)

	if !detail.Request.Binary || detail.Request.Text != nil {
		t.Errorf("request body = %+v, want it reported as binary with no text", detail.Request)
	}
	if detail.Request.Bytes != 3 {
		t.Errorf("bytes = %d, want 3", detail.Request.Bytes)
	}
}

// The API and the interface are the same application: what one changes, the
// other sees, and both rebuild the route table.
func TestAPICollectionRoundTrip(t *testing.T) {
	var saved core.CollectionInput
	created := testCollection()

	store := apiStore()
	store.createCollection = func(_ context.Context, _, _ uuid.UUID, in core.CollectionInput) (core.Collection, error) {
		saved = in
		created.Name = in.Name
		created.Seed = json.RawMessage(in.Seed)
		return created, nil
	}
	// The handler reads the collection back before answering, because creating
	// one applies its seed after the row comes back. The stub has to agree with
	// itself about what now exists, or it would be testing a store that does
	// not.
	store.collectionsByProject = func(context.Context, uuid.UUID, uuid.UUID) ([]core.Collection, error) {
		return []core.Collection{created}, nil
	}

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPost, pathAPIProjects+"/checkout/collections",
		`{"name":"orders","seed":[{"id":1,"total":10}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", resp.StatusCode, body)
	}
	// The seed arrives as JSON and reaches core as the text it validates, not
	// as a string a caller had to escape by hand.
	if !strings.Contains(saved.Seed, `"total":10`) {
		t.Errorf("seed = %q, want the array that was sent", saved.Seed)
	}

	var collection apiCollection
	decodeJSONBody(t, body, &collection)
	if collection.Name != "orders" {
		t.Errorf("name = %q, want orders", collection.Name)
	}
}

// A PATCH says only what changes; everything else stays as it was.
func TestAPICollectionPatchLeavesTheRestAlone(t *testing.T) {
	var saved core.CollectionInput
	store := apiStore()
	store.updateCollection = func(_ context.Context, _, _ uuid.UUID, in core.CollectionInput) (core.Collection, error) {
		saved = in
		return testCollection(), nil
	}

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	resp, body := caller.do(http.MethodPatch, pathAPIProjects+"/checkout/collections/users",
		`{"seed":[{"id":9}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	if saved.Name != "users" {
		t.Errorf("name = %q, want the collection's own name to survive a patch", saved.Name)
	}
	if saved.IDStrategy != core.IDSerial {
		t.Errorf("id_strategy = %q, want it unchanged", saved.IDStrategy)
	}
	if !strings.Contains(saved.Seed, `"id":9`) {
		t.Errorf("seed = %q, want the new one", saved.Seed)
	}
}

// The session cookie reaches the same handlers, so the interface could call its
// own API — and a test that only ever used a token would not notice if it
// could not.
func TestTheSessionCookieAlsoReachesTheAPI(t *testing.T) {
	b := newBrowser(t, apiStore())
	logIn(t, b)

	resp, body := b.get(pathAPIPrefix)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}

	var index apiIndex
	decodeJSONBody(t, body, &index)
	if index.AuthenticatedBy != "session" {
		t.Errorf("authenticated_by = %q, want session", index.AuthenticatedBy)
	}
}

// Mock traffic is unauthenticated and stays that way: a token is for the
// management API and buys nothing at /m/.
func TestATokenIsNotForMockTraffic(t *testing.T) {
	store := apiStore()
	store.mockData = func(context.Context) (core.MockData, error) {
		return core.MockData{Projects: []core.MockProject{{ID: testProjectID, Slug: "checkout"}}}, nil
	}

	b := newBrowser(t, store)
	caller := tokenFor(t, b)

	// The same 404 an anonymous caller gets: the token neither opens anything
	// nor is refused, because mock traffic does not read it.
	withToken, _ := caller.get("/m/checkout/nothing")
	anonymous, _ := b.script("").get("/m/checkout/nothing")

	if withToken.StatusCode != anonymous.StatusCode {
		t.Errorf("a token changed a mock response: %d with, %d without",
			withToken.StatusCode, anonymous.StatusCode)
	}
}

// The reset button in the interface is an HTMX post with a session, and it must
// keep getting a redirect rather than the JSON the API side answers with.
func TestTheResetButtonStillGetsARedirect(t *testing.T) {
	b := newBrowser(t, apiStore())
	logIn(t, b)

	_, page := b.get("/projects/checkout")
	resp, _ := b.raw(http.MethodPost, resetPath("checkout", "users"), http.Header{
		"HX-Request":     {"true"},
		"X-CSRF-Token":   {csrfToken(t, page)},
		"Referer":        {b.url + "/projects/checkout"},
		"Sec-Fetch-Site": {"same-origin"},
	}, "")

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 with a redirect header", resp.StatusCode)
	}
	if got := resp.Header.Get(headerHXRedirect); got != "/projects/checkout" {
		t.Errorf("HX-Redirect = %q, want the project page", got)
	}
}

// Every route the index advertises is a route the router has. A list that
// drifts from the mux is worse than no list.
func TestEveryAdvertisedRouteExists(t *testing.T) {
	srv := newServer(t, apiStore())

	for _, route := range apiRoutes {
		method, pattern, _ := strings.Cut(route, " ")
		path := strings.NewReplacer(
			"{slug}", "checkout",
			"{name}", "users",
			"{id}", testEndpointID.String(),
		).Replace(strings.TrimSpace(pattern))

		t.Run(route, func(t *testing.T) {
			req, err := http.NewRequest(method, path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.URL, _ = url.Parse(path)

			if _, matched := srv.mux.Handler(req); matched == patternCatchAll || matched == "" {
				t.Errorf("%s %s matches no route", method, path)
			}
		})
	}
}
