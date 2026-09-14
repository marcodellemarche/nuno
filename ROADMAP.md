# Roadmap

Milestones are intentionally small. Each one ends with something runnable and demonstrable, so an agent or a human can iterate without a big-bang rewrite.

## M0: design freeze (current)

- [x] Problem statement and scope (`README.md`)
- [x] Requirements (`REQUIREMENTS.md`)
- [x] Architecture draft (`ARCHITECTURE.md`)
- [x] Resolve ADR-0001 (stack): Go
- [ ] Resolve ADR-0002 (source of truth)
- [ ] Resolve ADR-0003 (budget splitting)
- [ ] Resolve ADR-0004 (license)

Done when the open ADRs are accepted and the interfaces in `ARCHITECTURE.md` are stable enough to code against.

## M1: skeleton and one provider read path

- [ ] Project scaffold, `Dockerfile`, `docker-compose.yml`, `/healthz`
- [ ] Config loading (env and file) with secret redaction
- [ ] SQLite store and migrations
- [ ] Provider interface and registry
- [ ] Nextcloud adapter: health, list users, read quota and usage
- [ ] CLI: `nuno providers health`, `nuno usage`

Done when `docker compose up` starts Nuno and the CLI prints real Nextcloud usage per user.

## M2: second provider and aggregated usage

- [ ] Immich adapter, same read path
- [ ] Aggregated usage view per user and per provider
- [ ] Read-only admin UI listing users and usage bars
- [ ] Unmanaged-user detection

Done when the UI shows every user's Nextcloud and Immich usage on one page.

## M3: policies and reconcile

- [ ] Tiers, budgets, allocations
- [ ] Identity sync from LLDAP (groups to tiers), manual users
- [ ] Planner and applier, idempotent, with dry-run
- [ ] Audit log
- [ ] CLI `nuno plan` and `nuno reconcile [--dry-run]`
- [ ] UI to edit tier or budget, trigger reconcile, show the last plan

Done when changing a user's tier in the UI updates both services on the next reconcile, and a second run is a no-op.

## M4: notifications, automation, v0.1

- [ ] Apprise notifications: reconcile failure, usage thresholds
- [ ] Scheduled reconcile (timer or cron)
- [ ] Contract tests and an integration test environment with real Nextcloud and Immich
- [ ] Documentation: deployment, provider matrix, contributing
- [ ] License and CI (lint, test, build, publish image to ghcr.io)

Done when tagged `v0.1.0`, with a published image, used by our own homelab.

## Post-v0.1 candidates

- Usage history and trend charts
- Prometheus `/metrics` and a Grafana dashboard
- More providers: S3/MinIO buckets, ZFS userquota, Seafile
- Several instances per provider type
- RBAC for the UI
- GitOps mode (policies as YAML)
- Claim-based provisioning at OIDC login
