-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Per-provider observation state. The usage API derives its per-provider
-- status from these (ADR-0022, ADR-0026), and they have to survive a restart:
-- a page that cannot tell "never read" from "read and failed" would report a
-- stale ceiling as current.

-- +goose Up

ALTER TABLE providers ADD COLUMN version TEXT NOT NULL DEFAULT '';
ALTER TABLE providers ADD COLUMN reachable INTEGER NOT NULL DEFAULT 0 CHECK (reachable IN (0, 1));
ALTER TABLE providers ADD COLUMN in_supported INTEGER NOT NULL DEFAULT 0 CHECK (in_supported IN (0, 1));
ALTER TABLE providers ADD COLUMN last_observe_at TEXT;
ALTER TABLE providers ADD COLUMN last_observe_ok INTEGER NOT NULL DEFAULT 0 CHECK (last_observe_ok IN (0, 1));
ALTER TABLE providers ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
-- Why the provider refuses to apply while still reading, for example a
-- competing writer (FR-75). Empty means not degraded.
ALTER TABLE providers ADD COLUMN degraded_reason TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE providers DROP COLUMN degraded_reason;
ALTER TABLE providers DROP COLUMN last_error;
ALTER TABLE providers DROP COLUMN last_observe_ok;
ALTER TABLE providers DROP COLUMN last_observe_at;
ALTER TABLE providers DROP COLUMN in_supported;
ALTER TABLE providers DROP COLUMN reachable;
ALTER TABLE providers DROP COLUMN version;
