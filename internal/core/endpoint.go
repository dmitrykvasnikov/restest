package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// Endpoint kinds. A static endpoint answers the same bytes every time; a
// collection endpoint is bound to a Collection and expands into the full set of
// REST routes rooted at its path (DESIGN.md §5).
const (
	KindStatic     = "static"
	KindCollection = "collection"
)

// Kinds are the kinds an endpoint may be, in the order the form offers them.
// They mirror the `endpoints_kind_fields` check constraint.
var Kinds = []string{KindStatic, KindCollection}

// MethodAny is the method that matches every verb. It is stored as it is
// written in the form, so a user reading the endpoint list sees the same "*"
// they typed.
const MethodAny = "*"

// Endpoint is one route the mock server answers.
type Endpoint struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	// Method is an upper-case verb or MethodAny.
	Method string
	// Path is the pattern, with named parameters in braces: /users/{id}/posts.
	// Stored normalised, so that /users and /users/ cannot become two rows that
	// the matcher would then have to choose between.
	Path      string
	Kind      string
	IsEnabled bool
	DelayMS   int

	// StatusCode and Body belong to KindStatic and are zero otherwise.
	StatusCode int
	// Body is served verbatim. It is text rather than jsonb because a mock
	// response is not always JSON, and these bytes are replayed rather than
	// queried.
	Body string

	// CollectionID belongs to KindCollection and is uuid.Nil otherwise. It names
	// the collection the six expanded routes read and write.
	CollectionID uuid.UUID

	// Headers are set on every response this endpoint produces, of either kind.
	Headers Headers

	CreatedAt time.Time
	UpdatedAt time.Time
}

// MockEndpoint is an endpoint together with the project slug that addresses it.
// The route table is keyed by slug because that is what arrives in the URL.
type MockEndpoint struct {
	Endpoint
	ProjectSlug string
}

// MockProject is the little a route table needs to know about a project: that
// it exists, and under what name.
type MockProject struct {
	ID   uuid.UUID
	Slug string
}

// MockData is one consistent-enough view of everything the mock server serves.
// Projects are listed separately from endpoints because a project with nothing
// defined still has to be recognised — "nothing is defined here" and "no such
// project" are different answers to give.
type MockData struct {
	Projects  []MockProject
	Endpoints []MockEndpoint
}

// EndpointInput is what a caller supplies to create or update an endpoint. A
// struct rather than nine arguments, because nine arguments of which four are
// ints is how values end up silently swapped.
//
// Kind decides which of the remaining fields matter: StatusCode and Body for
// KindStatic, CollectionID for KindCollection. The fields belonging to the other
// kind are ignored rather than refused, so that switching a form between the two
// does not have to clear them first.
type EndpointInput struct {
	Kind       string
	Method     string
	Path       string
	StatusCode int
	DelayMS    int
	IsEnabled  bool
	Body       string
	Headers    Headers

	CollectionID uuid.UUID
}

// endpointsPathConstraint is the unique index behind (project, method, path).
// Two endpoints answering the same verb at the same path would be a silent
// coin toss at match time, so the database refuses them.
const endpointsPathConstraint = "endpoints_project_id_method_path_pattern_key"

// ErrEndpointExists reports that the project already answers this verb at this
// path.
var ErrEndpointExists = errors.New("endpoint already defined")

// CreateEndpoint adds an endpoint to a project the caller owns.
func (s *Store) CreateEndpoint(ctx context.Context, ownerID, projectID uuid.UUID, in EndpointInput) (Endpoint, error) {
	in, err := in.normalize()
	if err != nil {
		return Endpoint{}, err
	}
	headers, err := in.Headers.encode()
	if err != nil {
		return Endpoint{}, err
	}

	var row dbgen.Endpoint
	if in.Kind == KindCollection {
		row, err = s.q.CreateCollectionEndpoint(ctx, dbgen.CreateCollectionEndpointParams{
			OwnerID:         fromUUID(ownerID),
			ProjectID:       fromUUID(projectID),
			Method:          in.Method,
			PathPattern:     in.Path,
			IsEnabled:       in.IsEnabled,
			DelayMs:         int32(in.DelayMS),
			CollectionID:    fromUUID(in.CollectionID),
			ResponseHeaders: headers,
		})
	} else {
		row, err = s.q.CreateEndpoint(ctx, dbgen.CreateEndpointParams{
			OwnerID:         fromUUID(ownerID),
			ProjectID:       fromUUID(projectID),
			Method:          in.Method,
			PathPattern:     in.Path,
			IsEnabled:       in.IsEnabled,
			DelayMs:         int32(in.DelayMS),
			StatusCode:      pgtype.Int2{Int16: int16(in.StatusCode), Valid: true},
			ResponseBody:    pgtype.Text{String: in.Body, Valid: true},
			ResponseHeaders: headers,
		})
	}
	if err != nil {
		// No row inserted means the select matched nothing: the project does not
		// exist, or is not this caller's, or — for a collection endpoint — the
		// collection is not in that project.
		if errors.Is(err, pgx.ErrNoRows) {
			return Endpoint{}, ErrNotFound
		}
		if uniqueViolation(err, endpointsPathConstraint) {
			return Endpoint{}, ErrEndpointExists
		}
		return Endpoint{}, fmt.Errorf("create endpoint: %w", err)
	}
	return toEndpoint(row)
}

// EndpointsByProject lists a project's endpoints for its page.
func (s *Store) EndpointsByProject(ctx context.Context, ownerID, projectID uuid.UUID) ([]Endpoint, error) {
	rows, err := s.q.EndpointsByProject(ctx, dbgen.EndpointsByProjectParams{
		OwnerID:   fromUUID(ownerID),
		ProjectID: fromUUID(projectID),
	})
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}

	endpoints := make([]Endpoint, len(rows))
	for i, row := range rows {
		if endpoints[i], err = toEndpoint(row); err != nil {
			return nil, err
		}
	}
	return endpoints, nil
}

// EndpointByOwnerAndID resolves the {id} in an edit URL. An endpoint in
// somebody else's project is ErrNotFound, the same answer as one that never
// existed.
func (s *Store) EndpointByOwnerAndID(ctx context.Context, ownerID, id uuid.UUID) (Endpoint, error) {
	row, err := s.q.EndpointByOwnerAndID(ctx, dbgen.EndpointByOwnerAndIDParams{
		OwnerID: fromUUID(ownerID),
		ID:      fromUUID(id),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Endpoint{}, ErrNotFound
		}
		return Endpoint{}, fmt.Errorf("find endpoint: %w", err)
	}
	return toEndpoint(row)
}

// UpdateEndpoint rewrites an endpoint the caller owns.
func (s *Store) UpdateEndpoint(ctx context.Context, ownerID, id uuid.UUID, in EndpointInput) (Endpoint, error) {
	in, err := in.normalize()
	if err != nil {
		return Endpoint{}, err
	}
	headers, err := in.Headers.encode()
	if err != nil {
		return Endpoint{}, err
	}

	var row dbgen.Endpoint
	if in.Kind == KindCollection {
		row, err = s.q.UpdateCollectionEndpoint(ctx, dbgen.UpdateCollectionEndpointParams{
			OwnerID:         fromUUID(ownerID),
			ID:              fromUUID(id),
			Method:          in.Method,
			PathPattern:     in.Path,
			IsEnabled:       in.IsEnabled,
			DelayMs:         int32(in.DelayMS),
			CollectionID:    fromUUID(in.CollectionID),
			ResponseHeaders: headers,
		})
	} else {
		row, err = s.q.UpdateEndpoint(ctx, dbgen.UpdateEndpointParams{
			OwnerID:         fromUUID(ownerID),
			ID:              fromUUID(id),
			Method:          in.Method,
			PathPattern:     in.Path,
			IsEnabled:       in.IsEnabled,
			DelayMs:         int32(in.DelayMS),
			StatusCode:      pgtype.Int2{Int16: int16(in.StatusCode), Valid: true},
			ResponseBody:    pgtype.Text{String: in.Body, Valid: true},
			ResponseHeaders: headers,
		})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Endpoint{}, ErrNotFound
		}
		if uniqueViolation(err, endpointsPathConstraint) {
			return Endpoint{}, ErrEndpointExists
		}
		return Endpoint{}, fmt.Errorf("update endpoint: %w", err)
	}
	return toEndpoint(row)
}

// DeleteEndpoint removes an endpoint the caller owns.
func (s *Store) DeleteEndpoint(ctx context.Context, ownerID, id uuid.UUID) error {
	n, err := s.q.DeleteEndpoint(ctx, dbgen.DeleteEndpointParams{
		OwnerID: fromUUID(ownerID),
		ID:      fromUUID(id),
	})
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MockData reads everything the route table is built from.
//
// The two statements are not wrapped in a transaction. A rebuild follows every
// change that could make them disagree, so the worst a torn read can produce is
// an endpoint whose project has just been deleted — which the table drops on
// the way in, and which the next rebuild settles anyway.
func (s *Store) MockData(ctx context.Context) (MockData, error) {
	projectRows, err := s.q.MockProjects(ctx)
	if err != nil {
		return MockData{}, fmt.Errorf("list projects for the route table: %w", err)
	}
	endpointRows, err := s.q.MockEndpoints(ctx)
	if err != nil {
		return MockData{}, fmt.Errorf("list endpoints for the route table: %w", err)
	}

	data := MockData{
		Projects:  make([]MockProject, len(projectRows)),
		Endpoints: make([]MockEndpoint, len(endpointRows)),
	}
	for i, row := range projectRows {
		data.Projects[i] = MockProject{ID: toUUID(row.ID), Slug: row.Slug}
	}
	for i, row := range endpointRows {
		endpoint, err := toEndpoint(dbgen.Endpoint{
			ID:              row.ID,
			ProjectID:       row.ProjectID,
			Method:          row.Method,
			PathPattern:     row.PathPattern,
			Kind:            row.Kind,
			IsEnabled:       row.IsEnabled,
			DelayMs:         row.DelayMs,
			StatusCode:      row.StatusCode,
			ResponseBody:    row.ResponseBody,
			CollectionID:    row.CollectionID,
			ResponseHeaders: row.ResponseHeaders,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
		if err != nil {
			return MockData{}, err
		}
		data.Endpoints[i] = MockEndpoint{Endpoint: endpoint, ProjectSlug: row.ProjectSlug}
	}
	return data, nil
}

func toEndpoint(row dbgen.Endpoint) (Endpoint, error) {
	headers, err := decodeHeaders(row.ResponseHeaders)
	if err != nil {
		// Our own column, written by encode(): this is corruption, not input.
		return Endpoint{}, fmt.Errorf("endpoint %s: decode response headers: %w", toUUID(row.ID), err)
	}

	return Endpoint{
		ID:           toUUID(row.ID),
		ProjectID:    toUUID(row.ProjectID),
		Method:       row.Method,
		Path:         row.PathPattern,
		Kind:         row.Kind,
		IsEnabled:    row.IsEnabled,
		DelayMS:      int(row.DelayMs),
		StatusCode:   int(row.StatusCode.Int16),
		Body:         row.ResponseBody.String,
		CollectionID: toUUID(row.CollectionID),
		Headers:      headers,
		CreatedAt:    toTime(row.CreatedAt),
		UpdatedAt:    toTime(row.UpdatedAt),
	}, nil
}

// Validate reports whether the input would be accepted, without storing it.
//
// The store calls the same rules on the way in, so this is not a gate anyone
// has to remember to pass through — it is for callers that want to know the
// answer before they commit to asking, and for tests that stand in for the
// store and must not become a second, laxer copy of the rules.
func (in EndpointInput) Validate() error {
	_, err := in.normalize()
	return err
}

// normalize tidies the input and then checks it, returning the tidied form so
// that a caller stores exactly what was validated rather than what was typed.
func (in EndpointInput) normalize() (EndpointInput, error) {
	if in.Kind == "" {
		in.Kind = KindStatic
	}
	if in.Kind == KindCollection {
		// A collection endpoint answers six verbs at two paths, so the verb is
		// not the user's to choose. It is stored as the wildcard, which is also
		// what makes the unique index on (project, method, path) refuse a second
		// collection rooted at the same place.
		in.Method = MethodAny
	}
	in.Method = NormalizeMethod(in.Method)
	in.Path = NormalizePath(in.Path)

	var fe FieldErrors
	validateKind(&fe, in.Kind)
	validateMethod(&fe, in.Method)
	validatePath(&fe, in.Path)
	validateDelay(&fe, in.DelayMS)
	validateHeaders(&fe, in.Headers)

	if in.Kind == KindCollection {
		validateCollectionPath(&fe, in.Path)
		if in.CollectionID == uuid.Nil {
			fe.Add("collection_id", "Choose the collection this endpoint serves.")
		}
	} else {
		validateStatusCode(&fe, in.StatusCode)
		validateBody(&fe, in.Body)
	}

	return in, fe.orNil()
}

// Headers are the response headers an endpoint sets, keyed by canonical name.
type Headers map[string]string

// Lines renders the headers the way the form takes them: one "Name: value" per
// line, sorted, so that editing an endpoint twice without touching the field
// does not reorder it.
func (h Headers) Lines() string {
	names := slices.Sorted(maps.Keys(h))

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(h[name])
	}
	return b.String()
}

// ParseHeaderLines reads the "Name: value" lines the form submits. It does not
// validate — validateHeaders does that, so that a malformed line comes back as
// a message beside the field rather than as an error the caller has to render
// twice.
func ParseHeaderLines(s string) Headers {
	headers := make(Headers)
	for line := range strings.Lines(s) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}

		name, value, found := strings.Cut(line, ":")
		if !found {
			// Kept as it stands so validateHeaders can name it. A key with no
			// value is what a half-typed line looks like.
			headers[strings.TrimSpace(line)] = ""
			continue
		}
		headers[canonicalHeaderName(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	return headers
}

// encode renders the headers for the jsonb column. An empty map is stored as
// {} rather than as SQL null, matching the column default.
func (h Headers) encode() ([]byte, error) {
	if h == nil {
		h = Headers{}
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("encode response headers: %w", err)
	}
	return b, nil
}

func decodeHeaders(b []byte) (Headers, error) {
	if len(b) == 0 {
		return Headers{}, nil
	}
	var h Headers
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	if h == nil {
		h = Headers{}
	}
	return h, nil
}
