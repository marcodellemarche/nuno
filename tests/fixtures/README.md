# Recorded responses

Captured on 2026-09-15 from a live Nextcloud 34.0.4 and Immich v3.2.0, and used as golden files for the adapter contract tests. Nothing here covers LLDAP: the identity source was not captured, and its behaviour in `ARCHITECTURE.md` rests on reading lldap v0.6.3, not on a recorded response.

Identifiers are anonymized: real emails, display names and UUIDs were replaced with stable placeholders, in every field that carries them (`name`, `userName`, `displayname`, `email`, account ids and OIDC subjects). The substitution preserves the properties the tests depend on, in particular that an Immich account's `oauthId` is byte-identical to the same person's Nextcloud account id, which is the cross-provider join described in [ADR-0021](../../docs/decisions.md#adr-0021-corrections-from-the-captured-responses). Byte counts, field presence, types and sentinel values are untouched.

| File | What it proves |
|---|---|
| `nextcloud-users-details.json` | `ocs.data.users` is a map keyed by account id, not an array. The configured quota is `quota.quota`. An unlimited account reports `-3` in `quota`, `total` and `free`. An account can have a null email. |
| `nextcloud-write-probe.json` | An app password cannot write a quota: HTTP 403 with `ocs.meta.statuscode` 403. On `/ocs/v2.php` the two agree, so the v1 convention of errors arriving with HTTP 200 does not apply here. |
| `nextcloud-roundtrip.json` | Writes are lossy: 26844594176 in, 26843545600 out, because the value is stored through `humanFileSize`. This is the test vector for `NormalizeQuota`. |
| `immich-admin-users.json` | `quotaSizeInBytes` and the cached `quotaUsageInBytes`. Accounts carry `status` and `deletedAt` (soft deletion) and an `oauthId` holding the OIDC subject. |
| `immich-server-statistics.json` | The live per-user usage aggregate, `usageByUser[].usage`, which is the authoritative source for `Account.Used`. Its `userId` joins to `immich-admin-users.json`. |
| `immich-nightly-tasks.json` | Context: `syncQuotaUsage` was enabled when the other two were captured, which is why the cached counter was close to the aggregate. |

Missing, and still to be captured or written by hand: a quota object serialized as `[]`, a user who has never logged in (`firstLoginTimestamp` of 0), a 429 from the write rate limit, any Immich write response, and an LDAP search against LLDAP showing that `memberOf` is absent from the wildcard attribute set.
