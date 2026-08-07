package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The request log over the API. The inspector shows it in a browser; this is
// the same log for a caller that has no browser — a CI job asking what its
// client actually sent, which is the question the inspector exists to answer
// and the one a failing test in a pipeline raises.
//
// It is read-only. Exchanges are a record of what happened, and a record that
// can be edited is not one.

// apiExchange is one exchange as a listing renders it: everything except the
// headers and the bodies, which is what makes a page of fifty a reasonable
// thing to send.
type apiExchange struct {
	ID      uuid.UUID `json:"id"`
	Matched bool      `json:"matched"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	Query   string    `json:"query,omitempty"`
	// Status is absent when the handler wrote nothing at all — the client hung
	// up mid-request — because 0 is not a status code.
	Status     int    `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	// Cursor is this exchange's position in the log, to be handed back as
	// `before` to page past it.
	Cursor    string    `json:"cursor"`
	CreatedAt time.Time `json:"created_at"`
}

// apiExchangeDetail adds what a single exchange is asked for: the headers as
// they were sent and the bodies as they were recorded.
type apiExchangeDetail struct {
	apiExchange
	RequestHeaders  core.HeaderSet `json:"request_headers"`
	Request         apiBody        `json:"request_body"`
	ResponseHeaders core.HeaderSet `json:"response_headers"`
	Response        apiBody        `json:"response_body"`
}

// apiBody is a recorded body. Text is null rather than mangled when the bytes
// are not valid UTF-8 — a JSON string cannot carry arbitrary bytes, and
// replacement characters would be a body that never existed.
type apiBody struct {
	Text      *string `json:"text"`
	Bytes     int     `json:"bytes"`
	Truncated bool    `json:"truncated"`
	Binary    bool    `json:"binary"`
}

func apiBodyOf(body []byte, truncated bool) apiBody {
	out := apiBody{Bytes: len(body), Truncated: truncated}
	if len(body) == 0 {
		return out
	}
	if !utf8.Valid(body) {
		out.Binary = true
		return out
	}

	// As recorded, not reformatted. The inspector indents JSON because a person
	// is reading it; a caller here is comparing bytes against what its own code
	// sent, and indentation would make that comparison fail.
	text := string(body)
	out.Text = &text
	return out
}

func apiExchangeOf(e core.Exchange) apiExchange {
	return apiExchange{
		ID:         e.ID,
		Matched:    e.Matched,
		Method:     e.Method,
		Path:       e.Path,
		Query:      e.Query,
		Status:     e.StatusCode,
		DurationMS: e.DurationMS(),
		RemoteAddr: e.RemoteAddr,
		Cursor:     e.Cursor().String(),
		CreatedAt:  e.CreatedAt.UTC(),
	}
}

// apiLogPage is a page of the log. `next` is the cursor to pass as `before` to
// get the page after this one, and is absent at the end of the log — so a
// script pages by following it until it stops, rather than by counting.
type apiLogPage struct {
	Count int           `json:"count"`
	Items []apiExchange `json:"items"`
	Next  string        `json:"next,omitempty"`
}

func (s *Server) handleAPILogList(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	query := r.URL.Query()
	before, err := core.ParseExchangeCursor(query.Get("before"))
	if err != nil {
		s.apiError(w, r, http.StatusBadRequest,
			"`before` is a cursor from a previous page, and %q is not one", query.Get("before"))
		return
	}

	limit := core.DefaultExchangeLimit
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > core.MaxExchangeLimit {
			s.apiError(w, r, http.StatusBadRequest,
				"`limit` is a number between 1 and %d", core.MaxExchangeLimit)
			return
		}
	}

	// One more than the page, so that "is there another page" is answered by
	// the rows themselves rather than by a second count over a partitioned
	// table.
	exchanges, err := s.store.ExchangesByProject(r.Context(), project.ID, before, limit+1)
	if err != nil {
		s.apiServerError(w, r, fmt.Errorf("list exchanges: %w", err))
		return
	}

	page := apiLogPage{Items: []apiExchange{}}
	if len(exchanges) > limit {
		exchanges = exchanges[:limit]
		page.Next = exchanges[len(exchanges)-1].Cursor().String()
	}
	for _, exchange := range exchanges {
		page.Items = append(page.Items, apiExchangeOf(exchange))
	}
	page.Count = len(page.Items)

	writeJSON(w, r, http.StatusOK, page)
}

func (s *Server) handleAPILogEntry(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.apiError(w, r, http.StatusNotFound, "%q does not name an exchange", r.PathValue("id"))
		return
	}

	exchange, err := s.store.ExchangeByID(r.Context(), project.ID, id)
	switch {
	case errors.Is(err, core.ErrNotFound):
		// Either it never existed or its month has been dropped by retention.
		// Both are the same answer, and the second is the more likely one.
		s.apiError(w, r, http.StatusNotFound,
			"no exchange %s in the log of %q — it may have aged out of the retention window", id, project.Slug)
		return
	case err != nil:
		s.apiServerError(w, r, fmt.Errorf("find exchange %s: %w", id, err))
		return
	}

	writeJSON(w, r, http.StatusOK, apiExchangeDetail{
		apiExchange:     apiExchangeOf(exchange),
		RequestHeaders:  exchange.RequestHeaders,
		Request:         apiBodyOf(exchange.RequestBody, exchange.RequestBodyTruncated),
		ResponseHeaders: exchange.ResponseHeaders,
		Response:        apiBodyOf(exchange.ResponseBody, exchange.ResponseBodyTruncated),
	})
}
