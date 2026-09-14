# Architecture Decision Records

This is a log. Accepted decisions are not rewritten. If one is reversed, a new entry supersedes it. Open decisions are marked **[OPEN]** and should be resolved before the milestone that depends on them.

Format: context, options, decision, consequences.

## ADR-0001: Tech stack

**Status: Accepted (2026-09-14).**

**Context.** Nuno is a long-running controller with a small admin UI, a plugin system, LDAP and HTTP clients, and it must ship as a small self-hosted container. It will be developed largely by iterating with an AI coding agent.

**Options.**
1. Python 3.12 with FastAPI, SQLModel, Alembic and HTMX. Fast to iterate, mature libraries, easy for contributors. Larger image and more RAM.
2. Go with `net/http`, `html/template` and embedded SQLite. Single static binary, small image, strong fit for a controller. More work for the UI.
3. Node/TypeScript. Rich UI ecosystem, heavier, less common for infrastructure controllers.

**Decision.** Go. The deciding factor is the deployment story: a self-hosted tool that people actually install is best served by a single static binary and a small image, and a controller is exactly the workload Go is good at. UI iteration cost is accepted.

**Consequences.**
- Layout uses `cmd/nuno` and `internal/...` as in `ARCHITECTURE.md`.
- SQLite driver: `modernc.org/sqlite` by default to avoid cgo, unless a cgo build is acceptable.
- The UI is server-rendered with `html/template` and HTMX. No Node build step.
- Contract tests use `httptest` and golden files.

## ADR-0002: Source of truth for desired state

**Status: [OPEN]**. Needed for M3.

**Context.** Three places could hold quota intent: LLDAP groups, Nuno's database, and each service. Duplicating intent causes drift.

**Options.**
1. LLDAP groups are the only source and Nuno is stateless. Pure, but LLDAP has no place for a numeric budget and per-user overrides get awkward.
2. Nuno's database is the source, and LLDAP provides identity plus the group-to-tier mapping. Rich model, per-user overrides, audit history. Nuno becomes stateful.
3. Nuno's database is a cache of a YAML file (GitOps). Reproducible, but needs a file-watch or deploy story.

**Recommendation.** Option 2. LLDAP is identity and group membership. Nuno's database holds tiers, budgets, allocations and overrides. A group maps to a tier, and the tier lives in Nuno. This keeps identity in one place and policy in another, with no duplication of the numeric budget.

**Consequences if accepted.** The database must be backed up, so provide export and import to YAML from day one.

## ADR-0003: Budget splitting across providers

**Status: [OPEN]**. Needed for M3.

**Context.** A person has one budget, but each service needs a concrete ceiling. Our current homelab uses 25% Nextcloud and 75% Immich.

**Options.**
1. Fixed percentages per tier. Simple, matches current practice.
2. Weights, normalized. More flexible, harder to explain.
3. Explicit absolute bytes per provider, summed to the budget. Most precise, most verbose.
4. No split: the same total on every provider. Simple but over-commits storage.

**Recommendation.** Support both 1 and 3. A tier declares allocations as either `percent` or `absolute`, and the resolved desired quota is always absolute bytes. Percentages are convenience, bytes are the truth.

**Consequences if accepted.** Round down to whole MB and document it.

## ADR-0004: License

**Status: [OPEN]**. Needed before M4, the public release.

**Options.** AGPL-3.0-or-later, which aligns with Nextcloud and keeps network forks open. MIT or Apache-2.0, maximum adoption. MPL-2.0, file-level copyleft as a middle ground.

**Recommendation.** AGPL-3.0-or-later.

## ADR-0005: Provider version compatibility

**Status: [OPEN]**. Needed for M2, the Immich adapter.

**Context.** Nextcloud's OCS Provisioning API is stable. Immich's API is explicitly unstable across versions.

**Options.**
1. Pin to a tested Immich version and refuse newer ones.
2. Feature-detect from `/api/server/version` and warn on untested versions.
3. Adapter variants per API version.

**Recommendation.** Option 2. Detect and warn, keep adapters tolerant to extra fields, and keep a version matrix in the docs. Never fail hard on an unknown minor version, but log it.

## ADR-0006: UI approach

**Status: [OPEN]**. Needed for M2.

**Context.** The UI is an admin panel: lists, usage bars, a few forms. No rich client-side state.

**Options.**
1. Server-rendered with `html/template`, HTMX and Tailwind. No build step, simple deploys.
2. A SPA with React, Vue or Svelte over a REST API.
3. TUI or CLI only.

**Recommendation.** Option 1 for the MVP. The REST API still exists underneath, so a SPA can be added later without changing the backend.
