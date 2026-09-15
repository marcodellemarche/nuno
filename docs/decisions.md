# Architecture Decision Records

This is a log. Accepted decisions are not rewritten. If one is reversed, a new entry supersedes it. Open decisions are marked **[OPEN]** and should be resolved before the milestone that depends on them.

Format: context, options, decision, consequences.

ADR-0001 to ADR-0010 were written during the design freeze of 2026-09-14. A review on 2026-09-15, checking the design against provider source code and against the stack it will actually run on, found that several of them rested on assumptions that turned out to be false. ADR-0011 to ADR-0024 supersede or extend them, the last five of those written against responses captured from the live stack. The originals are kept because the reasoning that was wrong is worth reading next to the reasoning that replaced it.

ADR-0025 to ADR-0027 were raised at the start of M1, when implementing the design surfaced three things it did not settle. Read this file from the bottom: a later entry always wins over an earlier one.

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

**Status: Accepted (2026-09-14), partly superseded 2026-09-15 by [ADR-0019](#adr-0019-what-v01-does-not-do).** The export/import consequence below is deferred; the source-of-truth decision stands.

**Context.** Three places could hold quota intent: LLDAP groups, Nuno's database, and each service. Duplicating intent causes drift.

**Options.**
1. LLDAP groups are the only source and Nuno is stateless. Pure, but LLDAP has no place for a numeric budget and per-user overrides get awkward.
2. Nuno's database is the source, and LLDAP provides identity plus the group-to-tier mapping. Rich model, per-user overrides, audit history. Nuno becomes stateful.
3. Nuno's database is a cache of a YAML file (GitOps). Reproducible, but needs a file-watch or deploy story.

**Decision.** Option 2. LLDAP is identity and group membership. Nuno's database holds tiers, budgets, allocations and overrides. A group maps to a tier, and the tier lives in Nuno. This keeps identity in one place and policy in another, with no duplication of the numeric budget.

**Consequences.**
- The database is the only place where policy lives, so it must be backed up. `nuno export` and `nuno import` (YAML) ship in M3, not later, so a lost SQLite file is recoverable.
- The export format is the same shape a future GitOps mode would consume (FR-16), so that mode becomes a loader, not a rewrite.
- A user removed from LLDAP is not deleted from Nuno. It is marked `orphaned`, keeps its audit history, and is excluded from reconcile until an admin deletes it or it reappears. Deleting a user in Nuno never touches provider data.

## ADR-0003: Budget splitting across providers

**Status: Accepted (2026-09-14), partly superseded 2026-09-15 by [ADR-0011](#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer) (quota representation and rounding).**

**Context.** A person has one budget, but each service needs a concrete ceiling. Our current homelab uses 25% Nextcloud and 75% Immich.

**Options.**
1. Fixed percentages per tier. Simple, matches current practice.
2. Weights, normalized. More flexible, harder to explain.
3. Explicit absolute bytes per provider, summed to the budget. Most precise, most verbose.
4. No split: the same total on every provider. Simple but over-commits storage.

**Decision.** Support both 1 and 3. A tier declares allocations as either `percent` or `absolute`, and the resolved desired quota is always absolute bytes. Percentages are convenience, bytes are the truth.

**Consequences.**
- Resolution rounds down to whole MB. A 25 percent allocation of 100 GB resolves to 25600 MB exactly, and any remainder is lost rather than over-committed.
- A tier that declares no allocation for a provider means **do not touch that provider for this user**. It does not mean zero, and it does not mean unlimited. This is the single most dangerous ambiguity in the model, so the planner never emits a change for a provider a tier is silent about.
- Explicit unlimited exists and is distinct from absent: `mode: unlimited` resolves to a nil quota. Only a tier that says so explicitly can lift a ceiling.
- Percentages need not sum to 100. Summing under 100 is legal and means part of the budget is unallocated. Summing over 100 is legal (deliberate over-commit) but the planner warns once per tier, since it lets a person exceed their nominal budget.
- Mixing modes inside one tier is legal: absolute allocations are subtracted from the budget first, then percentages apply to the remainder.

## ADR-0004: License

**Status: Accepted (2026-09-14).**

**Context.** Nuno is aimed at self-hosters and sits next to Nextcloud in the same stack. It will be published before M4.

**Options.** AGPL-3.0-or-later, which aligns with Nextcloud and keeps network forks open. MIT or Apache-2.0, maximum adoption. MPL-2.0, file-level copyleft as a middle ground.

**Decision.** AGPL-3.0-or-later. The value of the project is a shared ecosystem of provider adapters, not adoption by vendors who would close it. Aligning with Nextcloud's own license is also the least surprising choice for the audience.

**Consequences.**
- `LICENSE` lands with the first public commit, not at M4, so contributions are never made under an undefined license.
- Every source file carries an SPDX identifier: `// SPDX-License-Identifier: AGPL-3.0-or-later`.
- Provider adapters are part of the same work. A closed-source adapter is not a supported pattern, and the plugin system is compile-time, not a stable ABI.

## ADR-0005: Provider version compatibility

**Status: Superseded 2026-09-15 by [ADR-0015](#adr-0015-version-policy-simplified).** The premise about Immich was two years out of date.

**Context.** Nextcloud's OCS Provisioning API is stable. Immich's API is explicitly unstable across versions.

**Options.**
1. Pin to a tested Immich version and refuse newer ones.
2. Feature-detect from `/api/server/version` and warn on untested versions.
3. Adapter variants per API version.

**Decision.** Option 2. Detect and warn, keep adapters tolerant to extra fields, and keep a version matrix in the docs. Never fail hard on an unknown minor version, but log it.

**Consequences.**
- Every adapter reports a detected version in `Health()`, and the UI shows tested, untested or unknown per provider.
- Each adapter declares a tested range. Outside it, Nuno warns at startup and on every reconcile report, and the version matrix in the README is part of the definition of done for an adapter change.
- Decoding is tolerant: unknown JSON fields are ignored, missing optional fields degrade a capability rather than failing the run. A missing **required** field is still a hard `ProviderError`, because guessing a quota is worse than stopping.
- Contract test fixtures are recorded per tested version, so a version bump is a new fixture set rather than an edit of the old one.

## ADR-0006: UI approach

**Status: Accepted (2026-09-14).**

**Context.** The UI is an admin panel: lists, usage bars, a few forms. No rich client-side state.

**Options.**
1. Server-rendered with `html/template`, HTMX and Tailwind. No build step, simple deploys.
2. A SPA with React, Vue or Svelte over a REST API.
3. TUI or CLI only.

**Decision.** Option 1 for the MVP. The REST API still exists underneath, so a SPA can be added later without changing the backend.

**Consequences.**
- Templates and static assets are embedded with `embed.FS`. The binary serves the UI with no files on disk.
- No Tailwind. "Tailwind without a build step" means either a CDN, which breaks an offline container, or the standalone CLI, which is a build step with a downloaded binary. An admin panel of lists, bars and four forms does not justify either. A single hand-written stylesheet, a few hundred lines, is embedded instead.
- HTMX is vendored as a file in the repository and embedded, not loaded from a CDN. The container must work with no outbound internet access.
- Every UI action maps to an API route that works without HTMX. The UI is a client of the API, never a parallel path into the domain.

## ADR-0007: Tier resolution when a user is in several tier groups

**Status: Superseded 2026-09-15 by [ADR-0012](#adr-0012-tier-resolution-happens-per-provider).** The rule below is wrong: its own justification does not hold.

**Context.** FR-4 makes group membership the primary way to assign a tier, and FR-14 requires deterministic precedence. `ARCHITECTURE.md` resolves a user override first, then a group tier, then the default. The unresolved case is a user in two groups that both map to a tier: `photographers` and `staff`, for example. Go map iteration is randomized, so an unspecified rule is not merely undocumented, it is unstable between runs, which would make reconcile non-idempotent.

**Options.**
1. Explicit numeric priority on each tier. Ties are a configuration error reported at startup.
2. Highest budget wins.
3. First match in a configured ordered list of groups.
4. Refuse to resolve and mark the user as conflicted, requiring an override.

**Decision.** Option 2, highest `BudgetBytes` wins. It needs no extra configuration, it is stable across runs, and it fails in the direction that does no harm: a person in two groups gets the more generous of the two, never the stingier. Option 1 adds a field every admin must reason about to solve a case most stacks never hit.

**Consequences.**
- Ties on budget are broken by tier name, ascending, so the result is deterministic. Because two tiers with the same budget can still differ in allocations, every tie is reported: a warning at startup, a marker on the user row in the UI, and a line in the plan. Deterministic but visible, in the spirit of "fail loud".
- The resolved tier and the reason for it (`override`, `group:<name>`, `default`) are stored on the user and shown in the UI. An admin must never have to guess why someone got 500 GB.
- If a resolved tier has no allocation for a provider, ADR-0003 applies: that provider is left untouched.

## ADR-0008: Linking a person to their provider accounts

**Status: Accepted (2026-09-14), extended and partly superseded 2026-09-15 by [ADR-0013](#adr-0013-identity-keys-and-the-link-lifecycle).** The per-provider match key survives; the specific keys were wrong.

**Context.** Nuno writes quotas. Writing one to the wrong account is the worst thing it can do, and nothing in the spec said how a directory user is matched to an account on a provider. The providers disagree on identity: Nextcloud's primary key is the `uid` and email is optional, while Immich has no username at all and is keyed by email. Real stacks also contain mismatches, for example `alice` on Nextcloud and a personal address on Immich.

**Options.**
1. Match on email everywhere.
2. Match on username everywhere.
3. A per-provider match key, with an explicit manual link table for exceptions.
4. Manual links only, no automatic matching.

**Decision.** Option 3. Each adapter declares which identity field it matches on (Nextcloud: `uid`, Immich: `email`), the key is configurable per provider instance, and a `account_links` table holds explicit user-to-external-account links that always win over the automatic match.

**Consequences.**
- `ExternalUser` carries both `ExternalID` (the provider's own stable key, used for writes) and the match field. Once a link is established, writes address the account by its provider-side ID, so renaming a user in the directory does not retarget the write.
- Matching is case-insensitive and Unicode-normalized for email, exact for usernames.
- Ambiguity is never resolved by guessing. Two accounts matching the same user, or a user matching no account, are surfaced as `unlinked` or `ambiguous` and excluded from reconcile until an admin links them. This is the mechanism behind FR-3 (unmanaged users): an account on a provider that matches nobody is unmanaged, and Nuno never writes to it.
- Nuno does not create accounts. If a person has no account on a provider, there is nothing to set a quota on, and that is reported, not provisioned.
- The link table is part of any export. (Export itself is deferred, see [ADR-0019](#adr-0019-what-v01-does-not-do); the point stands that links are policy, not observation.)

## ADR-0009: Guardrail against shrinking a quota below current usage

**Status: Accepted (2026-09-14), extended 2026-09-15 by [ADR-0016](#adr-0016-safety-model-unknown-state-re-check-and-honest-partial-application).**

**Context.** NFR-7 says Nuno must not corrupt provider state, but the model allows a plan that sets a quota below the bytes a person already stores. Nextcloud and Immich both accept this and immediately block uploads, and Immich also stops mobile backup silently. A single wrong tier assignment, or a percentage edited from 75 to 25, is enough. This is the most realistic way Nuno can hurt real users, and it was not covered.

**Options.**
1. Apply it, log a warning.
2. Always refuse, no exception.
3. Classify the change as risky, require explicit consent to apply.

**Decision.** Option 3. The planner marks any change whose target is below the observed `UsedBytes` as `shrink-below-usage`. Such changes are computed and shown in every plan, but applied only with `--allow-shrink` on the CLI or an explicit confirmation in the UI. Without consent they are reported as skipped-by-guardrail, and the run's exit status reflects that the plan was not fully applied.

**Consequences.**
- This is the one sanctioned exception to "no silent skips": the skip is loud, named in the report, and visible in the UI. The run is not reported as clean.
- The guardrail compares against observed usage from the same reconcile cycle. If usage could not be read for that provider, the change is treated as risky, because unknown usage is not proof of safety.
- A scheduled reconcile (FR-36) never carries `--allow-shrink`. Automation cannot cross this line unattended.
- Skipped changes reappear in the next plan. The guardrail defers, it never rewrites the desired state.
- Notifications (FR-60) cover a guarded plan, not just outright failure.

## ADR-0010: Member-facing usage endpoint

**Status: Superseded 2026-09-15 by [ADR-0017](#adr-0017-usage-api-dashboard-first-per-person-second).** The endpoint survives; its priority and its first consumer were wrong.

**Context.** The reason for a per-person budget is that the person can see it. `REQUIREMENTS.md` originally put Members out of the MVP entirely, and Nuno implements no login, so there was no way for someone to answer "how much space do I have left" other than asking an admin. The intended consumer is a homepage dashboard, Glance in particular, whose custom-api widget consumes JSON.

**Options.**
1. Out of the MVP.
2. An HTML page behind the existing forward-auth layer.
3. A JSON endpoint authenticated by a per-user token issued by Nuno.
4. One unauthenticated JSON endpoint holding everyone, reachable on the internal network only.

**Decision.** Option 3. `GET /api/me/usage` returns the caller's budget, per-provider ceilings and usage, authenticated by an opaque per-user token in an `Authorization: Bearer` header. An admin issues, revokes and rotates tokens from the UI.

**Consequences.**
- Tokens are random 32-byte values, shown once at creation and stored only as a SHA-256 hash. A lost token is rotated, never recovered. Token values never appear in logs.
- A token grants read access to exactly one user's own usage. There is no member write path and no member ability to see anyone else.
- The response is a stable, documented JSON shape, versioned under `/api/v1`, because it becomes a contract with dashboard widgets. It is the same data the admin UI renders, served from the same domain code.
- This does not make Nuno an identity provider and does not add login (assumption 5 in `REQUIREMENTS.md` still stands). It is a read-only capability token, the same pattern as an RSS feed key.
- Rate limiting is per token. A public-facing deployment is still the reverse proxy's job.

## ADR-0011: Quota is a tagged value, not a nullable integer

**Status: Accepted (2026-09-15), corrected the same day by [ADR-0021](#adr-0021-corrections-from-the-captured-responses).** Supersedes the rounding rule in [ADR-0003](#adr-0003-budget-splitting-across-providers). The `ProviderDefault` kind below was removed: no adapter can emit it. Needed for M1.

**Context.** The provider interface modelled a quota as `*int64` where nil means unlimited. A quota read from a provider actually carries four distinct meanings, and three of them are not a number: the value is unknown (the read failed, or the provider returned a fabricated placeholder), the account is explicitly unlimited, the account inherits an instance-wide default, or it is a byte count. Collapsing those into a nullable integer produces silent wrong writes. Verification against Nextcloud `stable34` made this concrete: unlimited is `-3` on the wire (`FileInfo::SPACE_UNLIMITED`), the `quota` field can also be the string `"none"`, every field in the quota object is optional, and a failed storage lookup returns `used: 0` with no `total` at all. A naive decode reads unlimited as the number -3 and plans a change.

There is a second, worse problem. Nextcloud does not store the bytes it is given: `parseAndValidateQuota` passes the value through `Util::humanFileSize`, which rounds to one decimal per unit. Writing 26844594176 stores `"25 GB"` and reads back 26843545600. The planner then emits the same change forever, which violates FR-32 while passing every test against a mock. ADR-0003's "round down to whole MB" does not help above 1 GB.

**Options.**
1. Keep `*int64` and handle the special cases inside each adapter.
2. A tagged value in core, plus a per-provider normalization function used by the planner.
3. Compare with a tolerance band instead of for equality.

**Decision.** Option 2.

```go
type QuotaKind uint8 // Unknown, Unlimited, ProviderDefault, Bytes
type Quota struct { Kind QuotaKind; Bytes int64 }

// NormalizeQuota returns what this provider will actually store when asked for q.
NormalizeQuota(q Quota) Quota
```

The planner compares `NormalizeQuota(desired)` against the observed value, never raw bytes against raw bytes.

**Consequences.**
- `Unknown` is never a write target and never a write trigger. A provider whose state could not be read produces no plan entries for that provider, full stop.
- Every adapter must implement `NormalizeQuota` and satisfy the property `Normalize(Normalize(x)) == Normalize(x)`. The Nextcloud implementation is the `humanFileSize` round trip; for Immich it is the identity.
- Adapters must read the *configured* quota, not the effective one. On Nextcloud that is `ocs.data.quota.quota`, not `quota.total`, which is `free + used` and is clamped by actual disk space: reading `total` means a 1 TB quota on a 500 GB volume reports 500 GB forever.
- Rounding down to whole MB is dropped. Normalization replaces it, because it encodes what the provider will really do instead of guessing.
- Units are stated once and never again: all stored and transported values are bytes, all human-facing sizes are IEC (GiB, MiB), and the documented tiers of 100 GB and 200 GB mean 100 GiB and 200 GiB. Every previous example in the docs that wrote "GB" for a power-of-two value was wrong and is corrected.
- `audit_log.from` and `audit_log.to` store the tagged form, or unlimited and unknown collide in the one table meant to explain what happened.
- Go hazard, called out because it is a one-character silent failure: a nil `*int64` marshalled with `omitempty` disappears from the JSON body, and Immich reads an absent `quotaSizeInBytes` as "do not change" rather than as unlimited. The unlimited path must emit an explicit `null`.

## ADR-0012: Tier resolution happens per provider

**Status: Accepted (2026-09-15).** Supersedes [ADR-0007](#adr-0007-tier-resolution-when-a-user-is-in-several-tier-groups). Needed for M2.

**Context.** ADR-0007 resolved a multi-group user by picking the tier with the highest `BudgetBytes`, justified as "fails in the direction that does no harm, never the stingier". That justification is false, and the rule is not even well defined.

It is not well defined because a tier meant to be unbounded declares unlimited allocations and has no meaningful budget, so it competes as zero and loses to every bounded tier. It is false because the total budget is not monotone in the per-provider ceilings it produces. Concretely, with `photographers` at 100 GiB allocated entirely to Immich and `staff` at 120 GiB split 25 percent Nextcloud and 10 percent Immich, adding a photographer to `staff` as a promotion picks `staff` on budget and drops their Immich ceiling from 100 GiB to 12 GiB.

**Options.**
1. Keep whole-tier resolution, make `Budget` a tagged value so unlimited is the maximum, and drop the false claim.
2. Resolve per provider: for each provider, take the most generous resolved ceiling across all candidate tiers.
3. Explicit numeric priority per tier, ties are a configuration error.

**Decision.** Option 2. A tier is a set of independent per-provider offers, not a single ranked number. For each provider, the effective ceiling is the maximum resolved value across the tiers the user is entitled to, where `Unlimited` is the top element and an absent allocation contributes nothing.

**Consequences.**
- "Most generous" is now true per provider, which is the property that was actually wanted and the one users can verify on their own dashboard.
- Equal-budget ties disappear as a concept. There is nothing to break by tier name and no startup warning to emit, which also removes the unimplementable part of ADR-0007 (ties depend on group membership, which is not known at startup).
- The resolution reason becomes per provider: the UI and `nuno explain` show, for each provider, which tier supplied the winning ceiling. FR-19 is now per (user, provider).
- A user override still wins outright over all group-derived tiers, unchanged from FR-14.
- `Budget` remains a display concept: the sum of resolved ceilings, shown to the user, not the thing resolution is computed from. When any allocation is unlimited, the displayed budget is unlimited.
- Tiers get an integer primary key with a unique name, so renaming a tier does not break group mappings.
- The operational consequence, which an admin will meet on day one and which must be documented in the UI next to the tier field: **you cannot restrict someone by adding them to a restrictive tier**. The most generous ceiling always wins, so demoting a person means removing the generous group, not adding a stricter one. This is the price of "never penalized for belonging to one group too many", and it is the right trade, but it surprises anyone who expects last-write-wins.

## ADR-0013: Identity keys and the link lifecycle

**Status: Accepted (2026-09-15).** Extends and partly supersedes [ADR-0008](#adr-0008-linking-a-person-to-their-provider-accounts). Needed for M1.

**Context.** ADR-0008 established the right principle (a per-provider match key, explicit manual links, no write without an unambiguous link) and then named the wrong keys. In the target stack, Nextcloud sits behind `oidc_login`, which auto-registers accounts with a UUID as the user id: the account id is a UUID, not the directory username. The same is true of the `user_ldap` backend, whose Internal Username defaults to being derived from the UUID attribute. Matching Nextcloud on the naked uid would have produced zero links out of two users.

Three further gaps: the model had no revalidation rule, so a renamed, deleted or retargeted account would be written to forever or would abort every run; identity keyed on the LLDAP uid loses every override and link when a user is renamed, and the resulting fresh user resolves to the default tier and gets written; and `AccountLink` conflated a link with a match outcome, leaving no row shape for an account that matches nobody or for two users that match one account.

**Decision.**
- Identity is keyed on the directory's stable UUID. LLDAP exposes `entryuuid` for both users and groups. `User` carries `(source, source_uuid)` as its unique identity, with `uid` and `email` as mutable attributes. Group identity is `entryuuid` too, because a group's display name is its DN and renaming a group would otherwise move everyone to the default tier.
- The match key per provider is configurable, with honest defaults: `email` for Immich (it has no username), `email` for Nextcloud with the configured LDAP username attribute as an alternative, never the naked id. Username matching is case-insensitive, because LLDAP itself compares user ids case-insensitively. Email matching is NFC-normalized and ASCII-lowercased once at write time into an indexed column.
- If a provider yields zero links out of N users, that is a startup failure with an explanatory message, not N `unlinked` rows to scroll through.
- Links and match outcomes are separate. `account_links(user_id, provider_id, external_id, origin)` holds only real links and is unique on both `(user_id, provider_id)` and `(provider_id, external_id)`, the second constraint being what actually prevents two people's policies from writing to one account. Match problems live in `link_issues(provider_id, kind, user_id?, external_id?, detail)`, recomputed each observe, and feed FR-57.
- Links are revalidated every cycle against the accounts actually seen. A link whose external id is absent becomes `dangling`: excluded from the plan and reported, never an error that aborts the run. A `matched` link whose key no longer matches becomes `stale` and waits for an admin. A `manual` link is never auto-invalidated, only reported.
- Nuno never writes to an account that was not observed in the current cycle.

**Consequences.**
- `external_accounts` is keyed by `(provider_id, external_id)` with a nullable `user_id`, which is what makes FR-3 (unmanaged accounts) representable at all.
- A rename in the directory is a no-op: uid and email are attributes, the UUID is the identity.
- One deleted provider account can no longer block the reconcile of everyone else.

## ADR-0014: Provider write paths as they actually are

**Status: Accepted (2026-09-15).** Corrected the same day by [ADR-0021](#adr-0021-corrections-from-the-captured-responses) on two response shapes, and by [ADR-0025](#adr-0025-health-reads-the-write-probe-is-a-separate-step) on where the write probe runs. Needed for M1.

**Context.** The provider notes in `ARCHITECTURE.md` were written from memory. Verification against Nextcloud `stable34` and Immich v3.2.0 (spec `v3.2.1`) contradicted several of them, and two are blocking.

**Decisions, Nextcloud.**
- **App passwords cannot write.** `editUser` carries `PasswordConfirmationRequired`, and an app password can never stamp the confirmation, so every quota write returns `403 Password confirmation is required`. Upstream confirms this is by design. Reads are unaffected. Nuno therefore documents two supported configurations: `allowed_no_password_confirmation_ranges` whitelisting Nuno's container address (Nextcloud 32 and later), or a dedicated admin account without 2FA authenticating with its real password. The adapter's `Health()` performs a write probe, setting one user's quota to its current value, so a misconfiguration surfaces at startup rather than in the middle of a reconcile.
- **Writes are rate limited**: 50 calls per 10 minutes on `editUser`. A 429 is a typed `ThrottledError`, writes are paced, and a run that hits the ceiling stops cleanly and resumes next cycle rather than failing.
- **OCS returns HTTP 200 on failure.** The real result is `ocs.meta.statuscode` (997 unauthorized, 998 not found, 996 server error, 100/200 ok). Any error handling built on the HTTP status reads an auth failure as success. `format=json` must be requested explicitly, or the response is XML.
- **Reading is not free of side effects.** `GET /users/details` resolves each user's home folder and creates `/uid` and `/uid/files` if missing. Nuno's observe pass materializes home directories for users who never logged in. This is acceptable but must be documented, and it means "observe only reads" is false as a blanket statement.
- **Writes can be refused by instance policy**: `files/max_quota` and `files/allow_unlimited_quota` can make an explicit unlimited allocation unimplementable. That is a typed error and a capability, not a generic failure.
- Nuno cannot distinguish an explicitly set quota from an inherited default: Nextcloud resolves `default` to the instance value before the API sees it.
- A second writer will fight Nuno. `user_ldap`'s `ldapQuotaAttribute`/`ldapQuotaDefault` rewrite the quota on every user refresh; `oidc_login`'s `oidc_login_default_quota` applies at auto-registration. Both are documented preconditions and startup checks where detectable.

**Decisions, Immich.**
- `GET /api/admin/users` and `PUT /api/admin/users/{id}` are correct today, but `PUT` is deprecated as of v3 in favour of `PATCH`, which carries the same DTO and permission and is deliberately excluded from the OpenAPI spec. The adapter uses `PUT` now and carries a note, because codegen will never reveal the successor.
- `quotaUsageInBytes` is a cached counter, not a live measurement. It is adjusted on asset create and delete, fully recomputed only by a nightly job the admin can disable, and the recompute excludes external library assets. It is therefore not a trustworthy basis for the shrink guardrail on its own.
- `GET /api/server/statistics` returns live per-user usage as a SQL aggregate. That is the honest source for FR-40, and disagreement beyond a threshold between it and the cached counter marks the provider degraded.
- `0` is a legal quota and blocks all uploads. Omitting the key means "do not touch". Only an explicit `null` means unlimited.
- API keys are scoped. Nuno's documented key holds `adminUser.read` and `adminUser.update`, never `all`.
- Immich has no instance-wide default quota, so `CanSetDefaultQuota` is false for it. The OAuth `storageQuotaClaim` applies only at user creation and is never re-synced, which is the single strongest reason this project exists and now belongs in the README.

**Verified on 2026-09-15** against the live stack, not only against source. Recorded responses are in `tests/fixtures/`.

- The app password probe returned HTTP 403 with `ocs.meta.message` "Password confirmation is required". The same no-op write with a real admin password returned 200. `allowed_no_password_confirmation_ranges` is absent from that instance's config, so **the dedicated-admin path is the one that works today**.
- The round trip loses data exactly as predicted: writing 26844594176 stores `"25 GB"` and reads back 26843545600. `NormalizeQuota` is mandatory, not precautionary.
- The Nextcloud account id for the OIDC-registered user is a UUID (`11111111-...`). Matching on the naked username would have linked nobody.
- `-3` appears on the wire for an unlimited account. The string `"none"` was not observed on this instance, so it stays a case to handle rather than an assumption to rely on.
- Immich's cached counter and the live statistics aggregate disagreed by about 9.5 MB for one user and matched exactly for the other, with the nightly recompute enabled. Small, but enough to prove they are not the same number.

**`oidc_login` is a confirmed competing writer.** `LoginService::updateBasicProfile()` sets the quota on every OIDC login, and the branch has no condition on the current value: if the `ownCloudQuota` claim is absent and `oidc_login_default_quota` is set, the default is written over whatever the user had. A quota an admin set by hand survives only until that person's next browser login. This is the most damaging interaction available to Nuno on Nextcloud, because it makes reconcile permanently non-idempotent in a way that looks like a Nuno bug.

The remedy is not code: leave `oidc_login_default_quota` empty and set the instance default through `files/default_quota` instead, which new users inherit without anything being rewritten at login. The Nextcloud adapter reads the setting at startup and refuses to manage the instance while it is set, naming the fix in the error.

**Consequences.** The contract-test fixture set must include a 997 body returned with HTTP 200, a `quota` field serialized as `[]`, a `quota` of `-3` and of the string `"none"`, a user with `used: 0` and no `total`, and a 429. These are the shapes that break naive decoding, and each one is a real response. The fixtures recorded on 2026-09-15 cover the healthy shapes; the degraded ones still need to be written by hand or captured from a broken instance.

## ADR-0015: Version policy, simplified

**Status: Accepted (2026-09-15).** Supersedes [ADR-0005](#adr-0005-provider-version-compatibility). Needed for M1.

**Context.** ADR-0005 was built on "Immich's API is explicitly unstable". That was true of the 1.x era. Immich has followed semver since v2, confines breaking changes to majors, publishes `:v3` metatags and a migration guide, and marks endpoint stability in the spec itself. `/api/admin/users` has been stable since v2 and has not moved since 2024. The machinery ADR-0005 prescribed (a tested/untested/unknown tri-state, per-version fixture directories, startup and per-run warnings, a version matrix) is sized for a problem that no longer exists.

**Decision.** Keep version detection, drop the apparatus. Each adapter reports the detected version from `Health()` and declares a supported major. Outside it, Nuno warns once per run and keeps going. Inside it, nothing is said. Fixtures are recorded once per supported major, not per minor. Tolerant decoding stays, and it is enforced by the fixture list in [ADR-0014](#adr-0014-provider-write-paths-as-they-actually-are) rather than by intent.

**Consequences.** Version detection extends to the identity source too: LLDAP's root DSE exposes `vendorName` and `vendorVersion`, which is the same signal for free. The README keeps a short "known to work with" line instead of a matrix.

## ADR-0016: Safety model: unknown state, re-check, and honest partial application

**Status: Accepted (2026-09-15).** Extends [ADR-0009](#adr-0009-guardrail-against-shrinking-a-quota-below-current-usage). Needed for M3. The clause below that makes a missing login timestamp unknown is superseded by [ADR-0024](#adr-0024-a-person-who-has-never-logged-in-still-gets-their-quota): a complete quota object with `firstLoginTimestamp` of 0 is known and zero.

**Context.** ADR-0009's guardrail is sound but has three holes, and provider verification made two of them concrete.

First, it overloads one consent. Writing to a provider whose usage could not be read was folded into the same `--allow-shrink` flag as deliberately shrinking one person. An admin who needs one legitimate shrink during an Immich outage would write to every user blind. Second, the class is computed at plan time and consent is given at apply time, with a human in between: a phone finishing a backup in those forty seconds turns a `safe` change into an unguarded shrink below usage, which is precisely what the guardrail exists to prevent. Third, "no partial success" is not implementable, because there is no rollback for a quota already written.

And the fabricated-zero case is real, not theoretical: Nextcloud returns `used: 0` with no `total` for a user whose filesystem has never been set up, so a destructive shrink reads as safe.

**Decision.**
- Two distinct classes with two distinct rules. `shrink-below-usage` is consent-gated via `--allow-shrink` or a UI confirmation. `unknown-state` is never applied, with no flag and no override, because there is no safe consent for writing against state you do not have.
- Usage is `unknown` whenever the provider omits `total` or `relative`, whenever `used` is absent, whenever Nextcloud reports no login timestamp, and whenever Immich's cached counter disagrees with `/api/server/statistics` beyond a threshold. Unknown is a first-class value per [ADR-0011](#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer), not an absence to be guessed at.
- Plans are persisted with the observed snapshot they were computed from. At apply time the affected accounts are re-observed and re-classified, and any change whose class moved is refused. A plan older than a configurable window is refused outright.
- The comparison is `target <= used`, not `<`: a quota exactly equal to usage already blocks uploads.
- "No partial success" is replaced with what is true: the run stops at the first error, earlier writes stand and are audited, and the state is safe because re-running converges. Reports carry applied, failed, skipped and guarded counts. One provider failing degrades that provider and does not stop work on the others.
- Exit codes are a contract: 0 clean, 1 error, 2 applied but incomplete (guarded or throttled), 3 configuration or startup failure. `--dry-run` returns 2 when the plan is non-empty, following Terraform's precedent.
- Guarded changes reappear every cycle by design, so notification is keyed on `(user, provider, from, to)` and fires on first appearance and on change, never once per cycle.

**Consequences.** FR-32's property test is worded as "with consent given, or with no risky changes present", otherwise it is false whenever a guarded change exists. A fourth dedicated test joins the three already mandated: a shrink against unknown usage is refused.

## ADR-0017: Usage API: dashboard first, per-person second

**Status: Accepted (2026-09-15).** Supersedes [ADR-0010](#adr-0010-member-facing-usage-endpoint). Needed for M1.

**Context.** ADR-0010 chose a per-user token endpoint so a person could see their own remaining space on a homepage dashboard. The consumer was assumed to be Glance, which turns out not to support per-user dashboards at all. The actual consumer is Homepage v2.3.0, already deployed, already behind Authelia forward auth, already reading widget keys from `.env` as `HOMEPAGE_VAR_*`, and with an existing `customapi` widget mechanism. The stack has two human users and both are admins. The homelab's own quota document had already identified the missing piece: "a customapi widget that queries the two APIs and sums the values: possible, but a small project in itself."

So the per-person endpoint is not wrong, it is second. A shared dashboard needs one call that returns everyone.

**Decision.** Two endpoints, in this order.
- `GET /api/v1/usage` returns every user's budget, per-provider ceilings and usage. Authenticated by a single admin key. This is what the Homepage `customapi` widget consumes, and it ships in M1.
- `GET /api/v1/me/usage` returns one person's own data, authenticated by an opaque per-user token, hashed at rest, shown once, revocable. It ships when there are non-admin members to serve, which the SSO explicitly anticipates.

**Consequences.**
- The response shape is fixed before anything consumes it, because it is a contract. Per-provider entries carry their own `observed_at` and a `status` of `ok`, `stale` or `unavailable`, so a week-old Immich number cannot masquerade as current. `null` means unlimited at every level, `used_percent` is `null` when the ceiling is unlimited or zero rather than `NaN` (which Go refuses to marshal, turning the most privileged users into a 500). A top-level `"schema": 1` lets a widget branch.
- A `managed` flag per provider entry surfaces the ADR-0003 leak: a ceiling left over from a policy that no longer allocates that provider.
- The admin key and the member tokens are separate credential types with separate scopes. Neither grants writes.
- `observe()` needs to run without a reconcile, or the dashboard shows numbers as fresh as the last time someone clicked a button. A scheduled read-only usage refresh ships with the endpoint. None of FR-39's caution applies to it: observing does not write.
- The path is `/api/v1/...` everywhere. ADR-0010's `/api/me/usage` was an inconsistency with FR-45 and `ARCHITECTURE.md`, and this entry settles it.

## ADR-0018: One writer: the CLI talks to the server

**Status: Accepted (2026-09-15).** Needed for M1.

**Context.** FR-35 requires reconcile from both the UI and a CLI, and NFR-2 mandates a single embedded SQLite file. Nothing coordinated the two. A `nuno reconcile` in a shell while someone clicks Reconcile in the UI gives two processes computing plans from the same observed state and both writing quotas, with `SQLITE_BUSY` arriving mid-run. A newer CLI binary would also run migrations under a live server.

**Options.**
1. An advisory run lock plus a schema-version check in every subcommand.
2. The CLI is a thin API client and only touches the database when no server is running.

**Decision.** Option 2. When a server is reachable, `nuno reconcile`, `nuno plan` and friends are HTTP calls to it. Offline mode (no server) opens the database directly and takes an exclusive run lock. Only `nuno serve` and `nuno migrate` apply migrations; every other subcommand refuses to start on a schema mismatch.

**Consequences.**
- One process writes at a time, by construction, which removes the whole class rather than mitigating it.
- The SQLite driver is settled rather than offered as a choice: `modernc.org/sqlite` (no cgo), one DSN defined in one place with WAL, `busy_timeout`, `foreign_keys(1)` and `synchronous(NORMAL)`, a single-connection writer pool and a separate read pool. The two candidate drivers differ in DSN syntax and driver name in ways that fail silently, so naming one is part of the decision.
- Migrations run on boot from `nuno serve`, after copying the database file to `/data/backups/` and keeping the last few. A database newer than the binary refuses to start.

## ADR-0019: What v0.1 does not do

**Status: Accepted (2026-09-15).**

**Context.** The requirements grew to cover a general self-hosted stack while the actual target is a two-user homelab that is already running. Several MVP items are either solved elsewhere in that stack or not implementable against the real providers. Shipping them would cost time and would add failure modes for no benefit.

**Decision.** The following leave v0.1.
- **Apprise.** Apprise-as-a-service is a separate Python container, which contradicts NFR-1 and NFR-10 without acknowledging it. Nuno posts to a webhook instead, roughly thirty lines and no dependency. The target stack already runs ntfy.
- **FR-24, provider-wide default quota.** Nextcloud already sets it through `oidc_login_default_quota`, and Immich has no instance-wide default at all. As specified it also had no home in the model: nothing to plan against, nothing to audit, and `AuditEntry` is user-scoped.
- **NFR-13, YAML export and import.** The database lives where ZFS snapshots and off-site backup already reach it. A bespoke format would add a dangerous import path (silently retargeting account links from a stale file) for a guarantee the storage layer already provides. It returns when GitOps mode does, which is its only real consumer.
- **The version matrix apparatus**, per [ADR-0015](#adr-0015-version-policy-simplified).
- **Threshold notifications (FR-61).** Not re-notifying every cycle requires per-threshold state, which is a small subsystem. Failure notification is enough for v0.1.

**Consequences.** ADR-0002's argument still stands: the database is the only home for policy and must be recoverable. The answer is now documented SQLite backup (`sqlite3 .backup`, never a copy of a live WAL database) plus placement on a snapshotted dataset, not a bespoke text format.

## ADR-0020: Budget is an input to percentages and an output to people

**Status: Accepted (2026-09-15).** Closes a hole opened by [ADR-0012](#adr-0012-tier-resolution-happens-per-provider). Needed for M2.

**Context.** ADR-0012 moved resolution from whole-tier to per-provider and, in doing so, demoted `Budget` to "a display concept, not the thing resolution is computed from". That sentence is wrong in one direction and the model followed it: `Tier` lost its budget field entirely. But five MVP requirements still take a budget as an input (FR-10 models it, FR-12 maps a tier to it, FR-51 lets an admin edit it, FR-42 measures usage against it, FR-45 reports it), and a `percent` allocation is meaningless without a base. ADR-0012's own worked example resolves "25 percent of 120 GiB", which is precisely the computation it claimed did not happen.

The confusion is that two different quantities were wearing one name.

**Decision.** Name them separately and keep both.

- `Tier.Budget`, a tagged value, is an **input**: the base that `percent` allocations resolve against. A tier with only `absolute` or `unlimited` allocations may leave it unset, which is why it is tagged rather than an int64.
- **Effective budget** is an **output**: the sum of the ceilings actually resolved for a person across providers, unlimited if any of them is. It is what the UI and `/api/v1/usage` report, and it can legitimately differ from `Tier.Budget` when percentages do not sum to 100.

Per-provider resolution from ADR-0012 is unchanged: the input to the max is the resolved ceiling, and `percent` allocations are resolved against their own tier's budget before the comparison.

**Consequences.**
- `Tier` regains `Budget Quota`. The mixed-mode rule from [ADR-0003](#adr-0003-budget-splitting-across-providers) (absolute allocations subtracted first, percentages applied to the remainder) belongs in `ARCHITECTURE.md` and is implemented there, along with the clamp at zero: a tier whose absolute allocations exceed its budget is a configuration error refused at write time, never a negative ceiling. Nextcloud treats negative values as sentinels, so an unvalidated negative would grant unlimited rather than fail.
- FR-15, the per-user per-provider override, moves to MVP. It is not a nicety: `nuno adopt` (FR-70) imports each provider's current quota per user, and two people with different hand-set quotas cannot be expressed as tier overrides without inventing one tier per person. The model gains `user_provider_overrides(user_id, provider_id, quota)` and it sits above group tiers and below nothing except itself in precedence.
- Precedence, stated once and completely, per provider: user provider override, then user tier override, then the most generous group-derived tier, then the default tier.

## ADR-0021: Corrections from the captured responses

**Status: Accepted (2026-09-15).** Corrects [ADR-0014](#adr-0014-provider-write-paths-as-they-actually-are) where the captures disagree with it. Needed for M1. Point 4 below is superseded by [ADR-0024](#adr-0024-a-person-who-has-never-logged-in-still-gets-their-quota) on the `firstLoginTimestamp` case; the rest stands.

**Context.** ADR-0014 was written from upstream source and was right about behaviour, but wrong about two shapes and silent on two thresholds. The fixtures recorded on 2026-09-15 settle all four. A captured response outranks a reading of the source, and both outrank prose.

**Decisions.**

1. **The OCS error model is v2, not v1.** ADR-0014 said errors arrive with HTTP 200 and the truth is `ocs.meta.statuscode`. That is the `/ocs/v1.php` convention. On `/ocs/v2.php`, which is the path in use, the HTTP status and `ocs.meta.statuscode` agree: the write probe captured HTTP 403 with `ocs.meta.statuscode` 403. The rule is therefore: parse the envelope, read both, treat a disagreement as a `ProviderError`, and never trust one alone. The "997 body with HTTP 200" fixture is dropped as a requirement on this path.

2. **`ocs.data.users` is a map, not an array.** The documented decode path `data.quota.quota` does not exist. The real path is `ocs.data.users.<external_id>.quota.quota`, keyed by the account id. This matters beyond decoding: those keys are the UUIDs that ADR-0013 links on, so the map key is `ExternalID` and reading it is not optional.

3. **`quota.total` is not always `free + used`.** For the unlimited account the capture shows `free: -3, used: 65575770, total: -3`. The sentinel wins over the arithmetic. The rule stays "read `quota.quota`, never `quota.total`", but for the right reason: `total` is either the effective space or a sentinel, and neither is the configured quota.

4. **Unknown usage on Nextcloud has one rule**, replacing two that disagreed: usage is `Unknown` when the quota object is absent or serialized as `[]`, when `total` or `relative` is missing, or when `firstLoginTimestamp` is 0. Nextcloud returns 0 rather than omitting the field, and a user who has never logged in has no meaningful usage regardless of what `used` says. The `used == 0` conjunction is dropped: it made a never-logged-in user with files look known.

5. **The Immich divergence threshold is pinned.** Usage is `Unknown` when `|cached - aggregate| > max(64 MiB, 1% of aggregate)`. The captures show 9.5 MB of divergence on a 9.8 GB account (0.096%) and exact agreement on the other, so an absolute threshold below about 10 MB would have marked a healthy account permanently unwritable, and FR-38a offers no override. The authoritative value for `Account.Used` is `usageByUser[].usage` from `GET /api/server/statistics`; `quotaUsageInBytes` is only the cross-check.

6. **Writing unlimited to Nextcloud is `value=none`.** `parseAndValidateQuota` accepts `"none"`, `"default"`, a byte count and human sizes. Writing `-3` would be taken as a byte count and is the most destructive value available. `NormalizeQuota(Unlimited)` returns `Unlimited`, and `NormalizeQuota(Unknown)` is a programming error, not a value.

7. **`ProviderDefault` is removed from the quota kinds.** No adapter can emit it: Nextcloud resolves `default` to the instance value before the API sees it, and Immich has no instance-wide default. A state no provider can produce and no planner knows what to do with is invented scope in a frozen interface. The kinds are `Unknown`, `Unlimited` and `Bytes`. Nextcloud's string `"none"` decodes to `Unlimited`, the same as `-3`. If a provider ever exposes inheritance honestly, that is an interface change and gets its own ADR.

8. **`Account.Used` is restricted to `Unknown` or `Bytes`.** It reuses the `Quota` type for convenience, which would otherwise make `Used{Unlimited}` representable and meaningless. Adapters must not produce it and the planner must reject it.

9. **The Nextcloud normalization algorithm**, derived from the captured pairs and from reading `Util::humanFileSize`: divide by 1024 and round to **zero** decimals for the first step (bytes to KB), then divide by 1024 and round to **one** decimal for every step after it, until the value is below 1024; the result is a string like `"25 GB"`; reading back parses that string into bytes. The captured table is the test vector: 26844594176 becomes 26843545600, while 53687091200 and 161061273600 pass through unchanged because they are whole units. The transform is bytes to string to bytes, so both halves belong to `NormalizeQuota`, and the round trip is what the property test asserts.

**And one opportunity the captures revealed.** Immich's `oauthId` for one user is byte-identical to that user's Nextcloud account id: both are the OIDC subject issued by the identity provider. That is an exact, immutable, non-null join **between providers**, far stronger than email, which is null on at least one captured account. The linker should use it: accounts sharing an OIDC subject are the same person, with certainty and without heuristics.

It does not replace directory matching, and the distinction matters. The subject is issued by Authelia and is not necessarily LLDAP's `entryuuid`, so it clusters provider accounts together but does not by itself say which directory user they belong to. That last hop still goes through email or an explicit link. Whether the subject and the directory UUID coincide is unverified and worth ten minutes, because if they do, the whole chain becomes UUID-based and email leaves the design.

## ADR-0022: What the usage endpoint means before policy exists

**Status: Accepted (2026-09-15).** Extended by [ADR-0026](#adr-0026-the-usage-contract-needs-a-name-for-unknown), which adds the missing representation for unknown. Needed for M1.

**Context.** `/api/v1/usage` ships in M1 and its shape is explicitly a frozen contract, because a dashboard widget will depend on it. But half its fields describe policy: `budget_bytes`, `managed`, and a `quota_bytes` that is ambiguous between the observed ceiling and the desired one. In M1 there are no tiers, no allocations and no resolver. Freezing a contract whose fields have no defined meaning yet is how a v2 endpoint gets born three weeks later. There is also no way to mint the admin key the endpoint requires: the only issuance path in the spec is a UI that arrives later.

**Decisions.**

- `quota_bytes` is always the **observed** ceiling, in every milestone. The desired value is a property of a plan, not of usage, and mixing them would make the endpoint lie during the window between a plan and its application.
- Before policy exists, `budget_bytes` is the sum of observed ceilings (`null` if any is unlimited) and `managed` is `false` everywhere, because nothing manages anything yet. Both acquire their real meaning in M2 without changing type or shape.
- `status` is `ok` when `observed_at` is within twice the refresh interval, `stale` beyond that, `unavailable` when the last observe failed. The refresh interval defaults to 15 minutes, which makes `stale` mean half an hour without a successful read.
- Each user entry carries a stable `user_uuid` alongside the display name, and the array is sorted by it. A `customapi` widget addresses fields by path, so an unstable order would silently swap two people's numbers on a dashboard.
- The admin key is bootstrapped from `NUNO_ADMIN_KEY` in the environment, hashed on boot, never logged. When the credential UI lands it supersedes the env var for new keys without removing it: an operator must always be able to recover access from the compose file.
- Three credential types exist and are named distinctly everywhere: the **admin key** (machine, read-only, for `/api/v1/usage`), the **member token** (one person, read-only, for `/api/v1/me/usage`), and the **admin password** (a human logging into the UI when no proxy provides auth). They are never used interchangeably.

## ADR-0023: Nuno diagnoses, it does not reconfigure the services it manages

**Status: Accepted (2026-09-15).** Extended by [ADR-0027](#adr-0027-a-competing-writer-nuno-cannot-see), which bounds what FR-75 can detect and turns the printed remedy into an emitted script. Needed for M1.

**Context.** [ADR-0014](#adr-0014-provider-write-paths-as-they-actually-are) established that `oidc_login_default_quota` rewrites a user's quota on every login and must be moved to `files/default_quota` before Nuno can manage Nextcloud. The remedy was written as a manual precondition, which raises a fair question: a quota setting is Nuno's domain, so why is fixing it the operator's job?

**Options.**
1. Nuno applies the fix itself.
2. Nuno detects the condition and refuses to write, documenting the remedy.
3. Nuno detects the condition, refuses to write, and prints the exact commands that fix it.

**Decision.** Option 3, with the boundary stated as a general rule: **Nuno changes per-user quotas and nothing else.** It never edits the configuration of the services it manages.

Option 1 is not available even if it were wanted. `oidc_login_default_quota` is a Nextcloud *system* config, and the OCS API exposes app config but not system config, so it can only be changed through `occ` or by editing `config.php`. Nuno could set `files/default_quota` and not clear the other one, which leaves two defaults with the unremovable one winning: worse than doing nothing.

Reaching `occ` would mean mounting the Docker socket or exec'ing into another container, which turns a service that holds API credentials into root on the host. That is a large permanent increase in blast radius bought for a one-time setup step. Nuno's current worst case is a wrong quota, which is recoverable by definition; this would make its worst case the machine.

There is also a judgement Nuno is not entitled to make: it cannot tell a misconfiguration from a deliberate choice, and silently repairing an operator's configuration is not its call.

**Consequences.**
- `nuno doctor` is an M1 deliverable: it checks every precondition (credentials that can write, competing writers, provider versions, identity reachability, unlinked accounts) and prints, for each failure, what is wrong and the exact command that fixes it. It is what a new operator runs first and what an issue report pastes.
- The same text appears in the startup log and in the UI banner when a provider is degraded. A precondition discovered mid-reconcile is a failure of diagnosis, not of the operator.
- The rule generalizes to providers not yet written. A ZFS adapter sets `userquota` and does not touch dataset properties; an S3 adapter sets bucket quotas and does not rewrite bucket policy.

## ADR-0024: A person who has never logged in still gets their quota

**Status: Accepted (2026-09-15).** Closes a gap between [ADR-0021](#adr-0021-corrections-from-the-captured-responses) and FR-38a. Needed for M1.

**Context.** ADR-0021 rules that Nextcloud usage is `Unknown` when `firstLoginTimestamp` is 0, because a user whose filesystem was never set up reports a fabricated `used: 0`. FR-38a then forbids any write against unknown usage, with no flag to force it. Together they produce a result nobody chose: **a person provisioned through SSO who has not logged in yet never receives their quota**, because Nuno refuses to write until they log in, and the whole point was to have the quota waiting for them.

This is the product's primary flow, not an edge case. A new member is created in the directory, the admin assigns a tier, and the account materializes on first login. If Nuno can only act after that login, the first thing the person gets is whatever default the service applies, which is exactly the problem Nuno exists to solve.

**The confusion is between two different zeroes.** A fabricated `used: 0` from a failed storage lookup means "we do not know". A `used: 0` for someone who has never authenticated means "there is nothing, with certainty": they cannot have uploaded anything, through any client, without authenticating at least once.

**Decision.** Split the rule by what the response actually shows.

- The quota object is absent, serialized as `[]`, or missing `total` or `relative`: usage is `Unknown`. The lookup failed and nothing can be concluded.
- The quota object is complete and `firstLoginTimestamp` is 0: usage is **known and zero**. The account exists and holds nothing.
- Otherwise: usage is the reported `used`.

A never-logged-in account is therefore fully writable. It cannot be a `shrink-below-usage` by construction, since nothing can be below zero, unless the target is zero itself, which the guardrail still catches.

**Consequences.**
- A person's quota is correct before they ever open the service, which is the behaviour the README promises.
- The residual risk is a file placed under a user's home by an administrator out of band, before that user's first login. It is rare, it is visible in the plan, and treating it as a reason to never provision anyone would trade the main flow for a corner.
- `nuno doctor` and the plan both show a never-logged-in account as such, so the zero is never mistaken for a measurement.
- Immich needs no equivalent rule: an account with no assets legitimately reports zero, and there is no filesystem to initialize.

## ADR-0025: Health reads, the write probe is a separate step

**Status: Accepted (2026-09-15).** Raised at the start of M1. Corrects the probe placement in [ADR-0014](#adr-0014-provider-write-paths-as-they-actually-are) and changes the `Provider` interface in `ARCHITECTURE.md` section 2.

**Context.** ADR-0014 puts the write probe inside `Health()`: "setting one user's quota to its current value, so a misconfiguration surfaces at startup rather than in the middle of a reconcile". The reason for the probe is sound and is not in question. Its placement is, for three independent reasons.

`Health()` runs before `link()`. FR-7 says Nuno must not write to an account it cannot link, and the README states it as an invariant: "Nuno never writes to an account it has not linked". At probe time no link exists yet. On a fresh instance the only account certain to be present is the local admin, which in `tests/fixtures/nextcloud-users-details.json` has `email: null` and therefore never matches under the ADR-0013 match keys: the probe would write to precisely the account Nuno has decided not to manage.

The probe also spends the write rate limit, 50 calls per 10 minutes on `editUser`, from the same budget as the applier. `Health()` is what feeds FR-53, the provider status in the UI. If provider status is recomputed per page load, Nuno throttles itself and degrades its own provider. One `CanWrite bool` for a read that costs nothing and a write that costs a rate-limited call is the wrong shape, and `bool` cannot say "not proven".

Third, the rewritten value is a fixed point of normalization only because what Nuno read is already the image of a previous `humanFileSize` round trip. A quota set outside OCS, through `occ` or in the database, never passed `parseAndValidateQuota`, so rewriting it silently changes a person's quota during a startup the roadmap calls read only.

**Options.**
1. Keep the probe in `Health()`, constrain the target and cache the result.
2. Move the probe out of `Health()` into an explicit step that runs after linking.
3. Drop the probe from v0.1.

**Decision.** Option 2.

`Health()` performs reads only: reachability, version, whether the credential authenticates. It has no effect on provider state and may be called as often as the UI needs. It does not read user details, so the home-folder side effect from ADR-0014 stays confined to `observe()`.

The write probe becomes a separate operation, run by `nuno doctor` and once at startup after `observe()` and `link()`. Its target is a linked account, chosen deterministically by external id, whose observed quota satisfies `NormalizeQuota(q) == q`. It writes that value back and reads it again, and the probe passes only if the value is unchanged. With no eligible account the probe does not run and write access stays `Unproven`.

`HealthResult` loses `CanWrite` entirely, because a method with no memory cannot report the outcome of a step that runs later. Write access becomes a tri-state of its own, `Unproven`, `Yes` or `No`, produced by the probe and persisted with the provider instance. `Unproven` is the honest default and the value on a fresh install. The probe needs no new method on the interface: it is `GetAccount`, `SetQuota` with the observed value, `GetAccount` again, sequenced in `internal/reconcile` where I/O orchestration belongs.

**Consequences.**
- The invariant holds without an exception: every write Nuno makes, probe included, addresses a linked account.
- A fresh instance reports write access as `Unproven` rather than as a failure, and `nuno doctor` names `nuno link` as the command that makes the probe possible. A provider that has never proven a write is not the same thing as one that has proven it cannot.
- One probe per startup instead of one per status query, so the write budget stays with the applier.
- An account whose quota is not a fixed point is never a probe target, so the probe cannot alter anyone's quota. That account is worth reporting for a different reason: something wrote it outside OCS, and the first reconcile will show it as a change. `nuno doctor` lists it.
- Immich follows the same shape. Its no-op write must emit `quotaSizeInBytes` explicitly, and an explicit `null` for an unlimited account, never an omitted key, per the hazard in [ADR-0011](#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer).
- FR-76 and `ARCHITECTURE.md` section 2 are updated to match. ADR-0014 stands on everything else: the probe is necessary, it is just not part of a health read.

## ADR-0026: The usage contract needs a name for unknown

**Status: Accepted (2026-09-15).** Raised at the start of M1. Extends [ADR-0022](#adr-0022-what-the-usage-endpoint-means-before-policy-exists) and corrects FR-47.

**Context.** FR-47 fixes `null` as unlimited, and ADR-0022 fixes `status` at `ok`, `stale` and `unavailable`, with `budget_bytes` as the sum of observed ceilings. Between them there is no representation for `Unknown`, which the rest of the design treats as a first-class value ([ADR-0011](#adr-0011-quota-is-a-tagged-value-not-a-nullable-integer), [ADR-0016](#adr-0016-safety-model-unknown-state-re-check-and-honest-partial-application)).

The gap is not hypothetical. Nextcloud answers HTTP 200 with the quota object serialized as `[]`, so `quota.quota` is absent and the ceiling is `Unknown`, while the call succeeded and `observed_at` is fresh. Serialized under the frozen contract that is `quota_bytes: null` with `status: "ok"`, which reads as "unlimited" for an account whose storage could not be read, and it propagates to `budget_bytes: null` for the whole person. The consumer is a Homepage `customapi` widget that addresses fields by path, so it has no way to tell the two apart. The contract is declared frozen in M1, which means this has to be settled before anything consumes it, not after.

**Options.**
1. A fourth `status` value, with the numeric fields null and readable only together with `status`.
2. Treat an incomplete read as a failed observe for that account: `ObserveOK` false, `status: "unavailable"`, serve the last known values.
3. A reserved numeric sentinel distinct from null.

**Decision.** Option 1.

- Per-provider `status` takes a fourth value, `unknown`: the call succeeded, the values did not. It stays distinct from `unavailable`, which means the last observe failed.
- When `status` is `unavailable` or `unknown`, `quota_bytes`, `used_bytes` and `used_percent` are `null`. `null` means unlimited only when `status` is `ok` or `stale`. A consumer must read `status` before reading a number.
- The user entry gains `"complete": bool`. It is false when at least one of that person's providers contributed no ceiling or no usage. `budget_bytes` and the user-level `used_bytes` are the sum of the known values, never null because of an unknown: `null` at that level stays reserved for unlimited.

Option 2 is tempting because degrading an account needs no new field, but it fails on the first observe, when there are no last known values to serve, and it labels a working provider unavailable.

**Consequences.**
- FR-47 gains `unknown` and `complete`. The shape still does not change when policy arrives in M2, which is what ADR-0022 was protecting.
- The documented Homepage snippet in M4 reads `status` and `complete`, not just the numbers. A widget that ignores them shows a wrong value, so the snippet is part of the contract rather than an illustration.
- A never-logged-in account is not `unknown`. Under [ADR-0024](#adr-0024-a-person-who-has-never-logged-in-still-gets-their-quota) its usage is known and zero, and its status is `ok`.
- `unavailable` and `unknown` stay separate because they call for different actions: one is a credential or the network, the other is that account's storage on the provider.
- `managed` is untouched and stays false everywhere until M2.

## ADR-0027: A competing writer Nuno cannot see

**Status: Accepted (2026-09-15).** Raised at the start of M1. Bounds FR-75 and extends the remedy in [ADR-0023](#adr-0023-nuno-diagnoses-it-does-not-reconfigure-the-services-it-manages).

**Context.** FR-75 requires an adapter to detect a known competing writer at startup, naming three on Nextcloud: `oidc_login_default_quota`, `ldapQuotaAttribute` and `ldapQuotaDefault`. ADR-0023 establishes, as a verified fact, that the first is a Nextcloud *system* config and that "the OCS API exposes app config but not system config". So the adapter cannot read the one writer that does the most damage, the one that rewrites a quota on every OIDC login. ADR-0014 half admits it with "documented preconditions and startup checks where detectable", which is not a rule anyone can implement.

It is live, not theoretical: `tests/fixtures/nextcloud-write-probe.json` records `oidc_login_default_quota` set to `"25 GB"` on the target instance. The condition FR-75 exists to catch is present today and is invisible to the check that is supposed to catch it.

**Options.**
1. Require the operator to declare in configuration that the default was moved, and keep the provider degraded while the declaration is absent.
2. Diagnose without a gate: `nuno doctor` emits the commands, including the one that reads the value Nuno cannot read.
3. Persist the operator's pasted `occ` output as adapter state.

**Decision.** Option 2 for M1.

`nuno doctor` composes the remedy with the real values it can read and emits it as a runnable script (`nuno doctor --fix`), which the operator executes. The script performs the check the API does not allow and applies the fix. Nuno does not run `occ`, does not mount the Docker socket and has no access to the Nextcloud container, so ADR-0023's boundary and NFR-7 hold unchanged. Composing the commands is work Nuno can do; executing them is not work Nuno is entitled to do.

`ldapQuotaAttribute` and `ldapQuotaDefault` are app config, so they are reachable through the provisioning API's appconfig endpoint, and for those two FR-75 is implemented as written. That the endpoint exposes them on `stable34` is to be confirmed in the first adapter commit, not assumed.

For `oidc_login_default_quota` there is no detection. In M1 that has no observable consequence, because M1 applies nothing. Whether the apply gate can rest on something other than detection is a question for M3, when the applier exists and the instance has been through a week of use.

**Consequences.**
- FR-75 is reworded to say which of the three writers are detectable and which is not. A MUST nobody can implement is worse than a MUST with a stated perimeter, because it reads as done.
- `nuno doctor` has two modes: report, and `--fix` which emits a script. It never executes one.
- The precondition already in `ROADMAP.md` gains the script, and the deployment docs in M4 carry it.
- The general rule, for providers not yet written: when a service does not expose the state a precondition depends on, Nuno emits the command that exposes it and does not pretend to know. Diagnosis degrades honestly; it does not guess.

## ADR-0028: On Nextcloud the account id is the OIDC subject, when it looks like one

**Status: Accepted (2026-09-15).** Raised while writing the Nextcloud adapter. Completes [ADR-0021](#adr-0021-corrections-from-the-captured-responses) and [ADR-0013](#adr-0013-identity-keys-and-the-link-lifecycle).

**Context.** `Account.Subject` exists because ADR-0021 found an exact cross-provider join: Immich's `oauthId` for one person is byte-identical to that person's Nextcloud account id, both being the subject issued by the identity provider. Immich exposes it as a field. Nextcloud does not expose it at all: the subject *is* the account id, but only for accounts `oidc_login` auto-registered, and for everyone else the id is a local name. The captured response cannot tell the two apart, because `backend` is `Database` for both the OIDC-registered `alice` and the local `admin`.

Leaving `Subject` empty on Nextcloud is not free. The captured `admin` account has `email: null`, so on that account the ADR-0013 match keys have nothing to work with, and email is exactly the key ADR-0021 calls weaker.

Reading the target stack's Authelia configuration adds a fact and takes one away. It defines no `subject` and no sector identifier, so the `sub` is the opaque identifier Authelia generates and persists in its own storage. That means the subject does identify one person across every provider, which is what the join needs, and it does not identify that person in the directory, which is why the last hop from an account to a directory user still goes through email or an explicit link. The stack does not run on the machine this was written on, so this is read from configuration and not yet confirmed against a live `sub`.

**Options.**
1. Populate `Subject` from the account id when it has the shape of a UUID, and leave it empty otherwise.
2. Leave `Subject` empty on Nextcloud and match on email alone.
3. A per-instance setting naming where the subject comes from.

**Decision.** Option 1.

The safety of this does not rest on the shape test being right about what a subject is. The linker never acts on a value that looks like a subject; it acts on two providers reporting the same one. A false positive therefore requires two providers to report an identical UUID string for two different people, which does not happen by accident. A local Nextcloud account whose id happens to look like a UUID gets a `Subject` that joins to nothing, and nothing follows from it.

**Consequences.**
- The shape checked is a canonical RFC 4122 UUID, which is what Authelia issues. An identity provider whose subjects are not UUIDs (a bare number, a ULID) is not recognized, and those accounts fall back to email or a manual link. The heuristic is specific to UUID-issuing providers and says so in the code.
- `backend` is not a usable discriminator and is not consulted: both captured accounts report `Database`.
- The contract test asserts the outcome on both captured accounts: `alice` carries a subject, `admin` does not.
- The open question from ADR-0021 stays open, with a partial answer of no: the subject is Authelia's opaque identifier, not LLDAP's `entryuuid`, so email does not leave the design. Confirming it takes one `ldapsearch` and one query against Authelia's storage, on the host where the stack runs.

## ADR-0029: The per-person page trusts the proxy, not a token

**Status: Accepted (2026-09-15).** Extends [ADR-0017](#adr-0017-usage-api-dashboard-first-per-person-second). Needed for the shared dashboard.

**Context.** ADR-0017 deferred the per-person endpoint and imagined it authenticated by an opaque per-user token, issued from the UI. That was written before the dashboard was real. When the first widget shipped, the requirement came back in a form the token does not fit: a shared Homepage should show each person **their own** quota, and the widget that can do it is an `iframe`, not a `customapi`.

A `customapi` widget is fetched by the Homepage server with one key, so it cannot vary by viewer. That is why the first cards either named everyone or aggregated per service, and neither shows a person their own ceiling. An `iframe` is different: the browser loads it, so it carries the session cookie, and the proxy's forward auth runs again on that request. The identity is then already established by the proxy, and a token would be a second credential to issue, store, revoke and explain.

**Decision.** Add `GET /me`, which reads the `Remote-User` header the proxy sets and renders that person's own per-provider summary. The header is believed **only when the request arrives from `NUNO_TRUSTED_PROXY`**, the network the proxy runs on; with nothing configured, nothing is trusted and the page answers 403. The page is not behind the admin password, because the person reading it is not an admin: the proxy authenticates them.

**Consequences.**
- The per-user token from ADR-0017 is not built. It remains the answer for a stack with no proxy in front, which is a deployment shape Nuno supports but the homelab does not use. If one appears, it is a new ADR.
- Trusting a header is only safe because of the network check. Without `NUNO_TRUSTED_PROXY`, any container on the same network could claim to be somebody else; the setting defaults to empty and the check is the first thing `/me` does.
- The page is a widget body: no navigation, no admin actions, so an iframe cannot become a way into the admin surface.
- The proxy must allow the page to be framed, and it must not leave a client-supplied `Remote-User` in place. Authelia overwrites it via forward auth `copy_headers`, and the homelab's Caddy block removes `X-Frame-Options` and sets `frame-ancestors` for the dashboard origin.
- `?detail=service` and `?detail=provider` remain for a `customapi` widget, which is still the right tool when the audience is everyone or only admins.
