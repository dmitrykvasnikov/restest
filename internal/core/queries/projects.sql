-- Queries for projects.
--
-- Every statement is scoped by owner_id as well as by the row's own key. A
-- project that belongs to somebody else must be indistinguishable from one that
-- does not exist, and the surest way to get that is to make the ownership test
-- part of the query rather than a check the caller might forget.

-- name: CreateProject :one
insert into projects (owner_id, slug, name)
values (@owner_id, @slug, @name)
returning *;

-- ProjectsByOwner drives the project list. Newest first: the one just created
-- is the one being looked for.
--
-- name: ProjectsByOwner :many
select * from projects
where owner_id = @owner_id
order by created_at desc, slug;

-- name: ProjectByOwnerAndSlug :one
select * from projects
where owner_id = @owner_id and slug = @slug;

-- UpdateProject changes the two fields a user may edit. Renaming the slug
-- changes the project's mock URL, which is the user's decision to make.
--
-- name: UpdateProject :one
update projects
set slug = @slug, name = @name, updated_at = now()
where id = @id and owner_id = @owner_id
returning *;

-- DeleteProject reports the number of rows removed, which is how the caller
-- tells "deleted" from "never yours in the first place". Endpoints, collections
-- and documents go with it through on delete cascade.
--
-- name: DeleteProject :execrows
delete from projects where id = @id and owner_id = @owner_id;
