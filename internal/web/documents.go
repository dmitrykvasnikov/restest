package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/mock"
)

// headerTotalCount reports how many documents the filters matched, as opposed
// to how many are in the page being returned. A client paging through has no
// other way to know when to stop.
const headerTotalCount = "X-Total-Count"

// serveCollection performs the operation the matched route names.
//
// Which operation that is was decided by the matcher, not here: POST /users and
// PUT /users/{id} arrived as two different routes with two different Ops, so
// this is a dispatch and not a second reading of the method.
func (s *Server) serveCollection(w http.ResponseWriter, r *http.Request, result mock.Result) {
	route := result.Route

	if !s.mockDelay(w, r, route.DelayMS) {
		// The client gave up while we were waiting. Nothing has been written and
		// nothing has been stored, so there is nothing to finish.
		return
	}

	// The endpoint's own headers are set on collection responses too, which is
	// what lets somebody add the CORS header their browser client needs without
	// waiting for the setting that arrives in M7.
	for name, value := range route.Headers {
		w.Header().Set(name, value)
	}

	id := result.Params[mock.DocumentParam]

	switch route.Op {
	case mock.OpList:
		s.listDocuments(w, r, route)
	case mock.OpCreate:
		s.createDocument(w, r, route)
	case mock.OpGet:
		s.getDocument(w, r, route, id)
	case mock.OpReplace:
		s.writeDocument(w, r, route, id, s.store.ReplaceDocument)
	case mock.OpPatch:
		s.writeDocument(w, r, route, id, s.store.PatchDocument)
	case mock.OpDelete:
		s.deleteDocument(w, r, route, id)
	default:
		// Unreachable: OpRespond is handled before this function is reached, and
		// there is no other op. A new one that forgets to be wired in should say
		// so rather than answer 200 with nothing.
		s.mockFailed(w, r, fmt.Errorf("route %s %s has no handler for op %d", route.Method, route.Path, route.Op))
	}
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request, route mock.Route) {
	query, err := core.ParseListQuery(r.URL.Query())
	if err != nil {
		s.rejectMockRequest(w, r, err)
		return
	}

	page, err := s.store.ListDocuments(r.Context(), route.CollectionID, query)
	if err != nil {
		s.mockFailed(w, r, fmt.Errorf("list documents: %w", err))
		return
	}

	bodies := make([]json.RawMessage, len(page.Documents))
	for i, doc := range page.Documents {
		bodies[i] = doc.Body
	}

	w.Header().Set(headerTotalCount, strconv.Itoa(page.Total))
	s.writeMockJSON(w, r, http.StatusOK, bodies)
}

func (s *Server) getDocument(w http.ResponseWriter, r *http.Request, route mock.Route, id string) {
	doc, err := s.store.GetDocument(r.Context(), route.CollectionID, id)
	if err != nil {
		s.rejectDocument(w, r, id, err)
		return
	}
	s.writeMockJSON(w, r, http.StatusOK, doc.Body)
}

func (s *Server) createDocument(w http.ResponseWriter, r *http.Request, route mock.Route) {
	body, ok := s.readMockBody(w, r)
	if !ok {
		return
	}

	doc, err := s.store.CreateDocument(r.Context(), route.CollectionID, body)
	if err != nil {
		s.rejectDocument(w, r, "", err)
		return
	}

	// Location names where the new document can be fetched, which is the answer
	// to the question a 201 leaves open.
	w.Header().Set("Location", documentLocation(r, doc.PublicID))
	s.writeMockJSON(w, r, http.StatusCreated, doc.Body)
}

// documentWriter is the shape PUT and PATCH share: same arguments, same
// answers, different statement underneath.
type documentWriter func(ctx context.Context, collectionID uuid.UUID, publicID string, body []byte) (core.Document, error)

func (s *Server) writeDocument(w http.ResponseWriter, r *http.Request, route mock.Route, id string, write documentWriter) {
	body, ok := s.readMockBody(w, r)
	if !ok {
		return
	}

	doc, err := write(r.Context(), route.CollectionID, id, body)
	if err != nil {
		s.rejectDocument(w, r, id, err)
		return
	}
	s.writeMockJSON(w, r, http.StatusOK, doc.Body)
}

func (s *Server) deleteDocument(w http.ResponseWriter, r *http.Request, route mock.Route, id string) {
	if err := s.store.DeleteDocument(r.Context(), route.CollectionID, id); err != nil {
		s.rejectDocument(w, r, id, err)
		return
	}
	// 204 and no body. A deleted document has nothing to say about itself, and
	// returning the corpse is a habit from APIs that could not decide.
	w.WriteHeader(http.StatusNoContent)
}

// readMockBody reads a write request's body.
//
// The cap is applied above, once, by withBodyLimit — so this reads to the end
// of a body that has already been bounded, and reports the refusal in the mock
// server's own error shape when the cap was the reason the read stopped.
func (s *Server) readMockBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err == nil {
		return body, true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		s.writeMockError(w, r, http.StatusRequestEntityTooLarge,
			"the request body is larger than the %d byte limit", tooLarge.Limit)
		return nil, false
	}
	// A body that stopped arriving. There is no response worth composing for a
	// client that has already gone.
	Logger(r.Context()).Debug("read mock request body", slog.String("error", err.Error()))
	return nil, false
}

// rejectDocument turns what the store can report into the answer it deserves.
// Everything not named here is a fault in the server rather than in the
// request.
func (s *Server) rejectDocument(w http.ResponseWriter, r *http.Request, id string, err error) {
	switch {
	case errors.Is(err, core.ErrNotObject):
		s.writeMockError(w, r, http.StatusBadRequest,
			"the request body has to be a JSON object")
	case errors.Is(err, core.ErrNotFound) && id != "":
		s.writeMockError(w, r, http.StatusNotFound, "no document with the id %q", id)
	case errors.Is(err, core.ErrNotFound):
		// The collection went away between the route table being built and this
		// request arriving. The next rebuild takes the route with it.
		s.writeMockError(w, r, http.StatusNotFound,
			"the collection this endpoint serves no longer exists")
	default:
		s.mockFailed(w, r, err)
	}
}

// rejectMockRequest answers a listing query the server will not guess at.
func (s *Server) rejectMockRequest(w http.ResponseWriter, r *http.Request, err error) {
	var qe core.QueryError
	if errors.As(err, &qe) {
		s.writeMockError(w, r, http.StatusBadRequest, "%s", qe.Message)
		return
	}
	s.mockFailed(w, r, err)
}

// mockFailed is the end of the line for anything unexpected on the mock side.
// It answers JSON rather than the HTML page the UI would get, because the
// caller is a test client that has been parsing JSON all along.
func (s *Server) mockFailed(w http.ResponseWriter, r *http.Request, err error) {
	Logger(r.Context()).Error("mock request failed", slog.String("error", err.Error()))
	s.writeMockError(w, r, http.StatusInternalServerError,
		"the request could not be completed; the failure has been logged")
}

func (s *Server) writeMockError(w http.ResponseWriter, r *http.Request, status int, format string, args ...any) {
	s.writeMockJSON(w, r, status, mockError{
		Error:  fmt.Sprintf(format, args...),
		Method: r.Method,
		Path:   mockSubPath(r),
	})
}

// writeMockJSON writes a mock response, leaving a Content-Type the endpoint set
// for itself in place. Everything a collection sends is JSON, so the default is
// not in doubt — but an endpoint that was given a Content-Type meant it.
func (s *Server) writeMockJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		Logger(r.Context()).Error("write mock response body", slog.String("error", err.Error()))
	}
}

// documentLocation is where a newly created document can be fetched: the URL
// that was posted to, with the new identifier below it.
//
// Built from the escaped path so that a request to /m/shop/users/ and one to
// /m/shop/users produce the same answer, and so that an identifier needing
// encoding is encoded.
func documentLocation(r *http.Request, publicID string) string {
	return strings.TrimSuffix(r.URL.EscapedPath(), "/") + "/" + url.PathEscape(publicID)
}
