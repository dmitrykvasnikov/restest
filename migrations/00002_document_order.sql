-- +goose Up
-- +goose StatementBegin

-- Documents need a stable order, and neither column already on the table gives
-- one. `created_at` is identical for every row of a seed, because they are
-- inserted by a single statement and now() is fixed for the transaction; `id` is
-- a random uuid. Ordering by either would let two requests for the same page of
-- the same data return different rows, which is the one thing a mock server
-- exists not to do.
--
-- `seq` is insertion order, and it is what an unsorted listing is ordered by. It
-- is also the tie-break under `_sort`, so a field with equal values in several
-- documents still paginates deterministically.
alter table documents
    add column seq bigint not null generated always as identity;

-- The listing index moves with the ordering. (collection_id, created_at) was
-- never read by anything and would only be dead weight beside the new one.
drop index documents_listing_idx;
create index documents_order_idx on documents (collection_id, seq);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop index if exists documents_order_idx;
create index documents_listing_idx on documents (collection_id, created_at);
alter table documents drop column if exists seq;
-- +goose StatementEnd
