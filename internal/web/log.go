package web

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The inspector's URLs. The stream sits on a literal segment below the list and
// the detail view on a parameter beside it, which net/http's router orders the
// same way the mock matcher does: the literal wins, so an exchange can never be
// mistaken for the stream.
const (
	pathLog       = "/projects/{slug}/log"
	pathLogStream = "/projects/{slug}/log/stream"
	pathLogEntry  = "/projects/{slug}/log/{id}"
)

// Live tail settings.
const (
	// streamPoll is how often an open stream asks for what is new.
	//
	// The tail polls the database rather than being fed by the recorder in
	// process. It costs one small indexed query per second per open inspector,
	// and it buys two things worth more than that: a second instance's traffic
	// appears in the tail as readily as this one's, and what is streamed is what
	// was actually stored rather than what was hoped to be.
	streamPoll = time.Second
	// streamKeepAlive is how often a comment goes out on an idle stream, so that
	// a proxy between us and the browser does not decide the connection is dead.
	streamKeepAlive = 20 * time.Second
	// streamLifetime bounds one connection. EventSource reconnects on its own,
	// so a forgotten tab renews rather than holding a connection for days.
	streamLifetime = 30 * time.Minute
	// streamBatch caps how many exchanges one poll delivers. A burst larger than
	// this arrives over the following polls rather than in one write.
	streamBatch = 100
	// streamRetry is what the browser is told to wait before reconnecting.
	streamRetry = 3 * time.Second
)

// exchangeRow is one line of the list, and the unit the live tail streams.
type exchangeRow struct {
	core.Exchange
	// DetailPath is where the whole exchange can be read.
	DetailPath string
}

// logPage is the data behind the list view.
type logPage struct {
	Project   core.Project
	Rows      []exchangeRow
	StreamURL string
	// Newest is the cursor of the first row on the page. The stream starts from
	// it, so an exchange recorded between this page rendering and the stream
	// connecting is delivered rather than skipped.
	Newest string
	// OlderURL is the same page one step back in time, or "" at the end of the
	// log.
	OlderURL string
	// Dropped is how many exchanges this process could not record. It is shown
	// because a gap in a log that does not admit to gaps is worse than no log.
	Dropped int64
	// Recording is false when the process was built without a recorder, in which
	// case the page says so rather than looking like a project with no traffic.
	Recording bool
	// RetentionMonths is how long the log is kept, in months. Shown on the page
	// because "where did last quarter go" is worth answering before it is asked.
	RetentionMonths int
}

// logEntryPage is the data behind the detail view.
type logEntryPage struct {
	Project  core.Project
	Exchange core.Exchange
	BackURL  string
}

// bodyBlock is one recorded body as the page shows it: the text, and whether
// the recording limit cut it short. A struct rather than two template
// arguments, because html/template has no way to pass two.
type bodyBlock struct {
	Text      string
	Truncated bool
}

func (p logEntryPage) RequestBody() bodyBlock {
	return bodyBlock{Text: p.Exchange.RequestBodyText(), Truncated: p.Exchange.RequestBodyTruncated}
}

func (p logEntryPage) ResponseBody() bodyBlock {
	return bodyBlock{Text: p.Exchange.ResponseBodyText(), Truncated: p.Exchange.ResponseBodyTruncated}
}

func (s *Server) handleLogList(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	before, err := core.ParseExchangeCursor(r.URL.Query().Get("before"))
	if err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That is not a page of the log",
			"The link has been mangled. Start again from the newest entries.")
		return
	}

	// One more than the page, so that "is there an older page" is answered by
	// the rows themselves rather than by a second count(*) over a partitioned
	// table.
	exchanges, err := s.store.ExchangesByProject(r.Context(), project.ID, before, core.DefaultExchangeLimit+1)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("list exchanges: %w", err))
		return
	}

	page := logPage{
		Project:         project,
		StreamURL:       logStreamPath(project.Slug),
		Recording:       s.recorder != nil,
		RetentionMonths: s.logRetentionMonths,
	}
	if s.recorder != nil {
		page.Dropped = s.recorder.Dropped()
	}

	if len(exchanges) > core.DefaultExchangeLimit {
		exchanges = exchanges[:core.DefaultExchangeLimit]
		page.OlderURL = logPath(project.Slug) + "?before=" + exchanges[len(exchanges)-1].Cursor().String()
	}
	page.Rows = exchangeRows(project.Slug, exchanges)
	if len(exchanges) > 0 {
		page.Newest = exchanges[0].Cursor().String()
	}

	data := s.newPage(r, project.Name+" · request log")
	data.Form = page
	s.render(w, r, http.StatusOK, "log_list", data)
}

func (s *Server) handleLogEntry(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderMessage(w, r, http.StatusNotFound, "No such exchange",
			"The address does not name an entry in this log.")
		return
	}

	exchange, err := s.store.ExchangeByID(r.Context(), project.ID, id)
	switch {
	case errors.Is(err, core.ErrNotFound):
		// Either it never existed or its month has been dropped by retention.
		// Both are the same answer, and the second is the more likely one.
		s.renderMessage(w, r, http.StatusNotFound, "No such exchange",
			"It may have aged out of the log, or the address may be wrong.")
		return
	case err != nil:
		s.serverError(w, r, fmt.Errorf("find exchange %s: %w", id, err))
		return
	}

	data := s.newPage(r, exchange.Method+" "+exchange.Path)
	data.Form = logEntryPage{
		Project:  project,
		Exchange: exchange,
		BackURL:  logPath(project.Slug),
	}
	s.render(w, r, http.StatusOK, "log_entry", data)
}

// handleLogStream is the live tail: an SSE connection that sends a rendered row
// for every exchange recorded after the cursor it started from.
//
// Server-sent events rather than WebSockets because the traffic is one-way and
// a browser reconnects on its own (DESIGN.md §9.2). The rows are rendered here
// rather than in the browser so that the tail and the page it appends to cannot
// drift apart: they are the same template.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	cursor, err := core.ParseExchangeCursor(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, "that is not a position in the log", http.StatusBadRequest)
		return
	}
	if cursor.IsZero() {
		// No starting point given: begin at the end of what is already stored,
		// so a fresh stream sends what happens next rather than the whole log.
		if cursor, err = s.store.LatestExchangeCursor(r.Context(), project.ID); err != nil {
			s.serverError(w, r, fmt.Errorf("start the log stream: %w", err))
			return
		}
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	// For a proxy that buffers responses by default. An inspector that shows
	// everything at once, a minute late, is not a live tail.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	control := http.NewResponseController(w)

	// The server's WriteTimeout is meant for responses that end. This one does
	// not, so its deadline is lifted for this response alone rather than for
	// every route the server has.
	if err := control.SetWriteDeadline(time.Time{}); err != nil {
		Logger(r.Context()).Debug("lift the write deadline for the log stream",
			slog.String("error", err.Error()))
	}

	var out bytes.Buffer
	fmt.Fprintf(&out, "retry: %d\n\n", streamRetry.Milliseconds())
	if !s.writeStream(w, control, out.Bytes()) {
		return
	}

	poll := time.NewTicker(streamPoll)
	defer poll.Stop()
	keepAlive := time.NewTicker(streamKeepAlive)
	defer keepAlive.Stop()
	deadline := time.NewTimer(streamLifetime)
	defer deadline.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			// The browser reconnects and picks up from its own last cursor, so
			// nothing is missed by ending here.
			return

		case <-keepAlive.C:
			if !s.writeStream(w, control, []byte(": keep-alive\n\n")) {
				return
			}

		case <-poll.C:
			exchanges, err := s.store.ExchangesSince(r.Context(), project.ID, cursor, streamBatch)
			if err != nil {
				if r.Context().Err() != nil {
					return // the client went away mid-query
				}
				Logger(r.Context()).Error("poll the request log",
					slog.String("error", err.Error()))
				return
			}
			if len(exchanges) == 0 {
				continue
			}

			out.Reset()
			for _, exchange := range exchanges {
				row, err := s.renderExchangeRow(project.Slug, exchange)
				if err != nil {
					Logger(r.Context()).Error("render a log row",
						slog.String("error", err.Error()))
					return
				}
				writeSSEEvent(&out, "exchange", row)
			}
			cursor = exchanges[len(exchanges)-1].Cursor()

			if !s.writeStream(w, control, out.Bytes()) {
				return
			}
		}
	}
}

// writeStream sends one chunk and flushes it, reporting whether the stream is
// still worth continuing. A write that fails means the browser has gone, which
// is an ordinary end to a stream rather than an error to report.
func (s *Server) writeStream(w http.ResponseWriter, control *http.ResponseController, chunk []byte) bool {
	if _, err := w.Write(chunk); err != nil {
		return false
	}
	return control.Flush() == nil
}

// renderExchangeRow renders one row of the list, for the stream to send. It
// executes the same template the page does, so a row that arrives live and one
// that arrives on a refresh are the same markup.
func (s *Server) renderExchangeRow(slug string, exchange core.Exchange) ([]byte, error) {
	t, ok := s.templates["log_list"]
	if !ok {
		return nil, errors.New("no template named \"log_list\"")
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "exchange_row", exchangeRow{
		Exchange:   exchange,
		DetailPath: logEntryPath(slug, exchange.ID),
	}); err != nil {
		return nil, fmt.Errorf("execute the exchange row template: %w", err)
	}
	return buf.Bytes(), nil
}

// writeSSEEvent frames one event. Every line of the payload needs its own
// `data:` prefix — the browser rejoins them with newlines — and a blank line
// ends the event.
func writeSSEEvent(out *bytes.Buffer, event string, payload []byte) {
	fmt.Fprintf(out, "event: %s\n", event)
	for line := range bytes.SplitSeq(bytes.TrimRight(payload, "\n"), []byte("\n")) {
		fmt.Fprintf(out, "data: %s\n", bytes.TrimRight(line, "\r"))
	}
	fmt.Fprint(out, "\n")
}

func exchangeRows(slug string, exchanges []core.Exchange) []exchangeRow {
	rows := make([]exchangeRow, len(exchanges))
	for i, exchange := range exchanges {
		rows[i] = exchangeRow{Exchange: exchange, DetailPath: logEntryPath(slug, exchange.ID)}
	}
	return rows
}

// The URL shapes of this section, in one place.
func logPath(slug string) string { return projectPath(slug) + "/log" }

func logStreamPath(slug string) string { return logPath(slug) + "/stream" }

func logEntryPath(slug string, id uuid.UUID) string { return logPath(slug) + "/" + id.String() }
