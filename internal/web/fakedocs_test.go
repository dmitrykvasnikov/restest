package web

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// fakeDocuments is an in-memory stand-in for the document half of core.Store.
//
// It is a working little collection rather than a set of canned answers,
// because what these tests ask about is a *sequence* — POST something, then GET
// the thing that was posted — and a canned answer cannot fail that. What it is
// not is a second implementation to be trusted: jsonb containment, GIN-backed
// filtering and identifier allocation under concurrency are exercised against
// real Postgres in internal/integration. This file exists so that checking a
// Location header does not need Docker.
type fakeDocuments struct {
	mu          sync.Mutex
	idField     string
	next        int64
	order       []string
	byPublicID  map[string]map[string]any
	collectionO uuid.UUID
}

func newFakeDocuments() *fakeDocuments {
	return &fakeDocuments{
		idField:    "id",
		next:       1,
		byPublicID: make(map[string]map[string]any),
	}
}

// forCollection binds the fake to one collection id, so that a request routed
// to a different one is answered the way a deleted collection would be.
func (f *fakeDocuments) forCollection(id uuid.UUID) *fakeDocuments {
	f.collectionO = id
	return f
}

// seed adds documents the way a reset would, with the identifiers they state.
func (f *fakeDocuments) seed(bodies ...string) *fakeDocuments {
	for _, raw := range bodies {
		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			panic("fakeDocuments.seed: " + err.Error())
		}
		id := fmt.Sprint(object[f.idField])
		f.order = append(f.order, id)
		f.byPublicID[id] = object
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n >= f.next {
			f.next = n + 1
		}
	}
	return f
}

func (f *fakeDocuments) known(id uuid.UUID) bool {
	return f.collectionO == uuid.Nil || f.collectionO == id
}

func (f *fakeDocuments) list(id uuid.UUID, q core.ListQuery) (core.DocumentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.known(id) {
		return core.DocumentPage{}, core.ErrNotFound
	}

	matched := make([]string, 0, len(f.order))
	for _, publicID := range f.order {
		if matches(f.byPublicID[publicID], q.Filters) {
			matched = append(matched, publicID)
		}
	}
	if q.Sort != "" {
		slices.SortStableFunc(matched, func(a, b string) int {
			return strings.Compare(
				fmt.Sprint(f.byPublicID[a][q.Sort]),
				fmt.Sprint(f.byPublicID[b][q.Sort]))
		})
		if q.Desc {
			slices.Reverse(matched)
		}
	}

	page := core.DocumentPage{Documents: []core.Document{}, Total: len(matched)}
	for i, publicID := range matched {
		if i < (q.Page-1)*q.Limit || i >= q.Page*q.Limit {
			continue
		}
		page.Documents = append(page.Documents, core.Document{
			PublicID: publicID,
			Body:     encode(f.byPublicID[publicID]),
		})
	}
	return page, nil
}

func matches(document map[string]any, filters []core.Filter) bool {
	for _, filter := range filters {
		value, ok := document[filter.Field]
		if !ok || !slices.Contains(filter.Values, fmt.Sprint(value)) {
			return false
		}
	}
	return true
}

func (f *fakeDocuments) get(id uuid.UUID, publicID string) (core.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	object, ok := f.byPublicID[publicID]
	if !f.known(id) || !ok {
		return core.Document{}, core.ErrNotFound
	}
	return core.Document{PublicID: publicID, Body: encode(object)}, nil
}

func (f *fakeDocuments) create(id uuid.UUID, body []byte) (core.Document, error) {
	object, err := decode(body)
	if err != nil {
		return core.Document{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.known(id) {
		return core.Document{}, core.ErrNotFound
	}

	publicID := strconv.FormatInt(f.next, 10)
	f.next++
	object[f.idField] = json.Number(publicID)

	f.order = append(f.order, publicID)
	f.byPublicID[publicID] = object
	return core.Document{PublicID: publicID, Body: encode(object)}, nil
}

// write is PUT when merge is false and PATCH when it is true.
func (f *fakeDocuments) write(id uuid.UUID, publicID string, body []byte, merge bool) (core.Document, error) {
	object, err := decode(body)
	if err != nil {
		return core.Document{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	existing, ok := f.byPublicID[publicID]
	if !f.known(id) || !ok {
		return core.Document{}, core.ErrNotFound
	}

	if merge {
		merged := make(map[string]any, len(existing)+len(object))
		for k, v := range existing {
			merged[k] = v
		}
		for k, v := range object {
			merged[k] = v
		}
		object = merged
	}
	object[f.idField] = json.Number(publicID)

	f.byPublicID[publicID] = object
	return core.Document{PublicID: publicID, Body: encode(object)}, nil
}

func (f *fakeDocuments) remove(id uuid.UUID, publicID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.byPublicID[publicID]; !f.known(id) || !ok {
		return core.ErrNotFound
	}
	delete(f.byPublicID, publicID)
	f.order = slices.DeleteFunc(f.order, func(s string) bool { return s == publicID })
	return nil
}

// count reports how many documents the fake holds, for the assertions that care
// that a write reached the store rather than only that it was answered.
func (f *fakeDocuments) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byPublicID)
}

// decode applies the one rule the real store applies before storing anything: a
// document is a JSON object.
func decode(body []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, core.ErrNotObject
	}
	return object, nil
}

func encode(object map[string]any) json.RawMessage {
	body, err := json.Marshal(object)
	if err != nil {
		panic("fakeDocuments: " + err.Error())
	}
	return body
}
