package core

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/users", "/users"},
		{"users", "/users"},            // a missing leading slash is a typo
		{"/users/", "/users"},          // …and so is a trailing one
		{"//users//me//", "/users/me"}, // repeated slashes collapse
		{"/", "/"},
		{"", "/"},
		{"   /users   ", "/users"},
		{"/users/{id}/posts", "/users/{id}/posts"},
	}

	for _, tc := range tests {
		if got := NormalizePath(tc.in); got != tc.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizePathIsIdempotent matters because the normalised form is what the
// unique index compares. If normalising twice moved, the same pattern could be
// stored under two spellings and the index would let both in.
func TestNormalizePathIsIdempotent(t *testing.T) {
	for _, in := range []string{"", "/", "//", "/a/", "//a//b//", "/{id}/"} {
		once := NormalizePath(in)
		if twice := NormalizePath(once); twice != once {
			t.Errorf("NormalizePath(%q) = %q, then %q", in, once, twice)
		}
	}
}

func TestPathParams(t *testing.T) {
	tests := []struct {
		pattern string
		want    []string
	}{
		{"/users", nil},
		{"/users/{id}", []string{"id"}},
		{"/users/{id}/posts/{postID}", []string{"id", "postID"}},
		{"/{a}/{b}/{c}", []string{"a", "b", "c"}},
		// Not a parameter: it is not a whole segment. validatePath refuses it,
		// so nothing stored looks like this — but the reader has to agree.
		{"/v{n}/users", nil},
	}

	for _, tc := range tests {
		if got := PathParams(tc.pattern); !slices.Equal(got, tc.want) {
			t.Errorf("PathParams(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestValidateEndpointInput(t *testing.T) {
	valid := func() EndpointInput {
		return EndpointInput{
			Method:     "GET",
			Path:       "/users/{id}",
			StatusCode: 200,
			DelayMS:    0,
			Headers:    Headers{"Content-Type": "application/json"},
			Body:       `{"id":1}`,
		}
	}

	tests := []struct {
		name string
		in   func(*EndpointInput)
		// field is the form field the message must land on, or "" if the input
		// is expected to be accepted.
		field string
	}{
		{name: "as it stands", in: func(*EndpointInput) {}},
		{name: "the wildcard verb", in: func(e *EndpointInput) { e.Method = "*" }},
		{name: "lower case is normalised", in: func(e *EndpointInput) { e.Method = "get" }},
		{name: "a path with no parameters", in: func(e *EndpointInput) { e.Path = "/health" }},
		{name: "the root", in: func(e *EndpointInput) { e.Path = "/" }},
		{name: "an empty body", in: func(e *EndpointInput) { e.Body = "" }},
		{name: "no headers at all", in: func(e *EndpointInput) { e.Headers = nil }},
		{name: "the longest delay", in: func(e *EndpointInput) { e.DelayMS = 60000 }},

		{name: "an unknown verb", in: func(e *EndpointInput) { e.Method = "FETCH" }, field: "method"},
		{name: "no verb", in: func(e *EndpointInput) { e.Method = "" }, field: "method"},

		{name: "a partial parameter", in: func(e *EndpointInput) { e.Path = "/v{n}" }, field: "path"},
		{name: "an unclosed brace", in: func(e *EndpointInput) { e.Path = "/users/{id" }, field: "path"},
		{name: "a nameless parameter", in: func(e *EndpointInput) { e.Path = "/users/{}" }, field: "path"},
		{name: "a repeated parameter", in: func(e *EndpointInput) { e.Path = "/{id}/x/{id}" }, field: "path"},
		{name: "a query string", in: func(e *EndpointInput) { e.Path = "/users?page=1" }, field: "path"},
		{name: "a fragment", in: func(e *EndpointInput) { e.Path = "/users#top" }, field: "path"},
		{name: "a control character", in: func(e *EndpointInput) { e.Path = "/users\n/x" }, field: "path"},
		{
			name:  "too many segments",
			in:    func(e *EndpointInput) { e.Path = strings.Repeat("/a", maxPathSegments+1) },
			field: "path",
		},

		{name: "a status below 100", in: func(e *EndpointInput) { e.StatusCode = 99 }, field: "status_code"},
		{name: "a status above 599", in: func(e *EndpointInput) { e.StatusCode = 600 }, field: "status_code"},
		{name: "no status at all", in: func(e *EndpointInput) { e.StatusCode = 0 }, field: "status_code"},

		{name: "a negative delay", in: func(e *EndpointInput) { e.DelayMS = -1 }, field: "delay_ms"},
		{name: "a delay past the ceiling", in: func(e *EndpointInput) { e.DelayMS = 60001 }, field: "delay_ms"},

		{
			name:  "a body over the cap",
			in:    func(e *EndpointInput) { e.Body = strings.Repeat("x", maxBodyLen+1) },
			field: "body",
		},

		{
			name:  "a framing header",
			in:    func(e *EndpointInput) { e.Headers = Headers{"Content-Length": "12"} },
			field: "headers",
		},
		{
			name:  "another framing header",
			in:    func(e *EndpointInput) { e.Headers = Headers{"Transfer-Encoding": "chunked"} },
			field: "headers",
		},
		{
			name:  "a header name that is not a token",
			in:    func(e *EndpointInput) { e.Headers = Headers{"X Mock": "yes"} },
			field: "headers",
		},
		{
			name:  "a header value carrying a newline",
			in:    func(e *EndpointInput) { e.Headers = Headers{"X-Mock": "yes\r\nX-Evil: 1"} },
			field: "headers",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := valid()
			tc.in(&in)

			err := in.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}

			fe, ok := err.(FieldErrors) //nolint:errorlint // the concrete type is the assertion
			if !ok {
				t.Fatalf("err = %v (%T), want FieldErrors", err, err)
			}
			if _, ok := fe[tc.field]; !ok {
				t.Errorf("errors = %v, want a message on %q", fe, tc.field)
			}
		})
	}
}

// TestValidateNormalisesBeforeJudging: a path typed without a leading slash is
// a typo, not a rejection, and the same goes for a lower-case verb.
func TestValidateNormalisesBeforeJudging(t *testing.T) {
	in := EndpointInput{Method: "  post ", Path: "users/ ", StatusCode: 201}

	normalised, err := in.normalize()
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if normalised.Method != "POST" {
		t.Errorf("method = %q, want POST", normalised.Method)
	}
	if normalised.Path != "/users" {
		t.Errorf("path = %q, want /users", normalised.Path)
	}
}

func TestParseHeaderLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Headers
	}{
		{name: "empty", in: "", want: Headers{}},
		{name: "blank lines only", in: "\n\n  \n", want: Headers{}},
		{
			name: "one per line",
			in:   "Content-Type: application/json\nX-Mock: yes",
			want: Headers{"Content-Type": "application/json", "X-Mock": "yes"},
		},
		{
			name: "names are canonicalised",
			in:   "content-type: text/plain\nx-MOCK: yes",
			want: Headers{"Content-Type": "text/plain", "X-Mock": "yes"},
		},
		{
			name: "surrounding space is trimmed",
			in:   "  X-Mock :   yes  \n",
			want: Headers{"X-Mock": "yes"},
		},
		{
			name: "CRLF, which is what a browser submits",
			in:   "X-A: 1\r\nX-B: 2\r\n",
			want: Headers{"X-A": "1", "X-B": "2"},
		},
		{
			name: "a value containing a colon",
			in:   "Link: <https://example.test/x>; rel=next",
			want: Headers{"Link": "<https://example.test/x>; rel=next"},
		},
		{
			// Kept rather than dropped, so that validateHeaders can name it and
			// a half-typed line is not silently discarded.
			name: "a line with no colon",
			in:   "nonsense",
			want: Headers{"nonsense": ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseHeaderLines(tc.in); !maps.Equal(got, tc.want) {
				t.Errorf("ParseHeaderLines(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestHeaderLinesRoundTrip: what the form shows has to parse back to what was
// stored, or editing an endpoint without touching its headers would change them.
func TestHeaderLinesRoundTrip(t *testing.T) {
	original := Headers{
		"Content-Type":  "application/json",
		"X-Mock":        "yes",
		"Cache-Control": "no-store",
		"Link":          "<https://example.test/x>; rel=next",
	}

	back := ParseHeaderLines(original.Lines())
	if !maps.Equal(back, original) {
		t.Errorf("round trip = %v, want %v", back, original)
	}
}

func TestHeaderLinesAreSorted(t *testing.T) {
	h := Headers{"X-Z": "1", "X-A": "2", "X-M": "3"}

	const want = "X-A: 2\nX-M: 3\nX-Z: 1"
	if got := h.Lines(); got != want {
		t.Errorf("Lines() = %q, want %q", got, want)
	}
}
