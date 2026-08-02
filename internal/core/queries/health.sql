-- Queries backing the readiness probe.

-- CheckDatabase is what /readyz asks. It is trivial on purpose: the point is
-- not the answer but that the pool handed out a connection and the server
-- planned and ran a statement on it.
--
-- name: CheckDatabase :one
select 1 as ok;
