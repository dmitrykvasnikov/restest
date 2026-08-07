package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// Exchange directions. Everything restest records today is inbound — a request
// somebody's client sent to a mock. The column exists so that the phase 2
// outbound runner records into this table rather than into a parallel one
// (DESIGN.md §3), and nothing here assumes only one value.
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// MaxExchangeBody is the hard ceiling on a recorded body, whatever the
// configured cap says. The configured cap is a policy about how much is worth
// keeping; this is a bound on how much a single row can cost the database, and
// it applies on the way in so that no caller can raise it by accident.
const MaxExchangeBody = 1 << 20 // 1 MiB

// Exchange is one request and the response it produced.
//
// It is deliberately a flat record of what happened rather than a reference to
// the endpoint that answered: an endpoint edited or deleted afterwards must not
// change what the log says was served at the time.
type Exchange struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	// EndpointID is uuid.Nil when nothing matched, which is also what Matched
	// reports. Both are kept because an endpoint deleted later leaves an id
	// naming nothing, and "this was matched" is still true of the request.
	EndpointID uuid.UUID
	Direction  string
	Matched    bool

	Method string
	// Path is the path below /m/{slug}, as the matcher saw it.
	Path string
	// Query is the raw query string without its leading '?'.
	Query string

	RequestHeaders       HeaderSet
	RequestBody          []byte
	RequestBodyTruncated bool

	StatusCode            int
	ResponseHeaders       HeaderSet
	ResponseBody          []byte
	ResponseBodyTruncated bool

	Duration time.Duration
	// RemoteAddr is the peer address, or "" when it could not be parsed as one.
	RemoteAddr string
	CreatedAt  time.Time
}

// HeaderSet is a set of headers as they were actually sent, with repeats kept.
//
// It is map[string][]string rather than the map[string]string an endpoint's own
// response headers use, because this side is a record and not a setting: a
// client that sent two Accept headers sent two, and an inspector that showed
// one would be answering a question nobody asked.
type HeaderSet map[string][]string

// Names lists the header names in order, for a template that renders them.
func (h HeaderSet) Names() []string { return slices.Sorted(maps.Keys(h)) }

// Values renders one header's values as a template can print them. Repeats are
// joined with a comma, which is how HTTP itself would have combined them.
func (h HeaderSet) Values(name string) string { return strings.Join(h[name], ", ") }

// Len reports how many distinct header names there are.
func (h HeaderSet) Len() int { return len(h) }

// redactedMarker replaces the value of a header that carries a credential.
const redactedMarker = "[redacted]"

// sensitiveHeaders are the headers whose values are never written down, keyed
// by lower-case name.
//
// The inspector exists to show what a client actually sent, and this is the one
// place that principle is bent. A developer pointing a half-finished client at
// a mock server sends it whatever token that client happens to be carrying, and
// a log that keeps those for the whole retention window is a credential store
// nobody asked for. The name and the authentication scheme survive, so "is my
// client sending an Authorization header at all, and what kind" — the question
// this is usually inspected for — is still answered.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
	"x-auth-token":        true,
}

// Redacted returns the headers with credential values replaced. It is applied
// on the way into storage rather than on the way out, so that nothing which
// reaches the table can be un-redacted later.
func (h HeaderSet) Redacted() HeaderSet {
	if len(h) == 0 {
		return h
	}

	out := make(HeaderSet, len(h))
	for name, values := range h {
		if !sensitiveHeaders[strings.ToLower(name)] {
			out[name] = values
			continue
		}
		masked := make([]string, len(values))
		for i, value := range values {
			masked[i] = redactValue(value)
		}
		out[name] = masked
	}
	return out
}

// redactValue keeps an authentication scheme and drops the credential after it,
// so that `Bearer eyJhbGci…` is recorded as `Bearer [redacted]`.
func redactValue(value string) string {
	scheme, rest, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found || rest == "" || !isSchemeWord(scheme) {
		return redactedMarker
	}
	return scheme + " " + redactedMarker
}

func isSchemeWord(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func (h HeaderSet) encode() ([]byte, error) {
	if h == nil {
		h = HeaderSet{}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("encode exchange headers: %w", err)
	}
	return b, nil
}

func decodeHeaderSet(b []byte) HeaderSet {
	if len(b) == 0 {
		return HeaderSet{}
	}
	var h HeaderSet
	if err := json.Unmarshal(b, &h); err != nil || h == nil {
		// Our own column, written by encode(): a failure here is corruption, and
		// the rest of the exchange is still worth showing.
		return HeaderSet{}
	}
	return h
}

// The presentation helpers below are on the domain type because they answer
// questions about an exchange rather than about HTML — a template calls them,
// and so could a future API or a terminal client.

// DurationMS is the round trip in milliseconds, for a template that prints it.
func (e Exchange) DurationMS() int64 { return e.Duration.Milliseconds() }

// Outcome classifies the response for the list view: what colour a row is, in
// words rather than in CSS.
func (e Exchange) Outcome() string {
	switch {
	case !e.Matched:
		return "unmatched"
	case e.StatusCode >= 500:
		return "server-error"
	case e.StatusCode >= 400:
		return "client-error"
	default:
		return "ok"
	}
}

// Target is the path with the query string back on it, which is what the client
// actually asked for and what somebody comparing against their own code wants
// to see.
func (e Exchange) Target() string {
	if e.Query == "" {
		return e.Path
	}
	return e.Path + "?" + e.Query
}

// RequestBodyText and ResponseBodyText render a stored body for display.
func (e Exchange) RequestBodyText() string  { return bodyText(e.RequestBody) }
func (e Exchange) ResponseBodyText() string { return bodyText(e.ResponseBody) }

// bodyText makes a recorded body printable: JSON is indented, because a mock
// REST API mostly carries JSON and reading it as one long line is the thing an
// inspector exists to stop. Anything that is not valid UTF-8 is described
// rather than printed, so that an upload of binary does not fill the page with
// replacement characters.
func bodyText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if !utf8.Valid(body) {
		return fmt.Sprintf("(%d bytes of binary)", len(body))
	}
	if indented, err := indentJSON(body); err == nil {
		return indented
	}
	return string(body)
}

// indentJSON reformats a JSON body over several lines. It fails for anything
// that is not JSON, which is how bodyText decides whether to print the bytes as
// they came.
func indentJSON(body []byte) (string, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Cursor is where this exchange sits in its project's log, for paging and for
// the live tail.
func (e Exchange) Cursor() ExchangeCursor {
	return ExchangeCursor{At: e.CreatedAt, ID: e.ID}
}

// ExchangeCursor is a position in a project's log: the timestamp and id of one
// exchange, compared as a pair.
//
// The pair is the point. Exchanges are written in batches and several can share
// a timestamp to the microsecond, so a cursor of time alone would either repeat
// them or skip them depending on which way the comparison leaned.
type ExchangeCursor struct {
	At time.Time
	ID uuid.UUID
}

// IsZero reports whether this is the empty cursor — the start of the newest
// page, or a tail that has seen nothing yet.
func (c ExchangeCursor) IsZero() bool { return c.At.IsZero() }

// String encodes the cursor for a URL. Microseconds because that is the
// resolution Postgres stores, so the value round-trips exactly rather than
// rounding onto a row it was meant to exclude.
func (c ExchangeCursor) String() string {
	if c.IsZero() {
		return ""
	}
	return strconv.FormatInt(c.At.UnixMicro(), 10) + "-" + c.ID.String()
}

// ParseExchangeCursor reads what String wrote. A cursor that does not parse is
// an error rather than the first page: a link that has been mangled should say
// so instead of quietly showing something else.
func ParseExchangeCursor(s string) (ExchangeCursor, error) {
	if s == "" {
		return ExchangeCursor{}, nil
	}

	micros, rest, found := strings.Cut(s, "-")
	if !found {
		return ExchangeCursor{}, queryErrorf("%q is not a position in the log", s)
	}
	at, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return ExchangeCursor{}, queryErrorf("%q is not a position in the log", s)
	}
	id, err := uuid.Parse(rest)
	if err != nil {
		return ExchangeCursor{}, queryErrorf("%q is not a position in the log", s)
	}
	return ExchangeCursor{At: time.UnixMicro(at).UTC(), ID: id}, nil
}

// Exchange listing limits. The default is what the inspector's first page
// shows; the ceiling is what any caller may ask for.
const (
	DefaultExchangeLimit = 50
	MaxExchangeLimit     = 500
)

// InsertExchanges writes a batch of exchanges and returns how many rows landed.
//
// COPY rather than a statement per row: this is called by the batching writer
// behind the recording middleware, and the whole reason that writer exists is
// that the cost of logging should not scale with the number of requests being
// logged.
//
// Redaction and the body ceiling are applied here rather than by the caller.
// This is the boundary at which an exchange stops being a value in memory and
// becomes something kept for weeks, and a rule enforced anywhere else is a rule
// a second caller can forget.
func (s *Store) InsertExchanges(ctx context.Context, exchanges []Exchange) (int, error) {
	if len(exchanges) == 0 {
		return 0, nil
	}

	rows := make([]dbgen.InsertExchangesParams, len(exchanges))
	for i, ex := range exchanges {
		params, err := exchangeParams(ex)
		if err != nil {
			return 0, err
		}
		rows[i] = params
	}

	n, err := s.q.InsertExchanges(ctx, rows)
	if err != nil {
		return int(n), fmt.Errorf("insert exchanges: %w", err)
	}
	return int(n), nil
}

// ExchangesByProject returns one page of a project's log, newest first. An
// empty cursor asks for the newest page.
//
// The caller has to have established that the project belongs to whoever is
// asking: exchanges have no owner column, by design (see queries/exchanges.sql).
func (s *Store) ExchangesByProject(ctx context.Context, projectID uuid.UUID, before ExchangeCursor, limit int) ([]Exchange, error) {
	rows, err := s.q.ExchangesByProject(ctx, dbgen.ExchangesByProjectParams{
		ProjectID: fromUUID(projectID),
		BeforeAt:  fromCursorTime(before),
		BeforeID:  fromUUID(before.ID),
		PageLimit: int32(clampExchangeLimit(limit)),
	})
	if err != nil {
		return nil, fmt.Errorf("list exchanges: %w", err)
	}
	return toExchanges(rows), nil
}

// ExchangesSince returns what was recorded after the cursor, oldest first. It is
// what the live tail polls.
func (s *Store) ExchangesSince(ctx context.Context, projectID uuid.UUID, after ExchangeCursor, limit int) ([]Exchange, error) {
	if after.IsZero() {
		// A tail with no cursor would otherwise ask for the whole log. The
		// caller establishes its starting point with LatestExchangeCursor.
		return nil, nil
	}

	rows, err := s.q.ExchangesSince(ctx, dbgen.ExchangesSinceParams{
		ProjectID: fromUUID(projectID),
		AfterAt:   pgtype.Timestamptz{Time: after.At, Valid: true},
		AfterID:   fromUUID(after.ID),
		PageLimit: int32(clampExchangeLimit(limit)),
	})
	if err != nil {
		return nil, fmt.Errorf("tail exchanges: %w", err)
	}
	return toExchanges(rows), nil
}

// ExchangeByID returns one exchange of a project, or ErrNotFound.
func (s *Store) ExchangeByID(ctx context.Context, projectID, id uuid.UUID) (Exchange, error) {
	row, err := s.q.ExchangeByID(ctx, dbgen.ExchangeByIDParams{
		ProjectID: fromUUID(projectID),
		ID:        fromUUID(id),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Exchange{}, ErrNotFound
		}
		return Exchange{}, fmt.Errorf("find exchange: %w", err)
	}
	return toExchange(row), nil
}

// LatestExchangeCursor is where a live tail starts. A project with nothing
// recorded yet returns the zero cursor, which is not an error: it is a log that
// has not started.
func (s *Store) LatestExchangeCursor(ctx context.Context, projectID uuid.UUID) (ExchangeCursor, error) {
	row, err := s.q.LatestExchangeCursor(ctx, fromUUID(projectID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExchangeCursor{}, nil
		}
		return ExchangeCursor{}, fmt.Errorf("read the newest exchange: %w", err)
	}
	return ExchangeCursor{At: toTime(row.CreatedAt), ID: toUUID(row.ID)}, nil
}

func clampExchangeLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultExchangeLimit
	case limit > MaxExchangeLimit:
		return MaxExchangeLimit
	default:
		return limit
	}
}

// fromCursorTime renders the cursor's timestamp as the nullable parameter the
// listing statement guards its comparison with. An empty cursor becomes SQL
// null, which is how the statement says "the first page".
func fromCursorTime(c ExchangeCursor) pgtype.Timestamptz {
	if c.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: c.At, Valid: true}
}

func exchangeParams(ex Exchange) (dbgen.InsertExchangesParams, error) {
	requestHeaders, err := ex.RequestHeaders.Redacted().encode()
	if err != nil {
		return dbgen.InsertExchangesParams{}, err
	}
	responseHeaders, err := ex.ResponseHeaders.Redacted().encode()
	if err != nil {
		return dbgen.InsertExchangesParams{}, err
	}

	id := ex.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	createdAt := ex.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	requestBody, requestTruncated := capBody(ex.RequestBody, ex.RequestBodyTruncated)
	responseBody, responseTruncated := capBody(ex.ResponseBody, ex.ResponseBodyTruncated)

	direction := ex.Direction
	if direction == "" {
		direction = DirectionInbound
	}

	return dbgen.InsertExchangesParams{
		ID:                    fromUUID(id),
		ProjectID:             fromUUID(ex.ProjectID),
		EndpointID:            fromNullableUUID(ex.EndpointID),
		Direction:             direction,
		Matched:               ex.Matched,
		Method:                ex.Method,
		Path:                  ex.Path,
		Query:                 fromNullableText(ex.Query),
		RequestHeaders:        requestHeaders,
		RequestBody:           requestBody,
		RequestBodyTruncated:  requestTruncated,
		StatusCode:            fromNullableStatus(ex.StatusCode),
		ResponseHeaders:       responseHeaders,
		ResponseBody:          responseBody,
		ResponseBodyTruncated: responseTruncated,
		DurationMs:            int32(ex.Duration.Milliseconds()),
		RemoteAddr:            parseRemoteAddr(ex.RemoteAddr),
		CreatedAt:             pgtype.Timestamptz{Time: createdAt, Valid: true},
	}, nil
}

// capBody enforces the ceiling one row may cost, reporting truncation whether
// it happened here or before.
func capBody(body []byte, truncated bool) ([]byte, bool) {
	if len(body) > MaxExchangeBody {
		return body[:MaxExchangeBody], true
	}
	return body, truncated
}

func toExchanges(rows []dbgen.Exchange) []Exchange {
	exchanges := make([]Exchange, len(rows))
	for i, row := range rows {
		exchanges[i] = toExchange(row)
	}
	return exchanges
}

func toExchange(row dbgen.Exchange) Exchange {
	var remote string
	if row.RemoteAddr != nil {
		remote = row.RemoteAddr.String()
	}

	return Exchange{
		ID:                    toUUID(row.ID),
		ProjectID:             toUUID(row.ProjectID),
		EndpointID:            toUUID(row.EndpointID),
		Direction:             row.Direction,
		Matched:               row.Matched,
		Method:                row.Method,
		Path:                  row.Path,
		Query:                 row.Query.String,
		RequestHeaders:        decodeHeaderSet(row.RequestHeaders),
		RequestBody:           row.RequestBody,
		RequestBodyTruncated:  row.RequestBodyTruncated,
		StatusCode:            int(row.StatusCode.Int16),
		ResponseHeaders:       decodeHeaderSet(row.ResponseHeaders),
		ResponseBody:          row.ResponseBody,
		ResponseBodyTruncated: row.ResponseBodyTruncated,
		Duration:              time.Duration(row.DurationMs) * time.Millisecond,
		RemoteAddr:            remote,
		CreatedAt:             toTime(row.CreatedAt),
	}
}

// fromNullableUUID is fromUUID for a column that may be null: the nil uuid means
// "no endpoint answered", which is not the same as an endpoint whose id happens
// to be zero — there is no such endpoint.
func fromNullableUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return fromUUID(id)
}

func fromNullableText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// fromNullableStatus renders the response status. Zero means the handler wrote
// nothing at all — the client hung up mid-request — and null says that, where 0
// would look like a status code.
func fromNullableStatus(status int) pgtype.Int2 {
	if status == 0 {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: int16(status), Valid: true}
}

// parseRemoteAddr renders the peer address for the inet column. Anything that
// is not an address — a unix socket, or a test's fabricated value — is stored as
// null rather than refused, because the exchange is still worth keeping.
func parseRemoteAddr(addr string) *netip.Addr {
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil
	}
	return &parsed
}
