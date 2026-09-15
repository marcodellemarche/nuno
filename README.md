# Nuno

> Status: pre-alpha. The design is frozen and every decision is recorded, but nothing is implemented yet. This repository holds the specification we build against.

Nuno is a quota and usage control plane for self-hosted service stacks.

Self-hosters run several services that store user data (Nextcloud, Immich, Seafile, S3 buckets, ZFS datasets) and each one counts storage on its own. There is no single place to answer "how much space is this person using across everything?" or to say "this person gets 100 GiB, split across the services that support quotas." Nuno fills that gap.

It connects to an identity provider (LLDAP or any LDAP directory), reads and writes per-user quotas on each supported service, aggregates usage into one view, and gives you an admin UI to see and change it. It runs as a small container next to the services it manages.

## The problem

- Every service enforces its own quota, if it supports one at all.
- "Budget per person" is a human convention, not something the stack enforces.
- SSO gets quotas wrong in both directions at once. Immich's OIDC `storageQuotaClaim` is applied **only when the account is created** and never re-synced, so changing someone's entitlement in the directory does nothing. Nextcloud's `oidc_login_default_quota` does the opposite: it rewrites the quota on **every** login, so a value an admin set by hand quietly reverts. One mechanism never updates, the other never stops.
- Answering "who is using what" means opening N admin panels.
- Promoting someone to a bigger tier means remembering to touch every service, in the right way, with the right API.

## The idea

```text
                 +------------------------------+
                 |             Nuno             |
                 |                              |
  LLDAP -------->|  Identity   Policies  Usage  |
  (users,        |   sync      (desired)  view  |
   groups)       |        +------+-------+      |
                 |          reconciler          |
                 +-------+--------------+-------+
                         |              |
                 +-------v-----+  +-----v-------+
                 |  Nextcloud  |  |   Immich    |   ... more providers
                 +-------------+  +-------------+
```

1. Identity sync pulls people and groups from LLDAP/LDAP.
2. Policies map a person or a group to a storage budget and to how that budget is split across services.
3. A reconciler compares the desired state with what each service actually has and applies the difference, idempotently, with a dry-run mode and an audit log.
4. Usage from every provider is aggregated into one dashboard and history, and each person can read their own share of it from a homepage widget.

## Core concepts

| Concept | Meaning |
|---|---|
| Provider | An adapter to a service that can hold user data (Nextcloud, Immich, ...). Declares capabilities. |
| Identity source | Where users and groups come from (LLDAP / generic LDAP; manual entries are always allowed). |
| Budget | The total storage a person is allowed, across all providers. A convention that Nuno makes real by setting per-provider ceilings. |
| Allocation | How a budget is split onto a provider, for example 25% Nextcloud and 75% Immich. |
| Desired state | The quota Nuno wants each provider to have for a user. Stored in Nuno's database. |
| Observed state | What the provider reports right now (quota and usage). |
| Account link | The mapping between a person and their account on a provider. Nuno never writes to an account it has not linked. |
| Reconcile | Compute desired vs observed and apply the difference. |

## Planned features

### MVP (v0.1)

- [ ] Providers: Nextcloud (OCS Provisioning API) and Immich (admin API).
- [ ] Identity: LLDAP / generic LDAP (read users and groups), plus manual users.
- [ ] Read usage per user per provider, aggregated in one view.
- [ ] Policies: default tier, per-group tiers, per-user overrides.
- [ ] Manual reconcile (button and CLI) with dry-run, audit log, and guardrails: never shrink a quota to or below what someone already stores without explicit consent, and never write against state that could not be read.
- [ ] Admin UI: list users, usage bars, edit budget, trigger reconcile.
- [ ] A JSON usage endpoint for a shared homepage dashboard, so people see what is left without opening two admin panels.
- [ ] Webhook notification when a reconcile fails or is blocked.

### Later

- [ ] A per-person endpoint and token, for stacks with non-admin members.
- [ ] More providers: S3/MinIO buckets, ZFS userquota, Seafile.
- [ ] Claim-based provisioning at login (push quota through OIDC).
- [ ] Prometheus metrics and a Grafana dashboard.
- [ ] Usage history and trend charts.
- [ ] Several instances of the same provider type.
- [ ] RBAC for the admin UI.
- [ ] GitOps mode (policies as YAML).

## Non-goals

- Nuno is not a file server, a backup tool, or a storage layer. It sets quotas and reads usage; the data stays in the services.
- Nuno is not an identity provider. It consumes identity from LLDAP/LDAP. The member token is a read-only capability key, not a login.
- Nuno does not move data between services.
- Nuno does not create or delete accounts. A person with no account on a provider is reported, never provisioned.

## Documentation

| Document | Contents |
|---|---|
| [`REQUIREMENTS.md`](REQUIREMENTS.md) | Functional and non-functional requirements, user stories, scope |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Components, provider interface, data model, stack |
| [`ROADMAP.md`](ROADMAP.md) | Milestones from scaffold to v1 |
| [`docs/decisions.md`](docs/decisions.md) | Architecture decision records, open and accepted |
| [`AGENTS.md`](AGENTS.md) | How AI coding agents should work in this repository |

## Running Nuno (planned)

Not implemented yet. The intended deployment is a single container:

```yaml
services:
  nuno:
    image: ghcr.io/<org>/nuno:latest
    env_file: .env
    volumes:
      - ./data:/data          # SQLite DB and config
    networks:
      - edge
    restart: unless-stopped
```

It binds to localhost unless told otherwise and is meant to sit behind the same SSO or forward-auth layer as the rest of the stack. A static admin credential is supported so an unproxied deployment is never an unauthenticated one. Nuno does not implement login in the MVP.

Nuno needs credentials that can actually write. On Nextcloud an app password is not enough: quota changes require `allowed_no_password_confirmation_ranges` or a dedicated admin without 2FA. On Immich an API key scoped to `adminUser.read` and `adminUser.update` is enough. `nuno doctor` proves it with a write probe against an account Nuno already manages, instead of discovering it mid-reconcile.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`AGENTS.md`](AGENTS.md). Issues and pull requests are welcome once the design stabilizes.

## Known to work with

| Component | Verified against |
|---|---|
| Nextcloud | 34.0.4 (OCS Provisioning API) |
| Immich | v3.2.0 (admin API, major v3) |
| LLDAP | v0.6.3 (LDAP read, not captured) |

Nuno detects each provider's version and warns outside the supported major. It does not refuse to run on an untested minor.

## License

AGPL-3.0-or-later, to match the Nextcloud ecosystem and keep network forks open. See [ADR-0004](docs/decisions.md#adr-0004-license).
