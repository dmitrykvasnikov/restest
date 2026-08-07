package core

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// Built-in datasets: ready-made collections a project can be created with, so
// that somebody who wants to point a client at a list of users has one without
// first inventing eight people (DESIGN.md §6).
//
// A dataset is a collection plus the endpoint that serves it, and nothing else.
// It deliberately does not describe static endpoints, response headers or
// delays: those are the things a user is here to decide, and a template that
// decided them would be answering the wrong question.

//go:embed datasets/*.json
var datasetFS embed.FS

// Dataset is one built-in collection: its seed, and the name it is served
// under. The name is used three times over — as the collection name, as the
// path the endpoint is rooted at, and as the key a form submits — because a
// dataset called `users` that answered at /people would be a puzzle, not a
// convenience.
type Dataset struct {
	// Name is the collection name, the last segment of its path, and the value
	// the project form submits.
	Name string
	// Title is the name as the interface shows it.
	Title string
	// Summary is one sentence describing what is in it, shown beside the
	// checkbox that selects it.
	Summary string
	// Seed is the JSON array the collection is created with, ready to be stored
	// in the seed column and applied.
	Seed json.RawMessage
	// Documents is how many objects the seed holds, so the form can say so
	// without counting them in a template.
	Documents int
}

// Path is where the endpoint serving this dataset is rooted, below the
// project's own /m/{slug}/ prefix.
func (d Dataset) Path() string { return "/" + d.Name }

// datasetSpecs describes the built-in datasets in the order the form offers
// them. The order is deliberate rather than alphabetical: `posts` refer to
// `users` and `comments` refer to `posts`, so reading down the list is reading
// the relationships between them.
var datasetSpecs = []struct {
	name    string
	title   string
	summary string
}{
	{"users", "Users", "Eight people with a name, a username, an email address, a role and an active flag."},
	{"posts", "Posts", "Ten articles, each with a userId pointing into users, tags and a published flag."},
	{"comments", "Comments", "Twelve comments, each with a postId pointing into posts and an approved flag."},
	{"todos", "Todos", "Fifteen tasks with a userId, a due date and a completed flag."},
}

// builtinDatasets is parsed once, at package initialisation.
//
// A failure here is a broken file inside the binary rather than anything a
// request did, so it stops the process at the earliest possible moment instead
// of turning into a 500 for whoever first ticks a checkbox. TestBuiltinDatasets
// is what makes sure `go test` finds it before a deployment does.
var builtinDatasets = mustLoadDatasets()

// Datasets returns the built-in datasets, in the order they are offered.
//
// The slice is the package's own; callers read it and do not write to it. It is
// returned rather than exported as a variable so that the seeds cannot be
// rewritten in place by a caller holding the same backing array.
func Datasets() []Dataset { return slices.Clone(builtinDatasets) }

// DatasetNames are the names Datasets are addressed by, which is what a form
// submits and what selectDatasets resolves.
func DatasetNames() []string {
	names := make([]string, len(builtinDatasets))
	for i, d := range builtinDatasets {
		names[i] = d.Name
	}
	return names
}

func mustLoadDatasets() []Dataset {
	datasets := make([]Dataset, len(datasetSpecs))
	for i, spec := range datasetSpecs {
		seed, err := datasetFS.ReadFile("datasets/" + spec.name + ".json")
		if err != nil {
			panic(fmt.Sprintf("core: read the embedded %s dataset: %v", spec.name, err))
		}

		// Decoded and re-encoded rather than stored as it was written: what goes
		// into the seed column is what the editor will show, and the compact
		// form is what jsonb would return anyway. Decoding also proves the file
		// is the array of objects a seed has to be.
		var documents []map[string]json.RawMessage
		if err := json.Unmarshal(seed, &documents); err != nil {
			panic(fmt.Sprintf("core: the embedded %s dataset is not an array of objects: %v", spec.name, err))
		}
		compact, err := json.Marshal(documents)
		if err != nil {
			panic(fmt.Sprintf("core: re-encode the embedded %s dataset: %v", spec.name, err))
		}

		datasets[i] = Dataset{
			Name:      spec.name,
			Title:     spec.title,
			Summary:   spec.summary,
			Seed:      compact,
			Documents: len(documents),
		}
	}
	return datasets
}

// selectDatasets resolves the names a form submitted to the datasets they name,
// keeping the order the datasets are defined in rather than the order they
// arrived in, and ignoring a name repeated twice.
//
// An unrecognised name is a field error rather than something to skip: the only
// way to send one is to have edited the form, and quietly creating a project
// without the dataset that was asked for would look as though it had worked.
func selectDatasets(fe *FieldErrors, names []string) []Dataset {
	if len(names) == 0 {
		return nil
	}

	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		name = NormalizeCollectionName(name)
		if !slices.ContainsFunc(builtinDatasets, func(d Dataset) bool { return d.Name == name }) {
			fe.Add("datasets", fmt.Sprintf("There is no built-in dataset called %q.", name))
			return nil
		}
		wanted[name] = true
	}

	var chosen []Dataset
	for _, d := range builtinDatasets {
		if wanted[d.Name] {
			chosen = append(chosen, d)
		}
	}
	return chosen
}

// installDataset creates the dataset's collection, applies its seed and adds
// the collection endpoint that serves it, inside a transaction the caller owns.
//
// It runs in the caller's transaction rather than opening its own so that a
// project created with three datasets is created with all three or with none.
// Half a project is not something anybody asked for, and the failure that
// produces one — the database refusing a statement — is exactly when the user
// is least able to work out what is missing.
func installDataset(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, ownerID, projectID uuid.UUID, d Dataset) error {
	// The insert selects the project row to check ownership; inside this
	// transaction it sees the project the caller has just inserted.
	row, err := q.CreateCollection(ctx, dbgen.CreateCollectionParams{
		OwnerID:    fromUUID(ownerID),
		ProjectID:  fromUUID(projectID),
		Name:       d.Name,
		Seed:       d.Seed,
		IDField:    defaultIDField,
		IDStrategy: IDSerial,
	})
	if err != nil {
		return fmt.Errorf("create the %s collection: %w", d.Name, err)
	}

	if _, err := applySeed(ctx, tx, q, toUUID(row.ID), row.Seed, row.IDField, row.IDStrategy); err != nil {
		return fmt.Errorf("seed the %s collection: %w", d.Name, err)
	}

	// Stored under the wildcard verb, like every collection endpoint: it is not
	// a route anyone sends, it is the row the six routes are expanded from.
	if _, err := q.CreateCollectionEndpoint(ctx, dbgen.CreateCollectionEndpointParams{
		OwnerID:         fromUUID(ownerID),
		ProjectID:       fromUUID(projectID),
		Method:          MethodAny,
		PathPattern:     d.Path(),
		IsEnabled:       true,
		DelayMs:         0,
		CollectionID:    row.ID,
		ResponseHeaders: []byte(`{}`),
	}); err != nil {
		return fmt.Errorf("create the endpoint serving %s: %w", d.Name, err)
	}
	return nil
}
