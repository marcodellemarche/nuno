-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Every applied change, tied to the run that made it (FR-33). There is no
-- rollback for a quota already written, so this table is what explains what
-- happened.

-- +goose Up

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER REFERENCES reconcile_runs (id) ON DELETE SET NULL,
    at          TEXT    NOT NULL,
    actor       TEXT    NOT NULL,
    user_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    provider_id INTEGER REFERENCES providers (id) ON DELETE SET NULL,
    external_id TEXT    NOT NULL DEFAULT '',
    field       TEXT    NOT NULL DEFAULT 'quota',
    -- The tagged form, or unlimited and unknown collide in the one table
    -- meant to explain what happened (ADR-0011).
    from_value  TEXT    NOT NULL,
    to_value    TEXT    NOT NULL,
    result      TEXT    NOT NULL CHECK (result IN ('applied', 'failed', 'refused', 'skipped')),
    detail      TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX audit_log_user ON audit_log (user_id, id DESC);
CREATE INDEX audit_log_run ON audit_log (run_id);

-- Notifications for a persistently guarded change fire on first appearance
-- and on change, never once per cycle, which needs the key to be remembered
-- (FR-63).
CREATE TABLE notified_changes (
    change_key TEXT NOT NULL PRIMARY KEY,
    first_seen TEXT NOT NULL,
    last_seen  TEXT NOT NULL
) STRICT;

-- +goose Down

DROP TABLE notified_changes;
DROP TABLE audit_log;
