# Session notes 06-1 — M5 public datasets and the demo project

Date: 2026-08-07
Feature: 06 — M5 public datasets and the demo project
Branch: `feature/06-datasets`
Outcome: the milestone's "done when" is met — `curl /m/demo/users` works from a logged-out
browser. Verified by `TestTheM5Milestone` against real Postgres with a client that holds no
cookies at all, and by hand against the compose stack.

---

## Starting point

M4 left the request log. `PLAN.md` asked for four things: seed templates, projects creatable
pre-seeded from one, a demo project provisioned at startup, and a scheduled reset of it.
**No migration was needed**: `projects.is_demo` has been in `00001` since feature 00, and
`demo` has been in the reserved-slug list since M1, held for exactly this.

## What was built

| Path | Role |
|---|---|
| `internal/core/datasets/*.json` | The four seeds — `users`, `posts`, `comments`, `todos` — embedded |
| `internal/core/dataset.go` | `Dataset`, the loader, `selectDatasets`, `installDataset` |
| `internal/core/demo.go` | `EnsureDemoProject`, `ResetDemoProjects`, the reset loop, the demo account |
| `internal/core/project.go` | `CreateProject` now takes dataset names and runs in a transaction |
| `internal/core/collection.go` | `applySeed` extracted from `ResetCollection` and shared |
| `internal/web/projects.go` | The checkboxes: what the form offers, what it sends, what comes back |
| `internal/web/render.go` | `demoOffer` on every page, so an anonymous visitor is told the demo exists |
| `internal/web/templates/pages/project_form.html` | The dataset checkboxes, create page only |
| `internal/web/templates/pages/login.html` | The demo panel, with commands to paste |

Two new configuration variables — `RESTEST_DEMO_ENABLED`, `RESTEST_DEMO_RESET_INTERVAL` —
passed through by `docker-compose.yml` and documented in `.env.example`. Two new queries
(`ProjectBySlug`, `DemoCollections`) and one changed one (`CreateProject` takes `is_demo`).
No new Go dependencies, no new third-party code, no migration.

## Decisions made while building

The ones that changed `DESIGN.md` are marked. §6 gained a new §6.1, and §12.2 is answered and
struck through.

- **A dataset is a collection and the endpoint serving it, and nothing else** *(DESIGN.md
  §6.1)*. No static endpoints, no headers, no delay: those are what a user is here to decide.
- **A dataset is stored as the collection's seed**, which makes it the same object a user
  could have typed. Everything downstream — reset, edit, delete, the six expanded routes —
  then works with no case for "it came from a template". The JSON files are decoded and
  re-encoded at load, so a malformed one is a panic at process start rather than a 500 for
  whoever first ticks a checkbox, and `TestBuiltinDatasetsAreValidInput` runs them through the
  same validation a typed seed goes through.
- **They cross-reference each other** — `posts.userId`, `comments.postId`, `todos.userId` —
  so two of them together are worth writing a client against, and each still stands alone.
- **The project and its datasets are one transaction** *(DESIGN.md §6.1)*. `CreateProject`
  gained a `datasets []string` parameter rather than a second method beside it, because "create
  a project, optionally pre-seeded" is one operation and two methods doing nearly the same
  thing drift. `applySeed` was pulled out of `ResetCollection` so that installing a dataset and
  resetting one cannot come to hold different ideas of what a seed means.
- **An unknown dataset name is a field error, not something to skip.** Only an edited form can
  send one, and creating the project without the dataset it asked for would look as though it
  had worked. `TestProjectWithAnUnknownDatasetCreatesNothing` checks that the rejection leaves
  no project behind, which is the transaction doing its job.
- **Datasets are offered at creation and not on the edit page** *(DESIGN.md §6.1)*. Adding one
  to a project that exists is creating a collection, which has its own form; a checkbox there
  would imply a seed could be applied over data somebody is working with.
- **The demo is an ordinary project** *(DESIGN.md §6.1)* — a row with `is_demo`, real
  collections, real documents, real endpoints, and its traffic in the request log like anyone
  else's. Nothing about serving it takes a different path through the matcher or the handlers.
- **Its owner is an account nobody can log in as** *(DESIGN.md §6.1)*.
  `demo@restest.invalid`, registered with 32 random bytes that are hashed and then dropped.
  `.invalid` is reserved by RFC 6761, so the address is nobody's. A nullable owner was the
  alternative and would have put a null check on every query that reads a project. That the
  demo is in no account's project list and no account can open it then falls out of the
  existing owner-scoped queries rather than needing a rule of its own — `TestTheM5Milestone`
  registers an account and checks both.
- **Provisioning is idempotent and safe from two instances at once.** Look up, then create;
  a unique violation on the slug means another instance won the race, so the loser reads back
  what the winner made. The same for the account.
- **A project holding the demo slug that is not flagged as the demo stops provisioning**
  (`ErrDemoSlugTaken`). It is somebody's project — put there by hand, or by an instance older
  than the reserved list — and adopting or overwriting it would be worse than having no demo.
  The failure is logged and **not fatal**: refusing to serve anybody's mocks because the demo
  could not be created is the wrong trade.
- **§12.2 answered: anonymous writes are persisted, and the reset is hourly** *(DESIGN.md
  §6.1)*, configurable and floored at one minute. Persisting them is the point — a demo whose
  `POST` did not come back on the next `GET` would demonstrate the opposite of what restest
  does — and the reset is what makes that affordable. It also runs once at startup, so a
  restart is a way to put the demo back.
- **The route table is reloaded after provisioning.** It is built before the listener opens
  and the demo is created after it, so without the reload `/m/demo/` would answer "no such
  project" until the 30-second refresh caught up.
- **The login page offers the demo**, with commands to paste rather than an invitation. It is
  the first page an anonymous visitor sees, and a demo nobody is told about is a demo nobody
  uses. The panel is off when `RESTEST_DEMO_ENABLED` is.

## Verification

- `make test` — passes.
- `make test-integration` — passes, 174s, race detector on. That includes `TestTheM5Milestone`
  end to end: provision the demo the way `main.go` does, read every dataset from a client whose
  cookie jar is empty at the end of it, `POST` a user and get it back from its `Location`,
  `DELETE` a todo, reset, and find the visitor's document gone, the deleted one back, and the
  next `POST` handed the same id the first one got — the counter went back with the documents.
- `make lint` — `go vet` plus golangci-lint, 0 issues.
- 12 new test functions across `core`, `web` and `integration`.
- And against the compose stack, not only in tests. `make up`, then: `curl /m/demo/users`
  returns the eight seeded users, `/m/demo/users/1` is Ada, `?userId=1` filters posts through
  the GIN index, `X-Total-Count` on comments is 12, `POST /m/demo/todos` returns 201 with
  `Location: /m/demo/todos/16`, and `PUT /m/demo/users` is the 405 it deserves. The startup log
  says `demo project ready` and then `demo project reset` with four collections. A
  `docker compose restart app` against the existing volume created **no** second project,
  account or collection, and the todo the visitor added was gone — the startup reset, on real
  data. The demo's own traffic is in `exchanges` like anyone else's.

## Deliberately not done

- **No way to add a dataset to a project that already exists.** The seed of a built-in dataset
  is not offered from the collection form; it is a create-time choice.
- **No page for the demo project.** It is offered on the login page and read with `curl`.
  Giving it a browsable view means deciding who may see it, and it belongs to no account that
  can log in.
- **No per-dataset reset schedule, and no reset endpoint for the demo.** One interval for
  every demo collection, on a timer. `POST /api/v1/…/reset` needs an account and is M6's to
  make scriptable.
- **No isolation between visitors.** Two people writing to `/m/demo/users` at once see each
  other's documents. That is `DESIGN.md` §12.1's scope column, still additive and still not
  built.

## Notes for whoever picks this up

- **`CreateProject` takes `datasets []string`** — the last parameter. Every caller passes
  `nil` unless it means to pre-seed, and `web.Store` carries the same signature.
- **`createProject` (lower case) is the one that skips validation**, and it is what the demo
  uses: `demo` is a reserved slug the public method refuses, and `is_demo` is a flag no form
  can set. Anything else creating a project should go through the exported one.
- **`installDataset` runs in the caller's transaction** and uses the owner-scoped
  `CreateCollection` insert, which sees the project row the same transaction just inserted.
- **The dataset files are parsed at package initialisation** (`builtinDatasets`). A broken file
  panics, which is what `go test ./internal/core` is there to find first.
- **`ResetDemoProjects` finds its work through `is_demo`**, not through the slug, so a second
  demo project flagged by hand would be reset too. Nothing creates one.
- The demo's request log is recorded but unreadable in the UI: the inspector is reached
  through `/projects/{slug}/`, which is owner-scoped, and no one owns the demo. If that log is
  ever wanted, it is a decision about who may read it, not a missing route.

## State of the repository

Branch `feature/06-datasets`, merged to master by the finish sequence. `internal/core/dbgen/`
was regenerated by `make sqlc` and is committed, as generated output is here. `app.css` was
regenerated by `make assets` for the new classes in the two templates. No migration was added.

## Next step

Feature 07 / milestone M6: API tokens and the management API — token CRUD with the plaintext
shown once and a SHA-256 hash stored, `Authorization: Bearer` middleware updating
`last_used_at`, and `/api/v1/` covering projects, endpoints, collections and reset so a CI job
can configure mocks without the UI. The reset route already exists at its final URL and is
session-authenticated only (`DESIGN.md` §5.1); M6 is what makes it scriptable, at that URL
unchanged. The request log joins the API in the same milestone.
