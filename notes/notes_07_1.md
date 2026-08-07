# Session notes 07-1 — M6 API tokens and the management API

Date: 2026-08-07
Feature: 07 — M6 API tokens and the management API
Branch: `feature/07-api-tokens`
Outcome: the milestone's "done when" is met — a shell script can create a project, define
endpoints and reset state. Verified by `TestTheM6Milestone` against real Postgres with a client
that holds no cookies at all, and by hand against the compose stack.

---

## Starting point

M5 left the demo project. `PLAN.md` asked for three things: token CRUD with the plaintext shown
once, a bearer middleware updating `last_used_at`, and `/api/v1/` covering projects, endpoints,
collections and reset. M4 had also carried forward that the request log was not reachable over
the API; that joined this milestone, because the milestone routes the rest of the API anyway.

**No migration was needed.** `api_tokens` has been in `00001` since feature 00, with the columns
this needed: `prefix`, `token_hash unique`, `last_used_at`, `expires_at`.

## What was built

| Path | Role |
|---|---|
| `internal/core/queries/tokens.sql` | Four statements; the authentication one updates and reads in a single CTE |
| `internal/core/token.go` | `APIToken`, `CreateAPIToken`, `APITokensByUser`, `DeleteAPIToken`, `AuthenticateAPIToken`, `HashToken` |
| `internal/core/validate.go` | `validateTokenName`, `validateTokenExpiry`, beside the other rules |
| `internal/web/tokens.go` | The `/tokens` page: list, create, revoke |
| `internal/web/templates/pages/tokens.html` | The page, including the once-only panel |
| `internal/web/apiauth.go` | `bearerToken`, `requireAPIUser`, `decodeJSON` and the API's error shapes |
| `internal/web/api.go` | Paths, the index, `findAPIProject`, `isAPIPath` |
| `internal/web/api_projects.go` | Projects: list, create, show, patch, delete |
| `internal/web/api_endpoints.go` | Endpoints: list, create, show, put, delete |
| `internal/web/api_collections.go` | Collections: list, create, show, patch, delete — and reset, moved here |
| `internal/web/api_log.go` | The request log, read-only |
| `internal/web/csrf.go` | The exemption for a bearer-authenticated API request |
| `internal/web/notfound.go` | 404 and 405 answer JSON under `/api/v1/` |

## Decisions

- **A bearer request is authenticated by the token or not at all** *(DESIGN.md §8.1)*. This is
  the whole of the security argument for exempting it from CSRF, and it is one `if` in
  `requireAPIUser`: when an `Authorization: Bearer` header is present, the session is never
  consulted. If it were, a third-party page could bypass the guard by adding a nonsense header.
  The CSRF exemption and the authentication use the same predicate — `bearerToken(r)` — so the
  two cannot drift into disagreeing about which requests are exempt.
  `TestABadBearerNeverFallsBackToTheSession` is the test that would catch it.
- **The token is `rst_` plus 32 random bytes in raw URL-safe base64.** The mark makes one
  recognisable in a shell history; the encoding survives a query string, a shell and an unquoted
  YAML value. Stored as SHA-256 with no salt and no KDF, because those exist to make a
  low-entropy secret expensive to guess and there is nothing cheap to guess here. The prefix
  kept in the clear is the mark plus eight characters: 48 bits out of 256, enough to tell two
  rows apart.
- **Expiry and the `last_used_at` update are one statement.** Two would leave a window in which
  a token revoked between them still answered, and it also means an expired token does not get
  its last use bumped by a request it did not authorise. The write happens on every
  authenticated call; this is the management API, where a CI job makes a handful of requests,
  not mock traffic.
- **No route mints a token.** A token that could mint tokens turns one leaked CI credential into
  a permanent foothold that revoking the leaked one does not close. `/tokens` is in the
  interface, and the API has no equivalent.
- **The plaintext crosses the redirect in the session**, popped by the page that shows it. Not
  in the URL, where it would land in a proxy log and the browser's history; not rendered
  directly from the POST, because that page cannot be reloaded. A refresh loses it, which is
  the honest version of "copy it now".
- **Addressing follows what a script already knows**: a project by slug, a collection by name,
  an endpoint by id. The first two are what appear in a mock URL and in the reset route; the
  third because an endpoint has no name and a verb-and-path pair in a URL would need escaping.
  An endpoint names its collection **by name** in both directions, and the id never appears.
- **`PATCH` for projects and collections, `PUT` for endpoints.** An endpoint's `kind` decides
  which of its other fields mean anything, so a partial update would have to invent an answer
  for "the kind changed and the body was not mentioned". Projects and collections have
  independent fields, so an absent one keeping its value is unambiguous.
- **Unknown JSON fields are refused**, matching the rule for unknown `_`-prefixed query
  parameters from M3: a misspelling that is silently dropped looks exactly like one that worked.
- **`GET /api/v1/` is an index**, naming the account and how it was proved. A script handed a
  secret should be able to check it with a call that changes nothing, and
  `TestEveryAdvertisedRouteExists` keeps the list it prints from drifting from the mux.
- **A refusal under `/api/v1/` is always JSON**, including the 404 and the 405 that never reach a
  handler. `errorBody` grew an optional `fields` member carrying the same per-field messages the
  forms show, so a rejected definition reads the same way from either side.
- **The reset handler moved to `api_collections.go` and gained nothing else.** Same URL, same
  code; `wantsHTML` now also answers false when a bearer token is present, because the browser
  button never has one.
- **A collection is re-read before it is answered with.** Creating one applies its seed *after*
  the row comes back, so the row a write hands over is already stale about `documents` and
  `next_serial`. Caught against the compose stack, not by a test: the stub agreed with itself.

## Fixed along the way

- **A seed of `null` was a 500.** `json.Unmarshal("null", &[]json.RawMessage)` succeeds and
  leaves the slice nil, so `validateSeed` accepted it and the `collections_seed_is_array` check
  constraint refused it — reachable from the seed editor as well as from the API. It is now a
  message beside the field, with a case in `TestCollectionInputValidation`.

## Verification

- `make test` — passes.
- `make test-integration` — passes, 186 s, race detector on. That includes `TestTheM6Milestone`
  end to end: register through the form, mint a token through the form, and then, from a client
  with no cookie jar at all, create a project, a collection and two endpoints over `/api/v1/`,
  `curl` the mock URLs they created, write to one, reset it and find the write gone and the seed
  back, read the request log through the API and find the posted body in it, check
  `last_used_at` was written, and delete the project — after which `/m/ci/` stops answering.
  Plus `TestATokenSeesOnlyItsOwnAccount`, and store-level tests for the hash in the row, expiry
  and revocation.
- `make lint` — `go vet` plus golangci-lint, 0 issues.
- 42 new test functions across `core`, `web` and `integration`.
- And against the compose stack, not only in tests. Registered, minted a token through the page,
  and then with `Authorization: Bearer` alone: the index named the account, a project was
  created, a collection seeded (`next_serial: 2`, `documents: 1` — the re-read above),
  a static and a collection endpoint defined, `GET /m/ci2/health` answered `{"ok":true}`,
  `POST /m/ci2/users` returned 201 with `Location: /m/ci2/users/2`, the reset put it back to one
  document and Ada came back, and the log listed six exchanges. Without a token, 401 with a
  `WWW-Authenticate: Bearer` challenge; with a bad one, the same 401; an unknown route, JSON
  404; the reset URL under `GET`, JSON 405 with `Allow: POST`. The tokens page then showed the
  prefix, the created date and the last use — a minute earlier, from those calls.

## Deliberately not done

- **No token scopes.** A token is exactly as powerful as the account that made it: no per-project
  scope, no read-only token. Scoping is additive — a column on `api_tokens` and a check in
  `requireAPIUser` — and it is a decision about what the scopes should be, not a missing route.
- **No token management over the API**, as above.
- **No pagination on the project, endpoint or collection listings.** They are bounded by what one
  account can create through a form, and the log — the one listing that is not — pages already.
- **No search or filters on the log**, over the API any more than in the browser. It is the same
  gap M4 carried forward.
- **No rate limiting on `/api/v1/`.** That is M7's, with the rest of the hardening.
- **No OpenAPI document for the API.** `GET /api/v1/` lists the routes and the README documents
  the bodies; a generated spec is a build step this project does not have.

## Notes for whoever picks this up

- **`requireAPIUser` is the API's `requireUser`**, and every `/api/v1/` route goes through it.
  Adding a route means registering it in `routes()` *and* adding it to `apiRoutes` in `api.go` —
  `TestEveryAdvertisedRouteExists` fails if the list names a route the mux does not have, but
  nothing fails if the mux has one the list does not.
- **`wantsHTML` decides the shape of the reset route's answer**, and `serverErrorFor` is the
  version of `serverError` for handlers that answer both. Any future dual-purpose route wants
  both.
- **`decodeJSON` disallows unknown fields**, so adding a field to a request struct is how a new
  input becomes acceptable — there is no leniency to fall back on.
- **`core.HashToken` is exported for the tests**, which hash a token to find its row. Nothing in
  the application calls it from outside `core`.
- **The `fakeTokens` stub in `internal/web` keeps only hashes**, like the real store, so a test
  cannot pass by comparing plaintext.
- **`api_*.go` is one file per resource**, matching `projects.go` / `endpoints.go` /
  `collections.go` / `log.go` on the interface side.

## State of the repository

Branch `feature/07-api-tokens`, merged to master by the finish sequence. `internal/core/dbgen/`
was regenerated by `make sqlc` and is committed, as generated output is here. `app.css` was
regenerated by `make assets` for the classes in the new template. No migration was added:
`api_tokens` was already in `00001`.

## Next step

Feature 08 / milestone M7: hardening. Per-IP and per-project rate limiting on mock traffic —
and now on `/api/v1/` too, which is the one credentialed surface and currently bounded by
nothing; request body size caps and server read/write timeouts; security headers and a strict
CSP that the vendored CodeMirror actually satisfies; a backup and restore procedure written
down and tested by restoring; and Prometheus metrics for request rate, match rate, latency and
the exchange write queue depth.
