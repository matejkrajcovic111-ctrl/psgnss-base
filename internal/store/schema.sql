-- PSGNSS_base schema.
-- Applied idempotently at startup; schema_version gates migrations.

-- Journal mode and foreign keys are set by Open/migrate before the transaction.
-- This is the v1 baseline. Later schema changes live in migrate(), so fresh
-- databases and upgrades follow the same migration path.

CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);

-- ---------------------------------------------------------------- users

CREATE TABLE IF NOT EXISTS users (
    id                INTEGER PRIMARY KEY,
    username          TEXT    NOT NULL UNIQUE,
    -- AES-256-GCM ciphertext, not a hash. Admins can view passwords in the
    -- UI by design; see README for the trade-off.
    password_enc      BLOB    NOT NULL,
    password_nonce    BLOB    NOT NULL,
    connection_limit  INTEGER NOT NULL DEFAULT 5,
    enabled           INTEGER NOT NULL DEFAULT 1,
    note              TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

-- Which mountpoints a user may read. No rows = all mountpoints.
CREATE TABLE IF NOT EXISTS user_mountpoints (
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    mountpoint    TEXT    NOT NULL,
    PRIMARY KEY (user_id, mountpoint)
);

-- ------------------------------------------------------- connection log
-- One row per connection, closed out on disconnect. This is what makes
-- "where did this user connect from" answerable.

CREATE TABLE IF NOT EXISTS connections (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER REFERENCES users(id) ON DELETE SET NULL,
    username       TEXT    NOT NULL,      -- denormalised: survives user deletion
    mountpoint     TEXT    NOT NULL,
    -- Real client address. Behind the VPS this comes from the PROXY header,
    -- not the socket, which would otherwise show the proxy for every rover.
    client_ip      TEXT    NOT NULL,
    client_port    INTEGER NOT NULL DEFAULT 0,
    via_proxy      INTEGER NOT NULL DEFAULT 0,
    proxy_ip       TEXT    NOT NULL DEFAULT '',
    user_agent     TEXT    NOT NULL DEFAULT '',
    ntrip_version  INTEGER NOT NULL DEFAULT 1,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER,               -- NULL while still connected
    bytes_sent     INTEGER NOT NULL DEFAULT 0,
    bytes_recv     INTEGER NOT NULL DEFAULT 0,
    nmea_count     INTEGER NOT NULL DEFAULT 0,
    disconnect_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_conn_user    ON connections(user_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_conn_started ON connections(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_conn_live    ON connections(ended_at) WHERE ended_at IS NULL;

-- ------------------------------------------------------------ telemetry
-- One compact blob per epoch rather than a row per satellite-signal.
-- At ~30 satellites x 3 signals that is ~90x fewer rows.

CREATE TABLE IF NOT EXISTS telemetry_epoch (
    ts          INTEGER PRIMARY KEY,      -- unix seconds
    -- Packed per-satellite records decoded from UBX-NAV-SAT / NAV-SIG:
    -- gnss_id, sv_id, elevation, azimuth, and C/N0 per signal.
    sat_blob    BLOB    NOT NULL,
    sat_count   INTEGER NOT NULL,
    fix_type    INTEGER NOT NULL DEFAULT 0,
    -- Decimation tier: 1 = fine (recent), 0 = coarse (aged down).
    fine        INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_tel_fine ON telemetry_epoch(fine, ts);

-- Stream throughput and host health, sampled on the coarse interval.
CREATE TABLE IF NOT EXISTS health_sample (
    ts            INTEGER PRIMARY KEY,
    hub_bytes_in  INTEGER NOT NULL DEFAULT 0,
    rtcm_bps      INTEGER NOT NULL DEFAULT 0,
    ubx_bps       INTEGER NOT NULL DEFAULT 0,
    clients       INTEGER NOT NULL DEFAULT 0,
    cpu_pct       REAL    NOT NULL DEFAULT 0,
    temp_c        REAL    NOT NULL DEFAULT 0,
    disk_free_mb  INTEGER NOT NULL DEFAULT 0,
    mem_free_mb   INTEGER NOT NULL DEFAULT 0
);

-- ------------------------------------------------- receiver config audit
-- Every write to the receiver, with the read-back verification result.
-- Nothing changes the hardware without leaving a row here.

CREATE TABLE IF NOT EXISTS receiver_config_log (
    id           INTEGER PRIMARY KEY,
    ts           INTEGER NOT NULL,
    actor        TEXT    NOT NULL,       -- admin username, or 'system'
    key_id       TEXT    NOT NULL,       -- e.g. '0x20030009'
    key_name     TEXT    NOT NULL DEFAULT '',
    old_value    TEXT    NOT NULL DEFAULT '',
    new_value    TEXT    NOT NULL DEFAULT '',
    layers       TEXT    NOT NULL DEFAULT 'RAM,BBR,Flash',
    verified     INTEGER NOT NULL DEFAULT 0,
    reverted     INTEGER NOT NULL DEFAULT 0,
    note         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_rcfg_ts ON receiver_config_log(ts DESC);

-- -------------------------------------------------------------- admins

CREATE TABLE IF NOT EXISTS admins (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    -- Admin login IS hashed (argon2id). Only NTRIP passwords are reversible.
    password_hash TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_login    INTEGER
);

CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT    PRIMARY KEY,
    admin_id   INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    client_ip  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sess_expiry ON sessions(expires_at);
