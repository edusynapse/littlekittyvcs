create table repositories (
    id          uuid primary key,
    name        text not null unique
                check (name ~ '^[a-z0-9][a-z0-9._-]{0,100}$' and name not like '%..%'),
    push_seq    bigint not null default 0,
    created_at  timestamptz not null default now()
);

create table refs (
    repo_id     uuid not null references repositories(id) on delete cascade,
    name        text not null,
    object_id   bytea,
    symbolic    text,
    primary key (repo_id, name),
    check ((object_id is null) <> (symbolic is null)),
    check (object_id is null or octet_length(object_id) = 20)
);

create table packs (
    id          uuid primary key,
    repo_id     uuid not null references repositories(id) on delete cascade,
    pack_key    text not null unique,
    idx_key     text not null unique,
    bytes       bigint not null check (bytes > 0),
    idx_bytes   bigint not null check (idx_bytes > 0),
    created_at  timestamptz not null default now()
);
create index packs_repo_created on packs (repo_id, created_at);

create table tokens (
    id          uuid primary key,
    token_hash  bytea not null unique check (octet_length(token_hash) = 32),
    label       text not null check (length(label) between 1 and 200),
    write       boolean not null,
    admin       boolean not null,
    repo_id     uuid references repositories(id) on delete cascade,
    created_at  timestamptz not null default now(),
    check (not admin or write),
    check (not (admin and repo_id is not null))
);
