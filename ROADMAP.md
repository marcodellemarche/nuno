# Roadmap

Milestones are intentionally small. Each one ends with something runnable and demonstrable, so an agent or a human can iterate without a big-bang rewrite.

## M0: design freeze (done, 2026-09-15)

Closed in two passes. The first (2026-09-14) resolved the ten decisions the design needed. The second (2026-09-15) reviewed that design against provider source code and against the stack Nuno will run on, and found enough wrong to justify a second round: ADR-0011 to ADR-0024 supersede or extend six of the originals, the last five written against responses captured from the live stack.

- [x] Problem statement, requirements, architecture
- [x] ADR-0001 stack: Go
- [x] ADR-0002 source of truth: Nuno's database owns policy, LLDAP owns identity
- [x] ADR-0003 budget splitting: percent, absolute or unlimited; absent means do not touch
- [x] ADR-0004 license: AGPL-3.0-or-later
- [x] ADR-0006 UI: server-rendered, HTMX, no build step
- [x] ADR-0011 quota is a tagged value, compared after per-provider normalization
- [x] ADR-0012 tier resolution happens per provider (supersedes ADR-0007)
- [x] ADR-0013 UUID-based identity and the link lifecycle (supersedes part of ADR-0008)
- [x] ADR-0014 provider write paths as they actually are
- [x] ADR-0015 version policy, simplified (supersedes ADR-0005)
- [x] ADR-0016 safety model: unknown state, re-check at apply, honest partial application
- [x] ADR-0017 usage API: shared dashboard first (supersedes ADR-0010)
- [x] ADR-0018 one writer: the CLI talks to the server
- [x] ADR-0019 what v0.1 does not do
- [x] ADR-0020 budget is an input to percentages and an output to people
- [x] ADR-0021 corrections from the captured responses (corrects ADR-0014)
- [x] ADR-0022 what the usage endpoint means before policy exists
- [x] ADR-0023 Nuno diagnoses, it does not reconfigure
- [x] ADR-0024 a person who has never logged in still gets their quota

The provider claims in `ARCHITECTURE.md` are now verified against Nextcloud `stable34` and Immich v3.2.0 (spec `v3.2.1`) rather than written from memory.

## M1: one page, both services, read only

Starting M1 surfaced three things the design had not settled, so they were decided first: ADR-0025 moves the write probe out of `Health()`, ADR-0026 gives the usage contract a representation for unknown, and ADR-0027 bounds what FR-75 can actually detect.

- [ ] Project scaffold, `Dockerfile`, `docker-compose.yml`, `/healthz`, SPDX headers on every source file (`LICENSE` is already in the tree)
- [ ] Config loading (env and file) with secret redaction; bind to localhost by default, optional admin password (NFR-4, NFR-11, NFR-14)
- [ ] SQLite store, migrations, pre-migration backup, schema-version guard (NFR-2, FR-73)
- [ ] Core model with tagged quota values; provider interface and registry; `internal/core` import check in CI (FR-25, FR-28, FR-29, NFR-15)
- [ ] Nextcloud adapter: read-only health, list accounts, configured quota, usage with unknown detection, `NormalizeQuota` (FR-20, FR-22, FR-23)
- [ ] Immich adapter: same read path, usage cross-checked against `/api/server/statistics` (FR-21, FR-22, FR-23)
- [ ] Identity sync from LLDAP: users and groups keyed on `entryuuid` (FR-1, FR-2, FR-9a)
- [ ] Account linking: match keys including the OIDC subject, manual links, revalidation, unmanaged and orphan detection (FR-3, FR-6, FR-7, FR-8, FR-8a, FR-9, FR-9b)
- [ ] Competing-writer detection, degrading the provider rather than blocking reads (FR-75)
- [ ] The write probe, run after linking against a linked account, on both providers: the Immich write path is the one thing never exercised against the live stack (FR-76, ADR-0025)
- [ ] `nuno doctor`: every precondition checked, every failure paired with the command that fixes it, and `--fix` emitting them as a script (FR-76, ADR-0027)
- [ ] Version detection and out-of-range warning (NFR-12)
- [ ] Scheduled read-only usage refresh (FR-48)
- [ ] `GET /api/v1/usage` with an admin key from `NUNO_ADMIN_KEY`, and a plain aggregated HTML page (FR-40, FR-41, FR-42, FR-45, FR-46, FR-46a, FR-47, FR-50)
- [ ] The page degrades instead of crashing when a provider or a credential is missing (FR-55)
- [ ] CLI: `nuno providers health`, `nuno usage`, `nuno accounts`, `nuno link`, `nuno unlink` (FR-70a, FR-74)

Precondition, not code: the target Nextcloud must move its default from `oidc_login_default_quota` to `files/default_quota` first, and it has not yet (`tests/fixtures/nextcloud-write-probe.json` records it set to `25 GB`). Nuno cannot do this itself (it is a system config, reachable only through `occ`, and reaching `occ` would mean root on the host: see [ADR-0023](docs/decisions.md#adr-0023-nuno-diagnoses-it-does-not-reconfigure-the-services-it-manages)), and it cannot even read it, so `nuno doctor --fix` emits a script that checks and moves it. FR-75's degradation gate covers the two `user_ldap` settings, which OCS does expose: see [ADR-0027](docs/decisions.md#adr-0027-a-competing-writer-nuno-cannot-see).

Done when the aggregate page shows both services for both people, a Homepage `customapi` widget reads `/api/v1/usage` (proving it can send the bearer header, which is currently assumed), and every provider account is either linked or listed as unmanaged. This milestone is used daily from the day it lands.

## M2: policy and plan, no writes

- [ ] Tiers with a budget, allocations (percent, absolute, unlimited, absent), group-to-tier mapping, per-user and per-user-per-provider overrides (FR-4, FR-5, FR-10 to FR-19)
- [ ] Per-provider resolution as a pure function, with the reason recorded
- [ ] Planner with change classification (safe, shrink-below-usage, unknown-state) (FR-30, FR-38, FR-38a, FR-38d)
- [ ] `nuno plan`, `nuno explain <user>`, `nuno adopt` (FR-30, FR-31, FR-70, FR-71)
- [ ] Persisted plans and reconcile runs

Done when `nuno plan` against the real stack produces a plan that is agreed with, and `nuno adopt` makes it empty. No applier exists yet, so being wrong about the allocation model costs nothing but a text file.

## M3: apply

- [ ] Applier, idempotent, with dry-run, re-check at apply time and write pacing (FR-32, FR-33, FR-34, FR-35, FR-38b, FR-38c, FR-72)
- [ ] Shrink guardrail and the unknown-state rule, with exit codes
- [ ] Audit log tied to runs
- [ ] UI: edit tier or budget, trigger reconcile, show the last plan and its outcome, act on link issues, manage credentials (FR-19a, FR-50 to FR-57)
- [ ] Scheduled reconcile, which can never consent to a risky change (FR-36, FR-39)
- [ ] Webhook notification on failure and on newly guarded changes (FR-60, FR-62, FR-63): the timer must not run unattended without one
- [ ] Partial reconcile of one user or one provider (FR-37)

Done when changing a tier in the UI fixes both services, the second run is empty, a shrink is refused without consent, an unknown-state change is refused with consent, and the timer has run for a week without surprising anyone.

## M4: publish

- [ ] Contract tests as the CI gate; integration environment as a manual target plus a weekly job, with pinned digests
- [ ] Documentation: deployment, the Nextcloud credential requirement, the conflicting-writer preconditions, the Homepage widget snippet, "known to work with"
- [ ] CI (lint, test, build, publish image to ghcr.io)

Done when tagged `v0.1.0`, with a published image, used by our own homelab.

## Post-v0.1 candidates

- Usage history and trend charts
- Prometheus `/metrics` and a Grafana dashboard
- More providers: S3/MinIO buckets, ZFS userquota, Seafile
- Several instances per provider type (the schema is already shaped for it)
- Threshold notifications
- The per-person endpoint and token management, when there are non-admin members (FR-45a)
- YAML export and import, with GitOps mode
- RBAC for the UI
- Claim-based provisioning at OIDC login
