<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Deploying Nuno

Nuno is one container next to the services it manages. It holds credentials that can change quotas everywhere, so treat it as a privileged service and read the two preconditions below before pointing it at anything.

Run `nuno doctor` first. It checks every precondition and prints, for each failure, the command that fixes it. `nuno doctor --fix` emits those commands as a script, which Nuno does not run itself: reaching `occ` would mean the Docker socket or exec into another container, and that trades a one-time setup step for root on the host ([ADR-0023](decisions.md#adr-0023-nuno-diagnoses-it-does-not-reconfigure-the-services-it-manages)).

## The two preconditions that actually bite

### Nextcloud: an app password cannot write a quota

`editUser` carries `PasswordConfirmationRequired`, and an app password can never stamp that confirmation, so every quota write comes back `403 Password confirmation is required`. Reads are unaffected, which is why this only shows up when Nuno tries to apply something. Two configurations work:

- a dedicated admin account without 2FA, authenticating with its real password, which is the path verified against the live stack, or
- whitelisting Nuno's container address in `allowed_no_password_confirmation_ranges` (Nextcloud 32 and later).

`nuno doctor` proves which one you have with a write probe, against an account Nuno already manages, writing the value that is already there. It runs after linking and never from a health check, so it cannot touch an account nobody linked ([ADR-0025](decisions.md#adr-0025-health-reads-the-write-probe-is-a-separate-step)).

### Nextcloud: move the default quota before Nuno manages the instance

`oidc_login_default_quota` is written on **every** OIDC login, with no check on the current value. A quota an admin set by hand survives until that person's next browser login, which makes Nuno look permanently broken in a way that is not Nuno's fault.

```shell
# See what is set. OCS cannot read system config, so Nuno cannot check this
# for you: this is the one precondition you have to look at yourself.
occ config:system:get oidc_login_default_quota

# Move it to the instance default, which new users inherit without anything
# being rewritten at login.
occ config:system:delete oidc_login_default_quota
occ config:app:set files default_quota --value='25 GB'
```

`user_ldap`'s `ldapQuotaAttribute` and `ldapQuotaDefault` do the same thing on the LDAP path. Those two **are** app config, so Nuno reads them and marks the provider degraded when either is set: it keeps reading and refuses to write ([ADR-0027](decisions.md#adr-0027-a-competing-writer-nuno-cannot-see)).

## Credentials

| What | Where | Least privilege |
|---|---|---|
| Nextcloud | `NUNO_PROVIDER_<NAME>_USERNAME` and `_PASSWORD` | a dedicated admin without 2FA, per the above |
| Immich | `NUNO_PROVIDER_<NAME>_API_KEY` | scoped to `adminUser.read` and `adminUser.update`, never `all` |
| Directory | `NUNO_LDAP_BIND_DN` and `NUNO_LDAP_BIND_PASSWORD` | a read-only bind user, not the directory admin |
| Usage API | `NUNO_ADMIN_KEY` | read-only, and the only credential a dashboard needs |
| Admin UI | `NUNO_ADMIN_PASSWORD` | only needed when no proxy authenticates in front |
| Admin UI behind a proxy | `NUNO_PROXY_SECRET` | a header only the proxy knows, so a container on the shared Docker network cannot reach the panel directly |

The three Nuno-side credentials are never interchangeable: the **admin key** is a machine reading `/api/v1/usage`, the **member token** is one person reading their own usage (and does not exist yet), and the **admin password** is a human in the UI ([ADR-0022](decisions.md#adr-0022-what-the-usage-endpoint-means-before-policy-exists)).

Generate the admin key with `openssl rand -hex 32`, or issue one from the UI, where it is shown exactly once.

## Compose

```yaml
services:
  nuno:
    image: ghcr.io/marcodellemarche/nuno:v0.1.0
    container_name: nuno
    restart: unless-stopped
    env_file: .env
    environment:
      # Inside a container, loopback would make Nuno unreachable from the
      # proxy. The acknowledgement says the wide bind is deliberate, and the
      # port mapping is what keeps it off the network.
      NUNO_ADDR: 0.0.0.0:8080
      NUNO_ALLOW_PUBLIC_BIND: "true"
      NUNO_DATA_DIR: /data
      NUNO_REFRESH_INTERVAL: 15m
      # Leave this unset until you have watched a few reconciles by hand.
      # With it set, Nuno refuses to start the timer unless a webhook is
      # configured too.
      # NUNO_RECONCILE_INTERVAL: 1h
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      # Put this on a snapshotted dataset. It is the only home for policy.
      - /tank/docker-config/nuno:/data
    networks:
      - edge
```

The volume must be writable by uid 65532, which is what the distroless image runs as.

## Behind the proxy

```caddyfile
nuno.{$BASE_DOMAIN} {
	import wildcard
	import solo_lan
	import sso
	reverse_proxy nuno:8080
}
```

With forward auth in front, leave `NUNO_ADMIN_PASSWORD` unset. Without it, set one: "no proxy yet" must never mean "no auth".

### Keep the panel off the shared network

The admin panel has no password of its own when a proxy authenticates in front of it, and it shares a Docker network with every other service. Without a gate, any container on that network can reach `nuno:8080` directly and skip the proxy entirely. `NUNO_PROXY_SECRET` closes that: the proxy injects `X-Nuno-Proxy-Secret`, and the panel refuses every request that does not carry it. `/healthz` and the key-authenticated `/api/v1/usage` stay reachable, because a dashboard widget reads usage server-side and never sees the proxy.

```caddyfile
reverse_proxy nuno:8080 {
	header_up X-Nuno-Proxy-Secret {$NUNO_PROXY_SECRET}
}
```

Set the same value on both sides with `openssl rand -hex 32`. Leave it unset only when Nuno is not behind a proxy, in which case set `NUNO_ADMIN_PASSWORD` instead.

## The dashboard widget

`GET /api/v1/usage` is the contract a shared dashboard consumes. For Homepage:

A shared dashboard is seen by everyone behind the proxy, and the widget is
fetched server-side with one key, so it cannot show "only my row". The
default is therefore the aggregate per service, which names nobody:

```yaml
- Storage:
    - Quotas:
        icon: mdi-harddisk
        widget:
          type: customapi
          url: http://nuno:8080/api/v1/usage?detail=service
          method: GET
          headers:
            Authorization: Bearer {{HOMEPAGE_VAR_NUNO_KEY}}
          display: dynamic-list
          mappings:
            items: rows
            name: name
            label: summary
            target: https://nuno.example.org   # the admin UI, for the detail
```

`summary` is one string, because the widget renders one label per row:
`42.7 GiB / 300 GiB (14.2%)`. The ceiling is the sum of the people's ceilings,
which answers "how much of what we allocated is used", not the disk capacity.

Two other modes exist. `?detail=provider` is one row per person and provider,
for a dashboard only admins see. The plain response without `detail` is the
nested contract, with `with_accounts=1` to drop the service accounts in the
directory.

## Showing each person their own quota

None of the `customapi` modes can show only the caller, because that widget is
fetched server-side with one key and has no idea who is looking. The `iframe`
widget is different: the browser loads it, so it carries the session cookie,
and the proxy's forward auth runs again on that request. That is what `GET /me`
is for:

```yaml
- Storage:
    - My quota:
        icon: mdi-harddisk
        widget:
          type: iframe
          name: quota
          src: https://dashboard.example.org/me
```

The URL is under the **dashboard's own origin**, not the admin one, and the
proxy routes `/me` to Nuno. That matters when the dashboard is reachable more
widely than the admin UI: a family member who is not on the VPN can open the
dashboard, and an iframe pointing at a private name like `nuno.example.org`
would simply not load for them. The path rides whatever access the dashboard
already has.

`/me` reads `Remote-User`, the header forward auth sets, and renders that
person's own per-provider summary. `NUNO_TRUSTED_PROXY` names the network the
**proxy** runs on, not the user's: the browser always talks to the proxy, and
the proxy always talks to Nuno, so the check is about who may set the header,
never about where the person is. It believes the header **only from that
network**, and with nothing configured it believes nobody and answers 403. It
is not behind the admin password, because the person reading it is not an
admin; the proxy is what authenticates them.

Read `status` before reading a number. A `quota_bytes` of `null` means unlimited under `ok` and `stale`, and means nothing at all under `unknown` or `unavailable`, where the read succeeded but the values did not. A user entry also carries `complete`, which is false when one of that person's services contributed nothing, so the totals are partial ([ADR-0026](decisions.md#adr-0026-the-usage-contract-needs-a-name-for-unknown)).

The array is sorted by `user_uuid` and each provider list by name, so a widget addressing fields by path gets a stable answer.

## Day two

**Run the precondition fix before the first apply.** Nuno cannot do it itself and cannot even read the setting that makes it necessary, so if you skip this, every quota Nuno sets is rewritten at the next OIDC login and the reconcile looks broken for a reason that is not Nuno. This is the step to run first on any stack, before `observe`, before `plan`, before `reconcile`.

```shell
nuno doctor --fix > nuno-fix.sh   # emit the commands, do not run them
less nuno-fix.sh                  # read them: they move the default off oidc_login
sh nuno-fix.sh
```

Then the read-only sequence, which changes nothing on any service:

```shell
nuno doctor                     # every precondition, with the fix for each
nuno observe                    # read everything, write nothing
nuno accounts                   # who owns what, and what needs a decision
nuno plan                       # what a reconcile would change
nuno explain <uid>              # the whole chain that produced a number
nuno reconcile --dry-run        # the same, through the applier
nuno reconcile                  # apply, refusing anything guarded
nuno reconcile --allow-shrink   # apply, including quotas below current usage
nuno runs                       # recent runs and the changes they made
```

Exit codes are a contract: `0` clean, `1` error, `2` applied but incomplete (guarded, throttled, or a non-empty dry run), `3` configuration or startup failure.

While `nuno serve` is running, a command cannot open the database: one process writes at a time, and the HTTP client mode that would let both coexist is not written yet. Stop the server, or use the UI, which does the same things.

## Adopting a stack that already has quotas

```shell
nuno observe
nuno adopt      # import each provider's current quota as a per-user override
nuno plan       # should be empty
```

Anything the plan still proposes after adopting is a real difference, not an artefact. Adopt skips an account whose ceiling could not be read, because an unreadable value is not an instruction.

## Backup

The database is the only home for policy, so it has to be recoverable.

```shell
sqlite3 /tank/docker-config/nuno/nuno.db ".backup '/tank/backups/nuno.db'"
```

Never copy a live WAL database file. Nuno's own pre-migration copies use `VACUUM INTO`, which is safe on a live database, and they land in `/data/backups/` with the last five kept.

## Upgrading

Migrations run only from `nuno serve` and `nuno migrate`, after copying the database. A database written by a newer Nuno refuses to start rather than being read with a schema this binary does not know. Every other command refuses on a mismatch and names `nuno migrate`.
