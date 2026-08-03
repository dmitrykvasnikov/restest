-- Queries for endpoints.
--
-- The management statements are scoped by owner through a join on projects, for
-- the same reason the project queries are scoped by owner_id: an endpoint in
-- somebody else's project must be indistinguishable from one that never existed,
-- and the surest way to get that is to put the ownership test in the SQL.
--
-- The two statements at the bottom are the exception. They feed the in-memory
-- route table, which serves unauthenticated mock traffic and therefore reads
-- across every account by design.

-- name: CreateEndpoint :one
insert into endpoints (
    project_id, method, path_pattern, kind, is_enabled, delay_ms,
    status_code, response_body, response_headers
)
select @project_id, @method, @path_pattern, 'static', @is_enabled, @delay_ms,
       @status_code, @response_body, @response_headers
from projects p
where p.id = @project_id and p.owner_id = @owner_id
returning *;

-- EndpointsByProject drives the list on the project page. Ordered by path so
-- that routes sharing a prefix sit together, which is how they are read.
--
-- name: EndpointsByProject :many
select e.* from endpoints e
join projects p on p.id = e.project_id
where e.project_id = @project_id and p.owner_id = @owner_id
order by e.path_pattern, e.method;

-- name: EndpointByOwnerAndID :one
select e.* from endpoints e
join projects p on p.id = e.project_id
where e.id = @id and p.owner_id = @owner_id;

-- name: UpdateEndpoint :one
update endpoints e
set method           = @method,
    path_pattern     = @path_pattern,
    is_enabled       = @is_enabled,
    delay_ms         = @delay_ms,
    status_code      = @status_code,
    response_body    = @response_body,
    response_headers = @response_headers,
    updated_at       = now()
from projects p
where e.id = @id and p.id = e.project_id and p.owner_id = @owner_id
returning e.*;

-- name: DeleteEndpoint :execrows
delete from endpoints e
using projects p
where e.id = @id and p.id = e.project_id and p.owner_id = @owner_id;

-- MockProjects lists every project, including those with no endpoints yet. The
-- route table needs them: "this project has nothing defined" and "there is no
-- such project" are different answers, and only the endpoint rows would not
-- tell them apart.
--
-- name: MockProjects :many
select id, slug from projects;

-- MockEndpoints is everything the route table serves. Disabled endpoints are
-- left out rather than filtered at match time, so a disabled route is invisible
-- to the matcher instead of being a case it has to remember.
--
-- name: MockEndpoints :many
select e.*, p.slug as project_slug from endpoints e
join projects p on p.id = e.project_id
where e.is_enabled
order by e.path_pattern, e.method;
