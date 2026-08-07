# Session notes 05-1 — M4 request log and inspector

Date: 2026-08-07
Feature: 05 — M4 request log and inspector
Branch: `feature/05-request-log`
Outcome: the milestone's "done when" is met — requests appear in the UI as they arrive, without
a refresh. Verified by `TestTheM4Milestone`, which opens the SSE stream, sends a mock request and
reads the rendered row off the wire, against real Postgres and the real partitioned table.

---

## Starting point

M3 left stateful collections. The `exchanges` table, its check constraints, its
`(project_id, created_at desc)` index and its declaration as `partition by range (created_at)`
with a default partition were already in migration `00001` from feature 00. **M4 needed no
migration**: everything the log stores had been designed for in the initial schema, and the
months it needs are created as partitions at runtime rather than as DDL in a migration file.

## What was built

| Path | Role |
|---|---|
| `internal/core/exchange.go` | `Exchange`, `HeaderSet` and its redaction, `ExchangeCursor`, the store reads |
| `internal/core/recorder.go` | The queue and the batching writer: `Record`, `Run`, `Wait`, drop accounting |
| `internal/core/partition.go` | Monthly partition creation, retention by detach-and-drop, the daily loop |
| `internal/core/queries/exchanges.sql` | `COPY` insert, the paged list, the tail, the detail lookup, the cursor |
| `internal/web/recording.go` | The middleware around the mock handler; body and response capture |
| `internal/web/log.go` | The list, the detail view and the SSE stream |
| `internal/web/templates/pages/log_list.html` | The list, the drop banner, the retention line |
| `internal/web/templates/pages/log_entry.html` | One exchange in full, with `header_table` and `body_block` |
| `internal/web/templates/partials/exchange.html` | `exchange_row` — the one template the page and the stream share |
| `internal/web/static/js/logtail.js` | `EventSource`, prepend, cap at 200 rows |

Three new configuration variables — `RESTEST_LOG_BODY_LIMIT`, `RESTEST_LOG_BUFFER`,
`RESTEST_LOG_RETENTION_MONTHS` — passed through by `docker-compose.yml` and documented in
`.env.example`, so the supported way of running the project can reach them. No new Go
dependencies, no new third-party code.

## Decisions made while building

The ones that changed `DESIGN.md` are marked. §7 gained a new §7.1, and §12.3 is answered and
struck through.

- **The middleware wraps the mock handler and nothing else** *(DESIGN.md §7.1)*. The UI's own
  traffic is somebody administering their mocks, not a client under test; recording it would fill
  the inspector with the inspector. `TestUIRequestsAreNotRecorded` holds the line.
- **A request to a slug no project has is not recorded** *(DESIGN.md §7.1)*. There is no project
  whose log it would belong to, and inventing one means answering "who typed this" with somebody
  arbitrary. The 404 already said as much and the access log has the line.
- **The queue drops and says so** *(DESIGN.md §7.1)*. `Record` never blocks — blocking would put
  the database back in the request path at exactly the moment the database is the problem — and
  never discards quietly. Drops are counted, warned about at most every ten seconds, reported
  unconditionally at shutdown, and shown on the inspector page. Failed writes are counted
  separately from dropped ones: the first is the database refusing, the second is this process
  falling behind, and they call for different reactions.
- **The batch write has its own timeout** (`DefaultRecorderWrite`, 10s). Without it a database
  that accepts a connection and then stops answering wedges the drain goroutine; the queue behind
  it fills, everything after that is dropped, and *nothing says so* — because the goroutine that
  reports drops is the one stuck in the write. This is the failure the drop reporting exists to
  prevent, arriving through the reporting mechanism itself.
- **Credentials are redacted at capture, not at display** *(DESIGN.md §7.1)*. `Authorization`,
  `Proxy-Authorization`, `Cookie`, `Set-Cookie`. Redacting on the way out means the credential is
  in the database and one careless query away. The scheme is kept — `Bearer [redacted]` — because
  "was it sending an `Authorization` header at all, and of what kind" is the question the
  inspector is actually asked.
- **The request body is read eagerly to the cap, not teed as the handler reads it.** A static
  endpoint never reads the body at all, so a tee would record nothing for exactly the request
  somebody is most puzzled by. Only the cap is read; the rest stays on the connection, so a large
  upload does not become a large allocation. What was read is put back in front of the body with
  `io.MultiReader`, and `TestRecordingDoesNotTruncateWhatTheHandlerSees` proves the handler still
  sees all of it.
- **One byte past the limit is read to decide truncation**, so a body of exactly the cap is not
  reported as truncated. There is a test named after it.
- **The live tail polls the database rather than being fed by the recorder in process**
  *(DESIGN.md §7.1)*. One small indexed query per second per open inspector, and in exchange: a
  second instance's traffic appears in the tail as readily as this one's, and what is streamed is
  what was stored rather than what was hoped to be.
- **The stream sends rendered HTML, not JSON.** It executes `exchange_row` from the list's own
  template, so a row that arrives live and one that arrives on a refresh cannot drift apart.
- **The page hands the stream its own newest cursor** (`?after=`). Without it, an exchange
  recorded between the page rendering and the connection opening is lost — a gap of a few hundred
  milliseconds that would show up as a request that never happened.
- **The cursor is `(created_at, id)` compared as a row**, not an offset and not a timestamp
  alone. Several exchanges share a millisecond under load; a row comparison pages through each
  exactly once, which `TestExchangePagingReturnsEachExactlyOnce` checks with a batch that shares
  timestamps.
- **`WriteTimeout` stays where it is.** The stream lifts its own write deadline through
  `http.ResponseController`, the same trick a delayed mock endpoint already used. Weakening the
  guard on every route to accommodate two responses is the wrong trade, and the comment in
  `main.go` that anticipated doing exactly that has been replaced.
- **Retention runs daily, not monthly** *(DESIGN.md §7.1)*. A process restarted often would
  otherwise be the only thing that ever ran it, and one never restarted would run it once. Each
  run creates three months ahead, so a job failing unnoticed for a quarter is what it takes to
  reach the default partition.
- **Detach and drop in one transaction, under a 5s `lock_timeout`.** The lock these need is
  ACCESS EXCLUSIVE; a long query over the log would otherwise let a background job block every
  mock request behind it. Failing and trying again tomorrow is the right answer. Doing both in
  one transaction means a failure between them cannot leave an orphan table holding the log's
  data with nothing pointing at it.
- **§12.3 answered: 64 KiB per body, three months of retention**, both configurable
  (`RESTEST_LOG_BODY_LIMIT`, `RESTEST_LOG_BUFFER`, `RESTEST_LOG_RETENTION_MONTHS`). The cap is a
  decision about what is worth keeping rather than what is safe to accept — mock request bodies
  are already capped at 1 MiB on the way in — and it is bounded above by that ceiling. Retention
  refuses to go below one month: detaching the partition being written into would send live
  traffic to the default partition, which is a safety net and not a home.
- **`Recorder` is optional on the `Server`.** Without one nothing is recorded and the page says
  so rather than looking like a project with no traffic. It is what lets the handler tests stay
  about handlers.
- **Shutdown order: server, then recorder, then pool.** The exchanges queued by the requests that
  just finished are written by a goroutine that needs the pool open, and `defer pool.Close()`
  runs after `run` returns. The recorder's context is separate from the one the signal cancels,
  for exactly that reason, and `Wait` is what makes the sequence hold.

## Verification

The implementation was in the working tree, uncommitted, when this session started; no note had
been written for it. This session read it through, settled the documents, and ran everything:

- `make test` — passes.
- `make test-integration` — passes, 178s, race detector on. That includes `TestTheM4Milestone`
  end to end: define an endpoint in the form, `POST` to `/m/checkout/orders?trace=7`, wait for the
  recorder to write it, find it on the log page, open the detail view and find the body and the
  headers, then open the SSE stream, send `trace=8`, and read the rendered row off the wire —
  while checking the stream does *not* replay `trace=7`, which is the cursor working.
- `make lint` — `go vet` plus golangci-lint, 0 issues.
- The partition behaviour is checked against real Postgres, not reasoned about: rows land in
  their month's partition, an uncovered month falls into the default, retention drops what has
  expired and never the current month, and maintenance is idempotent.
- 61 new test functions across `core`, `web` and `integration`.
- And against the compose stack, not only in tests. `make up`, then one `POST` to
  `/m/checkout/users/7?trace=live` carrying an `Authorization: Bearer supersecret-token-value`.
  The 405 it deserved came back, and a second later the row was in the table: `matched = f`,
  the query string kept, the header stored as `["Bearer [redacted]"]` — the secret never reached
  the database — and `tableoid::regclass` reporting `exchanges_2026_08`, so partition routing
  works on the real table. Startup had already created `2026_08` through `2026_11`: the current
  month and three ahead, from the maintenance loop's first run.

## Deliberately not done

- **No search and no filters on the log.** It is a list newest-first plus a detail view; finding
  one request among a thousand means paging. Filtering by status, method or path is the obvious
  next thing, and the index that would serve it (`project_id, created_at desc`) is already there.
- **The log is not on `/api/v1/`.** Readable in a browser, not from a script. M6 routes the rest
  of the API and this joins it.
- **No outbound direction.** `exchanges.direction` accepts `inbound` and `outbound`; only
  `inbound` is ever written. `outbound` belongs to the phase 2 runner, which is not built.
- **No metrics.** Queue depth and drop count are in the process log and on the page, not in
  Prometheus. That is M7, which lists them explicitly.
- **No per-project recording switch.** Recording is on for every project, at one process-wide
  body cap.

## Notes for whoever picks this up

- **`noteExchange` is how the middleware learns which project a request belonged to.** The
  matcher's decision is known inside `handleMock` and nowhere else, and the middleware runs on
  both sides of a handler that returns nothing, so it travels through the context. A future mock
  handler that returns before calling it records nothing — the exchange is dropped as
  "unknown project", silently, because that is what an unknown project legitimately looks like.
- **`captureWriter` must keep `Unwrap`.** A delayed mock response extends its own write deadline
  through `http.ResponseController`, which walks the chain by unwrapping. Adding another wrapper
  around the mock handler means giving it one too.
- **The partition statements are not in sqlc.** Their table names contain the month they cover
  and the tables do not exist yet, so they are assembled in `partition.go` with
  `quoteIdentifier`. Every name reaching it was composed from a number in that file; nothing
  user-supplied does, and it should stay that way.
- **Retention reads the month back out of the table name** rather than parsing partition bound
  expressions. Anything not matching `exchanges_YYYY_MM` — the default partition above all, or a
  table somebody attached by hand — is reported as not ours and left alone.
- The stream's lifetime is capped at 30 minutes. `EventSource` reconnects on its own and resumes
  from its own last cursor, so a forgotten tab renews rather than holding a connection for days.

## State of the repository

Branch `feature/05-request-log`, merged to master by the finish sequence. `internal/core/dbgen/`
gained `exchanges.sql.go` and `copyfrom.go` from `make sqlc` and is committed, as generated output
is here. No migration was added.

## Next step

Feature 06 / milestone M5: public datasets and the demo project — seed templates for `users`,
`posts`, `comments` and `todos`, new projects creatable pre-seeded from one, a demo project
provisioned at startup and reachable at `/m/demo/…` with no account, and a scheduled reset so one
visitor cannot spoil it for the next. `DESIGN.md` §12.2 — how often the demo resets, and whether
anonymous writes are persisted at all or only echoed — is the open question that lands with it.
`demo` is already in the reserved-slug list, held for exactly this.
