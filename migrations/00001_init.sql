-- +goose Up
-- +goose StatementBegin

create extension if not exists citext;

-- ---------------------------------------------------------------- users

create table users (
    id            uuid        primary key default gen_random_uuid(),
    email         citext      not null unique,
    password_hash text        not null,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);

-- Session store schema required by alexedwards/scs postgresstore.
create table sessions (
    token  text        primary key,
    data   bytea       not null,
    expiry timestamptz not null
);
create index sessions_expiry_idx on sessions (expiry);

create table api_tokens (
    id           uuid        primary key default gen_random_uuid(),
    user_id      uuid        not null references users (id) on delete cascade,
    name         text        not null,
    -- Non-secret leading characters, shown in the UI so a token is identifiable
    -- after its plaintext has been discarded.
    prefix       text        not null,
    token_hash   bytea       not null unique,
    last_used_at timestamptz,
    expires_at   timestamptz,
    created_at   timestamptz not null default now()
);
create index api_tokens_user_id_idx on api_tokens (user_id);

-- ------------------------------------------------------------- projects

create table projects (
    id         uuid        primary key default gen_random_uuid(),
    owner_id   uuid        not null references users (id) on delete cascade,
    -- Appears in mock URLs as /m/{slug}/... . Because all mock traffic lives
    -- under the /m/ prefix, a slug can never collide with an application route.
    slug       text        not null unique,
    name       text        not null,
    is_demo    boolean     not null default false,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),

    constraint projects_slug_format
        check (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$')
);
create index projects_owner_id_idx on projects (owner_id);

-- ---------------------------------------------------------- collections

create table collections (
    id          uuid        primary key default gen_random_uuid(),
    project_id  uuid        not null references projects (id) on delete cascade,
    name        text        not null,
    -- Documents restored by "reset to seed".
    seed        jsonb       not null default '[]'::jsonb,
    -- Which JSON field carries the identifier exposed in URLs.
    id_field    text        not null default 'id',
    id_strategy text        not null default 'serial',
    -- Counter for id_strategy = 'serial'. Allocated with UPDATE ... RETURNING
    -- so concurrent creates cannot collide.
    next_serial bigint      not null default 1,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),

    constraint collections_seed_is_array check (jsonb_typeof(seed) = 'array'),
    constraint collections_id_strategy    check (id_strategy in ('serial', 'uuid')),
    unique (project_id, name)
);

create table documents (
    id            uuid        primary key default gen_random_uuid(),
    collection_id uuid        not null references collections (id) on delete cascade,
    -- The identifier as clients see it: "1", "42", or a uuid string. Kept
    -- separate from the internal primary key so seeded data keeps its own ids.
    public_id     text        not null,
    body          jsonb       not null,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),

    constraint documents_body_is_object check (jsonb_typeof(body) = 'object'),
    unique (collection_id, public_id)
);
-- Supports arbitrary field filters (?status=active) without a per-collection schema.
create index documents_body_gin_idx on documents using gin (body jsonb_path_ops);
create index documents_listing_idx  on documents (collection_id, created_at);

-- ------------------------------------------------------------ endpoints

create table endpoints (
    id            uuid        primary key default gen_random_uuid(),
    project_id    uuid        not null references projects (id) on delete cascade,
    -- Uppercase verb, or '*' to match any method.
    method        text        not null,
    -- Named parameters in braces: /users/{id}/posts
    path_pattern  text        not null,
    kind          text        not null,
    is_enabled    boolean     not null default true,
    delay_ms      integer     not null default 0,

    -- kind = 'static'
    status_code   smallint,
    -- Stored as text, not jsonb: responses are not always JSON, and these
    -- bodies are served verbatim rather than queried.
    response_body text,

    -- kind = 'collection'. One row expands to the full CRUD route set
    -- (list, get, create, replace, patch, delete) rooted at path_pattern.
    collection_id uuid        references collections (id) on delete cascade,

    response_headers jsonb    not null default '{}'::jsonb,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),

    constraint endpoints_method_valid check (
        method in ('GET','POST','PUT','PATCH','DELETE','HEAD','OPTIONS','*')),
    constraint endpoints_path_absolute check (path_pattern like '/%'),
    constraint endpoints_delay_sane    check (delay_ms between 0 and 60000),
    constraint endpoints_kind_fields   check (
        (kind = 'static'
            and status_code is not null
            and collection_id is null)
     or (kind = 'collection'
            and collection_id is not null
            and status_code is null)
    ),
    unique (project_id, method, path_pattern)
);
create index endpoints_project_id_idx on endpoints (project_id);

-- ------------------------------------------------------------ exchanges

-- One request/response pair. The `direction` column is what lets the phase 2
-- outbound runner reuse this table instead of introducing a parallel one.
--
-- Deliberately carries no foreign keys: it is the write-heavy table, and rows
-- are discarded wholesale by dropping partitions rather than by cascade.
create table exchanges (
    id                      uuid        not null default gen_random_uuid(),
    project_id              uuid        not null,
    endpoint_id             uuid,
    direction               text        not null default 'inbound',
    matched                 boolean     not null,

    method                  text        not null,
    path                    text        not null,
    query                   text,
    request_headers         jsonb       not null default '{}'::jsonb,
    request_body            bytea,
    request_body_truncated  boolean     not null default false,

    status_code             smallint,
    response_headers        jsonb       not null default '{}'::jsonb,
    response_body           bytea,
    response_body_truncated boolean     not null default false,

    duration_ms             integer     not null,
    remote_addr             inet,
    created_at              timestamptz not null default now(),

    constraint exchanges_direction_valid check (direction in ('inbound', 'outbound')),
    -- Partition key must be part of the primary key.
    primary key (id, created_at)
) partition by range (created_at);

-- Drives the inspector's per-project listing.
create index exchanges_project_recent_idx on exchanges (project_id, created_at desc);

-- Bootstrap partitions. Future months are created by a scheduled job in the
-- application; the default partition catches anything that job has not covered
-- yet so writes can never fail.
create table exchanges_default partition of exchanges default;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists exchanges;
drop table if exists endpoints;
drop table if exists documents;
drop table if exists collections;
drop table if exists projects;
drop table if exists api_tokens;
drop table if exists sessions;
drop table if exists users;
-- +goose StatementEnd
