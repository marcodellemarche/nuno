-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Policy: tiers, allocations, the group-to-tier edge, and the two kinds of
-- override. Allocations key on the provider instance, not on its type: keying
-- them on type while accounts key on instance is the inconsistency that would
-- force a migration the moment someone adds a second Nextcloud (FR-27).

-- +goose Up

CREATE TABLE tiers (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    -- Tagged, because a tier with only absolute or unlimited allocations has
    -- no meaningful budget and must not compete as zero (ADR-0020).
    budget     TEXT    NOT NULL DEFAULT 'unknown',
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
) STRICT;

-- One default tier at most, enforced rather than hoped for.
CREATE UNIQUE INDEX tiers_single_default ON tiers (is_default) WHERE is_default = 1;

-- No row means this tier is silent about that provider. Absent is not zero and
-- not unlimited, which is why absence is the lack of a row (FR-17).
CREATE TABLE allocations (
    tier_id     INTEGER NOT NULL REFERENCES tiers (id) ON DELETE CASCADE,
    provider_id INTEGER NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    mode        TEXT    NOT NULL CHECK (mode IN ('absolute', 'percent', 'unlimited')),
    value       INTEGER NOT NULL DEFAULT 0 CHECK (value >= 0),
    PRIMARY KEY (tier_id, provider_id)
) STRICT;

-- The edge FR-4 depends on. Keyed on the group row, whose identity is the
-- directory uuid, so renaming a group does not move everyone to the default.
CREATE TABLE group_tiers (
    group_id INTEGER NOT NULL PRIMARY KEY REFERENCES "groups" (id) ON DELETE CASCADE,
    tier_id  INTEGER NOT NULL REFERENCES tiers (id) ON DELETE CASCADE
) STRICT;

CREATE INDEX group_tiers_tier ON group_tiers (tier_id);

-- A tier assigned straight to a person, which replaces the group set (FR-5).
CREATE TABLE user_tier_overrides (
    user_id INTEGER NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    tier_id INTEGER NOT NULL REFERENCES tiers (id) ON DELETE CASCADE
) STRICT;

-- Above every tier, per provider. This is where nuno adopt stores what it
-- imports, so two people with different hand-set quotas need no tier each
-- (FR-15, ADR-0020).
CREATE TABLE user_provider_overrides (
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider_id INTEGER NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    quota       TEXT    NOT NULL,
    origin      TEXT    NOT NULL DEFAULT 'manual' CHECK (origin IN ('manual', 'adopted')),
    updated_at  TEXT    NOT NULL,
    PRIMARY KEY (user_id, provider_id)
) STRICT;

-- A run is one pass: plan now, apply later. The snapshot is what the plan was
-- computed from, because apply re-observes and refuses anything whose
-- classification moved (FR-38b, ADR-0016).
CREATE TABLE reconcile_runs (
    id             INTEGER PRIMARY KEY,
    started_at     TEXT    NOT NULL,
    finished_at    TEXT,
    mode           TEXT    NOT NULL CHECK (mode IN ('plan', 'dry-run', 'apply')),
    actor          TEXT    NOT NULL,
    consent_shrink INTEGER NOT NULL DEFAULT 0 CHECK (consent_shrink IN (0, 1)),
    status         TEXT    NOT NULL DEFAULT 'running',
    summary        TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE plans (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES reconcile_runs (id) ON DELETE CASCADE,
    computed_at TEXT    NOT NULL,
    -- The changes and the observed values they were computed from, as JSON.
    body        TEXT    NOT NULL
) STRICT;

CREATE INDEX plans_run ON plans (run_id);

-- +goose Down

DROP TABLE plans;
DROP TABLE reconcile_runs;
DROP TABLE user_provider_overrides;
DROP TABLE user_tier_overrides;
DROP TABLE group_tiers;
DROP TABLE allocations;
DROP INDEX tiers_single_default;
DROP TABLE tiers;
