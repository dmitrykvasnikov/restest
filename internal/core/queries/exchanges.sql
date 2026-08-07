-- Queries for the request log.
--
-- Exchanges carry no owner column and no foreign keys: it is the write-heavy
-- table, and rows leave it by dropping a partition rather than by cascade. The
-- ownership check therefore happens one step earlier — a handler resolves the
-- project through ProjectByOwnerAndSlug and only then asks for its exchanges,
-- so a project id reaching these statements has already been proved to belong
-- to the caller.
--
-- The partition statements are not here. Their table names contain the month
-- they cover, so they are assembled in internal/core/partition.go with quoted
-- identifiers; sqlc generates static SQL and cannot name a table that does not
-- exist yet.

-- InsertExchanges is the batching writer's statement. COPY rather than INSERT
-- because this is the one path in the application that writes in bulk, and it
-- writes on every mock request; the whole point of the queue behind it is that
-- the cost per request stays off the request path.
--
-- created_at is supplied rather than defaulted: it is the partition key, and
-- the value that belongs to a row is when the request finished, not when the
-- batch holding it happened to reach the database.
--
-- name: InsertExchanges :copyfrom
insert into exchanges (
    id, project_id, endpoint_id, direction, matched,
    method, path, query,
    request_headers, request_body, request_body_truncated,
    status_code, response_headers, response_body, response_body_truncated,
    duration_ms, remote_addr, created_at
) values (
    $1, $2, $3, $4, $5,
    $6, $7, $8,
    $9, $10, $11,
    $12, $13, $14, $15,
    $16, $17, $18
);

-- ExchangesByProject is the inspector's list: newest first, one page at a time.
--
-- The cursor is the (created_at, id) pair of the last row on the page before,
-- compared as a row rather than as two columns, so that several exchanges
-- sharing a timestamp still page through exactly once. A null cursor means the
-- first page, which is why the whole comparison is guarded rather than the
-- caller having to invent a timestamp far enough in the future.
--
-- name: ExchangesByProject :many
select * from exchanges
where project_id = @project_id
  and (@before_at::timestamptz is null
       or (created_at, id) < (@before_at::timestamptz, @before_id::uuid))
order by created_at desc, id desc
limit @page_limit;

-- ExchangesSince is the live tail: everything recorded after the cursor, oldest
-- first, so that a stream appends them in the order they happened.
--
-- name: ExchangesSince :many
select * from exchanges
where project_id = @project_id
  and (created_at, id) > (@after_at::timestamptz, @after_id::uuid)
order by created_at, id
limit @page_limit;

-- ExchangeByID is the detail view. The project is part of the lookup so that an
-- exchange id from another account's project is the same 404 as one that never
-- existed — the table itself has no owner to scope by.
--
-- name: ExchangeByID :one
select * from exchanges
where project_id = @project_id and id = @id;

-- LatestExchangeCursor is where a live tail starts: the newest row already on
-- the page, so the stream sends what arrives after it and not the page again.
--
-- name: LatestExchangeCursor :one
select created_at, id from exchanges
where project_id = @project_id
order by created_at desc, id desc
limit 1;
