package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// logSlug is the project the recording and inspector tests work in. It is the
// one projectStore() owns, so the same project can be driven through the mock
// server and then looked at through the UI.
const logSlug = "checkout"

// logBrowser builds a server that serves these endpoints, records what it
// serves, and shows the result in the inspector — all over one little in-memory
// log, so that "send a request, then go and look for it" is a thing a test can
// actually do.
func logBrowser(t *testing.T, log *fakeLog, bodyLimit int, endpoints ...mockEndpoint) *browser {
	t.Helper()
	return logBrowserWith(t, log, bodyLimit, nil, endpoints...)
}

// logBrowserWith is logBrowser for a test that also needs to adjust the
// server's options — the rate limits, mostly.
func logBrowserWith(t *testing.T, log *fakeLog, bodyLimit int, tweak func(*Options), endpoints ...mockEndpoint) *browser {
	t.Helper()

	project := core.MockProject{ID: testProjectID, Slug: logSlug}
	data := core.MockData{Projects: []core.MockProject{project}}

	for _, e := range endpoints {
		data.Endpoints = append(data.Endpoints, core.MockEndpoint{
			ProjectSlug: logSlug,
			Endpoint: core.Endpoint{
				ID:         uuid.New(),
				ProjectID:  project.ID,
				Method:     e.method,
				Path:       core.NormalizePath(e.path),
				Kind:       core.KindStatic,
				IsEnabled:  true,
				StatusCode: e.status,
				Body:       e.body,
				Headers:    e.headers,
				DelayMS:    e.delayMS,
			},
		})
	}

	store := projectStore()
	store.exchanges = log
	store.mockData = func(context.Context) (core.MockData, error) { return data, nil }

	return newBrowserWith(t, store, func(o *Options) {
		o.Recorder = log
		o.LogBodyLimit = bodyLimit
		o.LogRetentionMonths = 3
		if tweak != nil {
			tweak(o)
		}
	})
}

// echoEndpoint is the endpoint most of these tests send at: it answers a fixed
// body, so what is recorded on the response side is known exactly.
var echoEndpoint = mockEndpoint{
	method: "POST", path: "/orders", status: 201,
	body:    `{"ok":true}`,
	headers: core.Headers{"X-Mock": "yes"},
}

func TestMockRequestIsRecorded(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, echoEndpoint)

	req, err := http.NewRequest(http.MethodPost, b.url+"/m/checkout/orders?trace=7",
		strings.NewReader(`{"sku":"abc"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sekrit")

	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	readBody(t, resp)

	ex, ok := log.last()
	if !ok {
		t.Fatal("the request was not recorded")
	}

	if ex.ProjectID != testProjectID {
		t.Errorf("ProjectID = %s, want the project the slug names", ex.ProjectID)
	}
	if !ex.Matched {
		t.Error("Matched is false for a request an endpoint answered")
	}
	if ex.EndpointID == uuid.Nil {
		t.Error("EndpointID is empty for a matched request")
	}
	if ex.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", ex.Method)
	}
	// The path is the one below the project prefix — what the matcher saw, and
	// what the user wrote in the endpoint form.
	if ex.Path != "/orders" {
		t.Errorf("Path = %q, want /orders", ex.Path)
	}
	if ex.Query != "trace=7" {
		t.Errorf("Query = %q, want trace=7", ex.Query)
	}
	if ex.StatusCode != 201 {
		t.Errorf("StatusCode = %d, want 201", ex.StatusCode)
	}
	if got := string(ex.RequestBody); got != `{"sku":"abc"}` {
		t.Errorf("RequestBody = %q, want what the client sent", got)
	}
	if got := string(ex.ResponseBody); got != `{"ok":true}` {
		t.Errorf("ResponseBody = %q, want what the endpoint answered", got)
	}
	if got := ex.RequestHeaders.Values("Content-Type"); got != "application/json" {
		t.Errorf("recorded Content-Type = %q, want the one the client sent", got)
	}
	// The endpoint's own response headers are part of what was served.
	if got := ex.ResponseHeaders.Values("X-Mock"); got != "yes" {
		t.Errorf("recorded X-Mock = %q, want yes", got)
	}
	if ex.Direction != core.DirectionInbound {
		t.Errorf("Direction = %q, want inbound", ex.Direction)
	}
	if ex.RemoteAddr == "" {
		t.Error("RemoteAddr is empty")
	}
	if ex.CreatedAt.IsZero() {
		t.Error("CreatedAt is empty")
	}
}

// A request nothing answered is the most interesting kind: it is why somebody
// opens the inspector in the first place.
func TestUnmatchedRequestIsRecorded(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, echoEndpoint)

	b.get("/m/checkout/ordrs")

	ex, ok := log.last()
	if !ok {
		t.Fatal("the unmatched request was not recorded")
	}
	if ex.Matched {
		t.Error("Matched is true for a request nothing answered")
	}
	if ex.EndpointID != uuid.Nil {
		t.Errorf("EndpointID = %s, want empty for an unmatched request", ex.EndpointID)
	}
	if ex.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", ex.StatusCode)
	}
	if ex.Path != "/ordrs" {
		t.Errorf("Path = %q, want /ordrs", ex.Path)
	}
	// The 404 body names the nearest routes, and that is worth keeping: it is
	// what the client was told.
	if !strings.Contains(string(ex.ResponseBody), "/orders") {
		t.Errorf("ResponseBody = %q, want the suggestion the client got", ex.ResponseBody)
	}
}

// A slug no project has belongs to no log. Recording it would mean choosing
// somebody's project to file it under.
func TestUnknownProjectIsNotRecorded(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, echoEndpoint)

	resp, _ := b.get("/m/nosuchproject/orders")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := len(log.recorded()); got != 0 {
		t.Errorf("%d exchanges recorded for an unknown project, want none", got)
	}
}

// The log is of what a project served, not of somebody administering it.
func TestUIRequestsAreNotRecorded(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, echoEndpoint)
	logIn(t, b)

	b.get("/projects/checkout")

	if got := len(log.recorded()); got != 0 {
		t.Errorf("%d exchanges recorded for UI traffic, want none", got)
	}
}

func TestRecordedBodiesAreTruncated(t *testing.T) {
	const limit = 16

	log := newFakeLog()
	b := logBrowser(t, log, limit, mockEndpoint{
		method: "POST", path: "/orders", status: 200,
		body: strings.Repeat("y", limit*3),
	})

	sent := strings.Repeat("x", limit*2)
	b.do(http.MethodPost, "/m/checkout/orders", sent)

	ex, ok := log.last()
	if !ok {
		t.Fatal("the request was not recorded")
	}

	if len(ex.RequestBody) != limit {
		t.Errorf("recorded %d bytes of the request body, want the %d-byte limit",
			len(ex.RequestBody), limit)
	}
	if !ex.RequestBodyTruncated {
		t.Error("RequestBodyTruncated is false for a body that was cut short")
	}
	if len(ex.ResponseBody) != limit {
		t.Errorf("recorded %d bytes of the response body, want the %d-byte limit",
			len(ex.ResponseBody), limit)
	}
	if !ex.ResponseBodyTruncated {
		t.Error("ResponseBodyTruncated is false for a body that was cut short")
	}
	if got := string(ex.RequestBody); got != sent[:limit] {
		t.Errorf("recorded request body = %q, want the first %d bytes", got, limit)
	}
}

// A body of exactly the limit is complete, not truncated. The off-by-one here
// would be invisible in the interface and wrong in the record.
func TestABodyOfExactlyTheLimitIsNotTruncated(t *testing.T) {
	const limit = 16

	log := newFakeLog()
	b := logBrowser(t, log, limit, mockEndpoint{
		method: "POST", path: "/orders", status: 200, body: strings.Repeat("y", limit),
	})

	b.do(http.MethodPost, "/m/checkout/orders", strings.Repeat("x", limit))

	ex, _ := log.last()
	if ex.RequestBodyTruncated || ex.ResponseBodyTruncated {
		t.Error("a body of exactly the limit was recorded as truncated")
	}
}

// The recording cap is about what is kept, not about what is served. The
// handler below must still receive every byte the client sent — which is what
// this checks, by storing a document larger than the cap and reading it back.
func TestRecordingDoesNotTruncateWhatTheHandlerSees(t *testing.T) {
	const limit = 16

	collectionID := uuid.New()
	docs := newFakeDocuments().forCollection(collectionID)

	project := core.MockProject{ID: testProjectID, Slug: logSlug}
	store := projectStore()
	store.documents = docs
	store.exchanges = newFakeLog()
	store.mockData = func(context.Context) (core.MockData, error) {
		return core.MockData{
			Projects: []core.MockProject{project},
			Endpoints: []core.MockEndpoint{{
				ProjectSlug: logSlug,
				Endpoint: core.Endpoint{
					ID:           uuid.New(),
					ProjectID:    project.ID,
					Method:       core.MethodAny,
					Path:         "/items",
					Kind:         core.KindCollection,
					IsEnabled:    true,
					CollectionID: collectionID,
				},
			}},
		}, nil
	}

	log := store.exchanges
	b := newBrowserWith(t, store, func(o *Options) {
		o.Recorder = log
		o.LogBodyLimit = limit
	})

	note := strings.Repeat("z", limit*4)
	resp, body := b.do(http.MethodPost, "/m/checkout/items", `{"note":"`+note+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}

	_, stored := b.get("/m/checkout/items/1")
	if !strings.Contains(stored, note) {
		t.Errorf("the stored document lost part of the body the client sent:\n%s", stored)
	}

	// The first of the two exchanges is the POST; the second is the GET that
	// read it back.
	recorded := log.recorded()
	if len(recorded) != 2 {
		t.Fatalf("recorded %d exchanges, want 2", len(recorded))
	}
	if !recorded[0].RequestBodyTruncated {
		t.Error("the recorded copy was not marked as truncated")
	}
}

// A request with no body at all records none, rather than an empty one that
// looks like a client which sent "".
func TestARequestWithNoBodyRecordsNone(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, mockEndpoint{
		method: "GET", path: "/orders", status: 200, body: `[]`,
	})

	b.get("/m/checkout/orders")

	ex, _ := log.last()
	if len(ex.RequestBody) != 0 {
		t.Errorf("RequestBody = %q, want nothing", ex.RequestBody)
	}
	if ex.RequestBodyTruncated {
		t.Error("RequestBodyTruncated is true for a request with no body")
	}
}

// A delayed endpoint extends its own write deadline through
// http.ResponseController, which has to keep reaching the real writer through
// the recording wrapper. Without Unwrap it would not.
func TestRecordingLeavesADelayedResponseWorking(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, mockEndpoint{
		method: "GET", path: "/slow", status: 200, body: "late", delayMS: 20,
	})

	resp, body := b.get("/m/checkout/slow")
	if resp.StatusCode != http.StatusOK || body != "late" {
		t.Fatalf("status = %d, body = %q, want 200 and the endpoint's body", resp.StatusCode, body)
	}

	ex, ok := log.last()
	if !ok {
		t.Fatal("the delayed request was not recorded")
	}
	if ex.Duration <= 0 {
		t.Errorf("Duration = %v, want the time the request actually took", ex.Duration)
	}
}
