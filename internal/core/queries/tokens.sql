-- Queries for API tokens.
--
-- A token row never holds the token. It holds the SHA-256 of it, a short
-- non-secret prefix so the owner can tell one row from another after the
-- plaintext is gone, and the name they gave it. Everything that reads a token
-- back out reads those three; nothing in the schema can reconstruct the secret.
--
-- The listing statements are scoped by user_id for the same reason the project
-- statements are scoped by owner_id: somebody else's token must be
-- indistinguishable from one that never existed.

-- name: CreateAPIToken :one
insert into api_tokens (user_id, name, prefix, token_hash, expires_at)
values (@user_id, @name, @prefix, @token_hash, @expires_at)
returning *;

-- APITokensByUser drives the tokens page. Newest first: the one just created is
-- the one being looked for.
--
-- name: APITokensByUser :many
select * from api_tokens
where user_id = @user_id
order by created_at desc;

-- DeleteAPIToken reports the number of rows removed, which is how the caller
-- tells "revoked" from "never yours in the first place".
--
-- name: DeleteAPIToken :execrows
delete from api_tokens where id = @id and user_id = @user_id;

-- AuthenticateAPIToken resolves a presented token to its owner and marks it
-- used, in one statement.
--
-- The update and the lookup are one statement because they are one question:
-- "is this token good, and whose is it". Two statements would leave a window in
-- which a token revoked between them still answered. An expired token matches
-- nothing, so expiry needs no second check in Go — and the same row therefore
-- does not get its last_used_at bumped by a request it did not authorise.
--
-- The lookup is by hash, on the unique index, so it costs one index probe. No
-- constant-time comparison is needed or possible here: what is compared is a
-- SHA-256 of 32 random bytes, and the database is matching it as an index key.
--
-- name: AuthenticateAPIToken :one
with touched as (
    update api_tokens
    set last_used_at = now()
    where token_hash = @token_hash
      and (expires_at is null or expires_at > now())
    returning *
)
select
    t.id, t.user_id, t.name, t.prefix, t.last_used_at, t.expires_at, t.created_at,
    u.email      as user_email,
    u.created_at as user_created_at,
    u.updated_at as user_updated_at
from touched t
join users u on u.id = t.user_id;
