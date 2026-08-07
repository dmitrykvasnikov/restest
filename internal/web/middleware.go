package web

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// RequestIDHeader carries the request id in and out. An incoming value is
// honoured so that a request can be followed across a proxy or a CI job,
// subject to the sanity check in sanitizeRequestID.
const RequestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds an id we did not generate. Client input ends up in
// every log line for the request, so it is kept short and boring.
const maxRequestIDLen = 64

type contextKey int

const (
	requestIDKey contextKey = iota
	loggerKey
)

// RequestID returns the id of the request being served, or "" outside one.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Logger returns the request-scoped logger, which already carries the request
// id. Outside a request it falls back to the default logger, so a caller can
// use it without checking.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// withRequestID puts an id on every request and echoes it back in the response,
// so that a user reporting a problem can quote a string that finds the exact
// log lines.
func withRequestID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)

		ctx := context.WithValue(r.Context(), requestIDKey, id)
		ctx = context.WithValue(ctx, loggerKey, logger.With(slog.String("request_id", id)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newRequestID returns a random, URL-safe identifier. rand.Text gives 26
// base32 characters from a cryptographic source; uniqueness is what matters
// here, not unpredictability, but the strong source costs nothing.
func newRequestID() string { return rand.Text() }

// sanitizeRequestID accepts a client-supplied id only if it is short and made
// of unremarkable characters, and returns "" otherwise. Anything else would let
// a caller write arbitrary text into our logs.
func sanitizeRequestID(id string) string {
	if id == "" || len(id) > maxRequestIDLen {
		return ""
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return id
}

// logRequests writes exactly one line per request, after it has been served.
//
// This is the operational access log. It is not the request inspector: that is
// a per-project record of mock traffic with bodies attached, and it arrives in
// M4 as its own middleware around the mock handler.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		elapsed := time.Since(started)
		Logger(r.Context()).Log(r.Context(), levelFor(r, rec.status), "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.written),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
			slog.String("remote_ip", remoteIP(r)),
			slog.String("user_agent", r.UserAgent()),
		)
	})
}

// levelFor keeps the log readable: server errors stand out, client errors are
// worth noticing, and successful health probes — which arrive every few
// seconds from the container runtime — stay out of the way unless debugging.
func levelFor(r *http.Request, status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	// A rate-limited request is the guard working, not a fault, and it arrives
	// in exactly the volume the guard was added to absorb. A warning per
	// refusal would turn a flood of requests into a flood of log lines, which
	// is the second half of the same denial of service. The count is in
	// restest_rate_limited_total, where a number belongs.
	case status == http.StatusTooManyRequests:
		return slog.LevelDebug
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	case r.URL.Path == pathHealthz || r.URL.Path == pathReadyz:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

// remoteIP is the address this request is attributed to: the peer's, unless it
// arrived through a proxy an operator named in RESTEST_TRUSTED_PROXIES. The
// walk happens once, above this, in withClientIP — see clientip.go for why the
// header is not read by default.
//
// The fallback to the peer address is for the handler tests, which exercise
// middleware below withClientIP without the whole chain above it.
func remoteIP(r *http.Request) string {
	if ip := ClientIP(r.Context()); ip != "" {
		return ip
	}
	return peerIP(r)
}

// recoverPanic turns a panicking handler into a 500 rather than a dropped
// connection, and records the stack. It sits inside logRequests so that the
// access log still reports the request, with its real status.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// The client hung up mid-response; net/http uses this to unwind
			// quietly and so do we.
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}

			Logger(r.Context()).Error("panic serving request",
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())),
			)

			// Once the status line is out there is nothing left to say.
			if rec, ok := w.(*recorder); ok && rec.wroteHeader {
				return
			}
			writeJSON(w, r, http.StatusInternalServerError, errorBody{Error: "internal server error"})
		}()

		next.ServeHTTP(w, r)
	})
}

// recorder remembers what the handler below sent, for the access log.
type recorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController, which is
// how flushing and deadline control reach through a wrapper. The live log
// tail in M4 needs it.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
