# Architecture

> Design document. Nothing here is implemented yet. Decisions still open are marked **[OPEN]** and tracked in [`docs/decisions.md`](docs/decisions.md).

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
|  |  (HTMX)    |-->| (net/http) |   |   | (Apprise)            |     |
|  +------------+   +------------+   |   +----------------------+     |
+------------------------------------+-------------------------------+
                                     v
              +-----------+---------+----------+---------+
              | Nextcloud | Immich  | S3/MinIO |  ZFS    |  ...
              +-----------+---------+----------+---------+
```

### Layers

Dependency direction is outer to inner, never the reverse.

- `internal/providers/`: adapters to services. Depends only on core interfaces.
- `internal/identity/`: adapters to identity sources (LDAP). Depends only on core.
- `internal/core/`: domain model, policy resolution, planner, applier. No I/O.
- `internal/store/`: persistence. Implements core repositories.
- `internal/api/` and `internal/ui/`: HTTP surface. Depends on core and store.
- `internal/notify/`: channel abstraction.
- `cmd/nuno/`: wiring, the composition root.

The rule: core must not import any provider, store, or web package. This is what makes providers pluggable and the domain testable without a network.

## 2. The provider interface

Every provider declares what it can do, so the core never guesses. In Go:

```go
type Provider interface {
    Type() string
    Capabilities() Capabilities
    Health(ctx context.Context) HealthResult

    ListUsers(ctx context.Context) ([]ExternalUser, error)
    GetAccount(ctx context.Context, user ExternalUser) (Account, error)

    SetQuota(ctx context.Context, user ExternalUser, bytes *int64) error
    SetDefaultQuota(ctx context.Context, bytes *int64) error
}

type Capabilities struct {
    CanReadUsers       bool
    CanReadUsage       bool
    CanSetUserQuota    bool
    CanSetDefaultQuota bool
    HasGroups          bool
    QuotaIsBytes       bool
}
```

`SetQuota` and `SetDefaultQuota` return `UnsupportedError` when the capability is false. Callers check the capability first; the error is a safety net, not control flow.

Provider-specific rules live inside the adapter:

- Nextcloud: OCS Provisioning API. `GET /ocs/v2.php/cloud/users/details` returns `quota.used`, `quota.total` and `quota.relative` per user. `PUT /ocs/v2.php/cloud/users/{id}` with `key=quota&value=<bytes>` sets it. Auth is an admin user plus an app password, with header `OCS-APIRequest: true`. The API is stable.
- Immich: `GET /api/admin/users` returns `quotaSizeInBytes` and `quotaUsageInBytes`. `PUT /api/admin/users/{id}` with `{"quotaSizeInBytes": N}` sets it. Auth is an admin API key in `x-api-key`. The API is explicitly unstable, see [ADR-0005](docs/decisions.md#adr-0005-provider-version-compatibility).

Adapters must not return generic errors. They return typed errors (`AuthError`, `UnreachableError`, `UnsupportedError`, `ProviderError`) so the reconciler can decide what is fatal and what is not.

## 3. Domain model

The domain is small on purpose.

```
User
  ID                 internal
  ExternalID         lldap uid or email
  DisplayName
  Email
  Source             lldap | manual
  Groups             from the identity source
  TierOverride       optional
  ProviderOverrides  optional, Later

Tier
  Name               "standard", "admin"
  BudgetBytes        total
  Allocations        how the budget is split per provider
  IsDefault          bool

Allocation
  ProviderType
  Mode               absolute | percent
  Value              bytes, or 0..100

ExternalAccount      observed, refreshed by reconcile
  UserID
  ProviderID
  QuotaBytes         nil means unlimited or unknown
  UsedBytes
  ObservedAt

AuditEntry
  At, Actor, UserID, ProviderID, Field, From, To, Result
```

### Policy resolution

Precedence is deterministic:

```
effectiveTier(user) =
    user.TierOverride              if set
    else tierOf(user.Groups)       if exactly one group maps to a tier
    else defaultTier               otherwise

desiredQuota(user, provider) = effectiveTier(user).AllocationFor(provider)
```

If a user is in several tier groups, resolution must be deterministic and documented. Proposal: the tier with the highest explicit priority wins, and ties are a configuration error reported at startup, never resolved by map iteration order. This is an **[OPEN]** detail.

## 4. Reconcile loop

```
observe()   -> refresh ExternalAccounts from every provider
plan()      -> for each (user, provider):
                 desired  = resolve(user, provider)
                 observed = account.QuotaBytes
                 if desired != observed: emit Change(user, provider, from, to)
apply(plan) -> for each Change, unless dry-run:
                 provider.SetQuota(user, to)
                 write AuditEntry
report()    -> summary and notifications
```

Guarantees:

- Idempotent: `plan()` is pure given observed state. Applying then re-planning yields an empty plan.
- Fail loud: any `ProviderError` aborts the run with a non-zero result and a notification. No partial success.
- Dry-run: `apply` is skipped, the plan is returned and logged.
- Scoped: observe only reads. A read failure on one provider marks that provider degraded but does not wipe data.

## 5. Storage

SQLite by default (a single file under `/data`), accessed through a repository interface so Postgres can be added later. Migrations are explicit and versioned, using goose or a small embedded migration runner.

Schema sketch: `users`, `groups`, `tiers`, `allocations`, `providers`, `external_accounts`, `audit_log`, `settings`, and `usage_samples` (Later).

## 6. Tech stack

Accepted: **Go**, see [ADR-0001](docs/decisions.md#adr-0001-tech-stack).

- HTTP router: `net/http` with `chi` if routing grows.
- Database: SQLite via `modernc.org/sqlite` (pure Go, no cgo) or `mattn/go-sqlite3` if cgo is acceptable.
- Migrations: `pressly/goose` or an embedded runner.
- LDAP: `go-ldap/ldap`.
- Notifications: `apprise` over HTTP, or direct ntfy/webhook clients. Apprise keeps one abstraction for many channels.
- Templates: `html/template` with HTMX and Tailwind, no build step.
- HTTP client: `net/http` with explicit timeouts and a small retry helper.
- Packaging: multi-stage `Dockerfile` producing a static binary on a distroless or alpine base.

## 7. Security

- Nuno holds credentials that can change quotas on every managed service. Treat it as a privileged service.
- Secrets come from env or files (Docker secrets style). Never committed, never in the database, never in logs. Redact provider credentials in all output.
- The UI is meant to run behind the existing SSO or forward-auth layer. Nuno does not implement authentication in the MVP. It must document that it should not be exposed on a public interface without a proxy.
- Quota changes are the only write operation. There is no delete path for user data, by design.
- Every write is audited.

## 8. Observability

- `/healthz` for the process, plus per-provider health surfaced in the UI.
- Structured logs in JSON, one line per change.
- `/metrics` for Prometheus, Later.
- Notifications on failure and on thresholds, via Apprise.

## 9. Testing strategy

| Level | What | How |
|---|---|---|
| Unit | policy resolution, planner, provider parsers | pure functions, no network |
| Contract | every adapter against recorded fixtures | `httptest` servers and golden files |
| Integration | adapters against real Nextcloud and Immich | `docker compose -f docker-compose.test.yml` |
| End-to-end | reconcile a seeded user, assert the quota changed | integration environment plus CLI |

The integration environment is not optional for the two MVP providers. The whole value of Nuno is not corrupting quota state.

## 10. Extending Nuno with a new provider

1. Implement the provider interface in `internal/providers/<name>/`.
2. Declare capabilities honestly.
3. Add contract tests with recorded fixtures.
4. Register it in the provider registry, the composition root.
5. Add a row to the provider matrix in the README.

No core changes. If a provider needs a new capability, that is a core interface change and requires an ADR.
