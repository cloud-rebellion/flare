-- +goose Up
-- Native/mobile debug information is kept separate from hot telemetry.  The
-- artifact may contain source paths or private symbols, so APIs expose only
-- its bounded metadata and digest; event payloads never embed this content.
CREATE TABLE native_symbol_artifacts (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    org_id     TEXT NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    release    TEXT NOT NULL CHECK (octet_length(release) BETWEEN 1 AND 128),
    platform   TEXT NOT NULL CHECK (platform IN ('flutter', 'android', 'ios')),
    format     TEXT NOT NULL CHECK (format IN ('dart_symbols', 'proguard', 'breakpad', 'dsym', 'elf')),
    name       TEXT NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128),
    digest     TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    size       BIGINT NOT NULL CHECK (size > 0 AND size <= 31457280),
    content    BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, release, platform, format, name)
);

CREATE INDEX idx_native_symbol_artifacts_project_release
    ON native_symbol_artifacts (project_id, org_id, release, platform);

-- +goose Down
DROP TABLE IF EXISTS native_symbol_artifacts;
