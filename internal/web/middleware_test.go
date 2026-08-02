package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chain wraps h the way Server.Handler does, and returns the log output so a
// test can assert on what was recorded.
func chain(h http.Handler) (http.Handler, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return withRequestID(logger, logRequests(recoverPanic(h))), &buf
}

// logLines decodes the captured log into one map per line.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestRequestIDGenerated(t *testing.T) {
	var seen string
	h, _ := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no request id in the handler's context")
	}
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Errorf("%s response header = %q, want %q", RequestIDHeader, got, seen)
	}
}

func TestRequestIDIsUniquePerRequest(t *testing.T) {
	var ids []string
	h, _ := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, RequestID(r.Context()))
	}))

	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if ids[0] == ids[1] {
		t.Errorf("both requests got id %q", ids[0])
	}
}

// An incoming id is honoured when it is safe, so a request can be followed
// across a proxy, and replaced when it is not, so nobody can write whatever
// they like into our logs.
func TestRequestIDFromClient(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		kept     bool
	}{
		{"plain", "abc123", true},
		{"punctuation we allow", "ci-run_7.2", true},
		{"at the length limit", strings.Repeat("a", maxRequestIDLen), true},
		{"over the length limit", strings.Repeat("a", maxRequestIDLen+1), false},
		{"space", "two words", false},
		{"newline injection", "id\nlevel=ERROR msg=hacked", false},
		{"quote", `id","level":"ERROR`, false},
		{"non-ascii", "идентификатор", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			h, _ := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = RequestID(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(RequestIDHeader, tt.incoming)
			h.ServeHTTP(httptest.NewRecorder(), req)

			if tt.kept && seen != tt.incoming {
				t.Errorf("request id = %q, want the supplied %q", seen, tt.incoming)
			}
			if !tt.kept {
				if seen == tt.incoming {
					t.Errorf("request id %q was accepted, want it replaced", tt.incoming)
				}
				if seen == "" {
					t.Error("rejected id was not replaced by a generated one")
				}
			}
		})
	}
}

func TestRequestLogged(t *testing.T) {
	h, buf := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/things?secret=1", nil)
	req.RemoteAddr = "192.0.2.44:51000"
	req.Header.Set("User-Agent", "curl/8.0")
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want exactly one per request:\n%s", len(lines), buf)
	}

	line := lines[0]
	want := map[string]any{
		"msg":        "request",
		"level":      "WARN", // 418 is a client error
		"method":     "POST",
		"path":       "/things",
		"status":     float64(http.StatusTeapot),
		"bytes":      float64(5),
		"remote_ip":  "192.0.2.44",
		"user_agent": "curl/8.0",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log field %s = %v, want %v", k, line[k], v)
		}
	}
	if line["request_id"] == "" || line["request_id"] == nil {
		t.Error("log line carries no request_id")
	}
	if _, ok := line["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms = %v, want a number", line["duration_ms"])
	}
}

func TestRequestLogLevelByStatus(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"success", "/things", http.StatusOK, "INFO"},
		{"client error", "/things", http.StatusBadRequest, "WARN"},
		{"server error", "/things", http.StatusInternalServerError, "ERROR"},
		// Probes arrive every few seconds from the container runtime; at info
		// level they would drown everything else.
		{"healthy probe", pathHealthz, http.StatusOK, "DEBUG"},
		{"failing probe", pathReadyz, http.StatusServiceUnavailable, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, buf := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.path, nil))

			if got := logLines(t, buf)[0]["level"]; got != tt.want {
				t.Errorf("level = %v, want %v", got, tt.want)
			}
		})
	}
}

// A handler with a bug returns 500 and a stack trace in the log, rather than
// killing the connection and telling nobody why.
func TestPanicBecomes500(t *testing.T) {
	h, buf := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if body.Error == "" {
		t.Error("500 response carries no error field")
	}
	// The stack belongs in the log, never in the response.
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("response leaks panic detail: %s", rec.Body.String())
	}

	lines := logLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want the panic and the request:\n%s", len(lines), buf)
	}
	if lines[0]["panic"] != "boom" {
		t.Errorf("panic value = %v, want boom", lines[0]["panic"])
	}
	if stack, _ := lines[0]["stack"].(string); !strings.Contains(stack, "middleware_test.go") {
		t.Errorf("stack does not point at the panicking code: %v", lines[0]["stack"])
	}
	// The access log still reports what the client actually got.
	if lines[1]["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("access log status = %v, want 500", lines[1]["status"])
	}
	if lines[0]["request_id"] != lines[1]["request_id"] {
		t.Error("the panic and the request were logged under different ids")
	}
}

// Once bytes are on the wire the status is settled; the recoverer must not try
// to write a second header.
func TestPanicAfterWriteKeepsStatus(t *testing.T) {
	h, buf := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the 200 already sent", rec.Code)
	}
	if rec.Body.String() != "partial" {
		t.Errorf("body = %q, want the bytes already written", rec.Body.String())
	}
	if lines := logLines(t, buf); lines[0]["panic"] != "boom" {
		t.Errorf("panic was not logged: %v", lines[0])
	}
}

// http.ErrAbortHandler is how a handler says "the client is gone, unwind
// quietly". net/http handles it; we must not swallow it.
func TestAbortHandlerPropagates(t *testing.T) {
	h, _ := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		err, ok := recover().(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", err)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Error("ErrAbortHandler was swallowed")
}

// The live log tail in M4 streams, so flushing has to reach the real writer
// through our wrapper.
func TestResponseControllerReachesThroughRecorder(t *testing.T) {
	h, _ := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*recorder); !ok {
			t.Errorf("handler received %T, want the recording writer", w)
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through the wrapper: %v", err)
		}
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !rec.Flushed {
		t.Error("flush did not reach the underlying writer")
	}
}

// A handler that writes without setting a status has sent 200, and the log has
// to say so.
func TestImplicitStatusIsRecorded(t *testing.T) {
	h, buf := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	line := logLines(t, buf)[0]
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", line["status"])
	}
	if line["bytes"] != float64(4) {
		t.Errorf("bytes = %v, want 4", line["bytes"])
	}
}
