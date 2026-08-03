# Session notes 04-1 — M3 stateful collections

Date: 2026-08-03
Feature: 04 — M3 stateful collections
Branch: `feature/04-collections`
Outcome: the milestone's "done when" is met — `POST` a record, `GET` it back, filter for it,
delete it, reset, and it is gone. Verified against the compose stack by curl as well as in tests.

---

## Starting point

M2 left static mocks and the route matcher. The `collections` and `documents` tables and their
constraints were already in migration `00001` from feature 00, and `endpoints.kind` /
`endpoints.collection_id` with them. One thing was missing from the schema, and it was found while
writing the listing: nothing on `documents` gives a stable order.

## What was built

| Path | Role |
|---|---|
| `migrations/00002_document_order.sql` | `documents.seq`, and the listing index moved onto it |
| `internal/core/collection.go` | `Collection`, `CollectionInput`, store CRUD, seed application, `ResetCollection` |
| `internal/core/document.go` | `ListQuery` parsing, the dynamic listing statement, document CRUD |
| `internal/core/queries/collections.sql` | Collection CRUD, identifier allocation, the reset statements |
| `internal/core/queries/endpoints.sql` | A second create and a second update, for `kind = 'collection'` |
| `internal/core/validate.go` | Collection name, id field, id strategy, seed and collection-path rules |
| `internal/mock/mock.go` | `Op`, and `expand` — one endpoint row into six routes |
| `internal/web/documents.go` | The six operations over HTTP: bodies, `Location`, `X-Total-Count`, the error map |
| `internal/web/collections.go` | Collection CRUD handlers and the form |
| `internal/web/api.go` | `POST /api/v1/projects/{slug}/collections/{name}/reset` |
| `internal/web/templates/pages/collection_form.html` | The collection form, seed edited in CodeMirror |
| `internal/web/static/js/forms.js` | Shows one half of the endpoint form and hides the other |

No new Go dependencies, no new third-party code.

## Decisions made while building

The ones that changed `DESIGN.md` are marked; §5 gained a list and a new §5.1, and §12.1 was
answered.

- **A new migration, for one column** *(DESIGN.md §5)*. `documents.seq`, generated always as
  identity. Neither existing column can order a listing: `created_at` is identical across a whole
  seed, because it is one statement and `now()` is fixed for the transaction, and `id` is a random
  uuid. Without a stable order, paging through equal values can return a document twice or not at
  all. It is also the tie-break under `_sort`. The superseded `documents_listing_idx` was dropped
  in the same migration.
- **The expansion lives in `BuildTable`, not in the trie** *(DESIGN.md §5)*. The trie takes routes
  one at a time and knows nothing about collections, which is what keeps every M2 rule working
  over them unchanged: `/users/me` defined statically still beats the `/users/{id}` the expansion
  added, and a verb nothing claims still answers 405 listing the verbs that are claimed.
- **The endpoint row is stored under the wildcard verb** *(DESIGN.md §5)*. It is not a route
  anyone can send; it is the row six routes come from. It is also what makes the unique index on
  (project, method, path) refuse a second collection rooted at the same place. Each expanded route
  carries its own method and path, so a suggestion list never names `* /users`.
- **Two create statements and two update statements, not one of each with nullable parameters.**
  `endpoints_kind_fields` requires a status code and no collection for `static` and the reverse for
  `collection`, so one statement would be one the constraint can refuse depending on what was
  passed — and the failure would arrive as a check violation rather than as a message beside a
  field. Both updates write *every* kind-specific column, so changing an endpoint's kind leaves no
  remnant of what it was.
- **A collection path may not itself declare `{id}`**, because the expansion appends it. It *may*
  declare other parameters — `/tenants/{tenant}/users` is a legitimate root — and they are matched
  and collected but name nothing the collection reads. That is the shared-state answer below.
- **Shared state, as `DESIGN.md` §12.1 suggested** *(§12.1 updated)*. State is per collection, not
  per parameter value. Isolation is still the nullable scope column plus a client-supplied header,
  and it is still additive.
- **Filters are containment tests** *(DESIGN.md §5)*. `body @> '{"status":"active"}'` is what the
  `jsonb_path_ops` GIN index can answer; `body ->> 'status' = …` would be a sequential scan of the
  collection. A query string has no types, so a value that also reads as a JSON scalar is matched
  both ways — two containment tests ORed, both index-backed — and `?id=1` finds `{"id":1}` and
  `{"id":"1"}` alike. `?code=007` is not a JSON number and matches only the string, which is what
  a padded product code should do.
- **An unknown `_`-prefixed parameter is a 400** *(DESIGN.md §5)*. The underscore namespace is the
  server's, so `?_limits=5` is a typo; answering it with the first hundred documents would look as
  though the parameter had worked. Anything *without* an underscore is a field filter, so a
  collection with a field called `page` is still filterable on it.
- **A default limit of 100 and a ceiling of 1000** *(DESIGN.md §5)*, with `X-Total-Count` saying
  what was left out. An unlimited listing is a promise that gets harder to keep as a fixture grows.
  When a page falls past the end there are no rows and so no window function to read the total
  from, and a second statement asks for it — worth one extra query in the one case it happens,
  because a client paging through wants to be told it has run off the end.
- **The sort key is a bind parameter, not string interpolation.** `order by body -> $n` — the only
  reason the dynamic statement is safe to assemble by hand.
- **The identifier is the server's** *(DESIGN.md §5)*. A `POST` supplying its own has it
  overwritten; two clients posting one fixture would otherwise collide. A `PUT` or `PATCH` cannot
  rename the document it addressed — that would be a move, not a replace. A *seed* may state its
  own ids, which is how a fixture gets to say `/users/1`, and allocation steps over the ones it
  named. Two passes over the seed for exactly that reason: allocation has to see every stated id
  before it hands out its first.
- **The counter is advanced in the statement that reads it** *(DESIGN.md §5)*, returning
  `next_serial - 1` because `returning` sees the new row. Twelve concurrent creates in the
  integration suite get twelve distinct ids. A create that still collides — a seed edited after a
  reset can leave an id behind — retries up to eight times, each attempt stepping the counter on.
- **A whole-number id goes into the document as a number, a uuid as a string** *(DESIGN.md §5)*.
  `idJSON` round-trips through `FormatInt` before emitting a bare number, because `"007"` parses as
  7 but is not a JSON number and would produce a document Postgres refuses.
- **`PATCH` is `||`, shallow** *(DESIGN.md §5)*. A nested object in the request replaces the one in
  the document. A deep merge would leave no way to remove a nested field, and `PUT` is there for
  callers who want to say what the whole document is.
- **Saving a collection does not apply its seed; creating one does** *(DESIGN.md §5)*. Editing the
  seed prepares what the next reset restores; discarding the documents somebody is working with as
  a side effect of saving would be a surprise. Creating is different: a collection that needs a
  reset before it answers is a step nobody would guess at.
- **Reset is one transaction** *(DESIGN.md §5)*, with the collection row locked. A client reading
  the collection sees the old contents or the new ones, never an empty collection halfway through
  being refilled — which matters, because reset is what a test suite calls between runs and the run
  after it starts immediately.
- **The reset route is session-authenticated and CSRF-guarded** *(DESIGN.md §5.1 added)*. This is
  the one honest gap in the milestone. `PLAN.md` asks for the route and for test suites to be able
  to call it; the route exists at exactly the documented URL and the UI button uses it, but a shell
  script cannot, because exempting a cookie-authenticated mutating route from CSRF is the hole the
  guard exists to close. A bearer token is not a cookie and needs no exemption, so **M6 makes it
  scriptable at the same URL**. Building bearer auth now would be building M6 inside M3.
- **A CSRF refusal under `/api/v1/` answers JSON**, not the HTML page the UI gets. The caller on
  that side has been parsing JSON all along.
- **Mock request bodies are capped at 1 MiB**, matching the cap on a stored response body. Not
  strictly M3's, but a write path with no cap lets one request ask the process for as much memory
  as the client cares to send, and this milestone is the one that added write paths.
- **Document statements are hand-written pgx; collection statements are sqlc.** The number of
  filters and the sort key come from the request, and sqlc generates static SQL. The split is
  stated at the top of `collections.sql` so nobody looks for the missing half.
- **The endpoint form shows one kind at a time via 30 lines of JavaScript**, and works without it:
  both halves render, both submit, and the server reads only the fields belonging to the chosen
  kind. Hidden fields are `disabled` rather than merely hidden, because a hidden `required` input
  blocks submission with nothing on screen to point at.

## Verification

Everything below was run, not reasoned about.

- `make test` — race detector on. `ParseListQuery` including every refusal and the reserved-name
  rule; `containmentDocs` across strings, integers, negatives, decimals, booleans, null and a
  padded number; `idJSON`; `checkObject`; `seedDocuments` across stated ids, allocation around
  them, a non-default id field, the uuid strategy, an id beyond float precision, and four
  refusals; collection input validation; the matcher's expansion into six routes with the right
  `Op` and parameter, 405 `Allow` on both shapes, `HEAD`→`GET`, a static literal still outranking
  the expanded parameter route, a collection under a parameter, a collection at the project root,
  and all six routes appearing in the 404 suggestions.
- `make test` also covers the handlers: the whole round trip over HTTP, no cookie and no CSRF
  token on a mock write, `PUT` versus `PATCH`, a write that tries to rename a document, four
  missing-document cases, six not-an-object cases, a 1 MiB body answering 413, seven paging and
  filtering cases with `X-Total-Count`, an empty list answering `[]` rather than `null`, a refused
  listing query, 405 with `Allow`, endpoint headers reaching a collection response, and a
  collection whose row has gone answering 404 rather than 500.
- `make test-integration` — real Postgres 17 via testcontainers. Seed applied on creation and in
  seed order; allocation past the seed; a client-supplied id overwritten; the uuid strategy;
  **twelve concurrent creates getting twelve distinct ids**; replace and patch; patch proved
  shallow against a nested object; four missing-document paths; ten filter cases against the GIN
  index; nine sort and page cases including a page past the end and a sort field no document has;
  **twenty documents with one sort key paged through four times, each returned exactly once**;
  reset restoring a deleted, an edited and discarding a created document, and putting the counter
  back; saving a collection *not* applying the seed; cross-owner isolation at six entry points;
  name uniqueness per project; the cascade from collection to endpoint; an endpoint changing kind
  in both directions; a collection endpoint refused another project's collection; and nine
  deliberately invalid inserts run **past** the Go validation to prove the constraints agree
  (`CONTEXT.md` §7), including that `seq` cannot be supplied by hand.
- Migration `00002` run up and down against real Postgres, and the up path asserted directly:
  `seq` is `is_identity = YES`, `documents_order_idx` exists, `documents_listing_idx` is gone and
  `documents_body_gin_idx` survived. `TestMigrationDownRemovesEverything` covers the down path, and
  `TestMigrateIsIdempotent` now counts the embedded files rather than a hard-coded version.
- `TestTheM3Milestone` is the "done when" end to end: define a collection and an endpoint through
  the real forms with real CSRF tokens, `POST` and get `201` with `Location: /m/checkout/users/3`,
  `GET` it back, filter and sort it with `X-Total-Count`, `DELETE` it and find 404, `PATCH` a
  seeded record, reset over `/api/v1/…`, and find the fixture back and the counter with it.
- `make lint` — clean. `make assets` rerun; the stylesheet is committed.
- `make up`, then the whole thing by curl against the container, on a database **already holding
  M2 data** — so migration `00002` was applied over a populated schema, not only a fresh one. Seed
  listing, `POST` → 201 with `Location`, `GET` the new record, `?role=admin&_sort=name` with
  `X-Total-Count: 2`, `_limit=1&_page=2`, `PATCH`, `PUT`, `DELETE` → 204, the deleted id → 404,
  `DELETE` on the root → 405 `Allow: GET, HEAD, POST`, `?_limits=5` → 400 naming it, a non-object
  body → 400, reset as JSON, reset as the HTMX button → 204 `HX-Redirect`, an unknown collection →
  404, no session at all → 400 JSON. The next id after a reset was 3 again.
- The application log across all of it: **no `ERROR` lines**.

## Deliberately not done

- **`forms.js` was not driven in a real browser.** The form was exercised without JavaScript —
  every curl run above submitted a collection endpoint with the static half's fields still
  present and an invalid `status_code`, and it was accepted — so the fallback is proven and the
  enhancement is not. It is thirty lines and reversible.
- **No CORS defaults.** An endpoint can set `Access-Control-Allow-Origin` in its response headers
  and it now applies to collection responses too, but there is still no per-project setting. M7,
  as recorded at the end of M2.
- **No request logging**, so none of this appears in an inspector yet. M4.
- **No relationship expansion** — no `_embed` or `_expand`. `?_embed=posts` is refused by name
  rather than ignored, which is at least a clear answer.
- **No `POST` of a whole array**, and no bulk delete. One document per request.
- **No rate limiting on collection writes.** A mock server that stores what it is sent is more
  worth rate limiting than one that does not; still M7.

## Notes for whoever picks this up

- `internal/mock` still speaks no HTTP, and must not start. `expand` returns routes; `Op` says
  what a matched route does; `internal/web/documents.go` is where the wire is.
- **Six call sites now reload the route table**: the three project handlers, `endpointSaved`,
  the endpoint delete, and the collection delete and update. A seventh place that forgets will be
  stale for up to 30 seconds and then correct, which is the kind of bug that is hard to see.
- The document statements are the only hand-written SQL in `core`. If a filter or a sort is ever
  extended, the rule that keeps it safe is that **every value is a bind parameter**, including the
  sort key.
- `ResetCollection` takes a collection id and no owner, because mock traffic has no account. The
  ownership check is in the handler, through `CollectionByOwnerAndName`. Anything else that calls
  it has to do the same.
- The M6 work on this route is small and known: accept `Authorization: Bearer`, exempt bearer-
  authenticated requests from CSRF (cookie-authenticated ones must stay guarded), and the route
  becomes scriptable without moving.

## State of the repository

Branch `feature/04-collections`. `internal/core/dbgen/` is regenerated output and is committed;
`internal/web/static/css/app.css` is regenerated by `make assets` and committed. Compose stack left
running with two accounts and two projects: `checkout` from the M2 session, and `m3demo`
(`m3@example.com`) with a `users` collection and a collection endpoint at `/users`.

## Next step

Feature 05 / milestone M4: the request log and inspector — recording middleware around the mock
handler, a buffered channel drained by a batching writer **that reports drops rather than
discarding silently**, a per-project list live-tailing over SSE, a detail view, and monthly
partition creation with retention by detaching. `DESIGN.md` §12.3 — the retention window and the
body size cap — is the open question that lands with it.
