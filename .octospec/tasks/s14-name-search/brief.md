---
type: Task
title: "S14: shared contact and group name search"
description: Implement S14 F1/F2 core rules and a typed query contract for client entry adapters.
tags: [search, space, auth, wire-contract, testing]
timestamp: 2026-11-04
slug: s14-name-search
upstream: S14 v1.3
source: self
---

# Goal

Unify eligible contact/group candidates, literal/pinyin/initial/mixed name matching,
stable ordering, separate original/remark highlights, and complete pagination.
The user authorized A implementation after reviewing the design and task plan.

# Load-bearing list

- Authenticated identity and active Space membership; App Bot publication/scope.
- Independent global-human-friend versus Space-member relationships.
- Per-user remarks and real-name fallback; confirmed human/AI classification.
- Unicode-codepoint match provenance and deterministic ordering.
- Result revision, narrowing filters and paging across changes.
- Existing search/global and friend/sync compatibility is retained.

# Behavior and files

`pkg/namesearch` owns pure S14 matching and ranking. `modules/search/name_*`
owns candidate reads in repeatable-read transactions and revision-checked paging.
`modules/search/api_names.go` exposes contacts/groups without coupling to message
search enablement. Web Service/bridge and C's page adapters live in octo-web.
No new user-visible entry is added.

# Scope

Deliver A's shared core, directory/group providers, bounded group-member scopes,
narrowing-set support, typed endpoints, regression fixtures and module tests.
Selection narrowing does not replace or grant operation authorization. Existing
operation candidate sources must supply the entire current allowed subset, not
one page. Unconfigured operation scopes fail closed.

# Out of scope

- B's message/body/document/summary-task search.
- C's UI changes and four-client acceptance.
- New action privileges or automatic Bot friendships.
- Summary task/body search and report permissions (B).

2026-11-05 continuation: thread forward/filter and summary member/source providers
are now implemented in this server, after tracing the separate summary backend.
Operation rosters use R6; allowed_ids only narrows server-authorized candidates.

# Acceptance

- PRD nine scores, seven remark examples, ten human/AI examples and all N1–N9 edges.
- No cross-Space ordinary/Space App Bot escape through friend/member sources.
- Global human friends and platform App Bots survive Space/no-Space changes.
- MySQL integration with 257 members, 105 platform apps, pagination, permission
  revocation and per-user remarks; isolated s14_* schema only.
- Type/allowed-set filtering before paging; changed result rejects old cursor.
- Focused unit, HTTP, SQL, i18n and independent diff review evidence recorded.
