# Architecture

> Design document. Implementation started with M1 and follows it; where the two disagree, this document is the one that is wrong and gets an ADR. Every decision it rests on is recorded in [`docs/decisions.md`](docs/decisions.md). Changing something here means writing an ADR first.

## 1. Shape of the system

Nuno is a control plane: it holds a desired state, observes the world, and reconciles the difference. It is not in the data path. If Nuno is down, Nextcloud and Immich keep serving files exactly as before.

```
+---------------------------------------------------------------------+
|                                Nuno                                 |
|                                                                     |
|  +------------+   +--------------+   +---------------+              |
|  |  Identity  |   |   Policy     |   |   Reconciler  |              |
|  |   sync     |-->|   engine     |-->|  (planner +   |              |
|  |  (LDAP)    |   |  (desired)   |   |   applier)    |              |
|  +-----+------+   +------+-------+   +-------+-------+              |
|        |                 |                   |                      |
|        +--------+--------+---------+---------+                      |
|                 v                  v                                |
|           +-----------+      +------------+                         |
|           |  Store    |      |  Provider  |  registry               |
|           | (SQLite)  |      |  adapters  |                         |
|           +-----------+      +-----+------+                         |
|                                    |                                |
|  +------------+   +------------+   |   +----------------------+     |
|  |  Admin UI  |   |  HTTP API  |   |   | Notifications        |     |
|  |  (HTMX)    |-->| (net/http) |   |   | (webhook)            |     |
|  +------------+   +------------+   |   +----------------------+     |
+------------------------------------+-------------------------------+
                                     v
              +-----------+---------+----------+---------+
              | Nextcloud | Immich  | S3/MinIO |  ZFS    |  ...
              +-----------+---------+----------+---------+
```

### Layers

Dependency direction is outer to inner, never the reverse.

- `internal/core/`: the domain model, the port interfaces (`Provider`, repositories, typed errors), and the pure functions `Resolve`, `Diff` and `Classify`. No I/O.
- `internal/reconcile/`: the orchestration. Observe, link, plan, apply, report. It sequences I/O, owns contexts, deadlines, retries, pacing and per-provider degradation. It may do I/O; it holds no domain rules.
- `internal/providers/`: adapters to services. Depend only on core.
- `internal/identity/`: adapters to identity sources (LDAP). Depend only on core.
- `internal/store/`: persistence. Implements core repositories.
- `internal/api/` and `internal/ui/`: HTTP surface. Depends on core, store and reconcile.
- `internal/notify/`: webhook delivery.
- `cmd/nuno/`: wiring, the composition root.

The rule "core has no I/O" is only meaningful with the core/reconcile split above. The applier calls providers and writes audit entries, which is I/O sequencing, so it lives in `reconcile`. Without this split, the first retry added to the applier quietly voids the rule. The split is mechanically checkable and CI checks it: `internal/core` may not import any other `internal` package.

## 2. The provider interface

Every provider declares what it can do, so the core never guesses.

```go
type Provider interface {
    Type() string
    Capabilities() Capabilities
    Health(ctx context.Context) HealthResult

    ListAccounts(ctx context.Context) ([]Account, error)
    GetAccount(ctx context.Context, externalID string) (Account, error)
    NormalizeQuota(q Quota) Quota

    SetQuota(ctx context.Context, externalID string, q Quota) error
}

type Capabilities struct {
    CanReadUsers       bool
    CanReadUsage       bool
    CanSetUserQuota    bool
    CanSetDefaultQuota bool // reserved for FR-24 (Later); false for Immich, which has no such concept
    HasGroups          bool // reserved; group membership comes from the identity source
    MatchKeys          []MatchKey // what this provider can match on, in preference order
}

type Account struct {
    ExternalID string // the provider's stable key, and the only thing writes address
    Subject    string // OIDC subject when the provider exposes one: an exact cross-provider join
    Username   string // may be empty, may be a UUID, never assumed meaningful
    Email      string // may be empty: a real captured account has none
    Enabled    bool
    Deleted    bool   // Immich soft-deletes; a deleted account is never linked or written
    Quota      Quota  // the CONFIGURED quota, not the effective one
    Used       Quota  // Unknown or Bytes only; never Unlimited
    ObservedAt time.Time
}

type HealthResult struct {
    Reachable   bool
    Version     string
    InSupported bool
    Err         error
}

type WriteAccess uint8 // Unproven, Yes, No
```

Writes address `ExternalID` and nothing else. `Username` and `Email` exist to match an account to a person once; after that the link is stored, so a rename in the directory cannot retarget a write.

`GetAccount` exists because the apply-time re-check needs one account, not all of them: re-listing on Nextcloud has side effects and does not scale. `MatchKeys` is what the adapter *can* do; which one is used is instance configuration, and the instance's choice wins.

`Health()` reads and nothing else: reachability, version, whether the credential authenticates. It may be called as often as the UI needs, and it does not read user details, so the home-folder side effect stays inside `observe()`. Whether the credential can actually write is a separate question answered by the write probe, which needs no method of its own: `GetAccount`, then `SetQuota` with the value just observed, then `GetAccount` again, sequenced in `internal/reconcile` and owned by `nuno doctor`. It runs after linking, against a linked account whose quota is already a fixed point of `NormalizeQuota`, so it can never alter anyone's quota and never writes to an account Nuno does not manage. With no eligible account, `WriteAccess` stays `Unproven`, which is not the same as `No`. See [ADR-0025](docs/decisions.md#adr-0025-health-reads-the-write-probe-is-a-separate-step).

`NormalizeQuota` returns what the provider will actually store when asked for a value. The planner compares normalized to observed, never raw to raw, because at least one provider silently rewrites what you give it. `NormalizeQuota(Unlimited)` is `Unlimited`; `NormalizeQuota(Unknown)` is a programming error. See [ADR-0011](docs/decisions.md#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer).

### Quota is three states, not a number

```go
type QuotaKind uint8 // Unknown, Unlimited, Bytes
type Quota struct { Kind QuotaKind; Bytes int64 }
```

`Unknown` is never a write target and never a write trigger. All stored and transported values are bytes; all human-facing sizes are IEC.

There is deliberately no `ProviderDefault` kind: no adapter can produce one honestly (Nextcloud resolves `default` before the API sees it, Immich has no instance default), and a state nothing can emit is a state nothing will handle correctly. Nextcloud's `"none"` and `-3` both decode to `Unlimited`.

`NormalizeQuota` for Nextcloud is the `humanFileSize` round trip: divide by 1024 and round to **zero** decimals for the first step (bytes to KB), then divide by 1024 and round to **one** decimal for every step after it, until the value is below 1024, then parse the resulting string back to bytes. The zero-decimal first step is not a detail, it is what the captures show. Confirm it against `Util::humanFileSize` in the first adapter commit; the captured pairs in `tests/fixtures/` are the test vector, and the property to assert is the round trip. See [ADR-0021](docs/decisions.md#adr-0021-corrections-from-the-captured-responses) point 9.

### Provider notes

These are verified against Nextcloud `stable34` and Immich v3.2.0 (spec `v3.2.1`). The full reasoning is in [ADR-0014](docs/decisions.md#adr-0014-provider-write-paths-as-they-actually-are); what follows is what an implementer needs.

**Nextcloud**, OCS Provisioning API.

- Read: `GET /ocs/v2.php/cloud/users/details`, with `OCS-APIRequest: true` and `Accept: application/json` (XML otherwise). **`ocs.data.users` is a map keyed by account id, not an array**, and that key is the `ExternalID`. The configured quota is `ocs.data.users.<id>.quota.quota`, which may be a number, `-3` for unlimited, or the string `"none"`.
- Never read `quota.total` as the configured quota: it is the effective space for a bounded account and the sentinel `-3` for an unlimited one. Neither is what Nuno sets.
- Every field of the quota object is optional, and on a storage error the whole object serializes as `[]`. Usage is `Unknown` when the quota object is absent, `[]`, or missing `total` or `relative`: the lookup failed and nothing can be concluded.
- A complete quota object with `firstLoginTimestamp` of 0 means usage is **known and zero**, not unknown: the account exists and holds nothing, because nobody can upload without authenticating. Such an account is fully writable, which is what lets a new member's quota be correct before they first open the service. See [ADR-0024](docs/decisions.md#adr-0024-a-person-who-has-never-logged-in-still-gets-their-quota).
- Write: `PUT /ocs/v2.php/cloud/users/{id}` with `key=quota&value=<bytes>`. The value is stored through `humanFileSize`, which rounds to one decimal per unit, so `NormalizeQuota` must apply the same transform. Measured: 26844594176 in, 26843545600 out; 53687091200 stores as `"50 GB"`.
- Unlimited is written as `value=none`, never as `-3`: a numeric -3 is parsed as a byte count, which is the most destructive value the API accepts. `files/allow_unlimited_quota` can refuse it, which is a typed error, not a crash.
- Auth: an app password cannot write (`PasswordConfirmationRequired`, verified: 403). Either whitelist Nuno's address in `allowed_no_password_confirmation_ranges`, or use a dedicated admin without 2FA, which is verified to work. The write probe proves it, after linking, not from `Health()`.
- Startup check: refuse to manage an instance where `user_ldap` defines `ldapQuotaAttribute` or `ldapQuotaDefault`. Those are app config and OCS exposes them. `oidc_login_default_quota` rewrites quotas on every OIDC login and is the worst of the three, but it is system config and OCS cannot read it at all: `nuno doctor --fix` emits a script that checks and moves it. Do not write a startup check that pretends otherwise. See [ADR-0027](docs/decisions.md#adr-0027-a-competing-writer-nuno-cannot-see).
- Writes are limited to 50 per 10 minutes. A 429 is a `ThrottledError`, not a failure.
- On `/ocs/v2.php` the HTTP status and `ocs.meta.statuscode` agree (the write probe returned 403 in both). Parse the envelope, read both, treat a disagreement as a `ProviderError`. The 997-with-HTTP-200 convention belongs to `/ocs/v1.php` and does not apply here.
- Reading creates missing home folders. Observe is not side-effect free here.

**Immich**, admin API.

- Read: `GET /api/admin/users` for accounts and configured quota, plus `GET /api/server/statistics` for live per-user usage. The authoritative value for `Account.Used` is `usageByUser[].usage`; `quotaUsageInBytes` is only a cross-check.
- Usage is `Unknown` when `|quotaUsageInBytes - usage| > max(64 MiB, 1% of usage)`. Measured divergence on a healthy account was 9.5 MB out of 9.8 GB (0.096 percent), so a tighter absolute threshold would mark a working account permanently unwritable, and there is no override for unknown state.
- Accounts are soft-deleted: skip anything whose `status` is not `active` or whose `deletedAt` is set, before linking and before writing.
- `oauthId` carries the OIDC subject and is byte-identical to the Nextcloud account id for the same person. It is an exact cross-provider join and the linker prefers it over email.
- Write: `PUT /api/admin/users/{id}` with `{"quotaSizeInBytes": N}`. Deprecated in v3 in favour of `PATCH`, which is absent from the OpenAPI spec. Unlimited is an explicit `null`; an omitted key means "do not touch"; `0` is legal and blocks all uploads.
- Auth: `x-api-key` with scopes `adminUser.read` and `adminUser.update`.
- `CanSetDefaultQuota` is false. Immich has no instance-wide default.

Adapters return typed errors (`AuthError`, `UnreachableError`, `UnsupportedError`, `ThrottledError`, `ProviderError`) so the reconciler can decide what is fatal.

## 3. Domain model

```
User
  ID                 internal
  Source             lldap | manual
  SourceUUID         entryuuid, the identity; uid and email are attributes
  UID, Email, DisplayName
  Status             active | orphaned
  Groups             by group UUID
  TierOverride       optional

Tier
  ID                 integer key; renaming is free
  Name               unique
  Budget             tagged value; the base percent allocations resolve against
  Allocations        per provider type
  IsDefault

UserProviderOverride  above every tier, per provider
  UserID, ProviderID, Quota

Allocation
  ProviderID         an INSTANCE, for the reason stated below this block
  Mode               absolute | percent | unlimited
  Value              bytes, or 0..100

GroupTier            the edge FR-4 depends on
  GroupUUID, TierID

Provider             an INSTANCE, not a type
  ID, Type, Name
  MatchKey           which of the adapter's MatchKeys this instance uses
  ConfigRef          the env prefix its credentials and URL are read from

AccountLink          only real links
  UserID, ProviderID, ExternalID, Origin(matched|manual)
  unique(UserID, ProviderID) and unique(ProviderID, ExternalID)

LinkIssue            recomputed every observe
  ProviderID, Kind(unlinked|ambiguous|dangling|stale|unmanaged)
  UserID?, ExternalID?, Detail

ExternalAccount      observed state, keyed by the provider's account
  ProviderID, ExternalID, UserID?   (null = unmanaged)
  Quota, Used        tagged values
  ObservedAt, ObserveOK

ReconcileRun
  ID, StartedAt, FinishedAt, Mode, Actor, ConsentShrink, Status
  Snapshot           the observed state the plan was computed from

AuditEntry
  RunID, At, Actor, UserID, ProviderID, Field, From, To, Result

MemberToken / AdminKey
  Hash (sha256), Label, CreatedAt, LastUsedAt, RevokedAt
```

An `Allocation` that is absent for a provider is not a zero allocation. There is no row, the planner emits no change, and the provider keeps whatever it has. Only `mode: unlimited` lifts a ceiling, and only explicitly. The consequence is a leak worth surfacing: a user moved off a tier that allocated Immich keeps their old Immich ceiling forever, so `managed: false` appears on that provider in the UI and in the API.

`Provider` is an instance from day one even though the MVP runs one per type, and allocations key on the instance for the same reason. Keying allocations on type while accounts key on instance is the inconsistency that would force a migration, a resolver signature change and an API version bump the moment someone adds a second Nextcloud. An earlier draft of the block above said `ProviderType`, which contradicted this paragraph; the instance wins.

### Policy resolution

Resolution happens per provider, not per tier. See [ADR-0012](docs/decisions.md#adr-0012-tier-resolution-happens-per-provider) and [ADR-0020](docs/decisions.md#adr-0020-budget-is-an-input-to-percentages-and-an-output-to-people).

```
ceiling(user, provider) =
    override(user, provider)                 if set          // FR-15
    else max over candidates of allocationFor(provider)
    where candidates =
        {user.TierOverride}                  if set
        else {tier | group in user.Groups, groupTier(group) = tier}
        else {defaultTier}
    and Unlimited is the top element, an absent allocation contributes nothing
    result: Unlimited | Bytes | absent (no change is ever emitted)

allocationFor(tier, provider) =
    Unlimited                                if mode is unlimited
    tier.Allocation.Value                    if mode is absolute
    percent of (tier.Budget - sum of absolute allocations)   if mode is percent
```

A tier whose absolute allocations exceed its budget is a configuration error, refused when written and when imported, never resolved to a negative ceiling: Nextcloud reads negative values as sentinels, so an unvalidated negative grants unlimited instead of failing. Percentages need not sum to 100; over 100 is a deliberate over-commit and warns on the tier row and in the plan.

The **effective budget** reported to a person is the sum of their resolved ceilings, unlimited if any is. It is an output and may legitimately differ from any tier's `Budget`, which is only the base for percentages.

Taking the maximum per provider is what actually delivers "most generous", which whole-tier resolution by budget did not: the total budget is not monotone in the per-provider ceilings it produces. There are no ties to break.

Resolution is pure and records why: for each provider it stores which tier supplied the winning ceiling and whether it came from an override, a group or the default. `nuno explain <user>` prints that chain together with group membership, the account link and its origin, the last observed values and the recent audit entries. It is the first thing to reach for when a number is wrong, and the best available test harness for the resolver.

## 4. Reconcile loop

```
observe()   -> for each provider: list accounts, read configured quota and usage
               a provider that fails is degraded, not fatal; its accounts keep their last values
               and every change against it becomes unknown-state
link()      -> match accounts to users by the provider's match key plus manual links
               revalidate: absent external id -> dangling; changed key -> stale
               record every issue; never write to an account not seen this cycle
plan()      -> for each linked (user, provider):
                 ceiling  = resolve(user, provider)        // may be absent
                 desired  = provider.NormalizeQuota(ceiling)
                 observed = account.Quota
                 if absent or desired == observed: nothing
                 else emit Change(user, provider, from, to, class)
                   class = safe | shrink-below-usage | unknown-state
apply(plan) -> re-observe the affected accounts and re-classify
               refuse any change that became riskier, and any plan older than the window (default 5 minutes)
               for each Change, unless dry-run:
                 unknown-state: never applied, no flag exists
                 shrink-below-usage: applied only with consent, else skipped and reported
                 provider.SetQuota(account.ExternalID, to), paced against the rate limit
                 write AuditEntry with the run id
report()    -> applied, failed, skipped, guarded; notifications; exit code
```

Guarantees, stated as what is actually true:

- **Idempotent.** `plan()` is pure given observed state, and comparison is normalized, so applying then re-planning yields an empty plan. Guarded changes reappear by design.
- **Loud.** A `ProviderError` stops the run for that provider with a non-zero result and a notification. Earlier writes stand and are audited: there is no rollback for a quota already written, and the state is safe because re-running converges. Reports never claim nothing happened.
- **Bounded.** Writes are paced under the provider's rate limit. A throttled run stops cleanly and resumes next cycle.
- **Never blind.** No link, no write. No observed account this cycle, no write. Unknown state, no write.

### Change classification

`shrink-below-usage` means the target is at or below what the person already stores (`target <= used`: a quota exactly equal to usage already blocks uploads). Consent-gated, never available to a scheduled run.

`unknown-state` means usage could not be trusted: the provider read failed, Nextcloud returned an incomplete quota object, or Immich's cached counter disagrees with the live aggregate beyond the threshold. A never-logged-in account with a complete quota object is not unknown, it is zero (ADR-0024). It is never applied and there is no flag to force it, because there is no safe consent for writing against state you do not have.

The guardrail is the one sanctioned exception to "no silent skips" in `AGENTS.md`, and it is not silent: named in the plan, in the report, in the UI and in the exit status. Exit codes are a contract: 0 clean, 1 error, 2 applied but incomplete, 3 configuration or startup failure. `--dry-run` returns 2 when the plan is non-empty.

## 5. HTTP surface

Three audiences, one domain underneath. See [ADR-0017](docs/decisions.md#adr-0017-usage-api-dashboard-first-per-person-second).

The **admin surface** is the UI plus the routes it calls. It binds to `127.0.0.1` unless explicitly configured otherwise, and refuses to listen on a public interface without an acknowledgement in the configuration. A static admin password is supported so that "no proxy yet" never means "no auth".

The **dashboard endpoint** is what ships first, because the first consumer is a shared homepage:

```
GET /api/v1/usage
Authorization: Bearer <admin key>

{
  "schema": 1,
  "users": [
    {
      "user": "alice",
      "user_uuid": "11111111-1111-4111-8111-111111111111",
      "budget_bytes": 214748364800,
      "used_bytes": 49392123904,
      "used_percent": 23.0,
      "complete": true,
      "providers": [
        {"type": "nextcloud", "quota_bytes": 53687091200, "used_bytes": 1073741824,
         "used_percent": 2.0, "managed": true, "status": "ok",
         "observed_at": "2026-09-14T22:00:00Z"},
        {"type": "immich", "quota_bytes": 161061273600, "used_bytes": 48318382080,
         "used_percent": 30.0, "managed": true, "status": "ok",
         "observed_at": "2026-09-14T22:00:00Z"}
      ]
    }
  ]
}
```

The **member endpoint**, `GET /api/v1/me/usage`, returns one person's own entry in the same shape, authenticated by an opaque per-user token. It ships when there are non-admin members to serve.

Rules that hold for both: `quota_bytes` is always the **observed** ceiling, never a desired one; `null` means unlimited, but only where `status` is `ok` or `stale`; `used_percent` is `null` when the ceiling is unlimited or zero, never `NaN`; `observed_at` and `status` are per provider; `managed: false` marks a ceiling left over from a policy that no longer allocates that provider. Each user entry carries a stable `user_uuid` and the array is sorted by it, because a widget addresses fields by path and an unstable order would swap two people's numbers. Neither credential grants writes.

A number is readable only together with its `status`. Where `status` is `unknown` or `unavailable`, `quota_bytes`, `used_bytes` and `used_percent` are `null` and mean nothing, and the user entry's `complete` is `false`. The user-level `budget_bytes` and `used_bytes` sum the values that are known, so `null` there keeps meaning unlimited and never leaks an unknown. A consumer that reads the numbers and ignores `status` will show unlimited for an account whose storage could not be read, which is why the documented widget snippet reads both. See [ADR-0026](docs/decisions.md#adr-0026-the-usage-contract-needs-a-name-for-unknown).

`status` is `ok` while `observed_at` is within twice the refresh interval, `stale` beyond it, `unavailable` when the last observe failed, and `unknown` when the call succeeded but the provider's answer did not carry usable values (Nextcloud's `quota` serialized as `[]`, or Immich's two counters disagreeing beyond the threshold). The refresh interval defaults to 15 minutes. A never-logged-in account is `ok` with a usage of zero, per [ADR-0024](docs/decisions.md#adr-0024-a-person-who-has-never-logged-in-still-gets-their-quota), not `unknown`.

Before M2 there is no policy, so `budget_bytes` is the sum of observed ceilings and `managed` is `false` everywhere. The shape does not change when policy arrives, only the meaning of those two fields. See [ADR-0022](docs/decisions.md#adr-0022-what-the-usage-endpoint-means-before-policy-exists).

Three credential types exist and are never interchangeable: the **admin key** (machine, read-only, `/api/v1/usage`, bootstrapped from `NUNO_ADMIN_KEY`), the **member token** (one person, read-only, `/api/v1/me/usage`), and the **admin password** (a human in the UI when no proxy provides auth).

Because these numbers are only as fresh as the last observe, a scheduled read-only usage refresh ships with the endpoint. Observing does not write, so none of the caution around scheduled reconcile applies to it.

## 6. Storage

SQLite (a single file under `/data`), through repository interfaces so Postgres remains possible. Driver: `modernc.org/sqlite`, no cgo, with one DSN defined in one place: WAL, `busy_timeout`, `foreign_keys(1)`, `synchronous(NORMAL)`, a single-connection writer pool and a separate read pool.

Tables: `users`, `groups`, `group_tiers`, `tiers`, `allocations`, `user_provider_overrides`, `providers`, `account_links`, `link_issues`, `external_accounts`, `reconcile_runs`, `plans`, `audit_log`, `admin_keys`, `member_tokens`, `settings`, and `usage_samples` (Later).

Migrations run on boot from `nuno serve`, after copying the database to `/data/backups/` and keeping the last few. A database newer than the binary refuses to start. Only `nuno serve` and `nuno migrate` migrate.

The database is the only home for policy, so it must be recoverable. The supported backup is `sqlite3 .backup` on a snapshotted dataset, never a copy of a live WAL database. See [ADR-0019](docs/decisions.md#adr-0019-what-v01-does-not-do) for why this replaced a bespoke YAML export.

Concurrency is handled by having one writer: when a server is running, the CLI is an HTTP client to it. See [ADR-0018](docs/decisions.md#adr-0018-one-writer-the-cli-talks-to-the-server).

## 7. Tech stack

Accepted: **Go**, see [ADR-0001](docs/decisions.md#adr-0001-tech-stack).

- HTTP: `net/http`. Go 1.22 patterns cover method and wildcard routing; no router dependency at this route count.
- Database: `modernc.org/sqlite`, migrations with `pressly/goose`.
- LDAP: `go-ldap/ldap`. Against LLDAP: request `memberOf` explicitly (it is absent from the wildcard attribute set, so a `*` search returns no group membership), accept both `uid=` and `cn=` RDN forms, use `ldaps://` or plaintext on a private network (LLDAP does not implement StartTLS), and store `entryuuid` for users and groups.
- Notifications: a plain outbound webhook with a JSON body. No Apprise.
- Templates: `html/template` with HTMX, both embedded via `embed.FS`. HTMX is vendored, never a CDN: the container must work offline. One hand-written stylesheet, no Tailwind, no build step.
- Logging: `log/slog`, JSON.
- HTTP client: `net/http` with explicit timeouts, bounded retries and per-provider pacing.
- Packaging: multi-stage `Dockerfile` producing a static binary on distroless.

## 8. Security

- Nuno holds credentials that can change quotas on every managed service. Treat it as a privileged service.
- Secrets come from env or files. Never committed, never in the database, never in logs. Redact everywhere.
- The admin surface binds to localhost by default and must be explicitly configured to listen wider. A static admin password is available so an unproxied deployment is still authenticated. Forward-auth headers are trusted only from a configured proxy address.
- The member API is separable onto its own listener, so exposing usage to a dashboard never means exposing the admin panel.
- Provider credentials are least-privilege where the provider allows it: an Immich API key scoped to `adminUser.read` and `adminUser.update`, not `all`.
- Member tokens and admin keys are capability keys, not logins: random 32 bytes, shown once, stored hashed, revocable, rate limited, read-only. They never appear in logs.
- Quota changes are the only write operation. There is no delete path for user data, by design. Every write is audited with its run id.

## 9. Observability

- `/healthz` for the process, plus per-provider health in the UI: reachable, version, in supported range, and write access as `Unproven`, `Yes` or `No` from the last probe.
- Structured JSON logs, one line per change.
- `/metrics` for Prometheus, Later.
- Webhook notification on failure and on newly guarded changes, keyed so a persistent guarded change does not notify every cycle.

## 10. Testing strategy

| Level | What | How |
|---|---|---|
| Unit | resolution, planner, classification, provider parsers | pure functions, no network |
| Contract | every adapter against recorded fixtures | `httptest` servers and golden files |
| Integration | adapters against real Nextcloud and Immich | `docker compose -f docker-compose.test.yml`, tagged `//go:build integration` |
| End-to-end | reconcile a seeded user, assert the quota changed | integration environment plus CLI |

Contract tests are the CI gate on every pull request. The real-stack environment is a manual `make integration` plus a weekly scheduled job, with pinned image digests, never a merge blocker: keeping a live Immich and Nextcloud green is a recurring cost that would otherwise be paid in abandoned tests.

Four properties get a dedicated test each, because they are the ones that hurt when wrong:

1. Plan, apply, plan again yields an empty second plan, given consent or given no risky changes.
2. An absent allocation emits no change at all.
3. A shrink at or below current usage is never applied without consent.
4. A change against unknown usage is never applied, with or without consent.

Fixtures must include the shapes that break naive decoding, all of them real responses: a `quota` field serialized as `[]`, a quota of `-3` and of `"none"`, an incomplete quota object missing `total` or `relative`, a complete quota object with `firstLoginTimestamp` of 0 (known and zero, not unknown), and a 429. The OCS error body returned with HTTP 200 is not among them: on `/ocs/v2.php` the status and `ocs.meta.statuscode` agree, and [ADR-0021](docs/decisions.md#adr-0021-corrections-from-the-captured-responses) dropped it as a requirement on this path.

`tests/fixtures/` already holds responses captured from a live Nextcloud 34.0.4 and Immich v3.2.0 on 2026-09-15, including the 403 write probe and both sides of the lossy round trip. The degraded shapes are not among them and still need writing.

## 11. Extending Nuno with a new provider

1. Implement the provider interface in `internal/providers/<name>/`.
2. Declare capabilities honestly, including `MatchKey` and what identity the service actually exposes.
3. Implement `NormalizeQuota` to match what the service really stores, and prove `Normalize(Normalize(x)) == Normalize(x)`.
4. Add contract tests with recorded fixtures, including the degraded and error shapes.
5. Register it in the provider registry, the composition root.
6. Add a line to the README's "known to work with".

The interface as written is shaped by two HTTP services with per-user byte quotas. The first provider that does not fit that shape (ZFS has no HTTP and quotas per dataset; S3 has no native quota concept) is expected to change it. That is a core interface change and requires an ADR.
