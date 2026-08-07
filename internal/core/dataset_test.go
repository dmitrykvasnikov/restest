package core

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

// The built-in datasets are input to the same code paths a user's own seed goes
// through, so they have to satisfy the same rules. A dataset that the validator
// would reject is a checkbox that produces a 422 nobody can fix.
func TestBuiltinDatasetsAreValidInput(t *testing.T) {
	if len(Datasets()) == 0 {
		t.Fatal("no built-in datasets were loaded")
	}

	for _, d := range Datasets() {
		t.Run(d.Name, func(t *testing.T) {
			collection := CollectionInput{
				Name:       d.Name,
				IDField:    defaultIDField,
				IDStrategy: IDSerial,
				Seed:       string(d.Seed),
			}
			if err := collection.Validate(); err != nil {
				t.Errorf("the collection it creates is invalid: %v", err)
			}

			endpoint := EndpointInput{
				Kind:         KindCollection,
				Path:         d.Path(),
				IsEnabled:    true,
				CollectionID: uuid.New(),
			}
			if err := endpoint.Validate(); err != nil {
				t.Errorf("the endpoint serving it is invalid: %v", err)
			}
			// The path is stored normalised, and the expansion appends /{id} to
			// it. A dataset whose path was not already in that form would be
			// stored as something other than what it says here.
			if got := NormalizePath(d.Path()); got != d.Path() {
				t.Errorf("path %q is stored as %q", d.Path(), got)
			}

			if d.Title == "" || d.Summary == "" {
				t.Error("a dataset offered in the form needs a title and a summary")
			}
		})
	}
}

// What the form promises — "eight documents" — has to be what installing it
// produces, and the identifiers have to be the ones the summary implies, since
// /users/1 is the URL the whole point of a fixture is to make typeable.
func TestBuiltinDatasetsSeedCleanly(t *testing.T) {
	for _, d := range Datasets() {
		t.Run(d.Name, func(t *testing.T) {
			docs, next, err := seedDocuments(d.Seed, defaultIDField, IDSerial)
			if err != nil {
				t.Fatalf("seedDocuments: %v", err)
			}
			if len(docs) != d.Documents {
				t.Errorf("seeded %d documents, but the form says %d", len(docs), d.Documents)
			}

			// Consecutive whole numbers from 1, so the counter after the seed is
			// one past the last of them and the first POST cannot collide.
			for i, doc := range docs {
				if want := strconv.Itoa(i + 1); doc.publicID != want {
					t.Errorf("document %d has id %q, want %q", i, doc.publicID, want)
				}
			}
			if want := int64(len(docs) + 1); next != want {
				t.Errorf("next serial = %d, want %d", next, want)
			}
		})
	}
}

// The seed is what the collection editor opens on, so it has to be the array of
// objects a user would have typed — not a string holding an array, and not a
// single object.
func TestBuiltinDatasetSeedsAreArraysOfObjects(t *testing.T) {
	for _, d := range Datasets() {
		var objects []map[string]json.RawMessage
		if err := json.Unmarshal(d.Seed, &objects); err != nil {
			t.Errorf("%s: seed is not an array of objects: %v", d.Name, err)
			continue
		}
		if len(objects) == 0 {
			t.Errorf("%s: the seed is empty, so the dataset offers nothing", d.Name)
		}
	}
}

// Datasets hands out a copy. A caller that sorted or truncated the slice in
// place would otherwise change what every later caller is offered.
func TestDatasetsReturnsACopy(t *testing.T) {
	first := Datasets()
	first[0] = Dataset{Name: "clobbered"}

	if Datasets()[0].Name == "clobbered" {
		t.Error("Datasets hands out the package's own slice")
	}
}

func TestSelectDatasets(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
		// wantErr is the field the rejection is expected against.
		wantErr string
	}{
		{name: "nothing chosen is not an error", input: nil, want: nil},
		{name: "one", input: []string{"todos"}, want: []string{"todos"}},
		{
			// The offered order, not the submitted one, so a project's
			// collections read the same way whatever order the boxes were
			// ticked in.
			name:  "the defined order is kept",
			input: []string{"todos", "users"},
			want:  []string{"users", "todos"},
		},
		{
			name:  "a name sent twice installs one collection",
			input: []string{"users", "users"},
			want:  []string{"users"},
		},
		{
			name:  "names are normalised the way a collection name is",
			input: []string{" Users "},
			want:  []string{"users"},
		},
		{
			// Only an edited form can send this, and creating the project
			// without the dataset it asked for would look as though it worked.
			name:    "an unknown name is refused",
			input:   []string{"users", "invoices"},
			wantErr: "datasets",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var fe FieldErrors
			chosen := selectDatasets(&fe, tc.input)

			if tc.wantErr != "" {
				if _, ok := fe[tc.wantErr]; !ok {
					t.Fatalf("no error against %q, got %v", tc.wantErr, fe)
				}
				if chosen != nil {
					t.Errorf("a rejected selection still returned %d datasets", len(chosen))
				}
				return
			}
			if len(fe) != 0 {
				t.Fatalf("unexpected errors: %v", fe)
			}

			names := make([]string, len(chosen))
			for i, d := range chosen {
				names[i] = d.Name
			}
			if !slices.Equal(names, tc.want) {
				t.Errorf("selected %v, want %v", names, tc.want)
			}
		})
	}
}

// The demo lives at a slug the form refuses, which is what keeps an account
// from taking the name the demo answers on.
func TestDemoSlugIsReserved(t *testing.T) {
	var fe FieldErrors
	validateSlug(&fe, DemoSlug)

	if _, ok := fe["slug"]; !ok {
		t.Errorf("%q is not in the reserved list, so an account could take it", DemoSlug)
	}
}
