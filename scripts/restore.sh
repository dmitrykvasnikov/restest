#!/usr/bin/env bash
#
# Restore the restest database from a backup taken by scripts/backup.sh.
#
#   scripts/restore.sh backups/restest-20260807T101500Z.dump
#
# This **replaces** the database. Everything currently in it — every account,
# project, endpoint, collection, document and request log entry — is gone when
# this finishes, and what comes back is what was in the file.
#
# The application is stopped first and started again afterwards. Restoring under
# a running application would mean serving from a database being rebuilt
# underneath, and the route table the process holds in memory would go on
# answering for endpoints that no longer exist until its next refresh. Stopping
# it is a few seconds of downtime in exchange for a restore that is one state
# followed by another rather than a blend of the two.
#
# Set RESTEST_RESTORE_YES=1 to skip the confirmation, for a scripted drill.

set -euo pipefail

cd "$(dirname "$0")/.."

FILE="${1:-}"
if [[ -z "$FILE" ]]; then
  echo "usage: scripts/restore.sh <backup file>" >&2
  exit 1
fi
if [[ ! -r "$FILE" ]]; then
  echo "$FILE: cannot be read" >&2
  exit 1
fi

if [[ -f .env ]]; then
  # shellcheck disable=SC1091 # a deployment's own file, not in the repository
  set -a && source .env && set +a
fi

POSTGRES_USER="${POSTGRES_USER:-restest}"
POSTGRES_DB="${POSTGRES_DB:-restest}"

if ! docker compose ps --status running --services 2>/dev/null | grep -qx db; then
  echo "the db service is not running; start it with 'docker compose up -d db'" >&2
  exit 1
fi

if [[ "${RESTEST_RESTORE_YES:-}" != "1" ]]; then
  echo "This replaces the contents of the database '$POSTGRES_DB' with $FILE."
  echo "Everything currently in it will be lost."
  read -r -p "Type the database name to continue: " confirm
  if [[ "$confirm" != "$POSTGRES_DB" ]]; then
    echo "aborted" >&2
    exit 1
  fi
fi

echo "stopping the application…" >&2
docker compose stop app >/dev/null 2>&1 || true

# Dropped and recreated rather than restored over. pg_restore --clean would
# leave behind anything the dump does not mention — a table added by a migration
# that has since been rolled back, a partition from a month the backup predates
# — and a restore that leaves debris is a restore nobody can reason about.
#
# --force terminates the connections still holding the database open; the pool
# is gone with the application, but the session store's cleanup goroutine and a
# stray psql are both possible.
echo "recreating the database…" >&2
docker compose exec -T db dropdb --username="$POSTGRES_USER" --force --if-exists "$POSTGRES_DB"
docker compose exec -T db createdb --username="$POSTGRES_USER" "$POSTGRES_DB"

echo "restoring $FILE…" >&2
# --no-owner and --no-privileges so that a dump taken on one instance restores
# on another where the roles are named differently. --exit-on-error because a
# restore that reports a problem and carries on is a database nobody knows the
# state of.
docker compose exec -T db \
  pg_restore --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" \
             --no-owner --no-privileges --exit-on-error \
  < "$FILE"

echo "starting the application…" >&2
docker compose start app >/dev/null

# The application applies migrations at startup, so a backup from an older
# schema is brought forward by the process that just started. Waiting for the
# readiness probe is how this script reports that the restore actually worked
# rather than that the commands returned zero.
READY_URL="http://${RESTEST_BIND:-127.0.0.1}:${RESTEST_PORT:-8080}/readyz"
echo -n "waiting for $READY_URL" >&2
for _ in $(seq 30); do
  if curl -fsS "$READY_URL" >/dev/null 2>&1; then
    echo >&2
    echo "restored $FILE" >&2
    exit 0
  fi
  echo -n "." >&2
  sleep 1
done

echo >&2
echo "the application did not become ready; check 'docker compose logs app'" >&2
exit 1
