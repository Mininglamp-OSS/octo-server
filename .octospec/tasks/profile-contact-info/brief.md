# Profile contact information

## Goal and behavior list

Implement Mininglamp-OSS/octo-web#1740's backend contract and support
Mininglamp-OSS/octo-web#1764 through the existing profile cards and
`GET /v1/users/:uid`. Publish `profile_contact_info_on`, default false. When
enabled, an authorized full profile of an active human includes phone and email,
including explicit empty strings for unset values. Existing self, friend,
and active shared Space grants authorize contacts. Shared-group visibility
alone only authorizes the basic profile, not phone/email. Space grants require
an active parent Space and active membership for both users; an independent
friendship remains sufficient even if a shared Space is inactive.
Strangers retain the minimal profile; bots, system identities, disabled and
destroyed accounts do not gain contact disclosure. System identities include
the system-Bot UID whitelist, both `system` and `customerService` categories,
and every role accepted by `auth.IsManagerConsoleRole` (currently `admin`,
`superAdmin`, `dashboardReader`, `marketAdmin`). These accounts retain their
pre-existing self-only contact fields but never gain peer disclosure through
this feature. A caller blocked by the profile owner does not receive contact
details even when another relationship still makes the full profile visible.
User API Keys retain identity lookup for their bound Space but never receive
peer contact details; their owner's historical self fields remain compatible.
No new user entry point.
If the extra contact lookup fails, return the authorized basic profile without
peer contact keys. Empty phone/email values remain explicit empty strings;
zone/country-code metadata is optional and omitted when unset. The frontend
must distinguish missing keys from empty values and show the owner's phone
immediately with the country code, while peer numbers require reveal.

## File map

- `modules/common/system_settings.go` and `system_setting_schema.go`: the
  hot-reloaded `profile.contact_info_on` setting and manager configuration entry.
- `modules/common/api.go`: publish the flag on full and version-shortcut responses.
- `modules/user/api.go` and `profile_contact_info.go`: apply contact projection
  only after the existing profile authorization, using the authoritative user row.
- Common/user tests: configuration, wire contract and access regression coverage.
- `docs/profile-contact-info.md`: enablement, response and visibility contract.

## PR scope and load-bearing behavior

The configuration read path and profile response are in scope. Contact fields
must not enter shared service DTOs, batch responses or IM cache projections.
The existing shared user response remains unchanged. No migrations, contact
editing, frontend work, authentication changes or new routes are required.

## Verification and acceptance

- Test default/invalid/off/on settings and hot reload without a version bump.
- Test both appconfig response branches and manager setting validation.
- Test authorized self, friend and shared Space, group-only redaction, unrelated viewers, empty
  values, blocked callers, User API Keys, bots, destroyed/disabled accounts, and
  shared-service/batch redaction.
- Test restricted categories before the lookup and on the authoritative row,
  and manager roles on the authoritative row. HTTP tests must cover restricted
  accounts with actual friend/Space/group relations, plus self compatibility.
- Test shared Space ban/dissolution/reactivation, removal/rejoining of either
  member, disabled groups, friendship removal/restoration, and block/unblock.
- Inject a contact-only query failure over HTTP and assert that the basic
  profile remains available without contact keys or a false empty state.
- Run focused common/user tests, existing profile authorization tests and
  relevant channel regression tests; run gofmt and git diff --check.
- Manual integration: enable the setting, refresh appconfig and open the existing
  self/peer profile cards in octo-web#1764; reveal/copy phone and copy email.
