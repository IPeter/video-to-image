-- Queries for the stats and retention maintenance jobs.

-- name: TableRowEstimates :many
-- pg_class.reltuples is -1 for a table that was never vacuumed or analyzed.
SELECT c.relname::text AS table_name, c.reltuples::float8 AS estimate
FROM pg_catalog.pg_class AS c
WHERE c.oid IN (SELECT to_regclass(t) FROM unnest(@names::text[]) AS t);

-- name: CountJobsByStatus :many
SELECT status, count(*)::bigint AS jobs
FROM jobs
GROUP BY status;
