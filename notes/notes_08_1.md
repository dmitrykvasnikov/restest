# Session notes 08-1 — M7 hardening

Date: 2026-08-07
Feature: 08 — M7 hardening
Branch: `feature/08-hardening`
Outcome: the milestone is met. The process refuses what it should refuse, says what it is doing,
and has been restored from a backup — not reasoned about, actually restored, with an account
deleted from a running instance and brought back. The CSP was verified in a real headless Chrome
over the DevTools protocol, on every page including the CodeMirror editor and the live log tail:
zero violations.

**With M7 merged the MVP is complete.** M0–M7 are all done.

---

## Starting point

`PLAN.md` asked for four things: rate limiting per IP and per project on mock traffic plus body
caps and server timeouts; security headers and a strict CSP the vendored CodeMirror satisfies; a
backup and restore procedure tested by restoring; and Prometheus metrics. Two more joined them
because they belonged here and nowhere else:

- **`/api/v1/` rate limiting**, which M6's note carried forward — the one credentialed surface,
  bounded by nothing.
- **`X-Forwarded-For`**, which M0 left with a comment in `middleware.go` saying it was M7's. Rate
  limiting per address is not worth building on an address any caller can claim, so the two had
  to land together.

And one that was on nobody's list and turned out to matter most: see "The mock sandbox" below.

## What was built

| Path | Role |
|---|---|
| `internal/ratelimit/ratelimit.go` | Keyed token buckets over `golang.org/x/time/rate`; sweep, ceiling, nil-is-off |
| `internal/metrics/metrics.go` | Own Prometheus registry, the four collectors, `Gauge`/`Counter` for state other packages own |
| `internal/web/clientip.go` | The trusted-proxy walk, resolved once per request into the context |
| `internal/web/security.go` | The two policies, and `mockHeaderWriter` which makes one of them unoverridable |
| `internal/web/ratelimit.go` | The mock and API middleware, the keys, the 429 bodies |
| `internal/web/metrics.go` | `/metrics`, the surface classification, the bounded labels |
| `internal/web/server.go` | `withBodyLimit`, the rewritten `Handler()` chain, the new options |
| `internal/config/config.go` | Thirteen new settings, including CIDR parsing for the proxy list |
| `cmd/restest/main.go` | The gauges over the recorder, the router and the limiters; timeouts from config |
| `scripts/backup.sh`, `scripts/restore.sh` | `pg_dump`/`pg_restore` inside the container; `make backup` / `make restore` |
| `internal/web/templates/base.html`, `tailwind.css` | The `htmx-config` meta tag and the indicator rules it displaces |

No migration. M7 added nothing to the schema.

## Decisions

All of these are written up in `DESIGN.md` §8.2 with their reasoning; what follows is the short
form and the things that only came out while writing the code.

- **Three limit keys, and the interface has none.** Per client address and per project on mock
  traffic — the first never notices a project a CI fleet points at, the second never notices one
  client abusing many projects — and per credential on the API. The interface is behind a session
  and a limit there mostly locks somebody out of their own project.
- **The rate is a setting, the burst is derived** (twice the rate, floor of twenty). The pair is
  not independent enough to be worth two knobs: a burst below the rate refuses a client exactly
  at the limit, and one far above it makes the limit meaningless for the flood it absorbs.
- **The limiter wraps the recorder rather than sitting inside it.** This was the closest call in
  the milestone. Inside, a user would see their throttled requests in the inspector, which is
  friendlier; outside, a refused request costs neither a captured body nor a queued exchange nor
  a row. Shedding load is the entire point, so outside. The refusal is still legible — the 429
  body names the limit, and `restest_rate_limited_total` counts it — and `levelFor` was changed
  to log a 429 at debug, because a warning per refusal turns a flood of requests into a flood of
  log lines, which is the second half of the same denial of service.
- **A limiter's table is emptied rather than evicted from.** Sweep first; if it is still at its
  ceiling, empty it. Every bucket is a client currently being counted, so there is no unimportant
  one to evict, and keeping an ordering on every request to save a walk that only happens when
  somebody is deliberately cycling keys is the wrong trade. Emptying costs a moment of leniency,
  which is the right way for a guard to fail. It is counted.
- **A slug no project has never becomes a limiter key.** Otherwise inventing slugs fills the
  table. `mock.Table.HasProject` is a map lookup and the request is about to 404 anyway.
- **The API's limiter key is the SHA-256 of the presented token**, not the token. Same value the
  database stores; a secret should not be a map key in a process that holds it for a minute
  afterwards. It is applied in `requireAPIUser` before `AuthenticateAPIToken`, so a flood of
  wrong tokens is refused without a database round trip each — and because every `/api/v1/` route
  goes through that function, a route added later is limited by construction.
- **`X-Forwarded-For` is walked right to left from a trusted peer.** A proxy appends what it
  received from, so the rightmost entry is the only one written by something we chose. The common
  wrong version — take the leftmost — hands the caller whatever they wrote. Resolved once, in
  `withClientIP`, near the top of the chain: the access log, the inspector and the limiters must
  agree about who this is, and three separate walks are three chances to disagree.
- **The body cap is applied once, above every handler.** `readMockBody`'s own `MaxBytesReader`
  and `maxMockRequestBody` are gone; the error path stays, and now reports the cap that was
  actually applied via `MaxBytesError.Limit` rather than a constant that could drift from it.
  The management API keeps its tighter 2 MiB, because a definition is smaller than an upload.
- **Metrics label bounded sets only.** No project, path, address or token label. The status code
  *is* one — bounded by what this application sends, and telling 404 from 429 is most of what the
  number is for. The four matcher outcomes are separate counters so the match rate is computable
  rather than approximated.
- **`/metrics` has an optional token, never a session.** Both deployments exist: behind a proxy
  that does not route it, a token is a secret to manage for nothing; published as it stands, the
  scrape names every project's traffic volume. Compared in constant time.
- **A restore drops and recreates the database** rather than restoring over it. `--clean` leaves
  behind whatever the dump does not mention, and a restore that leaves debris is one nobody can
  reason about. The application is stopped for it: restoring underneath a running one leaves the
  in-memory route table answering for endpoints that no longer exist.

### The mock sandbox — not on the list, and the most important thing here

A project's response body is written by whoever owns the project and served from **this origin**.
Nothing stopped an endpoint from returning an HTML page with a script in it. Send that URL to
somebody who has a session here and the script runs with our origin's privileges: stored
cross-site scripting, arriving through the front door as a feature.

`Content-Security-Policy: sandbox` on everything under `/m/{slug}/` puts such a document in an
opaque origin with scripts disabled. Verified, not assumed — an endpoint was defined returning
`<script>document.title='SCRIPT RAN'</script>`, loaded in Chrome, and the result was:

```
PAGE: {"title":"before","origin":"null","cookie":"BLOCKED: SecurityError"}
LOG: error security Blocked script execution … because the document's frame is sandboxed
```

Two details make it sound rather than decorative:

- **An endpoint cannot override it.** Endpoint headers are applied inside the handler, so the
  security headers are written by `mockHeaderWriter` at `WriteHeader`, after the handler has had
  its turn. Without that, one project could re-open the hole for every other user of the
  instance. `TestAnEndpointCannotOverrideTheSandbox` is the test.
- **`Cross-Origin-Resource-Policy` is deliberately absent** from mock responses. A browser client
  fetching a mock from a page on another origin is exactly what this is for, and `same-origin`
  there would refuse the traffic rather than protect it.

CSP governs documents a browser renders, not responses a program fetched, so this costs nothing
for the traffic mocks are actually for.

## The CSP, and what it took

`default-src 'none'`, everything else named, no `'unsafe-inline'` anywhere. Affordable only
because no template has ever carried an inline script, an inline style or an `on…` attribute.
Two things still had to move:

- **HTMX injects a `<style>` element** for its `.htmx-indicator` rules, which `style-src 'self'`
  refuses. Turned off through `<meta name="htmx-config">` — a meta tag rather than a script,
  because a script here would be an inline one — and the rules copied verbatim into
  `tailwind.css`, so the feature still works. Nothing uses `hx-indicator` today; copying them was
  cheap insurance rather than dead weight.
- **HTMX compiles `hx-vals="js:…"` with the `Function` constructor**, which `script-src 'self'`
  refuses. `allowEval: false` in the same meta tag stops it trying and warning.

**CodeMirror needed nothing.** It writes `element.style.cssText`, which is CSSOM and not governed
by `style-src`; it never calls `setAttribute("style", …)` and never injects a `<style>` element.
That was checked by reading the vendored file *and* by driving the real editor in a browser.

## Verification

- `make test` — passes, race detector on.
- `make test-integration` — passes.
- `make lint` — `go vet` plus golangci-lint, 0 issues.
- **In a real browser.** Headless Chrome over the DevTools protocol, logged in, loading
  `/login`, `/projects`, `/projects/{slug}`, `/tokens`, `/projects/{slug}/log` and both editor
  forms. On the editor pages the script also drove CodeMirror — focus, `setValue`, cursor move,
  `replaceSelection` — so its measuring, cursor drawing and bracket matching ran rather than only
  its construction. Result on every page: `styleTags: 0`, `htmx 2.0.10` with
  `includeIndicatorStyles: false`, editor present and functional, and **0 CSP violations, 0
  console errors, 0 exceptions**. Then the sandbox check above.
- **Against the compose stack.** The interface's six headers and the mock's three, on a page, on
  a mock 200 and on a mock 404. `/metrics` carrying the build info, the route table (1 project,
  24 routes), the queue depth and capacity, the limiter key counts. A 200-request burst at 30
  concurrent: 122 served, 78 refused, `Retry-After: 1`, and the body naming the 50-per-second
  limit — with the sandbox header on the refusal too.
- **The backup drill, twice.** The first attempt used a document in the demo project as the
  canary and proved nothing: the demo is reset to its seeds at startup and a restore restarts the
  application, so the canary was gone either way. The second used a registered account — backed
  up, `DELETE FROM users`, restored, and the account was back, with the request log's 441 rows
  and the migration version intact. **That failure is worth keeping**: it is exactly the way a
  restore drill lies to you, and the README now warns about it.
- 60 new test functions across `ratelimit`, `metrics`, `config` and `web`.

## Deliberately not done

- **No shared rate limiter.** In-process buckets, so two instances enforce each limit twice.
  `DESIGN.md` §9.1 always said the in-process version comes first and Redis is what would fix it.
- **No search or filters on the request log.** The oldest carried-forward gap, now the oldest by
  some way. It is a feature, not hardening.
- **No per-project or read-only token scopes.** Still additive: a column and a check.
- **No CORS settings.** An endpoint can set `Access-Control-Allow-Origin` itself, and that still
  works — the sandbox does not touch it.
- **No `Forwarded` header (RFC 7239).** It is the standardised one and nothing in front of this
  is configured to send it; a parser that is never exercised is a liability.
- **No alerting rules or dashboard.** The metrics are there and the README has the two queries
  worth starting from; what to page on is a deployment's decision.

## Notes for whoever picks this up

- **`Handler()` is now a list of assignments rather than nested calls**, and the order is
  commented in place. The two that matter: `withClientIP` is above everything that reads an
  address, and `withSecurityHeaders` is low enough to see which pattern matched.
- **Three middlewares decide on the matched route pattern, not the path**: `withSession`,
  `withSecurityHeaders` and `observe`. `s.mux.Handler(r)` is how they ask. A new route that needs
  different treatment goes in the relevant switch, not in a path prefix test.
- **A nil `*ratelimit.Limiter` allows everything.** That is how a rate of zero is expressed, and
  it is why `newLimiters` can hand back nils and the call sites need no branch.
- **`metricsOption` in `main.go` exists for the typed-nil trap.** Assigning a nil
  `*metrics.Metrics` straight into the interface field produces a non-nil interface, and
  `web.New` would then register `/metrics` against a registry that does not exist.
- **`Options.Metrics` is an interface for the same reason `Options.Recorder` is**: a handler test
  needs neither a registry nor the Prometheus library. `countingMetrics` in the tests is a tally.
- **Adding a metric whose value lives elsewhere** means a `m.Gauge`/`m.Counter` call in
  `publishGauges`, not a new import in the package that owns the value. Nothing in `core`, `mock`
  or `web` knows Prometheus exists except `internal/web/metrics.go`.
- **The `htmx-config` meta tag is load-bearing.** Removing it puts a `<style>` element back in
  the head and the CSP starts refusing it. If the indicator rules ever need changing, they are in
  `tailwind.css` and copied from `htmx.min.js`.

## State of the repository

Branch `feature/08-hardening`, merged to master by the finish sequence. `go.mod` gained
`prometheus/client_golang` and moved `golang.org/x/time` from indirect to direct. `app.css` was
regenerated by `make assets` for the indicator rules. No migration, and `internal/core/dbgen` is
untouched: M7 added nothing to the schema.

## Next step

**The MVP is complete — M0 through M7 are done.** There is no next milestone in `PLAN.md`. What
is left is the list of gaps each milestone carried forward, in rough order of how often they will
be missed:

1. **Search and filters on the request log**, in the browser and over the API. Carried since M4
   and the one gap a user meets daily.
2. **Token scopes** — per-project, and read-only. A column on `api_tokens` and a check in
   `requireAPIUser`.
3. **Adding a built-in dataset to an existing project.** Carried since M5; today it means
   creating the collection by hand.
4. **State isolation for collections** (`DESIGN.md` §12.1), if parallel CI runs against one
   project become a real complaint. Nullable scope column plus a client-supplied header.

And then phase 2, the outbound runner (`DESIGN.md` §10), which is a new feature rather than a
gap — and which nothing built so far precludes.
