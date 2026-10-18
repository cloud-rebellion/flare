-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Existing keys retain their API authority. Dedicated MCP credentials cannot
-- authenticate REST and always expire within the reviewed bounded lifetime.
ALTER TABLE api_keys
    ADD COLUMN purpose TEXT NOT NULL DEFAULT 'api',
    ADD CONSTRAINT api_keys_purpose_check CHECK (purpose IN ('api','mcp')),
    ADD CONSTRAINT api_keys_mcp_expiry_check CHECK (
        purpose <> 'mcp' OR (expires_at IS NOT NULL AND expires_at > created_at
                            AND expires_at <= created_at + interval '90 days')
    );

-- +goose Down
-- Never turn an MCP-only token into a broader API credential on rollback.
DELETE FROM api_keys WHERE purpose='mcp';
ALTER TABLE api_keys DROP CONSTRAINT api_keys_mcp_expiry_check,
    DROP CONSTRAINT api_keys_purpose_check, DROP COLUMN purpose;
