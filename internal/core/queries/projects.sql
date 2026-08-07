-- Queries for projects.
--
-- Every statement is scoped by owner_id as well as by the row's own key. A
-- project that belongs to somebody else must be indistinguishable from one that
-- does not exist, and the surest way to get that is to make the ownership test
-- part of the query rather than a check the caller might forget.

-- CreateProject takes is_demo because the shared demo project is created by the
-- application at startup and is otherwise an ordinary project. No form can set
-- it: the flag arrives from core.EnsureDemoProject and nowhere else.
--
-- name: CreateProject :one
insert into projects (owner_id, slug, name, is_demo)
values (@owner_id, @slug, @name, @is_demo)
returning *;

-- ProjectBySlug is the one project lookup with no owner in it. The demo project
-- has an owner nobody can log in as, so provisioning it at startup has no user
-- to scope by; the slug it uses is reserved, so no account's project can hold
-- it.
--
-- name: ProjectBySlug :one
select * from projects where slug = @slug;

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
