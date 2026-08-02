CREATE TABLE IF NOT EXISTS jobs (
    id            TEXT PRIMARY KEY,
    type          TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    source        TEXT NOT NULL DEFAULT '',
    params        TEXT NOT NULL DEFAULT '{}',
    input_path    TEXT NOT NULL DEFAULT '',
    output_path   TEXT NOT NULL DEFAULT '',
    output_size   INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    progress      REAL NOT NULL DEFAULT 0,
    stage         TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    started_at    INTEGER,
    finished_at   INTEGER,
    expires_at    INTEGER
);

-- Ascending keys: listing sorts by (created_at DESC, rowid DESC), which SQLite
-- serves by walking an ascending index backwards with no temp b-tree, in both
-- the unfiltered and the status-filtered case. The two superseded indexes are
-- dropped by their old names - indexes only, never rows - and the IF NOT EXISTS
-- creates below make this a one-time swap on an existing database.
DROP INDEX IF EXISTS idx_jobs_created_at;
DROP INDEX IF EXISTS idx_jobs_status;
CREATE INDEX IF NOT EXISTS idx_jobs_created         ON jobs(created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_status_created  ON jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_expires_at      ON jobs(expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Login sessions. Kept here rather than in memory so that restarting the
-- container (an image upgrade, a crash, a compose restart) does not sign
-- everyone out.
CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

-- The credential the sessions above were issued under, as a single row. A
-- mismatch at startup drops them all, so rotating AUTH_PASSWORD_HASH also
-- revokes cookies issued before the rotation. A table of its own rather than a
-- column on sessions: this file re-runs against existing databases on every
-- start and ALTER TABLE ADD COLUMN is not idempotent.
CREATE TABLE IF NOT EXISTS session_credential (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    fingerprint TEXT NOT NULL
);
