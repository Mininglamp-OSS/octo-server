# Profile phone and email

This supports the existing profile cards shipped in
[octo-web#1764](https://github.com/Mininglamp-OSS/octo-web/pull/1764).

## Enablement

The feature defaults to **off**. A super administrator can update the existing
system-settings API, `POST /v1/manager/common/system_setting`:

```json
{
  "items": [
    { "category": "profile", "key": "contact_info_on", "value": "1" }
  ]
}
```

Use `"0"` to disable it. The schema is also available from
`GET /v1/manager/common/system_setting`. Changes take effect immediately on the
server handling the update; other instances reload within 60 seconds. No SQL
migration or restart is required.

`GET /v1/common/appconfig` always returns the boolean
`profile_contact_info_on`, including responses with a current `version` query.
After enabling, refresh the client configuration and reopen the profile card.

## Profile contract

When enabled, `GET /v1/users/:uid` includes the following fields for an active
human whose full profile is visible to the caller:

```json
{
  "phone": "13800001234",
  "email": "alice@example.com",
  "zone": "0086",
  "phone_country_code": "86"
}
```

Phone and email come from the user record. An unset value is returned as `""`,
so clients can display their localized empty state. The optional country code
is the stored zone with its `+` or international `00` prefix removed. Phone
reveal/copy and email copy remain client interactions; clicking reveal does
not perform an additional authorization request.

The existing full-profile authorization remains authoritative: self, a friend,
active shared Space membership, or an active shared group. A caller-provided
`group_no` does not grant visibility. Unrelated users retain the minimal profile,
which has no contact fields. Bots, system identities, disabled users and
completed account deletions do not gain contact disclosure.

For this feature, system identities explicitly include:

- UIDs in the shared system-Bot whitelist.
- Accounts with category `system` or `customerService`, even when `robot=0`
  and their UID is not whitelisted.
- Accounts with any manager-console role accepted by
  `auth.IsManagerConsoleRole`: currently `admin`, `superAdmin`, `dashboardReader`
  and `marketAdmin`, including accounts whose category is empty.

These restrictions apply even when the caller is a friend or shares an active
Space or group with the account. Categories are checked on both the profile
and the fresh user row; roles are checked on the fresh user row because the
shared profile DTO does not carry them. Their historical self-only contact
fields remain available to the account owner. Ordinary human accounts retain
the relationship-based behavior described above; a Space/group membership role
alone is not a manager-console role and does not exclude an ordinary account.

Disabling the setting stops additional contact disclosure on subsequent profile
requests, even if a client still has an enabled appconfig cached. Existing
self-only contact fields remain backward compatible. Login/current-user,
shared user services, IM channel data and batch identity responses are unchanged.

The phone currently comes from `user.phone`, matching the existing self-profile
read during the phone-encryption dual-write phase. When reads move to decrypted
`phone_encrypted` values, include `profileWithContactInfo` in that migration.

## Verification

With the repository's MySQL/Redis test services available:

```sh
go test ./modules/common -run '^TestProfileContactInfo' -count=1
go test ./modules/user -run 'TestProfileContactInfo|TestUserGet_|TestNewMinimalUserDetailResp|TestBatchUsersHuman' -count=1
```

Enable the setting, open a self profile and an authorized peer profile, then
reveal/copy a phone and copy an email. Check an empty contact, an unrelated user,
and a disabled setting after refreshing the client configuration.
