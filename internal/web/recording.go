package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/mock"
)

// Recorder is where the middleware below hands its exchanges. An interface, and
// an optional one: a Server built without it records nothing and behaves
// exactly as it did before M4, which is what lets the handler tests stay about
// handlers.
type Recorder interface {
	// Record must not block. What it does with an exchange it has no room for
	// is its own business, but it may not make the request wait to find out.
	Record(ex core.Exchange)
	// Dropped reports how many exchanges have been lost since the process
	// started, so that the inspector can say a gap is a gap.
	Dropped() int64
}

// recordExchanges wraps the mock handler and writes down what passed through
// it: the request as it arrived, the response as it left, and how long the
// round trip took.
//
// It wraps *the mock handler* rather than the whole application. The UI's own
// traffic is somebody administering their mocks, not a client under test, and
// recording it would fill the inspector with the inspector.
//
// Nothing here writes to the database. The exchange goes to a queue and this
// goroutine returns to the client; the cost of logging a request must not be
// paid by the request (DESIGN.md §7).
func (s *Server) recordExchanges(next http.Handler) http.Handler {
	if s.recorder == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()

		note := &exchangeNote{}
		r = r.WithContext(context.WithValue(r.Context(), exchangeNoteKey{}, note))

		body := captureBody(r, s.logBodyLimit)
		r.Body = body

		capture := &captureWriter{ResponseWriter: w, limit: s.logBodyLimit}
		next.ServeHTTP(capture, r)

		// A request addressed to a slug no project has is not recorded: there is
		// no project whose log it would belong to, and inventing one would mean
		// answering "who typed this" with somebody arbitrary. The 404 the client
		// got already said as much, and the access log has the line.
		if note.projectID == uuid.Nil {
			return
		}

		s.recorder.Record(core.Exchange{
			ID:         uuid.New(),
			ProjectID:  note.projectID,
			EndpointID: note.endpointID,
			Direction:  core.DirectionInbound,
			Matched:    note.matched,

			Method: r.Method,
			Path:   mockSubPath(r),
			Query:  r.URL.RawQuery,

			RequestHeaders:       headerSet(r.Header),
			RequestBody:          body.recorded(),
			RequestBodyTruncated: body.truncated,

			StatusCode:            capture.status,
			ResponseHeaders:       headerSet(capture.Header()),
			ResponseBody:          capture.recorded(),
			ResponseBodyTruncated: capture.truncated,

			Duration:   time.Since(started),
			RemoteAddr: remoteIP(r),
			// The instant the exchange finished, taken here rather than left to
			// the database: it is the partition key and the tail's cursor, and
			// both should say when the request happened rather than when the
			// batch carrying it happened to be flushed.
			CreatedAt: time.Now(),
		})
	})
}

// exchangeNote is what the mock handler tells the middleware about a request
// that only the matcher can know: which project it was addressed to, whether
// anything answered it, and what.
//
// It travels in the context because the middleware runs on both sides of the
// handler and the handler returns nothing. Only the handler goroutine touches
// it, before the middleware reads it, so it needs no lock.
type exchangeNote struct {
	projectID  uuid.UUID
	endpointID uuid.UUID
	matched    bool
}

type exchangeNoteKey struct{}

// noteExchange records the matcher's decision for the middleware. It is a no-op
// when recording is off, so the mock handler calls it unconditionally.
func noteExchange(ctx context.Context, result mock.Result) {
	note, ok := ctx.Value(exchangeNoteKey{}).(*exchangeNote)
	if !ok {
		return
	}

	note.projectID = result.Project.ID
	note.matched = result.Outcome == mock.Matched
	if note.matched {
		note.endpointID = result.Route.ID
	}
}

// capturedBody is a request body that keeps the first bytes of itself.
//
// The bytes are read here rather than teed as the handler reads them, because
// what the inspector is for is seeing what a client actually sent — and a
// static endpoint never reads the body at all. A tee would record nothing for
// exactly the request somebody is most likely to be puzzled by.
//
// Only the cap is read eagerly. Anything beyond it stays on the connection for
// the handler to read or for net/http to discard, so a large upload does not
// become a large allocation here.
type capturedBody struct {
	reader    io.Reader
	closer    io.Closer
	kept      []byte
	truncated bool
}

func (b *capturedBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *capturedBody) Close() error { return b.closer.Close() }

func (b *capturedBody) recorded() []byte { return b.kept }

// captureBody reads up to limit bytes of the request body and puts them back in
// front of it, so the handler sees a body indistinguishable from the original.
func captureBody(r *http.Request, limit int) *capturedBody {
	if r.Body == nil {
		return &capturedBody{reader: http.NoBody, closer: http.NoBody}
	}

	// One byte past the limit, so that a body of exactly the limit is not
	// reported as truncated.
	kept, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		// The body stopped arriving. What was read is still what the client
		// sent, and the handler will meet the same error on its own read.
		return &capturedBody{reader: bytes.NewReader(kept), closer: r.Body, kept: kept}
	}

	truncated := len(kept) > limit
	recorded := kept
	if truncated {
		recorded = kept[:limit]
	}

	return &capturedBody{
		reader:    io.MultiReader(bytes.NewReader(kept), r.Body),
		closer:    r.Body,
		kept:      recorded,
		truncated: truncated,
	}
}

// captureWriter is the response half: it passes everything through and keeps
// the status, the headers and the first bytes of the body.
type captureWriter struct {
	http.ResponseWriter
	limit     int
	status    int
	body      bytes.Buffer
	truncated bool
}

func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if room := w.limit - w.body.Len(); room > 0 {
		if len(b) <= room {
			w.body.Write(b)
		} else {
			w.body.Write(b[:room])
			w.truncated = true
		}
	} else if len(b) > 0 {
		w.truncated = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) recorded() []byte { return w.body.Bytes() }

// Unwrap keeps http.ResponseController working through this wrapper, which the
// mock handler needs: a delayed response extends its own write deadline.
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// headerSet copies headers into the form the log stores, with repeats kept.
// The values are copied rather than aliased because net/http reuses the header
// map's slices, and this one outlives the request.
func headerSet(h http.Header) core.HeaderSet {
	if len(h) == 0 {
		return core.HeaderSet{}
	}

	out := make(core.HeaderSet, len(h))
	for name, values := range h {
		out[name] = append([]string(nil), values...)
	}
	return out
}
