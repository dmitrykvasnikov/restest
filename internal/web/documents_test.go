package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// collectionRoot is where every collection test in this file roots its
// endpoint, under the same project slug the static mock tests use.
const collectionRoot = "/users"

// withCollection builds a server whose route table holds one collection
// endpoint, over a document store already holding the seeded bodies.
func withCollection(t *testing.T, seed ...string) (*browser, *fakeDocuments) {
	t.Helper()
	return withCollectionEndpoint(t, core.Headers{}, seed...)
}

func withCollectionEndpoint(t *testing.T, headers core.Headers, seed ...string) (*browser, *fakeDocuments) {
	t.Helper()

	project := core.MockProject{ID: uuid.New(), Slug: mockSlug}
	collectionID := uuid.New()

	data := core.MockData{
		Projects: []core.MockProject{project},
		Endpoints: []core.MockEndpoint{{
			ProjectSlug: mockSlug,
			Endpoint: core.Endpoint{
				ID:           uuid.New(),
				ProjectID:    project.ID,
				Method:       core.MethodAny,
				Path:         collectionRoot,
				Kind:         core.KindCollection,
				IsEnabled:    true,
				CollectionID: collectionID,
				Headers:      headers,
			},
		}},
	}

	docs := newFakeDocuments().forCollection(collectionID).seed(seed...)
	store := stubStore{
		documents: docs,
		mockData:  func(context.Context) (core.MockData, error) { return data, nil },
	}
	return newBrowser(t, store), docs
}

func mockPath(suffix string) string { return "/m/" + mockSlug + collectionRoot + suffix }

// The milestone's "done when", over HTTP and in one test: POST a record, GET it
// back, filter for it, delete it, and find it gone.
func TestCollectionRoundTrip(t *testing.T) {
	b, docs := withCollection(t, `{"id":1,"name":"Ada","role":"admin"}`)

	// POST a record.
	resp, body := b.do(http.MethodPost, mockPath(""), `{"name":"Grace","role":"admin"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201: %s", resp.StatusCode, body)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("a 201 with no Location leaves the caller nowhere to look")
	}
	created := decodeObject(t, body)
	if created["name"] != "Grace" {
		t.Errorf("created = %v, want the name that was sent", created)
	}
	if created["id"] == nil {
		t.Error("the created document carries no identifier")
	}

	// GET it back, at the address the Location header gave.
	resp, body = b.do(http.MethodGet, location, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", location, resp.StatusCode, body)
	}
	if fetched := decodeObject(t, body); fetched["name"] != "Grace" {
		t.Errorf("fetched = %v, want the record that was posted", fetched)
	}

	// Filter for it. The seeded Ada is an admin too, so a filter that returns
	// both proves nothing; the name is what separates them.
	resp, body = b.do(http.MethodGet, mockPath("?name=Grace"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered list status = %d, want 200: %s", resp.StatusCode, body)
	}
	list := decodeArray(t, body)
	if len(list) != 1 || list[0]["name"] != "Grace" {
		t.Fatalf("?name=Grace returned %v, want just Grace", list)
	}

	// Delete it.
	resp, body = b.do(http.MethodDelete, location, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204: %s", resp.StatusCode, body)
	}
	if body != "" {
		t.Errorf("a 204 carried a body: %q", body)
	}

	// And it is gone — from the collection as well as from that address.
	if resp, _ := b.do(http.MethodGet, location, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", resp.StatusCode)
	}
	if docs.count() != 1 {
		t.Errorf("the collection holds %d documents, want the one seeded record", docs.count())
	}
}

// A mock client is not a browser: it carries no session and no CSRF token, and
// a write that demanded one would make the whole feature unusable from curl.
func TestCollectionWritesNeedNoToken(t *testing.T) {
	b, _ := withCollection(t)

	resp, body := b.do(http.MethodPost, mockPath(""), `{"name":"Ada"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201: %s", resp.StatusCode, body)
	}
	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Errorf("mock traffic was given cookies: %v", cookies)
	}
}

// PUT says what the whole document is; PATCH says what changed. The difference
// is the field the request left out.
func TestReplaceAndPatch(t *testing.T) {
	b, _ := withCollection(t, `{"id":1,"name":"Ada","role":"admin","city":"London"}`)

	resp, body := b.do(http.MethodPut, mockPath("/1"), `{"name":"Ada Lovelace"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", resp.StatusCode, body)
	}
	replaced := decodeObject(t, body)
	if replaced["name"] != "Ada Lovelace" {
		t.Errorf("PUT returned %v, want the new name", replaced)
	}
	if _, kept := replaced["role"]; kept {
		t.Errorf("PUT kept a field the request left out: %v", replaced)
	}
	if replaced["id"] == nil {
		t.Errorf("PUT dropped the identifier: %v", replaced)
	}

	resp, body = b.do(http.MethodPatch, mockPath("/1"), `{"role":"engineer"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", resp.StatusCode, body)
	}
	patched := decodeObject(t, body)
	if patched["role"] != "engineer" {
		t.Errorf("PATCH returned %v, want the new role", patched)
	}
	if patched["name"] != "Ada Lovelace" {
		t.Errorf("PATCH discarded a field it was not asked about: %v", patched)
	}
}

// The identifier is the server's. A write that tried to change it addressed one
// document and would have produced another.
func TestWritesCannotRenameADocument(t *testing.T) {
	b, _ := withCollection(t, `{"id":1,"name":"Ada"}`)

	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		_, body := b.do(method, mockPath("/1"), `{"id":99,"name":"Ada"}`)
		if got := decodeObject(t, body)["id"]; got != json.Number("1") && got != float64(1) {
			t.Errorf("%s left the id as %v, want 1", method, got)
		}
	}
}

func TestMissingDocument(t *testing.T) {
	b, _ := withCollection(t, `{"id":1,"name":"Ada"}`)

	tests := []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPut, body: `{"name":"Nobody"}`},
		{method: http.MethodPatch, body: `{"name":"Nobody"}`},
		{method: http.MethodDelete},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			resp, body := b.do(tt.method, mockPath("/404"), tt.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, "404") {
				t.Errorf("the message does not name the id asked for: %s", body)
			}
		})
	}
}

// A document is an object. Anything else has no fields to filter, merge or
// address, and saying so is better than a check violation in the log.
func TestWritesRefuseWhatIsNotAnObject(t *testing.T) {
	b, _ := withCollection(t, `{"id":1}`)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "an array", method: http.MethodPost, path: "", body: `[{"a":1}]`},
		{name: "a number", method: http.MethodPost, path: "", body: `7`},
		{name: "nothing at all", method: http.MethodPost, path: "", body: ``},
		{name: "broken JSON", method: http.MethodPost, path: "", body: `{"a":`},
		{name: "an array on PUT", method: http.MethodPut, path: "/1", body: `[]`},
		{name: "an array on PATCH", method: http.MethodPatch, path: "/1", body: `[]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := b.do(tt.method, mockPath(tt.path), tt.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, "JSON object") {
				t.Errorf("the message does not say what was wrong: %s", body)
			}
		})
	}
}

// The body cap is what stops one request asking the process to hold as much
// memory as the client cares to send.
func TestOversizedBodyIsRefused(t *testing.T) {
	b, _ := withCollection(t)

	huge := `{"note":"` + strings.Repeat("x", maxMockRequestBody) + `"}`
	resp, body := b.do(http.MethodPost, mockPath(""), huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, body)
	}
}

func TestListingPagesAndCounts(t *testing.T) {
	var seed []string
	for i := 1; i <= 7; i++ {
		seed = append(seed, `{"id":`+strconv.Itoa(i)+`,"name":"user`+strconv.Itoa(i)+`"}`)
	}
	b, _ := withCollection(t, seed...)

	tests := []struct {
		name      string
		query     string
		wantLen   int
		wantTotal string
		wantFirst string
	}{
		{name: "everything by default", query: "", wantLen: 7, wantTotal: "7", wantFirst: "user1"},
		{name: "a page of three", query: "?_limit=3", wantLen: 3, wantTotal: "7", wantFirst: "user1"},
		{name: "the second page", query: "?_limit=3&_page=2", wantLen: 3, wantTotal: "7", wantFirst: "user4"},
		{name: "the last, short page", query: "?_limit=3&_page=3", wantLen: 1, wantTotal: "7", wantFirst: "user7"},
		// Past the end the total is still the truth, so a client paging through
		// is told it has run off the end rather than that the collection emptied.
		{name: "past the end", query: "?_limit=3&_page=9", wantLen: 0, wantTotal: "7"},
		{name: "a filter narrows the total too", query: "?name=user2", wantLen: 1, wantTotal: "1", wantFirst: "user2"},
		{name: "a filter matching nothing", query: "?name=nobody", wantLen: 0, wantTotal: "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := b.do(http.MethodGet, mockPath(tt.query), "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
			}
			if got := resp.Header.Get(headerTotalCount); got != tt.wantTotal {
				t.Errorf("%s = %q, want %q", headerTotalCount, got, tt.wantTotal)
			}

			list := decodeArray(t, body)
			if len(list) != tt.wantLen {
				t.Fatalf("returned %d documents, want %d: %s", len(list), tt.wantLen, body)
			}
			if tt.wantFirst != "" && list[0]["name"] != tt.wantFirst {
				t.Errorf("first document = %v, want %s", list[0], tt.wantFirst)
			}
		})
	}
}

// An empty collection answers with an empty array, not with null. A client
// iterating the response should not have to check for both.
func TestEmptyListIsAnArray(t *testing.T) {
	b, _ := withCollection(t)

	resp, body := b.do(http.MethodGet, mockPath(""), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("body = %q, want []", strings.TrimSpace(body))
	}
}

func TestBadListingQueryIsRefused(t *testing.T) {
	b, _ := withCollection(t)

	resp, body := b.do(http.MethodGet, mockPath("?_limit=lots"), "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "_limit") {
		t.Errorf("the message does not name the parameter: %s", body)
	}
}

// The six routes are routes, so the answers the matcher already gave still
// apply: a verb nothing claims is 405 with the verbs that are claimed.
func TestCollectionMethodNotAllowed(t *testing.T) {
	b, _ := withCollection(t, `{"id":1}`)

	resp, body := b.do(http.MethodPut, mockPath(""), `{}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", resp.StatusCode, body)
	}
	allow := resp.Header.Get("Allow")
	for _, verb := range []string{"GET", "HEAD", "POST"} {
		if !strings.Contains(allow, verb) {
			t.Errorf("Allow = %q, want it to include %s", allow, verb)
		}
	}
}

// The endpoint's own headers reach a collection response too, which is what
// lets somebody add the CORS header their browser client needs before the
// setting for it arrives in M7.
func TestEndpointHeadersReachCollectionResponses(t *testing.T) {
	b, _ := withCollectionEndpoint(t,
		core.Headers{"Access-Control-Allow-Origin": "*", "X-Mock": "yes"},
		`{"id":1}`)

	resp, _ := b.do(http.MethodGet, mockPath(""), "")
	if got := resp.Header.Get("X-Mock"); got != "yes" {
		t.Errorf("X-Mock = %q, want yes", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

// A collection whose row has gone answers 404 rather than 500. The route table
// is rebuilt on the change that removed it, so this is the window between the
// two and not a state anybody stays in.
func TestCollectionThatWentAway(t *testing.T) {
	project := core.MockProject{ID: uuid.New(), Slug: mockSlug}
	data := core.MockData{
		Projects: []core.MockProject{project},
		Endpoints: []core.MockEndpoint{{
			ProjectSlug: mockSlug,
			Endpoint: core.Endpoint{
				ID: uuid.New(), ProjectID: project.ID,
				Method: core.MethodAny, Path: collectionRoot,
				Kind: core.KindCollection, IsEnabled: true,
				CollectionID: uuid.New(), // not the one the store knows about
			},
		}},
	}
	store := stubStore{
		documents: newFakeDocuments().forCollection(uuid.New()),
		mockData:  func(context.Context) (core.MockData, error) { return data, nil },
	}
	b := newBrowser(t, store)

	resp, body := b.do(http.MethodPost, mockPath(""), `{"name":"Ada"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "no longer exists") {
		t.Errorf("the message does not explain itself: %s", body)
	}
}

func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()

	var object map[string]any
	if err := json.Unmarshal([]byte(body), &object); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return object
}

func decodeArray(t *testing.T, body string) []map[string]any {
	t.Helper()

	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return list
}
