//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// newStoreWithCollection is where most tests in this file start: a user, a
// project, and a collection seeded with three documents.
func newStoreWithCollection(t *testing.T) (*core.Store, core.User, core.Project, core.Collection) {
	t.Helper()

	store, user, project := newStoreWithProject(t)
	collection, err := store.CreateCollection(t.Context(), user.ID, project.ID, core.CollectionInput{
		Name:       "users",
		IDField:    "id",
		IDStrategy: core.IDSerial,
		Seed: `[
			{"id": 1, "name": "Ada",   "role": "admin",    "age": 36},
			{"id": 2, "name": "Alan",  "role": "engineer", "age": 41},
			{"id": 3, "name": "Grace", "role": "admin",    "age": 45}
		]`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	return store, user, project, collection
}

// A new collection is seeded on creation. A collection created empty when it
// was given something to hold would need a reset before it did anything, which
// is a step nobody would guess at.
func TestCreateCollectionAppliesTheSeed(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	page, err := store.ListDocuments(t.Context(), collection.ID, listQuery(t, ""))
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if page.Total != 3 || len(page.Documents) != 3 {
		t.Fatalf("listed %d of %d documents, want 3 of 3", len(page.Documents), page.Total)
	}

	// Seed order is listing order. `created_at` is identical across a seed —
	// one statement, one now() — so this is what the `seq` column is for.
	wantNames := []string{"Ada", "Alan", "Grace"}
	for i, doc := range page.Documents {
		if name := field(t, doc, "name"); name != wantNames[i] {
			t.Errorf("document %d is %v, want %s", i, name, wantNames[i])
		}
		if doc.PublicID != strconv.Itoa(i+1) {
			t.Errorf("document %d has id %q, want %d", i, doc.PublicID, i+1)
		}
	}
}

// The counter is left past every id the seed used, so the first create after a
// seed cannot collide with a seeded document.
func TestCreateAllocatesAfterTheSeed(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	doc, err := store.CreateDocument(t.Context(), collection.ID, []byte(`{"name":"Katherine"}`))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if doc.PublicID != "4" {
		t.Fatalf("id = %q, want 4 — past the seeded 1, 2 and 3", doc.PublicID)
	}
	// The identifier goes into the document as a number, because a client
	// mocking a real API expects the type the real one would send.
	if got := field(t, doc, "id"); got != float64(4) {
		t.Errorf("body carries id %#v, want the number 4", got)
	}

	fetched, err := store.GetDocument(t.Context(), collection.ID, "4")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if field(t, fetched, "name") != "Katherine" {
		t.Errorf("fetched %s, want the document that was created", fetched.Body)
	}
}

// A client-supplied identifier is overwritten. Two clients posting the same
// fixture would otherwise collide, and the second would get an error from a
// server that exists to be predictable.
func TestCreateIgnoresAClientSuppliedID(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	doc, err := store.CreateDocument(t.Context(), collection.ID, []byte(`{"id":999,"name":"Katherine"}`))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if doc.PublicID != "4" {
		t.Errorf("id = %q, want the server's 4 rather than the client's 999", doc.PublicID)
	}
}

// The uuid strategy leaves the counter alone and hands out identifiers nobody
// can guess the next of.
func TestCreateUnderTheUUIDStrategy(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	collection, err := store.CreateCollection(t.Context(), user.ID, project.ID, core.CollectionInput{
		Name: "sessions", IDStrategy: core.IDUUID, Seed: `[{"label":"first"}]`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	doc, err := store.CreateDocument(t.Context(), collection.ID, []byte(`{"label":"second"}`))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, err := uuid.Parse(doc.PublicID); err != nil {
		t.Fatalf("id %q is not a uuid: %v", doc.PublicID, err)
	}
	if got := field(t, doc, "id"); got != doc.PublicID {
		t.Errorf("body carries id %#v, want the uuid as a string", got)
	}
}

// Identifier allocation is the one place two requests can genuinely race. The
// counter is advanced in the same statement that reads it, so the second create
// waits on the row lock and reads what the first left behind.
func TestConcurrentCreatesGetDistinctIDs(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	const writers = 12
	ids := make([]string, writers)
	errs := make([]error, writers)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := store.CreateDocument(t.Context(), collection.ID,
				[]byte(`{"n":`+strconv.Itoa(i)+`}`))
			ids[i], errs[i] = doc.PublicID, err
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, writers)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if seen[ids[i]] {
			t.Fatalf("id %q was handed out twice", ids[i])
		}
		seen[ids[i]] = true
	}

	page, err := store.ListDocuments(t.Context(), collection.ID, listQuery(t, ""))
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if page.Total != 3+writers {
		t.Errorf("collection holds %d documents, want %d", page.Total, 3+writers)
	}
}

// PUT says what the whole document is; PATCH says what changed. The identifier
// survives both, because it is the thing the request addressed.
func TestReplaceAndPatch(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	replaced, err := store.ReplaceDocument(t.Context(), collection.ID, "1", []byte(`{"name":"Ada Lovelace"}`))
	if err != nil {
		t.Fatalf("ReplaceDocument: %v", err)
	}
	if field(t, replaced, "name") != "Ada Lovelace" {
		t.Errorf("body = %s, want the new name", replaced.Body)
	}
	if _, kept := object(t, replaced)["role"]; kept {
		t.Errorf("replace kept a field the request left out: %s", replaced.Body)
	}
	if field(t, replaced, "id") != float64(1) {
		t.Errorf("replace lost the identifier: %s", replaced.Body)
	}

	patched, err := store.PatchDocument(t.Context(), collection.ID, "1", []byte(`{"role":"pioneer"}`))
	if err != nil {
		t.Fatalf("PatchDocument: %v", err)
	}
	if field(t, patched, "role") != "pioneer" {
		t.Errorf("body = %s, want the new role", patched.Body)
	}
	if field(t, patched, "name") != "Ada Lovelace" {
		t.Errorf("patch discarded a field it was not asked about: %s", patched.Body)
	}
}

// The merge is one level deep, and deliberately so: a deep merge would leave no
// way to remove a nested field, and PUT is there for callers who want to say
// what the whole document is.
func TestPatchIsShallow(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	if _, err := store.ReplaceDocument(t.Context(), collection.ID, "1",
		[]byte(`{"address":{"city":"London","street":"Marylebone"}}`)); err != nil {
		t.Fatalf("ReplaceDocument: %v", err)
	}

	patched, err := store.PatchDocument(t.Context(), collection.ID, "1",
		[]byte(`{"address":{"city":"Turin"}}`))
	if err != nil {
		t.Fatalf("PatchDocument: %v", err)
	}

	address, ok := object(t, patched)["address"].(map[string]any)
	if !ok {
		t.Fatalf("address is not an object: %s", patched.Body)
	}
	if address["city"] != "Turin" {
		t.Errorf("city = %v, want Turin", address["city"])
	}
	if _, kept := address["street"]; kept {
		t.Errorf("the nested object was merged rather than replaced: %s", patched.Body)
	}
}

// A write against a document that is not there is a 404's worth of error, not a
// silently created one. PUT is a replace, and there is nothing to replace.
func TestWritesToAMissingDocument(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	if _, err := store.ReplaceDocument(t.Context(), collection.ID, "99", []byte(`{"a":1}`)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("ReplaceDocument = %v, want ErrNotFound", err)
	}
	if _, err := store.PatchDocument(t.Context(), collection.ID, "99", []byte(`{"a":1}`)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("PatchDocument = %v, want ErrNotFound", err)
	}
	if err := store.DeleteDocument(t.Context(), collection.ID, "99"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("DeleteDocument = %v, want ErrNotFound", err)
	}
	if _, err := store.GetDocument(t.Context(), collection.ID, "99"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetDocument = %v, want ErrNotFound", err)
	}
}

// Filters are containment tests against the GIN index. A query string has no
// types, so a value that also reads as a number is matched both ways.
func TestListFilters(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	collection, err := store.CreateCollection(t.Context(), user.ID, project.ID, core.CollectionInput{
		Name: "records",
		Seed: `[
			{"id": 1, "status": "active", "count": 5,  "flagged": true,  "code": "007"},
			{"id": 2, "status": "trial",  "count": 10, "flagged": false, "code": "008"},
			{"id": 3, "status": "active", "count": 5,  "flagged": true,  "code": "009"},
			{"id": 4, "status": "closed", "count": "5", "flagged": null, "code": "010"}
		]`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	tests := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{name: "a string field", query: "status=active", wantIDs: []string{"1", "3"}},
		{name: "either of two values", query: "status=active&status=trial", wantIDs: []string{"1", "2", "3"}},
		{
			// The number 5 and the string "5" both match, because the query
			// string cannot say which was meant.
			name: "a number matches both readings", query: "count=5", wantIDs: []string{"1", "3", "4"},
		},
		{name: "a boolean", query: "flagged=true", wantIDs: []string{"1", "3"}},
		{name: "null", query: "flagged=null", wantIDs: []string{"4"}},
		{
			// A leading zero is how a product code is written, and it is not a
			// JSON number, so only the string reading applies.
			name: "a padded number stays text", query: "code=007", wantIDs: []string{"1"},
		},
		{name: "two fields are both required", query: "status=active&count=5", wantIDs: []string{"1", "3"}},
		{name: "an impossible combination", query: "status=active&count=10", wantIDs: []string{}},
		{name: "a field nothing has", query: "colour=blue", wantIDs: []string{}},
		{name: "the identifier itself", query: "id=2", wantIDs: []string{"2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.ListDocuments(t.Context(), collection.ID, listQuery(t, tt.query))
			if err != nil {
				t.Fatalf("ListDocuments(%q): %v", tt.query, err)
			}
			if page.Total != len(tt.wantIDs) {
				t.Errorf("total = %d, want %d", page.Total, len(tt.wantIDs))
			}

			got := make([]string, len(page.Documents))
			for i, doc := range page.Documents {
				got[i] = doc.PublicID
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("ids = %v, want %v", got, tt.wantIDs)
			}
			for i := range got {
				if got[i] != tt.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", got, tt.wantIDs)
				}
			}
		})
	}
}

func TestListSortAndPage(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	tests := []struct {
		name      string
		query     string
		wantIDs   []string
		wantTotal int
	}{
		{name: "insertion order by default", query: "", wantIDs: []string{"1", "2", "3"}, wantTotal: 3},
		{name: "reversed insertion order", query: "_order=desc", wantIDs: []string{"3", "2", "1"}, wantTotal: 3},
		{name: "by a string field", query: "_sort=name", wantIDs: []string{"1", "2", "3"}, wantTotal: 3},
		{name: "descending", query: "_sort=name&_order=desc", wantIDs: []string{"3", "2", "1"}, wantTotal: 3},
		{
			// jsonb orders numbers numerically, so 36 comes before 41 rather
			// than after it the way text ordering would have them.
			name: "by a number field", query: "_sort=age", wantIDs: []string{"1", "2", "3"}, wantTotal: 3,
		},
		{name: "a page", query: "_limit=2", wantIDs: []string{"1", "2"}, wantTotal: 3},
		{name: "the second page", query: "_limit=2&_page=2", wantIDs: []string{"3"}, wantTotal: 3},
		{
			// Past the end the total is still the truth, so a client paging
			// through is told it has run off rather than that nothing is there.
			name: "past the end", query: "_limit=2&_page=9", wantIDs: []string{}, wantTotal: 3,
		},
		{
			// A field no document has sorts them all to the end, and `seq`
			// breaks the tie, so the order is still stable rather than random.
			name: "by a field nothing has", query: "_sort=nickname", wantIDs: []string{"1", "2", "3"}, wantTotal: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := store.ListDocuments(t.Context(), collection.ID, listQuery(t, tt.query))
			if err != nil {
				t.Fatalf("ListDocuments(%q): %v", tt.query, err)
			}
			if page.Total != tt.wantTotal {
				t.Errorf("total = %d, want %d", page.Total, tt.wantTotal)
			}

			got := make([]string, len(page.Documents))
			for i, doc := range page.Documents {
				got[i] = doc.PublicID
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("ids = %v, want %v", got, tt.wantIDs)
			}
			for i := range got {
				if got[i] != tt.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", got, tt.wantIDs)
				}
			}
		})
	}
}

// Paging through equal values has to return each document once. Without the
// `seq` tie-break the order within a group of equal sort keys is whatever the
// plan produced, and a document could appear on two pages or on none.
func TestPagingThroughEqualValuesIsStable(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	seed := make([]string, 20)
	for i := range seed {
		seed[i] = `{"id":` + strconv.Itoa(i+1) + `,"group":"same"}`
	}
	collection, err := store.CreateCollection(t.Context(), user.ID, project.ID, core.CollectionInput{
		Name: "rows", Seed: "[" + strings.Join(seed, ",") + "]",
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	seen := make(map[string]int)
	for page := 1; page <= 4; page++ {
		got, err := store.ListDocuments(t.Context(), collection.ID,
			listQuery(t, "_sort=group&_limit=5&_page="+strconv.Itoa(page)))
		if err != nil {
			t.Fatalf("ListDocuments page %d: %v", page, err)
		}
		for _, doc := range got.Documents {
			seen[doc.PublicID]++
		}
	}

	if len(seen) != 20 {
		t.Fatalf("paging returned %d distinct documents, want 20", len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("document %s appeared %d times across the pages", id, times)
		}
	}
}

// Reset is what a test suite calls between runs: everything written since the
// last one goes, the fixture comes back, and the counter goes back with it.
func TestResetRestoresTheSeed(t *testing.T) {
	store, _, _, collection := newStoreWithCollection(t)

	if _, err := store.CreateDocument(t.Context(), collection.ID, []byte(`{"name":"Katherine"}`)); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if err := store.DeleteDocument(t.Context(), collection.ID, "1"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if _, err := store.ReplaceDocument(t.Context(), collection.ID, "2", []byte(`{"name":"changed"}`)); err != nil {
		t.Fatalf("ReplaceDocument: %v", err)
	}

	count, err := store.ResetCollection(t.Context(), collection.ID)
	if err != nil {
		t.Fatalf("ResetCollection: %v", err)
	}
	if count != 3 {
		t.Fatalf("reset restored %d documents, want 3", count)
	}

	page, err := store.ListDocuments(t.Context(), collection.ID, listQuery(t, ""))
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("collection holds %d documents after a reset, want 3", page.Total)
	}
	// The deleted one is back, the edited one is as the fixture had it, and the
	// created one is gone.
	ada, err := store.GetDocument(t.Context(), collection.ID, "1")
	if err != nil {
		t.Fatalf("the deleted document did not come back: %v", err)
	}
	if field(t, ada, "name") != "Ada" {
		t.Errorf("document 1 = %s, want the seeded Ada", ada.Body)
	}
	alan, err := store.GetDocument(t.Context(), collection.ID, "2")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if field(t, alan, "name") != "Alan" {
		t.Errorf("document 2 = %s, want the edit undone", alan.Body)
	}
	if _, err := store.GetDocument(t.Context(), collection.ID, "4"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the document created since the last reset survived it")
	}

	// And the counter is back past the seed, so the next create is 4 again.
	next, err := store.CreateDocument(t.Context(), collection.ID, []byte(`{"name":"again"}`))
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if next.PublicID != "4" {
		t.Errorf("id after a reset = %q, want 4", next.PublicID)
	}
}

// Editing the seed does not apply it. Somebody preparing the fixture for the
// next run should not lose the documents they are working with as a side effect
// of saving.
func TestUpdateDoesNotApplyTheSeed(t *testing.T) {
	store, user, _, collection := newStoreWithCollection(t)

	if _, err := store.UpdateCollection(t.Context(), user.ID, collection.ID, core.CollectionInput{
		Name: "users", IDField: "id", IDStrategy: core.IDSerial,
		Seed: `[{"id":1,"name":"Only one now"}]`,
	}); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}

	page, err := store.ListDocuments(t.Context(), collection.ID, listQuery(t, ""))
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("saving the form changed the stored documents: %d, want the original 3", page.Total)
	}

	// The reset is the deliberate act, and it applies the new seed.
	count, err := store.ResetCollection(t.Context(), collection.ID)
	if err != nil {
		t.Fatalf("ResetCollection: %v", err)
	}
	if count != 1 {
		t.Errorf("reset restored %d documents, want the 1 the new seed holds", count)
	}
}

// A collection in somebody else's project is the same answer as one that never
// existed, at every entry point.
func TestCollectionsAreScopedToTheirOwner(t *testing.T) {
	store, _, project, collection := newStoreWithCollection(t)

	intruder, err := store.RegisterUser(t.Context(), "mallory@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	if _, err := store.CollectionByOwnerAndID(t.Context(), intruder.ID, collection.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("CollectionByOwnerAndID = %v, want ErrNotFound", err)
	}
	if _, err := store.CollectionByOwnerAndName(t.Context(), intruder.ID, project.Slug, "users"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("CollectionByOwnerAndName = %v, want ErrNotFound", err)
	}
	if _, err := store.UpdateCollection(t.Context(), intruder.ID, collection.ID, core.CollectionInput{
		Name: "stolen", Seed: "[]",
	}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("UpdateCollection = %v, want ErrNotFound", err)
	}
	if err := store.DeleteCollection(t.Context(), intruder.ID, collection.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("DeleteCollection = %v, want ErrNotFound", err)
	}
	if _, err := store.CreateCollection(t.Context(), intruder.ID, project.ID, core.CollectionInput{
		Name: "theirs", Seed: "[]",
	}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("CreateCollection into another account's project = %v, want ErrNotFound", err)
	}

	list, err := store.CollectionsByProject(t.Context(), intruder.ID, project.ID)
	if err != nil {
		t.Fatalf("CollectionsByProject: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("another account's collections are listed: %v", list)
	}
}

// Two collections of one name in one project would make the reset URL
// ambiguous, so the database refuses the second.
func TestCollectionNamesAreUniquePerProject(t *testing.T) {
	store, user, project, _ := newStoreWithCollection(t)

	_, err := store.CreateCollection(t.Context(), user.ID, project.ID, core.CollectionInput{
		Name: "users", Seed: "[]",
	})
	if !errors.Is(err, core.ErrCollectionExists) {
		t.Fatalf("CreateCollection = %v, want ErrCollectionExists", err)
	}

	// The same name in another project is fine: the pair is what is unique.
	other, err := store.CreateProject(t.Context(), user.ID, "billing", "Billing API")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := store.CreateCollection(t.Context(), user.ID, other.ID, core.CollectionInput{
		Name: "users", Seed: "[]",
	}); err != nil {
		t.Errorf("the same name in another project was refused: %v", err)
	}
}

// Deleting a collection takes its documents and the endpoint serving it, both
// by cascade. The endpoint is the part worth checking: without it, the route
// table would keep a route pointing at a collection that is gone.
func TestDeletingACollectionTakesItsEndpoint(t *testing.T) {
	store, user, project, collection := newStoreWithCollection(t)

	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, core.EndpointInput{
		Kind: core.KindCollection, Path: "/users", IsEnabled: true, CollectionID: collection.ID,
	}); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	if err := store.DeleteCollection(t.Context(), user.ID, collection.ID); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	endpoints, err := store.EndpointsByProject(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatalf("EndpointsByProject: %v", err)
	}
	if len(endpoints) != 0 {
		t.Errorf("the endpoint outlived the collection it served: %v", endpoints)
	}

	data, err := store.MockData(t.Context())
	if err != nil {
		t.Fatalf("MockData: %v", err)
	}
	if len(data.Endpoints) != 0 {
		t.Errorf("the route table would still serve %v", data.Endpoints)
	}
}

// A collection endpoint reaches the route table with the collection it names,
// which is the only thing the matcher needs in order to hand the request to the
// right documents.
func TestCollectionEndpointReachesTheRouteTable(t *testing.T) {
	store, user, project, collection := newStoreWithCollection(t)

	created, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, core.EndpointInput{
		Kind: core.KindCollection, Path: "/users/", IsEnabled: true, CollectionID: collection.ID,
	})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.Kind != core.KindCollection {
		t.Errorf("kind = %q, want %q", created.Kind, core.KindCollection)
	}
	if created.Method != core.MethodAny {
		t.Errorf("method = %q, want the wildcard", created.Method)
	}
	if created.Path != "/users" {
		t.Errorf("path = %q, want the normalised form", created.Path)
	}
	if created.CollectionID != collection.ID {
		t.Errorf("collection = %s, want %s", created.CollectionID, collection.ID)
	}

	data, err := store.MockData(t.Context())
	if err != nil {
		t.Fatalf("MockData: %v", err)
	}
	if len(data.Endpoints) != 1 || data.Endpoints[0].CollectionID != collection.ID {
		t.Fatalf("the route table would be built from %+v", data.Endpoints)
	}
}

// An endpoint may change kind. Both statements write every kind-specific
// column, so nothing is left behind for the check constraint to trip over.
func TestEndpointChangesKind(t *testing.T) {
	store, user, project, collection := newStoreWithCollection(t)

	endpoint, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, core.EndpointInput{
		Kind: core.KindStatic, Method: "GET", Path: "/users", StatusCode: 200,
		IsEnabled: true, Body: `{"static":true}`,
	})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	toCollection, err := store.UpdateEndpoint(t.Context(), user.ID, endpoint.ID, core.EndpointInput{
		Kind: core.KindCollection, Path: "/users", IsEnabled: true, CollectionID: collection.ID,
	})
	if err != nil {
		t.Fatalf("static to collection: %v", err)
	}
	if toCollection.Kind != core.KindCollection || toCollection.CollectionID != collection.ID {
		t.Fatalf("endpoint = %+v, want a collection endpoint", toCollection)
	}
	if toCollection.StatusCode != 0 {
		t.Errorf("status code = %d, want it cleared", toCollection.StatusCode)
	}

	backToStatic, err := store.UpdateEndpoint(t.Context(), user.ID, endpoint.ID, core.EndpointInput{
		Kind: core.KindStatic, Method: "GET", Path: "/users", StatusCode: 204, IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("collection to static: %v", err)
	}
	if backToStatic.Kind != core.KindStatic || backToStatic.CollectionID != uuid.Nil {
		t.Errorf("endpoint = %+v, want a static endpoint with no collection", backToStatic)
	}
}

// A collection endpoint may only name a collection in its own project.
func TestCollectionEndpointCannotBorrowAnotherProjectsCollection(t *testing.T) {
	store, user, _, collection := newStoreWithCollection(t)

	other, err := store.CreateProject(t.Context(), user.ID, "billing", "Billing API")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	_, err = store.CreateEndpoint(t.Context(), user.ID, other.ID, core.EndpointInput{
		Kind: core.KindCollection, Path: "/users", IsEnabled: true, CollectionID: collection.ID,
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("CreateEndpoint = %v, want ErrNotFound", err)
	}
}

// The Go rules and the database constraints have to agree. These inserts go
// straight past the validation, so what refuses them is the schema — which is
// what CONTEXT.md §7 asks for.
func TestSchemaRefusesWhatTheCollectionRulesRefuse(t *testing.T) {
	store, _, project, collection := newStoreWithCollection(t)
	pool := store.Pool()

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "a seed that is not an array",
			sql:  `insert into collections (project_id, name, seed) values ($1, 'bad', '{"id":1}'::jsonb)`,
			args: []any{project.ID},
		},
		{
			name: "an id strategy nobody implements",
			sql:  `insert into collections (project_id, name, id_strategy) values ($1, 'bad', 'snowflake')`,
			args: []any{project.ID},
		},
		{
			name: "a document that is not an object",
			sql:  `insert into documents (collection_id, public_id, body) values ($1, 'x', '[1,2]'::jsonb)`,
			args: []any{collection.ID},
		},
		{
			name: "two documents with one id",
			sql:  `insert into documents (collection_id, public_id, body) values ($1, '1', '{}'::jsonb)`,
			args: []any{collection.ID},
		},
		{
			name: "a collection endpoint with a status code",
			sql: `insert into endpoints (project_id, method, path_pattern, kind, collection_id, status_code)
			      values ($1, 'GET', '/x', 'collection', $2, 200)`,
			args: []any{project.ID, collection.ID},
		},
		{
			name: "a collection endpoint naming no collection",
			sql: `insert into endpoints (project_id, method, path_pattern, kind)
			      values ($1, 'GET', '/x', 'collection')`,
			args: []any{project.ID},
		},
		{
			name: "a static endpoint naming a collection",
			sql: `insert into endpoints (project_id, method, path_pattern, kind, status_code, collection_id)
			      values ($1, 'GET', '/x', 'static', 200, $2)`,
			args: []any{project.ID, collection.ID},
		},
		{
			name: "two collections of one name in one project",
			sql:  `insert into collections (project_id, name) values ($1, 'users')`,
			args: []any{project.ID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), tt.sql, tt.args...); err == nil {
				t.Errorf("the database accepted %s", tt.name)
			}
		})
	}

	// `seq` is generated always, so nothing can supply its own and disturb the
	// ordering the listing depends on.
	if _, err := pool.Exec(t.Context(),
		`insert into documents (collection_id, public_id, body, seq) values ($1, 'z', '{}'::jsonb, 1)`,
		collection.ID); err == nil {
		t.Error("the database accepted a document with a hand-picked seq")
	}
}

func listQuery(t *testing.T, query string) core.ListQuery {
	t.Helper()

	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	q, err := core.ParseListQuery(values)
	if err != nil {
		t.Fatalf("ParseListQuery(%q): %v", query, err)
	}
	return q
}

func object(t *testing.T, doc core.Document) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(doc.Body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", doc.Body, err)
	}
	return decoded
}

func field(t *testing.T, doc core.Document, name string) any {
	t.Helper()
	return object(t, doc)[name]
}

// TestTheM3Milestone is M3's "done when": POST a record, GET it back, filter
// for it, delete it, reset, and it is gone. Everything it touches is real —
// Postgres, jsonb, the GIN index, the route table, the templates and the CSRF
// guard — and nothing is restarted between defining the collection and it
// answering.
func TestTheM3Milestone(t *testing.T) {
	s := newSite(t)
	registerAndCreateProject(t, s)

	// --- define a collection through the form ---------------------------
	resp, body := s.submit("/projects/checkout/collections/new", "/projects/checkout/collections",
		formValues(map[string]string{
			"name": "users", "id_field": "id", "id_strategy": "serial",
			"seed": `[{"id":1,"name":"Ada","role":"admin"},{"id":2,"name":"Alan","role":"engineer"}]`,
		}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create collection: status = %d\n%s", resp.StatusCode, body)
	}
	// The seed is applied on creation, so the collection is useful at once.
	if n := s.count(`select count(*) from documents`); n != 2 {
		t.Fatalf("%d documents after creating the collection, want the 2 seeded", n)
	}

	// --- and point an endpoint at it ------------------------------------
	collectionID := s.value(`select id::text from collections where name = 'users'`)
	resp, body = s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints",
		formValues(map[string]string{
			"kind": "collection", "path": "/users", "collection_id": collectionID,
			"delay_ms": "0", "is_enabled": "1", "headers": "",
		}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create collection endpoint: status = %d\n%s", resp.StatusCode, body)
	}

	// --- POST a record ---------------------------------------------------
	resp, body = s.send(http.MethodPost, "/m/checkout/users", `{"name":"Grace","role":"admin"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST: status = %d\n%s", resp.StatusCode, body)
	}
	location := resp.Header.Get("Location")
	if location != "/m/checkout/users/3" {
		t.Fatalf("Location = %q, want /m/checkout/users/3 — the id after the seeded 1 and 2", location)
	}

	// --- GET it back ------------------------------------------------------
	resp, body = s.get(location)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d\n%s", location, resp.StatusCode, body)
	}
	created := decodeDocument(t, body)
	if created["name"] != "Grace" || created["id"] != float64(3) {
		t.Fatalf("GET returned %v, want the record that was posted, with id 3", created)
	}

	// --- filter for it ----------------------------------------------------
	resp, body = s.get("/m/checkout/users?role=admin&_sort=name")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered list: status = %d\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Total-Count"); got != "2" {
		t.Errorf("X-Total-Count = %q, want 2 — Ada and Grace", got)
	}
	admins := decodeDocuments(t, body)
	if len(admins) != 2 || admins[0]["name"] != "Ada" || admins[1]["name"] != "Grace" {
		t.Fatalf("?role=admin&_sort=name returned %v, want Ada then Grace", admins)
	}

	// --- delete it --------------------------------------------------------
	resp, body = s.request(http.MethodDelete, location)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status = %d\n%s", resp.StatusCode, body)
	}
	if body != "" {
		t.Errorf("the 204 carried a body: %q", body)
	}
	if resp, _ := s.get(location); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE = %d, want 404", resp.StatusCode)
	}

	// --- change one of the seeded records, then reset ---------------------
	if resp, body := s.send(http.MethodPatch, "/m/checkout/users/1", `{"role":"pioneer"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: status = %d\n%s", resp.StatusCode, body)
	}

	resp, body = s.submit("/projects/checkout", "/api/v1/projects/checkout/collections/users/reset", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset: status = %d\n%s", resp.StatusCode, body)
	}
	var reset struct {
		Project    string `json:"project"`
		Collection string `json:"collection"`
		Documents  int    `json:"documents"`
	}
	if err := json.Unmarshal([]byte(body), &reset); err != nil {
		t.Fatalf("the reset answer is not JSON: %v\n%s", err, body)
	}
	if reset.Project != "checkout" || reset.Collection != "users" || reset.Documents != 2 {
		t.Fatalf("reset answered %+v, want checkout/users restored to 2 documents", reset)
	}

	// --- and everything since the seed is gone ----------------------------
	resp, body = s.get("/m/checkout/users")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list after reset: status = %d\n%s", resp.StatusCode, body)
	}
	after := decodeDocuments(t, body)
	if len(after) != 2 {
		t.Fatalf("%d documents after the reset, want the 2 seeded: %s", len(after), body)
	}
	if after[0]["role"] != "admin" {
		t.Errorf("the patched record was not restored: %v", after[0])
	}
	// The counter went back with the documents, so ids start again where the
	// seed left off rather than continuing past what has been discarded.
	resp, _ = s.send(http.MethodPost, "/m/checkout/users", `{"name":"Katherine"}`)
	if got := resp.Header.Get("Location"); got != "/m/checkout/users/3" {
		t.Errorf("Location after a reset = %q, want /m/checkout/users/3", got)
	}
}

// A collection endpoint answers the verbs it defines and no others, and says so
// the same way a static endpoint does.
func TestCollectionEndpointOverHTTP(t *testing.T) {
	s := newSite(t)
	registerAndCreateProject(t, s)

	if resp, body := s.submit("/projects/checkout/collections/new", "/projects/checkout/collections",
		formValues(map[string]string{
			"name": "users", "id_field": "id", "id_strategy": "serial",
			"seed": `[{"id":1,"name":"Ada"}]`,
		})); resp.StatusCode != http.StatusOK {
		t.Fatalf("create collection: status = %d\n%s", resp.StatusCode, body)
	}
	collectionID := s.value(`select id::text from collections where name = 'users'`)
	if resp, body := s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints",
		formValues(map[string]string{
			"kind": "collection", "path": "/users", "collection_id": collectionID,
			"delay_ms": "0", "is_enabled": "1", "headers": "X-Mock: yes",
		})); resp.StatusCode != http.StatusOK {
		t.Fatalf("create endpoint: status = %d\n%s", resp.StatusCode, body)
	}

	// A verb the collection does not answer at that shape is 405 with the ones
	// it does — the same answer a static route gives.
	resp, body := s.request(http.MethodDelete, "/m/checkout/users")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE on the collection root: status = %d\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, POST")
	}

	// The endpoint's own headers reach a collection response.
	resp, _ = s.get("/m/checkout/users")
	if got := resp.Header.Get("X-Mock"); got != "yes" {
		t.Errorf("X-Mock = %q, want yes", got)
	}

	// A listing query the server will not guess at is refused rather than
	// answered with something plausible.
	resp, body = s.get("/m/checkout/users?_limit=lots")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad _limit: status = %d, want 400\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "_limit") {
		t.Errorf("the message does not name the parameter: %s", body)
	}

	// And a disabled collection endpoint contributes none of its six routes.
	endpointID := s.value(`select id::text from endpoints where kind = 'collection'`)
	if resp, body := s.submit("/projects/checkout/endpoints/"+endpointID+"/edit",
		"/projects/checkout/endpoints/"+endpointID,
		formValues(map[string]string{
			"kind": "collection", "path": "/users", "collection_id": collectionID,
			"delay_ms": "0", "headers": "",
		})); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable endpoint: status = %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := s.get("/m/checkout/users"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a disabled collection endpoint still answers: %d", resp.StatusCode)
	}
}

// send is request with a body, for the mock writes — which carry no CSRF token
// and no session, because a test client is not a browser.
func (s *site) send(method, path, body string) (*http.Response, string) {
	s.t.Helper()

	req, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp, readBody(s.t, resp)
}

// value is a one-string query, for reading back an id the UI generated.
func (s *site) value(query string, args ...any) string {
	s.t.Helper()

	var v string
	if err := s.pool.QueryRow(s.t.Context(), query, args...).Scan(&v); err != nil {
		s.t.Fatalf("query %q: %v", query, err)
	}
	return v
}

func decodeDocument(t *testing.T, body string) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return doc
}

func decodeDocuments(t *testing.T, body string) []map[string]any {
	t.Helper()

	var docs []map[string]any
	if err := json.Unmarshal([]byte(body), &docs); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return docs
}
