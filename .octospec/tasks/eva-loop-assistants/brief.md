# EVA Loop assistants — identity facts for local assistants

## Goal

Expose credential-bound owner/Project facts for Fleet and separately surface
self-reported Bot hosting telemetry. Hosting is not an authorization signal,
and assistants do not join Projects.

Related issue: Mininglamp-OSS/octo-server#857.

## Load-bearing list

- Session robot/owned_bots adds agent_hosting and agent_reported_hosting_at
  directly to its existing four fields; no include=agent_metadata switch and
  no agent_platform dependency.
- verify-bot opt-in owner_context, bound to the actual Bot credential.
- Separate Bot/owner identity and current Space/Project facts; fail closed on lookup errors.

## Out of scope

- Changing generic bot-task transport into a Loop-specific service.
- Project membership/role/cascade/governance changes and migrations.
- Fleet Agent/Runtime/installation creation for assistants.
- Shared-service deployments and changes to user credentials.

## Acceptance

- Existing owned Bot display fields and Session authentication stay available;
  the default list includes hosting and excludes disabled Bot accounts.
- Default verify-bot keeps its five fields. Only include=owner_context enables
  the credential-bound owner/Project facts; do not split this callback into a new API.
- Explicit owner queries bind identity, validate accounts/Space and expose only
  requested Project facts; absent/failed context grants nothing.
- Queries do not read agent_platform; existing IM storage/reporting is untouched.
  Hosting remains self-reported observability metadata and must not feed authz.
- Focused regression tests and broader auth/project/group checks are recorded in
  the workspace implementation report.

## Files and verification

Project policy remains unchanged; identity context stays in modules/user and robot.
Reuse existing DB sessions, membership queries, errors and test helpers.

### API contract

- `GET /v1/robot/owned_bots?space_id=...` uses the authenticated owner and the
  requested Space. Every item contains `uid`, `name`, `description`,
  `bot_commands`, `agent_hosting`, and `agent_reported_hosting_at`; no metadata
  query switch is required.
- `POST /v1/auth/verify-bot?include=owner_context` accepts `bot_token`,
  `space_id`, and a bounded list of `project_ids`. It derives the owner from
  the validated credential and rejects a caller-supplied `owner_uid`.
- The extension returns `bot_context` (identity, active state, self-reported
  hosting plus report time, and Space membership) separately from
  `owner_context` (identity, active state, Space membership and answers for the
  requested Projects). Owner membership must never be represented as the Bot's
  own Project membership.
- A failed context lookup sets `context_error` and omits both contexts;
  disabled, cooling-off, or destroyed identities and missing Space membership
  disclose no Project roles.
- Without `include=owner_context`, the original verifier's five fields and
  legacy `space_id` remain unchanged. Hosting is telemetry, not authority;
  consumers must not use it to classify authorization eligibility.

Tests cover default compatibility, account/Space isolation, spoofed owner
rejection, Project bounds, lookup failures, and independence from platform data.
The account and membership predicates execute against MySQL in the E2E lane;
they are not covered only through pre-computed SQL-mock booleans.
