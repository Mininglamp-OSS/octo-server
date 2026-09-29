# Profile contact information

## Goal and behavior list

Support Mininglamp-OSS/octo-web#1764 through the existing profile cards and
`GET /v1/users/:uid`. Publish `profile_contact_info_on`, default false. When
enabled, an authorized full profile of an active human includes phone and email,
including explicit empty strings for unset values. Existing self, friend,
active shared Space and shared-group visibility rules remain authoritative.
Strangers retain the minimal profile; bots, system identities, disabled and
destroyed accounts do not gain contact disclosure. No new user entry point.

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
- Test authorized self, friend, shared Space/group, unrelated viewers, empty
  values, bots, destroyed/disabled accounts, and shared-service/batch redaction.
- Run focused common/user tests, existing profile authorization tests and
  relevant channel regression tests; run gofmt and git diff --check.
- Manual integration: enable the setting, refresh appconfig and open the existing
  self/peer profile cards in octo-web#1764; reveal/copy phone and copy email.
