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

var testCollectionID = uuid.MustParse("44444444-4444-4444-8444-444444444444")

func testCollection() core.Collection {
	return core.Collection{
		ID:         testCollectionID,
		ProjectID:  testProjectID,
		Name:       "users",
		Seed:       json.RawMessage(`[{"id":1,"name":"Ada"}]`),
		IDField:    "id",
		IDStrategy: core.IDSerial,
		NextSerial: 2,
		Documents:  3,
	}
}

// collectionStore is projectStore plus one collection in that project.
func collectionStore() stubStore {
	store := projectStore()
	store.collectionsByProject = func(context.Context, uuid.UUID, uuid.UUID) ([]core.Collection, error) {
		return []core.Collection{testCollection()}, nil
	}
	store.collectionByOwner = func(_ context.Context, _ uuid.UUID, id uuid.UUID) (core.Collection, error) {
		if id != testCollectionID {
			return core.Collection{}, core.ErrNotFound
		}
		return testCollection(), nil
	}
	store.collectionByOwnerName = func(_ context.Context, _ uuid.UUID, slug, name string) (core.Collection, error) {
		if slug != "checkout" || name != "users" {
			return core.Collection{}, core.ErrNotFound
		}
		return testCollection(), nil
	}
	return store
}

func collectionValues() url.Values {
	return url.Values{
		"name":        {"orders"},
		"id_field":    {"id"},
		"id_strategy": {"serial"},
		"seed":        {`[{"id":1,"total":10}]`},
	}
}

func TestProjectPageListsCollections(t *testing.T) {
	b := newBrowser(t, collectionStore())
	logIn(t, b)

	_, body := b.get("/projects/checkout")

	for _, want := range []string{"users", "serial", "Reset"} {
		if !strings.Contains(body, want) {
			t.Errorf("the collection list does not show %q:\n%s", want, body)
		}
	}
	// The reset URL on the page is the one a script will use, so it is the
	// documented shape and not a second one invented for the button.
	if !strings.Contains(body, "/api/v1/projects/checkout/collections/users/reset") {
		t.Error("the reset button does not post to the management API path")
	}
}

func TestCreateCollection(t *testing.T) {
	var got core.CollectionInput
	store := collectionStore()
	store.createCollection = func(_ context.Context, _, projectID uuid.UUID, in core.CollectionInput) (core.Collection, error) {
		got = in
		if projectID != testProjectID {
			t.Errorf("collection created under project %s, want %s", projectID, testProjectID)
		}
		return testCollection(), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/checkout/collections/new", "/projects/checkout/collections", collectionValues())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if got.Name != "orders" || got.IDStrategy != core.IDSerial {
		t.Errorf("stored %+v, want the values that were submitted", got)
	}
	if !strings.Contains(got.Seed, `"total"`) {
		t.Errorf("the seed did not reach the store: %q", got.Seed)
	}
	// Creating a collection is half the job; the flash says what the other half
	// is, because a collection nothing serves answers nothing.
	if !strings.Contains(body, "collection") {
		t.Errorf("nothing on the page says what to do next:\n%s", body)
	}
}

// A seed that is not a JSON array comes back in the editor with the reason
// beside it, rather than as a check violation from the database.
func TestCreateCollectionRejectsABadSeed(t *testing.T) {
	store := collectionStore()
	store.createCollection = func(_ context.Context, _, _ uuid.UUID, in core.CollectionInput) (core.Collection, error) {
		return core.Collection{}, in.Validate()
	}

	b := newBrowser(t, store)
	logIn(t, b)

	values := collectionValues()
	values.Set("seed", `{"id":1}`)

	resp, body := b.post("/projects/checkout/collections/new", "/projects/checkout/collections", values)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "JSON array") {
		t.Errorf("the page does not say what was wrong:\n%s", body)
	}
	if !strings.Contains(body, `{&#34;id&#34;:1}`) && !strings.Contains(body, `{"id":1}`) {
		t.Errorf("the rejected seed did not come back in the editor:\n%s", body)
	}
}

// Saving the form does not apply the seed. Throwing away the documents somebody
// is working with, as a side effect of editing the fixture they will restore
// later, would be a surprise; the reset button is the deliberate act.
func TestEditingACollectionSaysItDidNotReset(t *testing.T) {
	store := collectionStore()
	store.updateCollection = func(_ context.Context, _, id uuid.UUID, _ core.CollectionInput) (core.Collection, error) {
		if id != testCollectionID {
			t.Errorf("updated %s, want %s", id, testCollectionID)
		}
		return testCollection(), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	edit := "/projects/checkout/collections/" + testCollectionID.String() + "/edit"
	action := "/projects/checkout/collections/" + testCollectionID.String()

	resp, body := b.post(edit, action, collectionValues())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "reset") {
		t.Errorf("nothing says the documents were left alone:\n%s", body)
	}
}

// A collection belonging to somebody else is the same 404 as one that never
// existed, and so is one that exists in another project.
func TestCollectionOfAnotherProjectIsNotFound(t *testing.T) {
	store := collectionStore()
	store.collectionByOwner = func(context.Context, uuid.UUID, uuid.UUID) (core.Collection, error) {
		other := testCollection()
		other.ProjectID = uuid.New()
		return other, nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, _ := b.get("/projects/checkout/collections/" + testCollectionID.String() + "/edit")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The reset route answers JSON to a programmatic caller and sends the browser
// back where it came from. One handler, because two would be two places for the
// ownership check to be got wrong.
func TestResetAnswersJSON(t *testing.T) {
	var reset uuid.UUID
	store := collectionStore()
	store.resetCollection = func(_ context.Context, id uuid.UUID) (int, error) {
		reset = id
		return 4, nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	// The token comes from the project page, which is where the button lives.
	values := url.Values{}
	resp, body := b.post("/projects/checkout", "/api/v1/projects/checkout/collections/users/reset", values)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	if reset != testCollectionID {
		t.Errorf("reset %s, want %s", reset, testCollectionID)
	}

	var result resetResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if result.Project != "checkout" || result.Collection != "users" || result.Documents != 4 {
		t.Errorf("result = %+v, want checkout/users with 4 documents", result)
	}
}

func TestResetOfAnUnknownCollection(t *testing.T) {
	b := newBrowser(t, collectionStore())
	logIn(t, b)

	resp, body := b.post("/projects/checkout", "/api/v1/projects/checkout/collections/nothing/reset", url.Values{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "nothing") {
		t.Errorf("the message does not name what was asked for: %s", body)
	}
}

// An anonymous caller on the API side gets 401 and a sentence, not a redirect
// to a login page it cannot fill in.
func TestResetWithoutAnAccount(t *testing.T) {
	b := newBrowser(t, collectionStore()).noFollow()

	resp, body := b.postRaw("/api/v1/projects/checkout/collections/users/reset",
		url.Values{}, b.sameOriginHeaders("/projects/checkout"))

	// No CSRF token was fetched and no bearer token was sent, so the request is
	// refused — by the guard or by the authentication, whichever answers first.
	// A caller with neither credential fails closed, which is what M6 did not
	// change when it made this route scriptable with a token.
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the request refused:\n%s", resp.StatusCode, body)
	}
	// And the refusal is JSON, because the caller on this side of the
	// application has been parsing JSON all along.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON on an API refusal", ct)
	}
	if !strings.Contains(body, `"error"`) {
		t.Errorf("the refusal is not the JSON error shape: %s", body)
	}
}

// Choosing the collection kind stores a collection endpoint: the wildcard verb,
// the collection it names, and no status code of its own.
func TestCreateCollectionEndpoint(t *testing.T) {
	var got core.EndpointInput
	store := collectionStore()
	store.createEndpoint = func(_ context.Context, _, _ uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
		got = in
		return core.Endpoint{
			ID: testEndpointID, ProjectID: testProjectID, Kind: core.KindCollection,
			Method: core.MethodAny, Path: in.Path, IsEnabled: true,
			CollectionID: in.CollectionID,
		}, nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	values := url.Values{
		"kind":          {core.KindCollection},
		"path":          {"/users"},
		"collection_id": {testCollectionID.String()},
		"delay_ms":      {"0"},
		"is_enabled":    {"1"},
		"headers":       {""},
		// The static half of the form is still submitted when JavaScript is off.
		// It has to be ignored rather than refused, and this is where that is
		// checked: the status code below is not a valid one.
		"status_code": {"not a number"},
		"method":      {"DELETE"},
		"body":        {"whatever"},
	}

	resp, body := b.post("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", values)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if got.Kind != core.KindCollection {
		t.Errorf("kind = %q, want %q", got.Kind, core.KindCollection)
	}
	if got.CollectionID != testCollectionID {
		t.Errorf("collection = %s, want %s", got.CollectionID, testCollectionID)
	}
	if got.Path != "/users" {
		t.Errorf("path = %q, want /users", got.Path)
	}
}

// A collection endpoint with no collection chosen is refused with a message on
// the selector, which is what happens when the project has none yet.
func TestCollectionEndpointNeedsACollection(t *testing.T) {
	store := collectionStore()
	store.createEndpoint = func(_ context.Context, _, _ uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
		return core.Endpoint{}, in.Validate()
	}

	b := newBrowser(t, store)
	logIn(t, b)

	values := url.Values{
		"kind": {core.KindCollection}, "path": {"/users"},
		"collection_id": {""}, "delay_ms": {"0"}, "is_enabled": {"1"},
		"status_code": {"200"}, "method": {"GET"},
	}

	resp, body := b.post("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", values)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Choose the collection") {
		t.Errorf("the page does not say what is missing:\n%s", body)
	}
}

// The endpoint form offers the project's collections, so that choosing one is
// picking from a list rather than pasting a uuid.
func TestEndpointFormOffersTheCollections(t *testing.T) {
	b := newBrowser(t, collectionStore())
	logIn(t, b)

	_, body := b.get("/projects/checkout/endpoints/new")
	if !strings.Contains(body, testCollectionID.String()) {
		t.Errorf("the collection is not offered:\n%s", body)
	}
	if !strings.Contains(body, `value="collection"`) {
		t.Errorf("the kind selector has no collection option:\n%s", body)
	}
}
