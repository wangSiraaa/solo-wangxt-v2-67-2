-- Registry schema: packages, immutable versions, cross-package
-- dependency locks, consumer declarations, compatibility reports and
-- content-addressed impact analyses.

CREATE TABLE IF NOT EXISTS packages (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS versions (
    id             BIGSERIAL PRIMARY KEY,
    package_id     BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    version        TEXT NOT NULL,
    content_hash   BYTEA NOT NULL,
    descriptor_set BYTEA NOT NULL,
    owned_paths    JSONB NOT NULL,
    -- Dependency lock summary recorded at registration time. Both are
    -- immutable history: later registrations never rewrite them.
    locks          JSONB NOT NULL DEFAULT '[]'::jsonb,
    lock_digest    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Immutability anchor: the service rejects same-version/different-content
    -- writes; it never updates this row.
    UNIQUE (package_id, version)
);

-- Backfill-friendly column additions for pre-lock databases.
ALTER TABLE versions ADD COLUMN IF NOT EXISTS locks JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE versions ADD COLUMN IF NOT EXISTS lock_digest TEXT NOT NULL DEFAULT '';

-- Owned file path -> owning package. Paths are globally unique; the
-- (path) uniqueness lets concurrent registrations fail atomically if two
-- packages try to claim the same file. Every version of a package owns
-- the same paths, so the row is keyed by path alone.
CREATE TABLE IF NOT EXISTS package_paths (
    path       TEXT PRIMARY KEY,
    package_id BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE
);

-- One row per resolved dependency edge of a version. Duplicated on
-- purpose in versions.locks (single-row read) for convenient fetches.
CREATE TABLE IF NOT EXISTS version_dependencies (
    id                  BIGSERIAL PRIMARY KEY,
    version_id          BIGINT NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    dep_package_id      BIGINT NOT NULL REFERENCES packages(id) ON DELETE RESTRICT,
    dep_version         TEXT NOT NULL,
    dep_content_hash    BYTEA NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (version_id, dep_package_id)
);

CREATE INDEX IF NOT EXISTS version_dependencies_lookup
    ON version_dependencies (dep_package_id, dep_version);

CREATE TABLE IF NOT EXISTS reports (
    id           BIGSERIAL PRIMARY KEY,
    package_id   BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    base_version TEXT NOT NULL,
    head_version TEXT NOT NULL,
    verdict      TEXT NOT NULL,
    report       JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS reports_package_lookup
    ON reports (package_id, base_version, head_version);

CREATE TABLE IF NOT EXISTS consumers (
    id         BIGSERIAL PRIMARY KEY,
    package_id BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    consumer   TEXT NOT NULL,
    encoding   TEXT NOT NULL CHECK (encoding IN ('wire', 'json', 'both')),
    usages     JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (package_id, consumer)
);

-- Impact analyses are memoized by the digest of their exact input
-- (analyzed triple + the full snapshot of content hashes and locks).
-- Repeating an identical analysis reads this row; historical results are
-- never rewritten when newer package versions get registered later.
CREATE TABLE IF NOT EXISTS impact_analyses (
    id           BIGSERIAL PRIMARY KEY,
    input_digest TEXT NOT NULL UNIQUE,
    package_id   BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    base_version TEXT NOT NULL,
    analysis     JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
