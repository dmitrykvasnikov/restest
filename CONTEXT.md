# restest — Working rules

Rules for how this project is developed. `DESIGN.md` says *what* is being built and why,
`PLAN.md` says *in what order*, this file says *how we work*.

---

## 1. Language

Conversation between the owner and any assistant may be in **Russian or English**, whichever
is convenient at the moment.

**Everything belonging to the project itself is in English, without exception:**

- source code, identifiers, comments
- documents in the repository, including these notes
- commit messages, branch names, pull request text
- user interface copy, error messages, API responses
- database object names and migration files

A Russian conversation never produces a Russian artifact.

## 2. Git workflow

The GitHub remote is connected manually by the owner before development begins. No assistant
creates, renames or reconfigures remotes.

1. **Every feature is developed on its own branch**, cut from the current main branch.
   Branch names: `feature/NN-short-slug`, where `NN` is the feature number from §4.
2. The feature branch is **pushed to the remote** while work is in progress, so that work in
   flight is never only on one machine.
3. When the feature is finished it goes through the **finish sequence** in §8.
4. **Feature branches are kept** — never deleted, locally or on the remote. They are part of
   the project's record.

Because branches are kept as history, merges use `--no-ff` so that each feature remains a
visible, self-contained unit in the graph rather than being flattened away.

Commits are made only when the owner asks for them.

## 3. Session notes

**Every working session ends with a note** in `notes/notes_xx_y.md`:

- `xx` — the two-digit feature number (§4)
- `y` — the session number *within that feature*, counting from 1

So `notes_03_2.md` is the second session spent on feature 03. Numbering restarts at 1 for each
new feature.

A note records what was decided and why, what was actually verified as opposed to assumed, and
what the next step is. It is written for someone resuming with no memory of the conversation.

## 4. Feature numbering

Feature numbers follow the milestones in `PLAN.md`:

| Feature | Milestone | Subject |
|---|---|---|
| 00 | — | Inception: problem statement, design, schema, plan. No application code. |
| 01 | M0 | Skeleton: config, logging, pgx pool, migrations, Docker, health checks |
| 02 | M1 | Accounts and projects |
| 03 | M2 | Static mocks and the route matcher |
| 04 | M3 | Stateful collections |
| 05 | M4 | Request log and inspector |
| 06 | M5 | Public datasets and the demo project |
| 07 | M6 | API tokens and management API |
| 08 | M7 | Hardening |

Work discovered later that does not fit a milestone gets the next free number and a row here.

## 5. Documents and their roles

| File | Role |
|---|---|
| `task.md` | **Frozen.** The owner's original problem statement. Never edited — decisions go elsewhere, so problem and solution stay visibly distinct. |
| `DESIGN.md` | Decisions and their reasoning. Updated when a decision changes, with the reasoning updated too. |
| `PLAN.md` | Milestones and their order. |
| `CONTEXT.md` | This file: how we work. |
| `notes/` | Session history, append-only. Earlier notes are not rewritten. |

## 6. Design constraints that must not drift

These were argued through in `DESIGN.md`; changing one is a decision to be made deliberately,
not a detail to be improvised during implementation.

- **The phase 2 outbound runner is not built ahead of time.** No plugin system, no interface
  shared between mock and runner, no worker infrastructure, no assertion language until the
  feature is actually started.
- **`internal/mock/` and `internal/runner/` never import each other.** Both depend only on
  `internal/core/`.
- **Business logic stays out of HTTP handlers**, so a background worker can drive it later.
- **No npm in the toolchain.** Tailwind runs from its standalone binary; CodeMirror is
  vendored. Assets embed via `go:embed`.
- **No ORM.** Plain SQL through sqlc and pgx.
- **No Redis, no queue broker.** Postgres with `FOR UPDATE SKIP LOCKED` is the queue.
- **No JWT for browser sessions.** Server-side sessions, revocable instantly.
- **Mock traffic stays under the `/m/{slug}/` path prefix.** Subdomains may only ever be added
  as an alias, never as a replacement.

## 7. Database changes

- Schema changes are always a new numbered goose migration. Existing migrations are never
  edited once they have been applied anywhere.
- Every migration has a working `Down`.
- **A migration is not finished until it has been run against a real Postgres**, up and then
  down, with the constraints exercised by deliberately invalid inserts. Reasoning about SQL is
  not the same as running it.
- Integration tests use testcontainers-go against real Postgres. The heavy use of `jsonb`
  means a mocked database would test nothing worth testing.

## 8. Definition of done for a feature

1. The milestone's "done when" condition in `PLAN.md` is demonstrably met.
2. Tests cover the logic added, and the whole suite passes.
3. `DESIGN.md` updated if any decision changed.
4. Session note written in `notes/`.
5. The finish sequence below has been run.

### The finish sequence

Once 1–4 hold and the owner says the feature is done, these five steps run in this order,
without pausing between them:

1. **Commit** everything outstanding on the feature branch.
2. **Push** the feature branch to the remote.
3. **Switch** to the main branch and bring it up to date with the remote.
4. **Merge** the feature branch with `--no-ff`.
5. **Push** the main branch.

```sh
git add -A && git commit          # 1
git push -u origin feature/NN-…   # 2
git switch master && git pull     # 3
git merge --no-ff feature/NN-…    # 4
git push                          # 5
```

The feature branch is left in place, locally and on the remote (§2.4). Step 4 is never a
fast-forward: the merge commit is what keeps the feature visible as a unit in the graph.
