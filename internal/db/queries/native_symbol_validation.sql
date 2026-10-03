-- name: GetNativeSymbolArtifactContentForValidation :one
-- Health reads may inspect one small Flutter artifact to distinguish a
-- recognised debug container from malformed bytes. The size predicate is part
-- of the query so an authenticated health request can never make the API load
-- a full 30 MiB artifact into memory; larger artifacts stay not_checked.
SELECT content
FROM native_symbol_artifacts
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND org_id = sqlc.arg(org_id)
  AND size <= LEAST(sqlc.arg(max_bytes), 8388608);
