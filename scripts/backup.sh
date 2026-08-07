#!/usr/bin/env bash
#
# Take a backup of the restest database.
#
# The dump is taken with pg_dump inside the database container, so the only
# thing this needs on the host is Docker — the same requirement the rest of the
# project has. The custom format (-Fc) is used rather than plain SQL because it
# is compressed, and because pg_restore can then be told what to do with it
# rather than psql simply doing whatever the file says.
#
# The application keeps running. pg_dump takes a consistent snapshot in one
# transaction, so a backup taken under traffic is a backup of one moment rather
# than a smear across several.
#
#   scripts/backup.sh [directory]
#
# The directory defaults to ./backups. The file is named for the instant the
# dump started, in UTC, so that the list sorts chronologically wherever it is
# read.

set -euo pipefail

cd "$(dirname "$0")/.."

# Settings come from .env when there is one, the same file compose reads, and
# fall back to the defaults in .env.example.
if [[ -f .env ]]; then
  # shellcheck disable=SC1091 # a deployment's own file, not in the repository
  set -a && source .env && set +a
fi

POSTGRES_USER="${POSTGRES_USER:-restest}"
POSTGRES_DB="${POSTGRES_DB:-restest}"

DIR="${1:-backups}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
FILE="$DIR/restest-$STAMP.dump"

if ! docker compose ps --status running --services 2>/dev/null | grep -qx db; then
  echo "the db service is not running; start it with 'make up' or 'docker compose up -d db'" >&2
  exit 1
fi

mkdir -p "$DIR"

# -T because there is no terminal here and the dump is binary on stdout. Writing
# to a temporary name first means an interrupted dump never leaves a file that
# looks like a backup.
docker compose exec -T db \
  pg_dump --format=custom --compress=9 --username="$POSTGRES_USER" "$POSTGRES_DB" \
  > "$FILE.partial"

mv "$FILE.partial" "$FILE"

echo "$FILE"
echo "  $(du -h "$FILE" | cut -f1), restore it with: scripts/restore.sh $FILE" >&2
