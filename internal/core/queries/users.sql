-- Queries for accounts.

-- CreateUser fails with a unique violation on a duplicate address. The check is
-- left to the constraint rather than done as a prior SELECT, because between
-- that SELECT and this INSERT another registration can slip in.
--
-- name: CreateUser :one
insert into users (email, password_hash)
values (@email, @password_hash)
returning *;

-- UserByEmail is the login lookup. `email` is citext, so the comparison is
-- case-insensitive in the database rather than in Go.
--
-- name: UserByEmail :one
select * from users where email = @email;

-- UserByID resolves the id held in the session cookie on every request that
-- needs to know who is asking.
--
-- name: UserByID :one
select * from users where id = @id;

-- UpdatePassword also bumps updated_at, which is the record of when the
-- credential last changed.
--
-- name: UpdatePassword :execrows
update users
set password_hash = @password_hash, updated_at = now()
where id = @id;
