# Requirements

This document is the contract for what Nuno must do. It is written to be implementation-agnostic. Anything not listed here is out of scope until it is added explicitly (see [`docs/decisions.md`](docs/decisions.md)).

Status legend: **MVP** means required for v0.1, **Later** means explicitly deferred, **Won't** means non-goal.

## 1. Personas

| Persona | Description |
|---|---|
| Admin | Runs the self-hosted stack. Has credentials for the services and the identity provider. The only user who can change anything in the MVP. |
| Member | A person with an account on the services. Does not log into Nuno. Sees their usage on the shared dashboard (FR-45), and later through their own token-authenticated endpoint (FR-45a). |
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
| FR-6 | Nuno MUST link a user to a provider account using a configurable per-provider match key, and MUST let an admin override that link explicitly. The key MUST NOT be assumed to be the username: Nextcloud auto-registers OIDC and LDAP users with a UUID. See [ADR-0013](docs/decisions.md#adr-0013-identity-keys-and-the-link-lifecycle). | MVP |
| FR-7 | Nuno MUST NOT write to an account it cannot link unambiguously. Unlinked and ambiguous accounts MUST be surfaced and excluded from reconcile. | MVP |
| FR-8 | Nuno MUST NOT create or delete accounts on a provider. A person without an account is reported, not provisioned. | MVP |
| FR-8a | Nuno MUST skip accounts that are disabled or soft-deleted on the provider, before linking and before writing. | MVP |
| FR-9 | A user that disappears from the identity source MUST be marked orphaned and excluded from reconcile, keeping its audit history, not deleted silently. | MVP |
| FR-9a | Identity MUST be keyed on the directory's stable UUID, for users and for groups. Renaming a user or a group MUST NOT lose overrides, links or tier assignments. | MVP |
| FR-9b | Nuno MUST revalidate links every cycle and MUST NOT write to an account it did not observe in that cycle. A link to a vanished account MUST be reported, not fatal. | MVP |

### Policies

| ID | Requirement | Status |
|---|---|---|
| FR-10 | Nuno MUST model a budget on a tier as the base that percentage allocations resolve against, and MUST report a person's effective budget as the sum of their resolved ceilings. The two are different quantities. See [ADR-0020](docs/decisions.md#adr-0020-budget-is-an-input-to-percentages-and-an-output-to-people). | MVP |
| FR-11 | Nuno MUST model allocations: how a budget maps onto each provider, absolute or percentage. | MVP |
| FR-12 | Nuno MUST provide named tiers (for example `standard`, `admin`) mapping to a budget and allocations. | MVP |
| FR-13 | Nuno MUST support a configurable default tier for new or unmatched users. | MVP |
| FR-14 | Nuno MUST resolve precedence deterministically: user override, then group tiers, then default tier. Resolution happens per provider, taking the most generous ceiling across candidate tiers, with unlimited as the maximum. See [ADR-0012](docs/decisions.md#adr-0012-tier-resolution-happens-per-provider). | MVP |
| FR-15 | Nuno MUST allow a per-user, per-provider override independent of the tier. It sits above every tier in precedence and is where `nuno adopt` (FR-70) stores what it imports. | MVP |
| FR-16 | Nuno SHOULD support policies as versioned YAML (GitOps mode). | Later |
| FR-17 | A tier that declares no allocation for a provider MUST leave that provider untouched for the user. Absent MUST NOT be read as zero or as unlimited. | MVP |
| FR-18 | Nuno MUST support an explicit unlimited allocation, distinct from an absent one. | MVP |
| FR-19 | Nuno MUST show, per user and per provider, which tier produced the ceiling and by which rule (override, group, or default). | MVP |
| FR-19a | The UI MUST state, where tiers are assigned, that a person cannot be restricted by adding a stricter tier: the most generous ceiling wins, so demotion means removing the generous group. | MVP |

### Providers

| ID | Requirement | Status |
|---|---|---|
| FR-20 | Nuno MUST support Nextcloud as a provider: read users, read quota and usage, set per-user quota. | MVP |
| FR-21 | Nuno MUST support Immich as a provider: read users, read quota and usage, set per-user quota. | MVP |
| FR-22 | Every provider MUST declare capabilities: can it set quota, does it have a default, does it have groups. | MVP |
| FR-23 | Nuno MUST never assume a capability. It MUST check and degrade gracefully. | MVP |
| FR-24 | Nuno SHOULD support setting a provider-wide default quota when the provider offers one. Dropped from v0.1: Nextcloud sets it outside Nuno and Immich has no such concept. See [ADR-0019](docs/decisions.md#adr-0019-what-v01-does-not-do). | Later |
| FR-25 | Adding a new provider MUST require only implementing the provider interface, with no changes to core. | MVP |
| FR-26 | Nuno SHOULD ship more providers: S3/MinIO, ZFS userquota, Seafile. | Later |
| FR-27 | Nuno SHOULD support several instances of the same provider type. The schema MUST model providers as instances from the start so this needs no migration. | Later, schema MVP |
| FR-28 | A provider MUST expose a quota as a tagged value (unknown, unlimited, bytes) and MUST declare how it normalizes a requested value, because at least one provider rewrites what it is given. See [ADR-0011](docs/decisions.md#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer). | MVP |
| FR-29 | Nuno MUST read the configured quota, not the effective one, and MUST source usage from a provider endpoint it can trust. | MVP |

### Reconciliation

| ID | Requirement | Status |
|---|---|---|
| FR-30 | Nuno MUST compute desired vs observed quota and produce a plan before changing anything. | MVP |
| FR-31 | Nuno MUST support dry-run that prints the plan without applying it. | MVP |
| FR-32 | Applying a plan MUST be idempotent: running twice changes nothing the second time, given consent or given no risky changes. Comparison MUST be normalized (FR-28), or a provider that rewrites the value will loop forever. | MVP |
| FR-33 | Nuno MUST record every applied change in an audit log: who, when, user, provider, from and to. | MVP |
| FR-34 | Nuno MUST NOT silently skip an error. A failed change MUST stop the run for that provider and be reported. Changes already applied stand and are audited; reports MUST state applied, failed, skipped and guarded counts rather than claiming nothing happened. | MVP |
| FR-35 | Nuno MUST expose reconcile as both a UI action and a CLI command. | MVP |
| FR-36 | Nuno MUST run reconcile on a schedule. Without it Nuno reports rather than controls, and a new account keeps whatever default the service gave it. | MVP |
| FR-37 | Nuno MUST support partial reconcile of one user or one provider. It is the primary day-2 debugging tool and is nearly free once the planner takes a filter. | MVP |
| FR-38 | Nuno MUST classify a change that lowers a quota to or below current usage as risky and MUST apply it only with explicit consent (`--allow-shrink` or a UI confirmation). Refused changes MUST be reported, and the run MUST NOT be reported as clean. See [ADR-0009](docs/decisions.md#adr-0009-guardrail-against-shrinking-a-quota-below-current-usage). | MVP |
| FR-38a | Nuno MUST NOT apply any change against a provider whose usage is unknown, and MUST offer no flag to force it. Usage is unknown only when the provider's answer is incomplete, never merely because an account has no usage yet. See [ADR-0016](docs/decisions.md#adr-0016-safety-model-unknown-state-re-check-and-honest-partial-application). | MVP |
| FR-38d | An account that exists but has never been used MUST be treated as having zero usage and MUST receive its quota. A person's quota MUST be correct before they first open the service. See [ADR-0024](docs/decisions.md#adr-0024-a-person-who-has-never-logged-in-still-gets-their-quota). | MVP |
| FR-38b | Nuno MUST re-observe and re-classify affected accounts at apply time, and MUST refuse any change whose classification moved since the plan was computed. | MVP |
| FR-38c | Nuno MUST pace writes under a provider's rate limit and MUST treat throttling as an incomplete run, not a failure. | MVP |
| FR-39 | Scheduled reconcile MUST NOT be able to apply risky changes. | MVP |

### Usage

| ID | Requirement | Status |
|---|---|---|
| FR-40 | Nuno MUST read current usage per user from every provider that can report it. | MVP |
| FR-41 | Nuno MUST show aggregated usage across providers per user, and per provider. | MVP |
| FR-42 | Nuno MUST show usage relative to the allocation (used over ceiling, percentage). | MVP |
| FR-43 | Nuno SHOULD store usage samples over time and show trends. | Later |
| FR-44 | Nuno SHOULD export usage as CSV or JSON. | Later |
| FR-45 | Nuno MUST expose `GET /api/v1/usage`, returning every user's budget, per-provider ceilings and usage, authenticated by an admin key. This is the contract for a shared dashboard widget (Homepage `customapi`). See [ADR-0017](docs/decisions.md#adr-0017-usage-api-dashboard-first-per-person-second). | MVP |
| FR-45a | Nuno MUST expose `GET /api/v1/me/usage`, the same shape scoped to one person, authenticated by a per-user token. | Later, when non-admin members exist |
| FR-47 | Usage responses MUST carry per-provider `observed_at` and `status`, MUST represent unlimited as `null`, MUST NOT emit `NaN`, MUST report the observed ceiling rather than a desired one, MUST carry a stable `user_uuid` and a stable ordering, and MUST mark a ceiling left over from a policy that no longer allocates that provider. | MVP |
| FR-48 | Nuno MUST refresh usage on a schedule without reconciling, so displayed numbers are not as stale as the last manual run. | MVP |
| FR-46 | Credentials for the usage API MUST be read-only, storable only as a hash, and revocable. A member token MUST grant access to one user's own data and nothing else. | MVP |
| FR-46a | The admin key MUST be bootstrappable from the environment (`NUNO_ADMIN_KEY`), so the endpoint is usable before any credential UI exists. The three credential types (admin key, member token, admin password) MUST be named distinctly and MUST NOT be interchangeable. See [ADR-0022](docs/decisions.md#adr-0022-what-the-usage-endpoint-means-before-policy-exists). | MVP |

### Admin UI

| ID | Requirement | Status |
|---|---|---|
| FR-50 | The UI MUST list users with identity source, tier, budget and per-provider usage bars. | MVP |
| FR-51 | The UI MUST let an admin change a user's tier or budget and trigger a reconcile. | MVP |
| FR-52 | The UI MUST show the last reconcile result, plan and outcome. | MVP |
| FR-53 | The UI MUST show provider connection status: reachable or auth failed. | MVP |
| FR-54 | The UI SHOULD show usage history charts. | Later |
| FR-55 | The UI MUST be read-only when Nuno is misconfigured (no credentials) instead of crashing. | MVP |
| FR-56 | The UI MUST let an admin issue, rotate and revoke API credentials, showing a value exactly once. | MVP |
| FR-57 | The UI MUST surface unlinked, ambiguous and orphaned accounts as an actionable list, not as a log line. | MVP |

### Notifications

| ID | Requirement | Status |
|---|---|---|
| FR-60 | Nuno MUST notify on reconcile failure. | MVP |
| FR-61 | Nuno SHOULD notify when a user crosses configurable usage thresholds (for example 80, 90, 95 percent). Deferred: avoiding repeat notifications needs per-threshold state. | Later |
| FR-62 | Notifications MUST be delivered by a plain outbound webhook with a JSON body. No Apprise: running it as a service contradicts NFR-1 and NFR-10. See [ADR-0019](docs/decisions.md#adr-0019-what-v01-does-not-do). | MVP |
| FR-63 | A notification for a persistently guarded change MUST fire on first appearance and on change, not once per cycle. | MVP |

### Operations

| ID | Requirement | Status |
|---|---|---|
| FR-70 | Nuno MUST provide `nuno adopt`, importing each provider's current quota as the user's starting override, so the first plan against an existing stack is empty by construction. | MVP |
| FR-70a | Nuno MUST provide `nuno link` and `nuno unlink`, so an ambiguous or stale link can be resolved without a UI. | MVP |
| FR-76 | Nuno MUST provide `nuno doctor`, checking every precondition (write-capable credentials, competing writers, provider versions, identity reachability, unresolved links) and printing, for each failure, the exact command that fixes it. The same text MUST appear at startup and in the UI when a provider is degraded. | MVP |
| FR-71 | Nuno MUST provide `nuno explain <user>`, printing group membership, candidate tiers, the winning ceiling per provider and why, the account link and its origin, the last observed values, and recent audit entries. | MVP |
| FR-72 | Exit codes MUST be a contract: 0 clean, 1 error, 2 applied but incomplete, 3 configuration or startup failure. `--dry-run` MUST return 2 when the plan is non-empty. | MVP |
| FR-73 | Migrations MUST run only from `nuno serve` and `nuno migrate`, MUST copy the database before applying, and MUST refuse a database newer than the binary. | MVP |
| FR-74 | When a server is running, the CLI MUST act as a client to it rather than opening the database directly. See [ADR-0018](docs/decisions.md#adr-0018-one-writer-the-cli-talks-to-the-server). | MVP |
| FR-75 | An adapter MUST detect a known competing writer at startup and MUST refuse to **apply** against that instance, naming the remedy. Reading and reporting continue, marked degraded: observation is safe and M1 is read-only. On Nextcloud these are `oidc_login_default_quota`, `ldapQuotaAttribute` and `ldapQuotaDefault`. | MVP |

## 3. Non-functional requirements

| ID | Requirement | Status |
|---|---|---|
| NFR-1 | Nuno MUST be deployable as a single container via docker compose. | MVP |
| NFR-2 | Nuno MUST persist state in a single embedded database (SQLite) by default. | MVP |
| NFR-3 | Nuno MUST be stateless with respect to user data: it never stores file contents. | MVP |
| NFR-4 | Nuno MUST keep secrets (service credentials, API keys) out of the database and out of logs. | MVP |
| NFR-5 | Nuno MUST NOT be a single point of failure: if it is down, managed services keep working. | MVP |
| NFR-6 | All provider calls MUST have timeouts and bounded retries. | MVP |
| NFR-7 | Nuno MUST NOT lose or corrupt data on a provider. Per-user quota changes are the only writes: Nuno MUST NOT edit the configuration of a service it manages, and MUST NOT require host-level or container-level access to do its job. See [ADR-0023](docs/decisions.md#adr-0023-nuno-diagnoses-it-does-not-reconfigure-the-services-it-manages). | MVP |
| NFR-8 | Nuno SHOULD be verifiable: unit tests with mocked providers plus an integration test against real services. | MVP |
| NFR-9 | Nuno SHOULD expose a `/healthz` endpoint and Prometheus `/metrics`. | MVP for health, Later for metrics |
| NFR-10 | The container SHOULD stay small (under 200 MB) and idle under 100 MB RAM. | MVP |
| NFR-11 | Configuration MUST be file or env based, with no interactive setup required. | MVP |
| NFR-13 | Nuno SHOULD provide YAML export and import. Replaced for v0.1 by documented SQLite backup on a snapshotted dataset. See [ADR-0019](docs/decisions.md#adr-0019-what-v01-does-not-do). | Later |
| NFR-14 | The admin surface MUST bind to localhost by default and MUST require explicit configuration to listen wider. Nuno MUST support a static admin credential so an unproxied deployment is still authenticated, and MUST allow the usage API on a separate listener. | MVP |
| NFR-15 | `internal/core` MUST NOT import any other internal package, and CI MUST check it. | MVP |
| NFR-12 | Nuno MUST detect a provider's version and warn outside its supported major. No per-version test matrix is required. See [ADR-0015](docs/decisions.md#adr-0015-version-policy-simplified). | MVP |

## 4. User stories

- US-1: As an admin, I want to see all users and how much space they use across Nextcloud and Immich on one page, so I do not open two admin panels.
- US-2: As an admin, I want to define a `standard` and an `admin` tier once, so new users get sane quotas without manual work.
- US-3: As an admin, I want to move a user to a tier by changing one setting, and have Nuno fix every service on the next reconcile.
- US-4: As an admin, I want a dry-run that tells me exactly what would change, so I trust Nuno before it writes.
- US-5: As an admin, I want to be told if a reconcile fails or is blocked. (Being warned before someone fills their quota is FR-61, deferred.)
- US-6: As an operator, I want to add support for a new service by writing one adapter, without touching the core.
- US-7: As a member, I want to see how much of my budget is left on my homepage dashboard, without asking an admin and without logging into Nuno.
- US-8: As an admin, I want Nuno to refuse to shrink someone's quota below what they already store, unless I say so explicitly.
- US-9: As an admin, I want to point Nuno at a stack that already has quotas set by hand and have it adopt them, so its first plan does not propose rewriting everything.
- US-10: As an admin, when a number looks wrong I want one command that shows me the whole chain that produced it.

## 5. Assumptions

- The services expose admin APIs usable with a long-lived credential. An Immich API key scoped to `adminUser.read` and `adminUser.update` is documented to cover admin user management; **the Immich write path has not been exercised yet**, only its reads, so the first Immich adapter commit must prove it with a probe. A Nextcloud **app password cannot write**: quota changes require either `allowed_no_password_confirmation_ranges` (Nextcloud 32 and later) or a dedicated admin without 2FA. See [ADR-0014](docs/decisions.md#adr-0014-provider-write-paths-as-they-actually-are).
- No other component writes the same quotas. Verified on 2026-09-15: `oidc_login_default_quota` rewrites the quota on **every** OIDC login with no check on the current value, so it must be empty for Nuno to work at all. The instance default belongs in `files/default_quota`, which new users inherit without anything being rewritten. `ldapQuotaAttribute` and `ldapQuotaDefault` behave the same way on the `user_ldap` path.
- Reading Nextcloud user details creates missing home folders. Observing is not entirely free of side effects.
- The identity source is reachable over the network (LDAP).
- The admin already has a reverse proxy or SSO layer in front of the services. Nuno reuses it and does not implement login in the MVP.
- A single Nuno instance manages a single stack. Multi-instance is Later.

## 6. Resolved questions

Every question that blocked M0 is now answered. The reasoning lives in the ADRs; this table is the index.

| # | Question | Answer | Where |
|---|---|---|---|
| Q1 | How to split a budget across providers? | Per-tier allocations, `percent` or `absolute`, resolved to bytes. Absent means do not touch. | [ADR-0003](docs/decisions.md#adr-0003-budget-splitting-across-providers) |
| Q2 | Is Nuno's database the source of desired state? | Yes. LLDAP owns identity and groups, Nuno owns policy. | [ADR-0002](docs/decisions.md#adr-0002-source-of-truth-for-desired-state) |
| Q3 | How to follow Immich's API? | Detect the version, warn outside the supported major, decode tolerantly. Fixtures once per major, not per version. Superseded: Immich has followed semver since v2. | [ADR-0015](docs/decisions.md#adr-0015-version-policy-simplified) |
| Q4 | Server-rendered UI or SPA? | Server-rendered with `html/template` and HTMX, no Tailwind, no build step. | [ADR-0006](docs/decisions.md#adr-0006-ui-approach) |
| Q5 | License. | AGPL-3.0-or-later. | [ADR-0004](docs/decisions.md#adr-0004-license) |
| Q6 | Which tier wins when a user is in several tier groups? | Highest budget, ties by name and reported. | [ADR-0007](docs/decisions.md#adr-0007-tier-resolution-when-a-user-is-in-several-tier-groups) |
| Q7 | How is a person matched to their provider accounts? | Per-provider match key plus an explicit link table. No writes without an unambiguous link. | [ADR-0008](docs/decisions.md#adr-0008-linking-a-person-to-their-provider-accounts) |
| Q8 | What happens when a quota would drop below current usage? | Marked risky, applied only with explicit consent. | [ADR-0009](docs/decisions.md#adr-0009-guardrail-against-shrinking-a-quota-below-current-usage) |
| Q9 | How does a member see their own remaining space? | A shared dashboard endpoint first (the homepage is shared and behind SSO), a per-person token endpoint when non-admin members exist. | [ADR-0017](docs/decisions.md#adr-0017-usage-api-dashboard-first-per-person-second) |

A review on 2026-09-15, against provider source, and then a second pass against captured responses from the live stack, reopened several of these and raised more. Where a row below conflicts with one above, the row below wins.

| # | Question | Answer | Where |
|---|---|---|---|
| Q10 | How is a quota represented, given that providers report unknown, unlimited, inherited and numeric states, and at least one rewrites what it is given? | A tagged value plus a per-provider normalization function. Comparison is normalized to normalized. | [ADR-0011](docs/decisions.md#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer) |
| Q11 | Which tier wins for a user in several groups? | None: resolution is per provider, taking the most generous ceiling. Q6's answer was wrong. | [ADR-0012](docs/decisions.md#adr-0012-tier-resolution-happens-per-provider) |
| Q12 | What are the real match keys, and when does a link stop being valid? | UUID-based identity, configurable per-provider keys that are never the naked username, and revalidation every cycle. | [ADR-0013](docs/decisions.md#adr-0013-identity-keys-and-the-link-lifecycle) |
| Q13 | Do the provider APIs behave as assumed? | No. App passwords cannot write to Nextcloud, OCS returns 200 on failure, writes are rate limited and lossy, reads have side effects, and Immich's usage counter is cached. | [ADR-0014](docs/decisions.md#adr-0014-provider-write-paths-as-they-actually-are) |
| Q14 | What happens when usage cannot be trusted, or changes between plan and apply? | Unknown state is never written, with no override. Plans are re-checked at apply time. Partial application is reported honestly. | [ADR-0016](docs/decisions.md#adr-0016-safety-model-unknown-state-re-check-and-honest-partial-application) |
| Q15 | How do the CLI and the server share one SQLite file? | They do not. The CLI is an HTTP client when a server is running. | [ADR-0018](docs/decisions.md#adr-0018-one-writer-the-cli-talks-to-the-server) |
| Q16 | What is out of scope for v0.1? | Apprise, provider default quotas, YAML export, the version matrix, threshold notifications. | [ADR-0019](docs/decisions.md#adr-0019-what-v01-does-not-do) |
| Q17 | If resolution is per provider, what is a budget for? | Two quantities, not one: a tier's budget is the base for percentages, a person's effective budget is the sum of their ceilings. | [ADR-0020](docs/decisions.md#adr-0020-budget-is-an-input-to-percentages-and-an-output-to-people) |
| Q18 | Do the captured responses match what the design assumed? | Not everywhere. The OCS error model is v2, `users` is a map, the unknown-usage rule needed unifying, and the Immich threshold needed a number. | [ADR-0021](docs/decisions.md#adr-0021-corrections-from-the-captured-responses) |
| Q19 | What do the policy fields of the usage endpoint mean before policy exists? | Defined degenerate values that do not change shape when M2 lands, plus a bootstrap path for the admin key. | [ADR-0022](docs/decisions.md#adr-0022-what-the-usage-endpoint-means-before-policy-exists) |
