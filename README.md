# Nuno

> Status: pre-alpha / design. Nothing is implemented yet. This repository holds the specification we intend to build against.

Nuno is a quota and usage control plane for self-hosted service stacks.

Self-hosters run several services that store user data (Nextcloud, Immich, Seafile, S3 buckets, ZFS datasets) and each one counts storage on its own. There is no single place to answer "how much space is this person using across everything?" or to say "this person gets 100 GB, split across the services that support quotas." Nuno fills that gap.

It connects to an identity provider (LLDAP or any LDAP directory), reads and writes per-user quotas on each supported service, aggregates usage into one view, and gives you an admin UI to see and change it. It runs as a small container next to the services it manages.

## The problem

- Every service enforces its own quota, if it supports one at all.
- "Budget per person" is a human convention, not something the stack enforces.
- SSO provisions new users with inconsistent defaults: Nextcloud has one, Immich does not.
- Answering "who is using what" means opening N admin panels.
- Promoting someone to a bigger tier means remembering to touch every service, in the right way, with the right API.

## The idea

```
                 +------------------------------+
                 |             Nuno             |
                 |                              |
  LLDAP -------->|  Identity   Policies  Usage  |
  (users,        |   sync      (desired)  view   |
   groups)       |        +------+-------+       |
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
4. Usage from every provider is aggregated into one dashboard and history.

## Core concepts

| Concept | Meaning |
|---|---|
| Provider | An adapter to a service that can hold user data (Nextcloud, Immich, ...). Declares capabilities. |
| Identity source | Where users and groups come from (LLDAP / generic LDAP; manual entries are always allowed). |
| Budget | The total storage a person is allowed, across all providers. A convention that Nuno makes real by setting per-provider ceilings. |
| Allocation | How a budget is split onto a provider, for example 25% Nextcloud and 75% Immich. |
| Desired state | The quota Nuno wants each provider to have for a user. Stored in Nuno's database. |
| Observed state | What the provider reports right now (quota and usage). |
| Reconcile | Compute desired vs observed and apply the difference. |

## Planned features

### MVP (v0.1)

- [ ] Providers: Nextcloud (OCS Provisioning API) and Immich (admin API).
- [ ] Identity: LLDAP / generic LDAP (read users and groups), plus manual users.
- [ ] Read usage per user per provider, aggregated in one view.
- [ ] Policies: default tier, per-group tiers, per-user overrides.
- [ ] Manual reconcile (button and CLI) with dry-run and audit log.
- [ ] Admin UI: list users, usage bars, edit budget, trigger reconcile.
- [ ] Notifications on reconcile result and on quota thresholds, via Apprise (ntfy, email, webhook, ...).

### Later

- [ ] Scheduled automatic reconcile.
- [ ] More providers: S3/MinIO buckets, ZFS userquota, Seafile.
- [ ] Claim-based provisioning at login (push quota through OIDC).
- [ ] Prometheus metrics and a Grafana dashboard.
- [ ] Usage history and trend charts.
- [ ] Several instances of the same provider type.
- [ ] RBAC for the admin UI.
- [ ] GitOps mode (policies as YAML).

## Non-goals

- Nuno is not a file server, a backup tool, or a storage layer. It sets quotas and reads usage; the data stays in the services.
- Nuno is not an identity provider. It consumes identity from LLDAP/LDAP.
- Nuno does not move data between services.

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

It is meant to sit behind the same SSO or forward-auth layer as the rest of the stack. Nuno does not implement login in the MVP.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`AGENTS.md`](AGENTS.md). Issues and pull requests are welcome once the design stabilizes.

## License

TBD, see [ADR-0004](docs/decisions.md#adr-0004-license). Working proposal: AGPL-3.0-or-later, to match the Nextcloud ecosystem.
