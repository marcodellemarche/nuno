-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- The M1 tables: identity, observation and linking. Policy tables (tiers,
-- allocations, overrides) arrive with M2, runs and audit with M3.

-- +goose Up

-- A provider is an instance, not a type, from the first migration. Keying
-- accounts on the instance is what avoids a migration when a second Nextcloud
-- appears (FR-27).
CREATE TABLE providers (
    id              INTEGER PRIMARY KEY,
    type            TEXT    NOT NULL,
    name            TEXT    NOT NULL UNIQUE,
    match_key       TEXT    NOT NULL CHECK (match_key IN ('subject', 'email', 'username')),
    config_ref      TEXT    NOT NULL,
    -- The outcome of the write probe, which runs after linking and not from
    -- Health(). Unproven is not the same as no. See ADR-0025.
    write_access    TEXT    NOT NULL DEFAULT 'unproven' CHECK (write_access IN ('unproven', 'yes', 'no')),
    write_probed_at TEXT,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
) STRICT;

-- Identity is (source, source_uuid). uid and email are attributes, so a rename
-- in the directory loses nothing (FR-9a, ADR-0013).
CREATE TABLE users (
    id               INTEGER PRIMARY KEY,
    source           TEXT    NOT NULL CHECK (source IN ('lldap', 'manual')),
    source_uuid      TEXT    NOT NULL,
    uid              TEXT    NOT NULL,
    email            TEXT    NOT NULL DEFAULT '',
    email_normalized TEXT    NOT NULL DEFAULT '',
    display_name     TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'orphaned')),
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL,
    UNIQUE (source, source_uuid)
) STRICT;

CREATE INDEX users_email_normalized ON users (email_normalized) WHERE email_normalized <> '';
CREATE INDEX users_uid ON users (uid);

CREATE TABLE "groups" (
    id           INTEGER PRIMARY KEY,
    source       TEXT    NOT NULL CHECK (source IN ('lldap', 'manual')),
    source_uuid  TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    display_name TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,
    UNIQUE (source, source_uuid)
) STRICT;

CREATE TABLE user_groups (
    user_id  INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    group_id INTEGER NOT NULL REFERENCES "groups" (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, group_id)
) STRICT;

CREATE INDEX user_groups_group ON user_groups (group_id);

-- Observed state, keyed by the provider's own account. A null user_id is what
-- makes an unmanaged account representable (FR-3).
CREATE TABLE external_accounts (
    provider_id      INTEGER NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    external_id      TEXT    NOT NULL,
    user_id          INTEGER REFERENCES users (id) ON DELETE SET NULL,
    subject          TEXT    NOT NULL DEFAULT '',
    username         TEXT    NOT NULL DEFAULT '',
    email            TEXT    NOT NULL DEFAULT '',
    email_normalized TEXT    NOT NULL DEFAULT '',
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    deleted          INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
    -- Tagged form ('unknown', 'unlimited', 'bytes:N'), or unlimited and
    -- unknown collide. See ADR-0011.
    quota            TEXT    NOT NULL DEFAULT 'unknown',
    used             TEXT    NOT NULL DEFAULT 'unknown',
    -- The account exists and holds nothing, as opposed to unreadable usage.
    -- See ADR-0024.
    never_used       INTEGER NOT NULL DEFAULT 0 CHECK (never_used IN (0, 1)),
    observed_at      TEXT    NOT NULL,
    observe_ok       INTEGER NOT NULL DEFAULT 1 CHECK (observe_ok IN (0, 1)),
    PRIMARY KEY (provider_id, external_id)
) STRICT;

CREATE INDEX external_accounts_user ON external_accounts (user_id);
CREATE INDEX external_accounts_subject ON external_accounts (provider_id, subject) WHERE subject <> '';
CREATE INDEX external_accounts_email ON external_accounts (provider_id, email_normalized) WHERE email_normalized <> '';

-- Only real links live here. The unique constraint on (provider_id,
-- external_id) is what prevents two people's policies from writing to one
-- account (ADR-0013).
CREATE TABLE account_links (
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider_id INTEGER NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL,
    origin      TEXT    NOT NULL CHECK (origin IN ('matched', 'manual')),
    linked_at   TEXT    NOT NULL,
    PRIMARY KEY (user_id, provider_id),
    UNIQUE (provider_id, external_id)
) STRICT;

-- Match problems, recomputed every observe. They feed FR-57 as a list, not a
-- log line.
CREATE TABLE link_issues (
    id          INTEGER PRIMARY KEY,
    provider_id INTEGER NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL CHECK (kind IN ('unlinked', 'ambiguous', 'dangling', 'stale', 'unmanaged')),
    user_id     INTEGER REFERENCES users (id) ON DELETE CASCADE,
    external_id TEXT    NOT NULL DEFAULT '',
    detail      TEXT    NOT NULL DEFAULT '',
    computed_at TEXT    NOT NULL
) STRICT;

CREATE INDEX link_issues_provider ON link_issues (provider_id, kind);

-- Capability keys: 32 random bytes, shown once, stored hashed, revocable. The
-- value itself never reaches this table.
CREATE TABLE admin_keys (
    id           INTEGER PRIMARY KEY,
    hash         TEXT NOT NULL UNIQUE,
    label        TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL DEFAULT 'ui' CHECK (source IN ('ui', 'env')),
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
) STRICT;

CREATE TABLE settings (
    key        TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- +goose Down

DROP TABLE settings;
DROP TABLE admin_keys;
DROP TABLE link_issues;
DROP TABLE account_links;
DROP TABLE external_accounts;
DROP TABLE user_groups;
DROP TABLE "groups";
DROP TABLE users;
DROP TABLE providers;
