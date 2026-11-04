alter table refs add column peeled bytea;
alter table refs add constraint refs_peeled check (
    peeled is null or (object_id is not null and octet_length(peeled) = 20));
