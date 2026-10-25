# Internal Project member list

## Goal

Provide `GET /v1/internal/project-memberships?space_id=...&project_id=...&page=1&limit=50`
for a trusted peer such as Fleet. Its response must match the ordinary
`GET /v1/projects/{id}/members`: a bare `MemberResp[]` and `X-Total-Count`,
including names, robot ownership, collaboration roles and membership timestamps.
Fleet can authorize the bf caller and its verified owner fallback before using
this service credential, without obtaining a human session token or an OBO grant.

Keep the existing `internal_membership` module name. It groups Project-related
service-to-service internal APIs: epochs, verify, projects and member lists.
The member-list HTTP handler directly invokes the Project read service, which
owns member business logic. The existing store interface and implementations
remain unchanged for the other internal APIs; no member-list adapter is added.

## Load-bearing list

- `modules/internal_membership`: reuse the existing deployment-issued
  `X-Internal-Token`, enablement gate, shared strict-IP bucket and read deadline.
  The peer must authorize its end user; the server independently binds the active
  Space and Project. Ordinary user/Bot tokens are not internal credentials.
- `modules/project`: own the SQL, member models and response projection in
  their original `db_read.go`, `model.go`, `model_read.go` and `api_read.go` files.
  `ListProjectMembersInSpace` is the narrow cross-module service: active scope
  checks, counted reads, identity/robot/role enrichment and projection use one
  bounded read-only REPEATABLE READ transaction. The service directly calls
  `queryProjectReadTx`, then validates Project status, identity and Space
  ownership; the existing DAO operations stay unchanged. It does not initialize
  the Project API or workers. The ordinary route retains its human/Space/Project
  checks and transaction boundary, calling the same private roster read helper.
- `modules/internal_membership`: call the Project service and forward its member
  response array and count. Roster SQL/DTOs are not moved into `pkg/project`,
  which retains its existing scalar fact/predicate responsibilities.
- Internal HTTP regression tests and their self-contained fixtures belong to
  `modules/internal_membership`, in the external `internal_membership_test`
  package. Pure Project DB/snapshot/paging tests remain in
  `modules/project/service_member_list_test.go`. This keeps the production
  dependency `internal_membership -> modules/project` free of test import cycles.
  Separate legacy-table queries preserve mixed MySQL collation compatibility.

## Contract

- Required, non-empty, unpadded, single `space_id` and `project_id` (max 256 bytes).
  No caller UID is accepted or inferred from a service credential.
- `page`/`limit` have the public parser's lenient defaults (1/50) and caps
  (100000/200). Malformed query encoding, repeated or unknown keys return 400.
- Success: 200 JSON array, `X-Total-Count`, `Cache-Control: no-store`.
  Ordering and filtering match the ordinary roster: active Project seats,
  `removing=0`, matching Project/seat Space, role descending then creation and UID.
  Empty pages and empty collaboration-role collections encode as `[]`.
- Fields: `uid`, `name`, `role`, `invite_uid`, `robot`, `owner_uid`,
  `collaboration_roles`, `created_at`, `joined_at`. Role objects contain
  `role_id`, optional `builtin_key`, `name`, `source`.
- Missing/inactive/cross-Space Project or inactive Space: 404.
  Invalid internal credential: 401; shared quota exhaustion: 429;
  database/cancellation errors: 500, never a partial or empty successful result.
- This roster is relationship/display data, not a replacement for resource ACL
  decisions or the existing `_verify` authorization contract.

## Out of scope

- Fleet/CLI consumer changes and owner-fallback implementation.
- Changes to public/Bot authorization, OBO, existing internal verification or
  Project activation policy, database schema, and deployment configuration.
- Direct writes to `main`, merging the PR, or deployment.

## Acceptance

- [x] New-route HTTP parity tests failed with 404 before implementation.
- [x] Public and production internal routes return identical JSON and counts
  for default, limited, later and empty pages, with humans, bots, builtin/custom
  roles, legacy NULL joined_at and closed/mis-scoped seat filtering.
- [x] Internal credentials, selectors, error redaction, paging, request
  cancellation, cross-Space/inactive state and production middleware are tested.
- [x] Full affected package tests and race checks pass on isolated MySQL/Redis.
- [x] Production build, vet, formatting, diff and i18n gates pass.
- Delivery requires a feature-branch PR targeting GitHub `main` and successful checks.
