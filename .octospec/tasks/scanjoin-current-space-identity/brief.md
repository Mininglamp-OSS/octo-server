---
type: Task
title: "Task: scanjoin-current-space-identity"
description: Preserve the scanner's verified current Space identity when joining a group.
tags: [group, space, isolation, wire-contract, testing]
timestamp: 2026-11-09T00:00:00Z
slug: scanjoin-current-space-identity
upstream: "Mininglamp-OSS/octo-server#952"
source: self
---

# Task: scanjoin-current-space-identity

## Goal

When a user belongs to both Space A and Space B and scans a Space B group's
QR code while using Space A, allow the user to join with their Space A
identity if the group allows external members. Persist `is_external = 1`
and `source_space_id = A` so existing external-group projections can show
the group in Space A.

## Background

Previously, `groupScanJoin` chose internal membership as soon as the user
belonged to the group's Space. That bypassed the existing current-Space
selection for dual-Space members, attributed the group only to Space B,
and caused the H5 success flow to prompt a Space switch.

This is a clarification of the existing
[external-group design](../../../docs/external-group-design.md). The
`X-Space-ID` request header is a requested identity, never proof of Space
membership.

## Load-bearing list

- `groupScanJoin` must validate the authenticated scanner's group-Space
  membership and any distinct `X-Space-ID` before selecting an identity.
- The resolved `IsExternal` and `SourceSpaceID` must flow into the existing
  `MemberAdmission` transaction path.
- `allow_external = 0` must continue to admit group-Space members internally
  and reject users outside the group Space.
- Invalid or unavailable header membership must be ignored and logged;
  external users retain the default-Space fallback.
- The response adds `my_source_space_id`; both H5 join pages use it to
  suppress a switch notice only when it equals the current local Space.

## Out of scope

- Database schema changes, membership migrations, and retroactive conversion
  of existing group memberships.
- Changes to invitation/add-member paths, general Space switching, or
  authorization outside scan-join.
- Sprint, workflow, or branch-protection policy changes.

## Acceptance

1. A user belonging to A and B, scanning from A into an external-enabled
   B group, resolves to `IsExternal = 1, SourceSpaceID = A`.
2. Scanning from B, or scanning without a usable distinct header as a B
   member, preserves internal membership and an empty source Space.
3. If external members are forbidden, B members remain internal and
   non-B members are rejected.
4. A non-B member with a valid A header joins externally from A; a missing,
   forged, or failed header membership check uses the default Space.
5. A group-Space membership query error fails the request. A group without
   a Space does not query Space membership.
6. The handler passes the trimmed header, scanner membership checker, and
   default-Space provider to the resolver and uses its admission fields.
7. Both H5 entry points retain the existing success notice unless the
   response's nonempty `my_source_space_id` equals `currentSpaceId`.

Focused automated verification for acceptance items 1-6:

```sh
go test ./modules/group -run 'TestResolveScanJoinIdentity|TestGroupScanJoin_SourceSpaceID|TestGroupScanJoin_XSpaceID' -count=1
```

The package's `TestMain` needs the configured test database even for a
focused run. To verify only the resolver and existing source guards without
that integration harness, run from `modules/group`:

```sh
go test scanjoin_identity.go scanjoin_identity_test.go api_source_space_header_test.go api_xspaceid_membership_test.go -count=1
```

The isolated command does not validate the full package or live persistence.

Manual end-to-end verification remains required for the actual group
projection and H5 flow: use an A/B member in A, scan a B group, confirm
joining, verify the group appears in A without a switch notice, then repeat
from B and with external members forbidden. Unit tests and static review
do not establish real-client acceptance or full CI success.
