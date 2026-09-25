-- Registry schema: packages, immutable versions, consumer declarations,
-- and persisted compatibility reports.

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
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Immutability anchor: the service rejects same-version/different-content
    -- writes; it never updates this row.
    UNIQUE (package_id, version)
);

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

-- Import-path ownership: an import path resolves to the package version
-- that most recently registered a file at that path. This is how
-- cross-package imports are pinned at registration time.
CREATE TABLE IF NOT EXISTS path_owners (
    path        TEXT PRIMARY KEY,
    package_id  BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    version     TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Dependency locks: each registered version pins every direct dependency
-- to an immutable (dep_package, dep_version) pair plus the content digest
-- that pair had when the lock was taken. Written atomically with the
-- version row; never updated afterwards.
CREATE TABLE IF NOT EXISTS version_deps (
    id          BIGSERIAL PRIMARY KEY,
    package_id  BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    version     TEXT NOT NULL,
    dep_package TEXT NOT NULL,
    dep_version TEXT NOT NULL,
    dep_hash    BYTEA NOT NULL,
    UNIQUE (package_id, version, dep_package)
);

CREATE INDEX IF NOT EXISTS version_deps_reverse
    ON version_deps (dep_package);

-- Impact analyses, keyed by their exact input so repeated analysis of the
-- same (package, base, head) returns the same stored result.
CREATE TABLE IF NOT EXISTS impact_analyses (
    id           BIGSERIAL PRIMARY KEY,
    package_id   BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    base_version TEXT NOT NULL,
    head_version TEXT NOT NULL,
    input_hash   BYTEA NOT NULL,
    result       JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (package_id, base_version, head_version)
);
