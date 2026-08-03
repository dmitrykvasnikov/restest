# Session notes 02-1 — M1 accounts and projects

Date: 2026-08-03
Feature: 02 — M1 accounts and projects
Branch: `feature/02-accounts-projects`
Outcome: the milestone's "done when" is met — a new user can register, log in and create a
project. Verified in a browser-shaped way against the compose stack, not only in tests.

---

## Starting point

Feature 01 left the skeleton: config, logging, pgx pool, migrations at startup, health probes,
Docker. The schema from feature 00 already had `users`, `sessions`, `projects` and their
constraints, so no migration was needed this session and none was written.

## What was built

| Path | Role |
|---|---|
| `internal/core/password.go` | Argon2id hashing, PHC-encoded |
| `internal/core/validate.go` | Email, password, slug and name rules; reserved slugs |
| `internal/core/errors.go` | Sentinel errors and `FieldErrors` |
| `internal/core/user.go`, `project.go` | Domain types and the store methods over them |
| `internal/core/queries/users.sql`, `projects.sql` | The first real sqlc input |
| `internal/web/session.go` | scs over `pgxstore`, cookie policy, flash messages, `requireUser` |
| `internal/web/csrf.go` | nosurf wiring and the rejection page |
| `internal/web/render.go` | Template set, page data, `serverError` |
| `internal/web/auth.go`, `projects.go` | Handlers for the whole M1 journey |
| `internal/web/notfound.go`, `static.go`, `htmx.go` | Catch-all route, embedded assets, redirects |
| `internal/web/templates/`, `static/` | Layout, five pages, partials; Tailwind output and HTMX |

New direct dependencies: `alexedwards/scs/v2` + `scs/pgxstore`, `justinas/nosurf`,
`google/uuid`, and `golang.org/x/crypto` promoted from indirect. `pgxstore` has no tagged
release, so it resolves to a pseudo-version; that was accepted rather than hand-rolling a
session store, because `DESIGN.md` §9 names the scs Postgres store specifically.

## Decisions made while building

Not settled in `DESIGN.md` beforehand. The two that changed `DESIGN.md` are marked.

- **`RESTEST_BASE_URL` is one setting doing two jobs** *(DESIGN.md §8 updated)*. It is the
  address users actually reach the instance on, shown in the UI as the root of every mock URL,
  and its scheme decides whether cookies are marked `Secure`. A browser never returns a
  `Secure` cookie over plain HTTP, so a hard-coded `true` breaks local development and a
  hard-coded `false` ships that mistake to production. It also feeds nosurf's `SetIsTLSFunc`,
  which is how the `Origin`/`Referer` check stays correct behind a TLS-terminating proxy.
- **The session manager owns the cookie policy, and the CSRF cookie follows it.**
  `web.Options` has no `SecureCookies` field: the server reads `Sessions.Cookie.Secure`. Two
  settings for one question can disagree.
- **Templates and assets live in `internal/web/`**, not a top-level `web/` as the original
  layout sketch had it *(DESIGN.md §11 updated)*. One package owns the browser-facing side and
  its `go:embed` paths stay inside itself. The sketch was already self-contradictory — its
  comment on `internal/web/` said "templates, static assets".
- **Asset URLs are stamped with a hash of the embedded assets, not the build revision.** A
  working tree rebuilt during development keeps the same revision while its stylesheet changes,
  which is exactly when a stale cache costs the most. Hashing content answers the question
  actually being asked, and makes `Cache-Control: immutable` honest.
- **Argon2id at m=19 MiB, t=2, p=1**, per the OWASP cheat sheet. The parameters travel inside
  each hash, so raising them later re-hashes nothing: old passwords keep verifying with the
  parameters they were made with. A malformed hash returns `ErrInvalidHash`, deliberately
  distinct from a wrong password — one is our corruption, the other is the user's typing, and
  reporting the first as the second hides a real fault behind a confused user.
- **An unknown address costs the same time as a known one.** `Authenticate` verifies against a
  decoy hash when the lookup misses, computed at most once per process. Without it, a fast "no
  such user" and a slow "wrong password" are an enumeration oracle by another name.
- **Validation lives in `core`, not in handlers**, returning `FieldErrors` keyed by form field.
  The management API in M6 and the phase 2 runner then enforce the same rules as the browser
  forms rather than a second copy of them.
- **Every project query is scoped by `owner_id` in the SQL**, not by a check the caller might
  forget. A project belonging to somebody else returns `ErrNotFound`, the same answer as one
  that never existed.
- **Slugs are normalised before validation**: trimmed and lower-cased. `"  MyAPI "` becomes
  `myapi` rather than being rejected. Whitespace and case are a typing accident, not intent.
  Everything else the database constraint refuses, Go refuses first.
- **Renaming a slug is allowed**, and the flash says the old mock URL will stop matching. This
  is a development tool; a slug typed wrong once should not be permanent.
- **A catch-all `/` route renders the application's own 404**, because net/http's plain-text
  one is a poor answer for a browser. The catch-all swallows the 405 the router would otherwise
  produce, so `handleNotFound` asks the router directly which other methods that path answers
  and sets `Allow` itself. Requests that match no route are also exempted from the CSRF check —
  a rejected token is a worse answer than the 404 or 405 the request actually earned.
- **Probes and static assets skip the session and CSRF middleware**, decided on the matched
  route pattern rather than the path. A `POST /healthz` matches no route and must reach the
  session chain, because the page that answers it renders like any other.
- **HTMX is used narrowly**: the delete buttons are real forms that work without JavaScript,
  with `hx-post` and `hx-confirm` layered on top. The handler answers HTMX with `HX-Redirect`
  and a 204 instead of a 303, because XHR follows a 303 transparently and would swap the
  resulting page into a fragment with the address bar left behind. No Alpine.js yet — nothing
  in M1 needed it.
- **Tailwind runs from the pinned standalone binary** (`make assets`, v4.3.3, configured in CSS
  rather than a JS config file). The binary lands in gitignored `bin/`; the generated
  stylesheet is committed, so `go build` needs no network. HTMX 2.0.10 is vendored by
  `make vendor-htmx`, which is only run when changing versions. npm never enters the toolchain.
- **`web.Store` is an interface, not `*core.Store`.** Handler tests then run without Docker,
  while `internal/integration` exercises the real implementation against real Postgres.

## Verification

Everything below was run, not reasoned about.

- `make test` — unit tests, race detector on. Argon2id round trip, per-call salt, verification
  against a hash carrying *older* parameters, and eight shapes of malformed hash. Every
  validation rule including all six reserved slugs. The web tests drive a real `httptest`
  server through a cookie jar: register, log in, log out, session renewal on login, cookie
  attributes in both the plain and TLS configurations, a stale session for a deleted account,
  the post-login return to the page originally asked for, project CRUD, the HTMX delete, CSRF
  rejection with and without a foreign `Origin`, 404 and 405 pages, and asset versioning.
- `make test-integration` — race detector on, real Postgres 17 via testcontainers. The store
  against the real schema: citext case-insensitivity, the duplicate-address and duplicate-slug
  paths through the actual unique indexes, cross-owner isolation for read, update and delete,
  and the `on delete cascade` from users to projects. `TestSchemaConstraintsMatchTheGoRules`
  inserts deliberately invalid slugs **directly**, past the Go validation, to prove the two
  rules agree in both directions (`CONTEXT.md` §7). `TestTheMilestone` is the "done when",
  end to end over HTTP.
- `make lint` — clean. `bodyclose` is excluded for `_test.go`: the test clients close every
  response through one helper the linter cannot see through.
- `make up`, then the whole journey by curl against the container: register → 303 to
  `/projects`; create project → 303 to `/projects/checkout` with its mock URL on the page;
  reserved slug → 422 saying "reserved"; duplicate slug → 422 saying "taken"; HTMX delete →
  204 with `HX-Redirect: /projects`; log out → 303 and the private page then redirects; log
  back in with a differently-cased address → 303 to `/projects`; wrong password and unknown
  address → 422 with the *same* message. `POST /healthz` → 405, `/nope` → 404, both assets 200.
- In the database afterwards: one user whose `password_hash` starts
  `$argon2id$v=19$m=19456,t=2,p=1$`, one row in `sessions`, and no projects after the delete.
- The application log across all of it: no `ERROR` lines. Login costs 31–78 ms, which is the
  Argon2id work.

## Deliberately not done

- **No password change or reset.** `UpdatePassword` exists in the query file, unused, because
  it belongs with the account settings page rather than with a half-built reset flow. Reset
  needs email delivery, which is a decision nobody has made yet.
- **No account deletion in the UI.** The cascade is tested; the button is not written.
- **No rate limiting on the login form.** That is M7, alongside the rest of the hardening, and
  doing it now would mean doing it twice.
- **No CSP.** M7, where it has to be settled together with the vendored CodeMirror.
- **No Alpine.js.** Nothing in M1 needed it; adding it now would be vendoring a dependency for
  a hypothetical.

## Notes for whoever picks this up

- `pgxstore` resolves to a pseudo-version. If that becomes uncomfortable, the store is three
  methods over a table this project already owns.
- nosurf 1.2 requires `Origin`, `Referer` **or** `Sec-Fetch-Site` on every unsafe request, not
  only on TLS ones. Browsers send them; a test client or a `curl` script has to be told to. If
  a form ever starts failing for a user with a referrer-stripping extension, that is the cause.
- Two CSRF fields appear on a logged-in page — the logout form and the page's own form. A
  script scraping the token wants the right one.

## State of the repository

Branch `feature/02-accounts-projects`, **not committed** — `CONTEXT.md` §2 says commits happen
when the owner asks. `internal/core/dbgen/` is regenerated output and is meant to be committed.
`internal/web/static/css/app.css` and `static/js/htmx.min.js` are generated and vendored
respectively, and are also committed; `bin/tailwindcss` is not. Compose stack left running with
one registered account (`sam@example.com`) and no projects.

## Next step

Feature 03 / milestone M2: static mocks and the route matcher — endpoint CRUD with CodeMirror
for the response body, the in-memory radix trie behind an `RWMutex`, `/m/{slug}/…`, the 404
that lists nearby routes, and 405 on method fallthrough. `PLAN.md` calls the matcher the
trickiest code in the project and asks for its table-driven tests to be written alongside it,
not after.
