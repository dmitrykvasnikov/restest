-- Queries for collections.
--
-- Scoped by owner through a join on projects, for the same reason the endpoint
-- queries are: a collection in somebody else's project must be
-- indistinguishable from one that never existed, and the surest way to get that
-- is to put the ownership test in the SQL.
--
-- The statements about *documents* are not here. They are built at run time in
-- internal/core/document.go, because a listing carries an arbitrary number of
-- field filters and a sort key chosen by the caller, and sqlc generates static
-- statements. The two collection statements at the bottom are the exception
-- within this file: they serve unauthenticated mock traffic, which has no owner
-- to scope by.

-- name: CreateCollection :one
insert into collections (project_id, name, seed, id_field, id_strategy)
select @project_id, @name, @seed, @id_field, @id_strategy
from projects p
where p.id = @project_id and p.owner_id = @owner_id
returning *;

-- CollectionsByProject drives the list on the project page. The document count
-- comes with it because "how much is in there" is the first thing anyone
-- looking at the list wants to know.
--
-- name: CollectionsByProject :many
select c.*, (select count(*) from documents d where d.collection_id = c.id) as document_count
from collections c
join projects p on p.id = c.project_id
where c.project_id = @project_id and p.owner_id = @owner_id
order by c.name;

-- name: CollectionByOwnerAndID :one
select c.* from collections c
join projects p on p.id = c.project_id
where c.id = @id and p.owner_id = @owner_id;

-- CollectionByOwnerAndName resolves the {slug} and {name} of the reset URL in
-- one statement, so that a name in another account's project is the same 404 as
-- a name that was never used.
--
-- name: CollectionByOwnerAndName :one
select c.* from collections c
join projects p on p.id = c.project_id
where p.owner_id = @owner_id and p.slug = @slug and c.name = @name;

-- name: UpdateCollection :one
update collections c
set name        = @name,
    seed        = @seed,
    id_field    = @id_field,
    id_strategy = @id_strategy,
    updated_at  = now()
from projects p
where c.id = @id and p.id = c.project_id and p.owner_id = @owner_id
returning c.*;

-- name: DeleteCollection :execrows
delete from collections c
using projects p
where c.id = @id and p.id = c.project_id and p.owner_id = @owner_id;

-- CollectionIDField is what a write needs to know before it can put the
-- identifier back into the document: which field carries it.
--
-- name: CollectionIDField :one
select id_field from collections where id = @id;

-- CollectionForReset reads what rebuilding the collection from its seed needs,
-- and holds the row while it happens. The lock is what stops two resets — or a
-- reset and a create — from interleaving their identifier allocation and
-- leaving the counter pointing at an id that already exists.
--
-- name: CollectionForReset :one
select seed, id_field, id_strategy from collections where id = @id for update;

-- name: SetNextSerial :exec
update collections set next_serial = @next_serial, updated_at = now() where id = @id;

-- AllocateDocumentID reserves the next identifier for a create.
--
-- The counter is advanced in the same statement that reads it, so two
-- concurrent creates cannot be handed the same number: the second waits on the
-- row lock the first is holding and reads the value the first left behind.
-- `next_serial - 1` is returned because `returning` sees the new row, and what
-- the caller wants is the value it just claimed rather than the one after it.
--
-- The counter is left alone under id_strategy = 'uuid', where the caller
-- generates its own identifier and `allocated` is ignored.
--
-- name: AllocateDocumentID :one
update collections
set next_serial = case when id_strategy = 'serial' then next_serial + 1 else next_serial end
where id = @id
returning id_field, id_strategy, (next_serial - 1)::bigint as allocated;
