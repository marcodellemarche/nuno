# Requirements

This document is the contract for what Nuno must do. It is written to be implementation-agnostic. Anything not listed here is out of scope until it is added explicitly (see [`docs/decisions.md`](docs/decisions.md)).

Status legend: **MVP** means required for v0.1, **Later** means explicitly deferred, **Won't** means non-goal.

## 1. Personas

| Persona | Description |
|---|---|
| Admin | Runs the self-hosted stack. Has credentials for the services and the identity provider. The only user of Nuno in the MVP. |
| Member | A person with an account on the services. Does not use Nuno directly in the MVP. |
| Operator/contributor | Deploys and extends Nuno. Cares about container size, config and extensibility. |

## 2. Functional requirements

### Identity

| ID | Requirement | Status |
|---|---|---|
| FR-1 | Nuno MUST import users and groups from an LDAP-compatible directory (LLDAP first). | MVP |
| FR-2 | Nuno MUST allow manually created users that exist in no directory, for services not behind SSO. | MVP |
| FR-3 | Nuno MUST detect users that exist in a provider but not in the identity source and surface them as unmanaged. | MVP |
| FR-4 | Nuno SHOULD support group membership as the primary way to assign a tier. | MVP |
| FR-5 | Nuno MUST let an admin assign a tier directly to a user, overriding group-derived tiers. | MVP |

### Policies

| ID | Requirement | Status |
|---|---|---|
| FR-10 | Nuno MUST model a per-person budget (total bytes) independent of any provider. | MVP |
| FR-11 | Nuno MUST model allocations: how a budget maps onto each provider, absolute or percentage. | MVP |
| FR-12 | Nuno MUST provide named tiers (for example `standard`, `admin`) mapping to a budget and allocations. | MVP |
| FR-13 | Nuno MUST support a configurable default tier for new or unmatched users. | MVP |
| FR-14 | Nuno MUST resolve precedence deterministically: user override, then group tier, then default tier. | MVP |
| FR-15 | Nuno SHOULD allow a per-user, per-provider override independent of the tier. | Later |
| FR-16 | Nuno SHOULD support policies as versioned YAML (GitOps mode). | Later |

### Providers

| ID | Requirement | Status |
|---|---|---|
| FR-20 | Nuno MUST support Nextcloud as a provider: read users, read quota and usage, set per-user quota. | MVP |
| FR-21 | Nuno MUST support Immich as a provider: read users, read quota and usage, set per-user quota. | MVP |
| FR-22 | Every provider MUST declare capabilities: can it set quota, does it have a default, does it have groups. | MVP |
| FR-23 | Nuno MUST never assume a capability. It MUST check and degrade gracefully. | MVP |
| FR-24 | Nuno SHOULD support setting a provider-wide default quota when the provider offers one. | MVP |
| FR-25 | Adding a new provider MUST require only implementing the provider interface, with no changes to core. | MVP |
| FR-26 | Nuno SHOULD ship more providers: S3/MinIO, ZFS userquota, Seafile. | Later |
| FR-27 | Nuno SHOULD support several instances of the same provider type. | Later |

### Reconciliation

| ID | Requirement | Status |
|---|---|---|
| FR-30 | Nuno MUST compute desired vs observed quota and produce a plan before changing anything. | MVP |
| FR-31 | Nuno MUST support dry-run that prints the plan without applying it. | MVP |
| FR-32 | Applying a plan MUST be idempotent: running twice changes nothing the second time. | MVP |
| FR-33 | Nuno MUST record every applied change in an audit log: who, when, user, provider, from and to. | MVP |
| FR-34 | Nuno MUST NOT silently skip an error. A failed change MUST fail the reconcile run and be reported. | MVP |
| FR-35 | Nuno MUST expose reconcile as both a UI action and a CLI command. | MVP |
| FR-36 | Nuno SHOULD run reconcile on a schedule (timer or cron). | Later |
| FR-37 | Nuno SHOULD support partial reconcile of one user or one provider. | Later |

### Usage

| ID | Requirement | Status |
|---|---|---|
| FR-40 | Nuno MUST read current usage per user from every provider that can report it. | MVP |
| FR-41 | Nuno MUST show aggregated usage across providers per user, and per provider. | MVP |
| FR-42 | Nuno MUST show usage relative to the allocation (used over ceiling, percentage). | MVP |
| FR-43 | Nuno SHOULD store usage samples over time and show trends. | Later |
| FR-44 | Nuno SHOULD export usage as CSV or JSON. | Later |

### Admin UI

| ID | Requirement | Status |
|---|---|---|
| FR-50 | The UI MUST list users with identity source, tier, budget and per-provider usage bars. | MVP |
| FR-51 | The UI MUST let an admin change a user's tier or budget and trigger a reconcile. | MVP |
| FR-52 | The UI MUST show the last reconcile result, plan and outcome. | MVP |
| FR-53 | The UI MUST show provider connection status: reachable or auth failed. | MVP |
| FR-54 | The UI SHOULD show usage history charts. | Later |
| FR-55 | The UI MUST be read-only when Nuno is misconfigured (no credentials) instead of crashing. | MVP |

### Notifications

| ID | Requirement | Status |
|---|---|---|
| FR-60 | Nuno MUST notify on reconcile failure. | MVP |
| FR-61 | Nuno SHOULD notify when a user crosses configurable usage thresholds (for example 80, 90, 95 percent). | MVP |
| FR-62 | Notifications SHOULD go through a channel abstraction (Apprise: ntfy, email, webhook). | MVP |

## 3. Non-functional requirements

| ID | Requirement | Status |
|---|---|---|
| NFR-1 | Nuno MUST be deployable as a single container via docker compose. | MVP |
| NFR-2 | Nuno MUST persist state in a single embedded database (SQLite) by default. | MVP |
| NFR-3 | Nuno MUST be stateless with respect to user data: it never stores file contents. | MVP |
| NFR-4 | Nuno MUST keep secrets (service credentials, API keys) out of the database and out of logs. | MVP |
| NFR-5 | Nuno MUST NOT be a single point of failure: if it is down, managed services keep working. | MVP |
| NFR-6 | All provider calls MUST have timeouts and bounded retries. | MVP |
| NFR-7 | Nuno MUST NOT lose or corrupt data on a provider. Quota changes are the only writes. | MVP |
| NFR-8 | Nuno SHOULD be verifiable: unit tests with mocked providers plus an integration test against real services. | MVP |
| NFR-9 | Nuno SHOULD expose a `/healthz` endpoint and Prometheus `/metrics`. | MVP for health, Later for metrics |
| NFR-10 | The container SHOULD stay small (under 200 MB) and idle under 100 MB RAM. | MVP |
| NFR-11 | Configuration MUST be file or env based, with no interactive setup required. | MVP |
| NFR-12 | Nuno SHOULD be provider-version aware: detect API differences and warn. | MVP |

## 4. User stories

- US-1: As an admin, I want to see all users and how much space they use across Nextcloud and Immich on one page, so I do not open two admin panels.
- US-2: As an admin, I want to define a `standard` and an `admin` tier once, so new users get sane quotas without manual work.
- US-3: As an admin, I want to move a user to a tier by changing one setting, and have Nuno fix every service on the next reconcile.
- US-4: As an admin, I want a dry-run that tells me exactly what would change, so I trust Nuno before it writes.
- US-5: As an admin, I want to be told if a reconcile fails or if someone is about to fill their quota.
- US-6: As an operator, I want to add support for a new service by writing one adapter, without touching the core.

## 5. Assumptions

- The services expose admin APIs usable with a long-lived credential (Nextcloud app password, Immich API key).
- The identity source is reachable over the network (LDAP).
- The admin already has a reverse proxy or SSO layer in front of the services. Nuno reuses it and does not implement login in the MVP.
- A single Nuno instance manages a single stack. Multi-instance is Later.

## 6. Open questions

| # | Question | Where |
|---|---|---|
| Q1 | How to split a budget across providers: fixed percentages, weights, or per-tier explicit amounts? | [ADR-0003](docs/decisions.md#adr-0003-budget-splitting) |
| Q2 | Is Nuno's database the source of desired state, or is it derived from LLDAP groups? | [ADR-0002](docs/decisions.md#adr-0002-source-of-truth) |
| Q3 | How aggressively to follow Immich's unstable API: pin per version or feature-detect? | [ADR-0005](docs/decisions.md#adr-0005-provider-version-compatibility) |
| Q4 | Server-rendered UI or SPA? | [ADR-0006](docs/decisions.md#adr-0006-ui-approach) |
| Q5 | License. | [ADR-0004](docs/decisions.md#adr-0004-license) |
