package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// seedDocuments is where a fixture becomes rows. Everything about identifiers —
// which the seed states, which are allocated, and where the counter is left —
// is decided here, and getting it wrong shows up much later as a POST that
// collides with a seeded document.
func TestSeedDocuments(t *testing.T) {
	tests := []struct {
		name     string
		seed     string
		idField  string
		strategy string
		wantIDs  []string
		wantNext int64
		// wantBody, when set, is checked against the document at that index.
		bodyAt   int
		wantBody string
	}{
		{
			name:    "a seed that states its own ids keeps them",
			seed:    `[{"id":1,"name":"Ada"},{"id":2,"name":"Alan"}]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{"1", "2"}, wantNext: 3,
		},
		{
			name:    "the counter clears the highest id, not the count",
			seed:    `[{"id":10},{"id":3}]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{"10", "3"}, wantNext: 11,
		},
		{
			name:    "string ids are kept as written",
			seed:    `[{"id":"ada"},{"id":"alan"}]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{"ada", "alan"}, wantNext: 1,
		},
		{
			name:    "an entry with no id is allocated one, and carries it",
			seed:    `[{"name":"Ada"}]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{"1"}, wantNext: 2,
			bodyAt: 0, wantBody: `{"id":1,"name":"Ada"}`,
		},
		{
			// The reason for the two passes: allocation has to see every stated
			// id before it hands out its first, or it would hand out 1 and then
			// find 1 already taken.
			name:    "allocation steps over the ids the seed claimed",
			seed:    `[{"name":"Ada"},{"id":1,"name":"Alan"},{"name":"Grace"}]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{"2", "1", "3"}, wantNext: 4,
		},
		{
			name:    "a different id field is read and written",
			seed:    `[{"slug":"ada"},{"name":"Alan"}]`,
			idField: "slug", strategy: IDSerial,
			wantIDs: []string{"ada", "1"}, wantNext: 2,
			bodyAt: 1, wantBody: `{"name":"Alan","slug":1}`,
		},
		{
			name:    "an empty seed leaves the counter at one",
			seed:    `[]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{}, wantNext: 1,
		},
		{
			name:    "a large id is kept whole rather than turned into a float",
			seed:    `[{"id":9007199254740993}]`,
			idField: "id", strategy: IDSerial,
			wantIDs: []string{"9007199254740993"}, wantNext: 9007199254740994,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, next, err := seedDocuments([]byte(tt.seed), tt.idField, tt.strategy)
			if err != nil {
				t.Fatalf("seedDocuments: %v", err)
			}

			ids := make([]string, len(docs))
			for i, doc := range docs {
				ids[i] = doc.publicID
			}
			if strings.Join(ids, ",") != strings.Join(tt.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", ids, tt.wantIDs)
			}
			if next != tt.wantNext {
				t.Errorf("next serial = %d, want %d", next, tt.wantNext)
			}
			if tt.wantBody != "" && string(docs[tt.bodyAt].body) != tt.wantBody {
				t.Errorf("body %d = %s, want %s", tt.bodyAt, docs[tt.bodyAt].body, tt.wantBody)
			}
		})
	}
}

// Under the uuid strategy the counter is not what identifies anything, so it
// stays where it was and the ids are random and distinct.
func TestSeedDocumentsUUIDStrategy(t *testing.T) {
	docs, next, err := seedDocuments([]byte(`[{"name":"Ada"},{"name":"Alan"}]`), "id", IDUUID)
	if err != nil {
		t.Fatalf("seedDocuments: %v", err)
	}
	if next != 1 {
		t.Errorf("next serial = %d, want it left at 1", next)
	}
	if len(docs) != 2 || docs[0].publicID == docs[1].publicID {
		t.Fatalf("ids are not distinct: %+v", docs)
	}
	for _, doc := range docs {
		if len(doc.publicID) != 36 {
			t.Errorf("id %q is not a uuid", doc.publicID)
		}
		var object map[string]any
		if err := json.Unmarshal(doc.body, &object); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if object["id"] != doc.publicID {
			t.Errorf("body carries id %v, want the string %q", object["id"], doc.publicID)
		}
	}
}

func TestSeedDocumentsRefusals(t *testing.T) {
	tests := []struct {
		name  string
		seed  string
		says  string
		field string
	}{
		{
			name: "two entries claiming one id", seed: `[{"id":1},{"id":1}]`,
			says: "share", field: "seed",
		},
		{
			name: "an id that is neither a string nor a number", seed: `[{"id":{"a":1}}]`,
			says: "neither", field: "seed",
		},
		{
			name: "a boolean id", seed: `[{"id":true}]`,
			says: "neither", field: "seed",
		},
		{
			name: "an entry that is not an object", seed: `[7]`,
			says: "not a JSON object", field: "seed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := seedDocuments([]byte(tt.seed), "id", IDSerial)
			if err == nil {
				t.Fatalf("seedDocuments(%s) was accepted", tt.seed)
			}

			fe, ok := err.(FieldErrors) //nolint:errorlint // returned directly
			if !ok {
				t.Fatalf("error is %T, want FieldErrors so it lands beside the editor", err)
			}
			if !strings.Contains(fe[tt.field], tt.says) {
				t.Errorf("message %q does not say %q", fe[tt.field], tt.says)
			}
		})
	}
}

func TestCollectionInputValidation(t *testing.T) {
	valid := CollectionInput{Name: "users", IDField: "id", IDStrategy: IDSerial, Seed: "[]"}

	tests := []struct {
		name  string
		in    CollectionInput
		field string
	}{
		{name: "the valid one", in: valid},
		{name: "a name is required", in: with(valid, func(c *CollectionInput) { c.Name = "" }), field: "name"},
		{name: "no capitals", in: with(valid, func(c *CollectionInput) { c.Name = "Users" })},
		{name: "no spaces", in: with(valid, func(c *CollectionInput) { c.Name = "line items" }), field: "name"},
		{name: "no slashes", in: with(valid, func(c *CollectionInput) { c.Name = "a/b" }), field: "name"},
		{name: "underscores are fine", in: with(valid, func(c *CollectionInput) { c.Name = "line_items" })},
		{name: "no leading hyphen", in: with(valid, func(c *CollectionInput) { c.Name = "-users" }), field: "name"},
		{name: "one character is enough", in: with(valid, func(c *CollectionInput) { c.Name = "a" })},
		{
			name:  "a name has a ceiling",
			in:    with(valid, func(c *CollectionInput) { c.Name = strings.Repeat("a", 41) }),
			field: "name",
		},
		{name: "an id field cannot start with a digit", in: with(valid, func(c *CollectionInput) { c.IDField = "1st" }), field: "id_field"},
		{name: "an id field may be an underscore", in: with(valid, func(c *CollectionInput) { c.IDField = "_id" })},
		{name: "a made-up strategy", in: with(valid, func(c *CollectionInput) { c.IDStrategy = "snowflake" }), field: "id_strategy"},
		{name: "uuid is a strategy", in: with(valid, func(c *CollectionInput) { c.IDStrategy = IDUUID })},
		{name: "the seed has to be JSON", in: with(valid, func(c *CollectionInput) { c.Seed = "{" }), field: "seed"},
		{name: "the seed has to be an array", in: with(valid, func(c *CollectionInput) { c.Seed = `{"id":1}` }), field: "seed"},
		// A JSON null decodes into a slice without being an array, so it would
		// otherwise reach the column and be refused by a check constraint —
		// a 500 where a sentence beside the field belongs.
		{name: "and null is not one", in: with(valid, func(c *CollectionInput) { c.Seed = "null" }), field: "seed"},
		{name: "of objects", in: with(valid, func(c *CollectionInput) { c.Seed = `[1,2]` }), field: "seed"},
		{name: "an array of objects is the point", in: with(valid, func(c *CollectionInput) { c.Seed = `[{"id":1}]` })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()

			if tt.field == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted %+v", tt.in)
			}
			fe, ok := err.(FieldErrors) //nolint:errorlint // returned directly
			if !ok {
				t.Fatalf("error is %T, want FieldErrors", err)
			}
			if _, named := fe[tt.field]; !named {
				t.Errorf("errors are %v, want one against %q", fe, tt.field)
			}
		})
	}
}

// Blank fields take the defaults rather than being refused: a collection that
// accepts `id` and `serial` should need no decision made about them.
func TestCollectionInputDefaults(t *testing.T) {
	in := CollectionInput{Name: "users"}

	got, seed, err := in.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got.IDField != "id" {
		t.Errorf("id field = %q, want id", got.IDField)
	}
	if got.IDStrategy != IDSerial {
		t.Errorf("strategy = %q, want %q", got.IDStrategy, IDSerial)
	}
	if string(seed) != "[]" {
		t.Errorf("seed = %s, want []", seed)
	}
}

// The name is lower-cased and trimmed on the way in, because it is what the
// reset URL carries and a URL that works in one capitalisation and not another
// is a bug waiting to be reported.
func TestNormalizeCollectionName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"users", "users"},
		{"  users  ", "users"},
		{"Users", "users"},
		{"LINE_ITEMS", "line_items"},
	}

	for _, tt := range tests {
		if got := NormalizeCollectionName(tt.in); got != tt.want {
			t.Errorf("NormalizeCollectionName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSeedPretty(t *testing.T) {
	c := Collection{Seed: json.RawMessage(`[{"id": 1, "name": "Ada"}]`)}

	got := c.SeedPretty()
	if !strings.Contains(got, "\n") {
		t.Errorf("SeedPretty() = %q, want it spread over lines for the editor", got)
	}

	// An empty collection opens on something editable rather than on nothing.
	if got := (Collection{}).SeedPretty(); got != "[]" {
		t.Errorf("SeedPretty() of an empty seed = %q, want []", got)
	}
}

// with copies an input and applies one change, so a table of near-misses reads
// as the difference from the valid case rather than as nine repeated structs.
func with(in CollectionInput, change func(*CollectionInput)) CollectionInput {
	change(&in)
	return in
}
