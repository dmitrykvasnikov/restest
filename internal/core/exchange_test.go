package core

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The one place the inspector deliberately does not show what was sent. The
// scheme survives so that "is my client sending a bearer token" is still
// answerable; the credential does not.
func TestHeadersAreRedactedOnTheWayIn(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{"bearer token keeps its scheme", "Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.x", "Bearer [redacted]"},
		{"basic credentials too", "Authorization", "Basic dXNlcjpwYXNz", "Basic [redacted]"},
		{"lower case is the same header", "authorization", "Bearer abc", "Bearer [redacted]"},
		{"a scheme-less value goes entirely", "Authorization", "abc123", "[redacted]"},
		{"a cookie has no scheme to keep", "Cookie", "session=abc; theme=dark", "[redacted]"},
		{"and neither has a set-cookie", "Set-Cookie", "session=abc; HttpOnly", "[redacted]"},
		{"an api key header", "X-Api-Key", "k-12345", "[redacted]"},
		{"a proxy credential", "Proxy-Authorization", "Bearer abc", "Bearer [redacted]"},
		{"an ordinary header is untouched", "Content-Type", "application/json", "application/json"},
		{"and so is one that merely sounds alarming", "X-Auth-Style", "cookie", "cookie"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HeaderSet{tt.header: {tt.value}}.Redacted()
			if got.Values(tt.header) != tt.want {
				t.Errorf("Redacted()[%q] = %q, want %q", tt.header, got.Values(tt.header), tt.want)
			}
		})
	}
}

// Redaction copies rather than editing in place: the value being recorded must
// not change the request's own headers, which the handler may still be using.
func TestRedactionDoesNotAlterTheOriginal(t *testing.T) {
	original := HeaderSet{"Authorization": {"Bearer sekrit"}}
	_ = original.Redacted()

	if got := original.Values("Authorization"); got != "Bearer sekrit" {
		t.Errorf("the original header is now %q", got)
	}
}

func TestHeaderSetKeepsRepeats(t *testing.T) {
	h := HeaderSet{"Accept": {"application/json", "text/plain"}}

	if got := h.Values("Accept"); got != "application/json, text/plain" {
		t.Errorf("Values = %q, want both values", got)
	}
	if names := h.Names(); len(names) != 1 || names[0] != "Accept" {
		t.Errorf("Names = %v, want [Accept]", names)
	}
}

// The cursor is what paging and the live tail are built on, so it has to
// survive a trip through a URL exactly — including the microseconds, which is
// the resolution Postgres keeps and therefore the resolution a comparison
// against a stored row needs.
func TestExchangeCursorRoundTrip(t *testing.T) {
	want := ExchangeCursor{
		At: time.Date(2026, 8, 5, 12, 30, 45, 123456000, time.UTC),
		ID: uuid.MustParse("11111111-2222-4333-8444-555555555555"),
	}

	got, err := ParseExchangeCursor(want.String())
	if err != nil {
		t.Fatalf("ParseExchangeCursor: %v", err)
	}
	if !got.At.Equal(want.At) {
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %v, want %v", got.ID, want.ID)
	}
}

func TestExchangeCursorRefusesNonsense(t *testing.T) {
	for _, bad := range []string{
		"yesterday",
		"1754397045123456", // a time with no id
		"abc-11111111-2222-4333-8444-555555555555",     // an id with no time
		"1754397045123456-not-a-uuid",                  // an id that is not one
		"1754397045123456-11111111-2222-4333-8444-555", // and a truncated one
	} {
		if _, err := ParseExchangeCursor(bad); err == nil {
			t.Errorf("ParseExchangeCursor(%q) was accepted", bad)
		}
	}
}

// An empty cursor is the first page, not an error: it is what a fresh link
// carries.
func TestTheEmptyCursorIsNotAnError(t *testing.T) {
	got, err := ParseExchangeCursor("")
	if err != nil {
		t.Fatalf("ParseExchangeCursor(\"\"): %v", err)
	}
	if !got.IsZero() {
		t.Errorf("cursor = %v, want the zero cursor", got)
	}
	if got.String() != "" {
		t.Errorf("String = %q, want empty", got.String())
	}
}

func TestExchangeOutcome(t *testing.T) {
	tests := []struct {
		name    string
		matched bool
		status  int
		want    string
	}{
		{"a served request", true, 200, "ok"},
		{"a redirect is still an answer", true, 302, "ok"},
		{"a refused one", true, 404, "client-error"},
		{"a broken one", true, 500, "server-error"},
		{"nothing matched, whatever the status", false, 404, "unmatched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := Exchange{Matched: tt.matched, StatusCode: tt.status}
			if got := ex.Outcome(); got != tt.want {
				t.Errorf("Outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExchangeTarget(t *testing.T) {
	if got := (Exchange{Path: "/users"}).Target(); got != "/users" {
		t.Errorf("Target = %q, want /users", got)
	}
	if got := (Exchange{Path: "/users", Query: "a=1&b=2"}).Target(); got != "/users?a=1&b=2" {
		t.Errorf("Target = %q, want the query back on the path", got)
	}
}

func TestBodyText(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"nothing at all", nil, ""},
		{"JSON is reformatted", []byte(`{"a":1}`), "{\n  \"a\": 1\n}"},
		{"anything else is shown as it came", []byte("plain text"), "plain text"},
		{"broken JSON is not repaired", []byte(`{"a":`), `{"a":`},
		{"binary is described rather than printed", []byte{0xff, 0xfe, 0x00}, "(3 bytes of binary)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bodyText(tt.body); got != tt.want {
				t.Errorf("bodyText = %q, want %q", got, tt.want)
			}
		})
	}
}

// The ceiling on what one row may cost applies wherever the exchange came from,
// so that no caller can raise it by passing a bigger body.
func TestTheBodyCeilingIsEnforcedOnTheWayIn(t *testing.T) {
	huge := []byte(strings.Repeat("x", MaxExchangeBody+100))

	params, err := exchangeParams(Exchange{
		ProjectID:   uuid.New(),
		Method:      "POST",
		Path:        "/x",
		RequestBody: huge,
	})
	if err != nil {
		t.Fatalf("exchangeParams: %v", err)
	}

	if len(params.RequestBody) != MaxExchangeBody {
		t.Errorf("stored %d bytes, want the %d-byte ceiling", len(params.RequestBody), MaxExchangeBody)
	}
	if !params.RequestBodyTruncated {
		t.Error("a body cut down to the ceiling was not marked as truncated")
	}
}

// A truncation that happened before the store still has to be recorded as one.
func TestTruncationReportedByTheCallerSurvives(t *testing.T) {
	params, err := exchangeParams(Exchange{
		ProjectID:            uuid.New(),
		RequestBody:          []byte("short"),
		RequestBodyTruncated: true,
	})
	if err != nil {
		t.Fatalf("exchangeParams: %v", err)
	}
	if !params.RequestBodyTruncated {
		t.Error("the caller's truncation flag was dropped")
	}
}

func TestExchangeParamsFillsInWhatWasLeftOut(t *testing.T) {
	params, err := exchangeParams(Exchange{ProjectID: uuid.New(), Method: "GET", Path: "/x"})
	if err != nil {
		t.Fatalf("exchangeParams: %v", err)
	}

	if !params.ID.Valid {
		t.Error("no id was allocated")
	}
	if !params.CreatedAt.Valid {
		t.Error("no timestamp was allocated")
	}
	if params.Direction != DirectionInbound {
		t.Errorf("Direction = %q, want inbound by default", params.Direction)
	}
	// A request nothing matched has no endpoint, no query and no status, and
	// null says so where a zero would look like an answer.
	if params.EndpointID.Valid {
		t.Error("EndpointID is set for an exchange that named no endpoint")
	}
	if params.Query.Valid {
		t.Error("Query is set for a request that carried none")
	}
	if params.StatusCode.Valid {
		t.Error("StatusCode is set for a response that was never written")
	}
	if params.RemoteAddr != nil {
		t.Error("RemoteAddr is set for an exchange with no address")
	}
}

func TestRemoteAddrIsStoredOnlyWhenItIsOne(t *testing.T) {
	for _, addr := range []string{"192.0.2.10", "2001:db8::1"} {
		if got := parseRemoteAddr(addr); got == nil || got.String() != addr {
			t.Errorf("parseRemoteAddr(%q) = %v, want the address itself", addr, got)
		}
	}
	for _, addr := range []string{"", "@", "localhost", "192.0.2.10:8080"} {
		if got := parseRemoteAddr(addr); got != nil {
			t.Errorf("parseRemoteAddr(%q) = %v, want nothing", addr, got)
		}
	}
}
