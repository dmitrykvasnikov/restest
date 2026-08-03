package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// Identifier strategies. `serial` counts up from one, which is what makes a
// mock dataset readable — /users/1 is a URL somebody can type. `uuid` is for
// clients that must not be able to guess the next id, or that are being tested
// against identifiers they cannot fit in an int.
const (
	IDSerial = "serial"
	IDUUID   = "uuid"
)

// IDStrategies are the strategies a collection may use, in the order the form
// offers them. They mirror the `collections_id_strategy` constraint.
var IDStrategies = []string{IDSerial, IDUUID}

// Collection is a named set of JSON documents with a seed to return to.
//
// It is schema-less on purpose: a mock dataset is edited by hand and changes
// shape while somebody works out what their client needs, and a schema would
// turn every such change into a migration.
type Collection struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	Name      string

	// Seed is the JSON array that ResetCollection restores. It is stored as
	// jsonb, so what comes back is normalised rather than character-for-character
	// what was typed.
	Seed json.RawMessage
	// IDField is the JSON key carrying the identifier that appears in URLs.
	IDField string
	// IDStrategy is IDSerial or IDUUID.
	IDStrategy string
	// NextSerial is the counter behind IDSerial. It is exposed because the UI
	// shows it: someone who has just reset a collection wants to know what the
	// next POST will be called.
	NextSerial int64

	// Documents is how many documents the collection holds. It is filled in by
	// CollectionsByProject and left at zero elsewhere, because counting is a
	// question the list page asks and nothing else does.
	Documents int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CollectionInput is what a caller supplies to create or update a collection.
// Seed is the text as it was typed, not a decoded array: an unparseable seed has
// to come back in the editor the way it was written, next to the reason it was
// refused.
type CollectionInput struct {
	Name       string
	IDField    string
	IDStrategy string
	Seed       string
}

// collectionsNameConstraint is the unique index behind (project, name). Two
// collections of one name in one project would make the reset URL ambiguous.
const collectionsNameConstraint = "collections_project_id_name_key"

// ErrCollectionExists reports that the project already has a collection of this
// name.
var ErrCollectionExists = errors.New("collection already defined")

// CreateCollection adds a collection to a project the caller owns, seeded and
// ready: the seed is applied immediately, so a collection is never created
// empty when it was given something to hold.
func (s *Store) CreateCollection(ctx context.Context, ownerID, projectID uuid.UUID, in CollectionInput) (Collection, error) {
	in, seed, err := in.normalize()
	if err != nil {
		return Collection{}, err
	}

	row, err := s.q.CreateCollection(ctx, dbgen.CreateCollectionParams{
		OwnerID:    fromUUID(ownerID),
		ProjectID:  fromUUID(projectID),
		Name:       in.Name,
		Seed:       seed,
		IDField:    in.IDField,
		IDStrategy: in.IDStrategy,
	})
	if err != nil {
		// No row inserted means the select found no project: either it does not
		// exist or it is not this caller's.
		if errors.Is(err, pgx.ErrNoRows) {
			return Collection{}, ErrNotFound
		}
		if uniqueViolation(err, collectionsNameConstraint) {
			return Collection{}, ErrCollectionExists
		}
		return Collection{}, fmt.Errorf("create collection: %w", err)
	}

	collection := toCollection(row)
	if _, err := s.ResetCollection(ctx, collection.ID); err != nil {
		return Collection{}, fmt.Errorf("apply the seed of a new collection: %w", err)
	}
	return collection, nil
}

// CollectionsByProject lists a project's collections with their sizes, for the
// project page.
func (s *Store) CollectionsByProject(ctx context.Context, ownerID, projectID uuid.UUID) ([]Collection, error) {
	rows, err := s.q.CollectionsByProject(ctx, dbgen.CollectionsByProjectParams{
		OwnerID:   fromUUID(ownerID),
		ProjectID: fromUUID(projectID),
	})
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}

	collections := make([]Collection, len(rows))
	for i, row := range rows {
		collections[i] = toCollection(dbgen.Collection{
			ID:         row.ID,
			ProjectID:  row.ProjectID,
			Name:       row.Name,
			Seed:       row.Seed,
			IDField:    row.IDField,
			IDStrategy: row.IDStrategy,
			NextSerial: row.NextSerial,
			CreatedAt:  row.CreatedAt,
			UpdatedAt:  row.UpdatedAt,
		})
		collections[i].Documents = row.DocumentCount
	}
	return collections, nil
}

// CollectionByOwnerAndID resolves the {id} in an edit URL.
func (s *Store) CollectionByOwnerAndID(ctx context.Context, ownerID, id uuid.UUID) (Collection, error) {
	row, err := s.q.CollectionByOwnerAndID(ctx, dbgen.CollectionByOwnerAndIDParams{
		OwnerID: fromUUID(ownerID),
		ID:      fromUUID(id),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Collection{}, ErrNotFound
		}
		return Collection{}, fmt.Errorf("find collection: %w", err)
	}
	return toCollection(row), nil
}

// CollectionByOwnerAndName resolves the {slug} and {name} of the reset URL.
func (s *Store) CollectionByOwnerAndName(ctx context.Context, ownerID uuid.UUID, slug, name string) (Collection, error) {
	row, err := s.q.CollectionByOwnerAndName(ctx, dbgen.CollectionByOwnerAndNameParams{
		OwnerID: fromUUID(ownerID),
		Slug:    slug,
		Name:    name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Collection{}, ErrNotFound
		}
		return Collection{}, fmt.Errorf("find collection: %w", err)
	}
	return toCollection(row), nil
}

// UpdateCollection rewrites a collection the caller owns.
//
// It does not re-apply the seed. Editing the seed is how somebody prepares what
// the *next* reset will restore, and throwing away the documents they are
// working with as a side effect of saving that would be a surprise; the reset
// button is the deliberate act.
func (s *Store) UpdateCollection(ctx context.Context, ownerID, id uuid.UUID, in CollectionInput) (Collection, error) {
	in, seed, err := in.normalize()
	if err != nil {
		return Collection{}, err
	}

	row, err := s.q.UpdateCollection(ctx, dbgen.UpdateCollectionParams{
		OwnerID:    fromUUID(ownerID),
		ID:         fromUUID(id),
		Name:       in.Name,
		Seed:       seed,
		IDField:    in.IDField,
		IDStrategy: in.IDStrategy,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Collection{}, ErrNotFound
		}
		if uniqueViolation(err, collectionsNameConstraint) {
			return Collection{}, ErrCollectionExists
		}
		return Collection{}, fmt.Errorf("update collection: %w", err)
	}
	return toCollection(row), nil
}

// DeleteCollection removes a collection the caller owns, and with it its
// documents and any endpoint serving it — both by cascade, which is why the
// caller has to rebuild the route table afterwards.
func (s *Store) DeleteCollection(ctx context.Context, ownerID, id uuid.UUID) error {
	n, err := s.q.DeleteCollection(ctx, dbgen.DeleteCollectionParams{
		OwnerID: fromUUID(ownerID),
		ID:      fromUUID(id),
	})
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResetCollection throws away every document and rebuilds the collection from
// its seed, returning how many documents it now holds.
//
// The whole thing is one transaction, so a client reading the collection sees
// either the old contents or the new ones and never an empty collection that is
// halfway through being refilled. That matters because reset is what a test
// suite calls between runs, and the run after it starts immediately.
func (s *Store) ResetCollection(ctx context.Context, id uuid.UUID) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin reset: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this needs no flag.
	defer tx.Rollback(ctx) //nolint:errcheck // the commit path has already reported

	q := s.q.WithTx(tx)

	row, err := q.CollectionForReset(ctx, fromUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("read collection seed: %w", err)
	}

	docs, next, err := seedDocuments(row.Seed, row.IDField, row.IDStrategy)
	if err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, `delete from documents where collection_id = $1`, fromUUID(id)); err != nil {
		return 0, fmt.Errorf("clear documents: %w", err)
	}
	if err := insertDocuments(ctx, tx, id, docs); err != nil {
		return 0, err
	}
	if err := q.SetNextSerial(ctx, dbgen.SetNextSerialParams{
		ID:         fromUUID(id),
		NextSerial: next,
	}); err != nil {
		return 0, fmt.Errorf("reset the identifier counter: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit reset: %w", err)
	}
	return len(docs), nil
}

// seedDoc is one document of a seed, after its identifier has been settled.
type seedDoc struct {
	publicID string
	body     []byte
}

// seedDocuments turns a seed array into the rows to insert, and works out where
// the serial counter should be left afterwards.
//
// A seed may name its own identifiers, and usually does — that is how a fixture
// gets to say /users/1 rather than whatever the counter happened to be on. What
// it does not name is allocated, skipping the values it did name, so that a seed
// mixing the two cannot produce a duplicate.
func seedDocuments(seed []byte, idField, strategy string) ([]seedDoc, int64, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(seed, &elements); err != nil {
		// Our own column, written through validateSeed: this is corruption.
		return nil, 0, fmt.Errorf("decode seed: %w", err)
	}

	objects := make([]map[string]json.RawMessage, len(elements))
	ids := make([]string, len(elements))
	taken := make(map[string]bool, len(elements))

	// First pass: read the identifiers the seed states, so that the second pass
	// can allocate around them.
	for i, element := range elements {
		if err := json.Unmarshal(element, &objects[i]); err != nil {
			var fe FieldErrors
			fe.Add("seed", fmt.Sprintf("Entry %d is not a JSON object.", i+1))
			return nil, 0, fe
		}
		raw, ok := objects[i][idField]
		if !ok {
			continue
		}
		id, ok := scalarText(raw)
		if !ok {
			var fe FieldErrors
			fe.Add("seed", fmt.Sprintf("Entry %d has a %q that is neither a string nor a number.", i+1, idField))
			return nil, 0, fe
		}
		if taken[id] {
			var fe FieldErrors
			fe.Add("seed", fmt.Sprintf("Two entries share the %s %q.", idField, id))
			return nil, 0, fe
		}
		taken[id] = true
		ids[i] = id
	}

	// Second pass: allocate what was not stated, and put the identifier into the
	// document so that every document carries its own id however it got one.
	var counter int64
	docs := make([]seedDoc, len(elements))
	for i := range elements {
		if ids[i] == "" {
			ids[i] = allocateSeedID(strategy, &counter, taken)
			taken[ids[i]] = true
			objects[i][idField] = json.RawMessage(idJSON(ids[i]))
		}

		body, err := json.Marshal(objects[i])
		if err != nil {
			return nil, 0, fmt.Errorf("encode seed entry %d: %w", i+1, err)
		}
		docs[i] = seedDoc{publicID: ids[i], body: body}
	}
	return docs, nextSerialAfter(ids), nil
}

// allocateSeedID hands out an identifier for a seed entry that did not state
// one, stepping over anything the seed claimed for itself.
func allocateSeedID(strategy string, counter *int64, taken map[string]bool) string {
	if strategy == IDUUID {
		return uuid.NewString()
	}
	for {
		*counter++
		if id := strconv.FormatInt(*counter, 10); !taken[id] {
			return id
		}
	}
}

// nextSerialAfter is where the counter is left once a seed is in place: past
// every numeric identifier the seed used, so the first POST after a reset
// cannot collide with a seeded document.
func nextSerialAfter(ids []string) int64 {
	var highest int64
	for _, id := range ids {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > highest {
			highest = n
		}
	}
	return highest + 1
}

// insertDocuments writes a whole seed in one statement.
//
// The arrays are unnested with ordinality and inserted in that order, because
// `seq` is assigned as rows are produced and `seq` is the order an unsorted
// listing comes back in. A seed that reads top to bottom in the editor should
// read the same way through the API.
func insertDocuments(ctx context.Context, tx pgx.Tx, collectionID uuid.UUID, docs []seedDoc) error {
	if len(docs) == 0 {
		return nil
	}

	ids := make([]string, len(docs))
	bodies := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.publicID
		bodies[i] = string(doc.body)
	}

	const insert = `
		insert into documents (collection_id, public_id, body)
		select $1, entry.public_id, entry.body::jsonb
		from unnest($2::text[], $3::text[]) with ordinality as entry(public_id, body, ord)
		order by entry.ord`

	if _, err := tx.Exec(ctx, insert, fromUUID(collectionID), ids, bodies); err != nil {
		return fmt.Errorf("insert seed documents: %w", err)
	}
	return nil
}

// scalarText renders a JSON string or number as the text a URL would carry.
// Anything else — a boolean, an object, an array — is not an identifier.
func scalarText(raw json.RawMessage) (string, bool) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	switch v := value.(type) {
	case string:
		return v, v != ""
	case float64:
		// Rendered from the source text rather than from the float, so that a
		// large integer id survives instead of arriving as 1.234568e+06.
		return string(raw), true
	default:
		return "", false
	}
}

// idJSON is the value written into a document's identifier field. A whole
// number stays a number, because a client filtering ?id=1 against a mock of a
// real API expects the same JSON type the real one would send.
//
// The round trip through FormatInt is what makes it safe to paste into a jsonb
// literal: "007" parses as 7 but is not a JSON number, and emitting it as one
// would produce a document Postgres refuses.
func idJSON(publicID string) string {
	if n, err := strconv.ParseInt(publicID, 10, 64); err == nil && strconv.FormatInt(n, 10) == publicID {
		return publicID
	}
	return strconv.Quote(publicID)
}

// normalize tidies the input and then checks it, returning the tidied form and
// the seed as the bytes to store, so that a caller stores exactly what was
// validated rather than what was typed.
func (in CollectionInput) normalize() (CollectionInput, []byte, error) {
	in.Name = NormalizeCollectionName(in.Name)
	in.IDField = strings.TrimSpace(in.IDField)
	if in.IDField == "" {
		in.IDField = defaultIDField
	}
	if in.IDStrategy == "" {
		in.IDStrategy = IDSerial
	}
	if strings.TrimSpace(in.Seed) == "" {
		in.Seed = "[]"
	}

	var fe FieldErrors
	validateCollectionName(&fe, in.Name)
	validateIDField(&fe, in.IDField)
	validateIDStrategy(&fe, in.IDStrategy)
	validateSeed(&fe, in.Seed)

	if err := fe.orNil(); err != nil {
		return in, nil, err
	}
	return in, []byte(in.Seed), nil
}

// Validate reports whether the input would be accepted, without storing it. The
// store applies the same rules on the way in; this exists so that a caller can
// ask first, and so that a test standing in for the store cannot become a
// second, laxer copy of them.
func (in CollectionInput) Validate() error {
	_, _, err := in.normalize()
	return err
}

// SeedPretty is the seed as the editor should show it: indented, one field per
// line. The stored value is jsonb and comes back with its whitespace gone, so
// without this every edit would open on a single very long line.
func (c Collection) SeedPretty() string {
	if len(c.Seed) == 0 {
		return "[]"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, c.Seed, "", "  "); err != nil {
		// It came out of a jsonb column, so it parses. Showing it unformatted is
		// a better answer than showing nothing.
		return string(c.Seed)
	}
	return out.String()
}

func toCollection(row dbgen.Collection) Collection {
	return Collection{
		ID:         toUUID(row.ID),
		ProjectID:  toUUID(row.ProjectID),
		Name:       row.Name,
		Seed:       json.RawMessage(row.Seed),
		IDField:    row.IDField,
		IDStrategy: row.IDStrategy,
		NextSerial: row.NextSerial,
		CreatedAt:  toTime(row.CreatedAt),
		UpdatedAt:  toTime(row.UpdatedAt),
	}
}
