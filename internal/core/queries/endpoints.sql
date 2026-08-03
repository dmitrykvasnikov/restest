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

-- The two kinds get a statement each rather than one statement with nullable
-- parameters. `endpoints_kind_fields` requires a status code and no collection
-- for 'static' and the reverse for 'collection', so a single statement would be
-- one the constraint can refuse depending on what was passed — and the failure
-- would arrive as a check violation rather than as a message beside a field.
-- Two statements each satisfy the constraint by construction.

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

-- CreateCollectionEndpoint joins collections as well as projects, so that a
-- collection id belonging to another project cannot be attached to this one.
--
-- name: CreateCollectionEndpoint :one
insert into endpoints (
    project_id, method, path_pattern, kind, is_enabled, delay_ms,
    collection_id, response_headers
)
select @project_id, @method, @path_pattern, 'collection', @is_enabled, @delay_ms,
       @collection_id, @response_headers
from projects p
join collections c on c.project_id = p.id
where p.id = @project_id and p.owner_id = @owner_id and c.id = @collection_id
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

-- Both update statements write `kind` and both sides of the kind-specific
-- columns, so that changing an endpoint from one kind to the other leaves no
-- remnant of what it was. Setting only the columns the new kind uses would
-- leave the old ones populated and the check constraint would refuse the row.
--
-- name: UpdateEndpoint :one
update endpoints e
set method           = @method,
    path_pattern     = @path_pattern,
    kind             = 'static',
    is_enabled       = @is_enabled,
    delay_ms         = @delay_ms,
    status_code      = @status_code,
    response_body    = @response_body,
    collection_id    = null,
    response_headers = @response_headers,
    updated_at       = now()
from projects p
where e.id = @id and p.id = e.project_id and p.owner_id = @owner_id
returning e.*;

-- name: UpdateCollectionEndpoint :one
update endpoints e
set method           = @method,
    path_pattern     = @path_pattern,
    kind             = 'collection',
    is_enabled       = @is_enabled,
    delay_ms         = @delay_ms,
    status_code      = null,
    response_body    = null,
    collection_id    = @collection_id,
    response_headers = @response_headers,
    updated_at       = now()
from projects p, collections c
where e.id = @id and p.id = e.project_id and p.owner_id = @owner_id
  and c.id = @collection_id and c.project_id = p.id
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
