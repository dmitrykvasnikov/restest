# Session notes 00-2 — Database schema and MVP plan

Date: 2026-08-02
Feature: 00 — project inception
Outcome: schema written and verified against a real Postgres 17, milestone plan written,
working rules agreed. Still no application code.

---

## Starting point

Session 00-1 settled scope and stack and produced `DESIGN.md`. This session took the agreed
next step: the database schema and the order of implementation work.

## Deliverables

- `migrations/00001_init.sql` — first goose migration, eight tables.
- `PLAN.md` — eight milestones, M0 through M7.
- `CONTEXT.md` — working rules for the project (git flow, notes convention, language).

## Schema decisions made while writing it

These were not settled in `DESIGN.md` and were decided here:

- **`documents.public_id` is separate from the internal primary key.** Clients see `1`, `42`
  or a uuid; the primary key stays a uuid. Without the split, seeded data could not keep its
  own identifiers. Strategy is per-collection via `id_strategy` (`serial` | `uuid`), and
  serial ids come from `UPDATE collections SET next_serial = next_serial + 1 RETURNING`, so
  concurrent creates cannot collide.
- **`endpoints.response_body` is `text`, not `jsonb`.** Mock responses are not always JSON,
  and they are served verbatim rather than queried. By contrast `documents.body` is `jsonb`
  with a `jsonb_path_ops` GIN index, because that *is* queried — it backs `?field=value`
  filtering with no per-collection schema.
- **One `collection` endpoint row expands to all six CRUD routes** rooted at its path, rather
  than making the user create six rows.
- **`exchanges` carries no foreign keys.** It is the write-heavy table and rows are discarded
  by dropping partitions, not by cascade. Accepted consequence: after a project is deleted its
  log rows survive until their partition expires.
- Partition key must be part of the primary key, so `exchanges` uses `(id, created_at)`.
  A default partition catches anything the partition-creation job has not covered, so writes
  can never fail for want of a partition.
- Slug format is enforced in the database, not only in application code:
  `^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`.

## Verification

Rather than assume the SQL was correct, it was run against Postgres 17.10 in a throwaway
container.

- Up migration applies cleanly; Down leaves zero tables in `public`.
- Happy path works end to end: user → project → collection → endpoints → documents → exchange.
- `body @> '{"status":"active"}'` returns the right row — this is the future `?status=active`.
- `citext` genuinely makes email case-insensitive: `A@B.C` cannot register a second account.
- A partitioned insert lands in `exchanges_default`.
- Deleting a project cascades to endpoints, collections and documents; exchanges survive, as
  designed.
- **All eleven negative cases were rejected**: static endpoint without `status_code`,
  collection endpoint carrying one, uppercase slug, slug with trailing hyphen, seed object
  instead of array, document body as array instead of object, duplicate `public_id`, relative
  path pattern, delay above the cap, unknown `direction`, duplicate email in different case.

## Milestone plan

M0 skeleton · M1 accounts and projects · M2 static mocks · M3 stateful collections ·
M4 request log and inspector · M5 public datasets · M6 API tokens and management API ·
M7 hardening.

M2 is the first genuinely useful milestone; M3 is the first that satisfies the core scenario
in `task.md`. The route matcher in M2 is flagged as the riskiest code in the project — its
precedence tests belong alongside it, not after.

## Working rules agreed at the end of the session

Recorded in `CONTEXT.md`:

- Each feature is developed on its own git branch, pushed to the GitHub remote (which the
  owner connects manually before development starts).
- On completion the branch is merged into the main branch and main is pushed. **Feature
  branches are kept**, not deleted.
- Every session ends with notes in `notes/notes_xx_y.md`, where `xx` is the feature number and
  `y` the session number within that feature.
- Conversation may be Russian or English; **everything in the project itself — code,
  comments, documents, UI copy, commit messages — is English.**

## Next step

Feature 01 / milestone M0: `go.mod`, configuration, slog, pgx pool, embedded goose migrations,
docker-compose, `/healthz` and `/readyz`. First step is creating branch `feature/01-skeleton`
once the remote exists.
