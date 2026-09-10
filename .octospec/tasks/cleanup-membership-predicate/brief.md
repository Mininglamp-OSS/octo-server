---
type: Task
title: "Task: cleanup-membership-predicate"
description: Give the removal-cleanup gates their own membership predicate so a disbanded Space stops voiding cleanup and a banned Space stops triggering it.
tags: [space, isolation, acl, cleanup, member-removal]
timestamp: 2026-08-22T00:00:00Z
# --- octospec extension fields ---
slug: cleanup-membership-predicate
upstream: Mininglamp-OSS/octo-server#797
source: self
---

# Task: cleanup-membership-predicate

## Goal

The member-removal cleanup pipeline has **two** rejoin guards that ask the same question with
**different predicates**, and both answers are wrong at one edge:

| gate | current predicate | wrong when |
|---|---|---|
| worker (`modules/space/member_removal.go:333`, `queryMember`) | `sm.status=1` **only** | Space is **disbanded** — an orphan `sm.status=1` row voids the job, cleanup never runs |
| cascade step (`modules/group/space_member_removal.go:46`, `CheckMembership`) | `sm.status=1 AND s.status=1` | Space is **banned** — a legitimate member is torn out of every group |

Introduce one predicate that means *"this member still holds their seat, so cleanup must
skip"* — `sm.status=1 AND s.status <> 0` — and use it at **both** gates.

## Background

Both entries come from #797. The issue originally proposed relaxing the shared
`CheckMembership` helper to `space.status <> 0`; that was **corrected** — `pkg/space/membership.go:9`
has 37 non-test call sites including `pkg/space/middleware.go:102` (`SpaceMiddleware`), so
relaxing it would admit banned Spaces across the entire authenticated API. The issue's other
entry proposed adopting `CheckMembership` in the worker, which contradicts the first. Both
want the same new, separately-named predicate.

Two questions are being conflated:

- **may this principal access the Space?** → `sm.status=1 AND s.status=1` — `CheckMembership`, **unchanged**
- **is this member still in place, i.e. should cleanup skip?** → `sm.status=1 AND s.status <> 0` — new

Space status (`modules/space/model.go:13-17`): `0` disbanded, `1` normal, `2` banned.

The worker's wrong answer is reachable in production: it composes with the join-vs-disband
orphan (#797, separate item) which produces exactly `sm.status=1` inside a `s.status=0` Space.
A member in that state keeps their `group_member` rows and IM group subscriptions in a
disbanded Space, with no job left to fix it.

## Load-bearing list

- `space` / `isolation` / `acl` — this is the predicate that decides whether a removed member
  gets torn out of the Space's groups. Both directions are load-bearing: a false "still a
  member" leaves a removed person inside; a false "not a member" tears a legitimate member out.
- `CheckMembership` semantics must be **byte-for-byte unchanged** — 37 non-test call sites,
  one of which is `SpaceMiddleware`, the primary authorization gate.
- The cascade step's existing disband behaviour (`TestGroupCascadeStillRunsAfterSpaceDisbanded`)
  and rejoin behaviour (`TestGroupCascadeSkipsRejoinedMember`) must both stay green — the new
  predicate preserves them by construction, and the tests are the proof.
- `test` — TDD: every behaviour change lands as a failing test first.

## Out of scope

- The **join-vs-disband orphan root cause** (locking the `space` row in the three
  membership-creating transactions). This task makes the cleanup pipeline handle the orphan
  state correctly; it does not stop the orphan from being created. Separate #797 item.
- The **membership epoch** on `space_member` that would close the rejoin window to zero.
- Every other #797 batch: queue lifecycle/observability, the Redis `DEL` error, the
  `(group_no, created_at)` index and lock ordering, the durable IM-pending outbox.
- Historical backlog remediation (blocked on this task, but not part of it).
- Changing `CheckMembership`, `HaveCommonSpace`, or `CheckBothMembers`.

## Acceptance

New tests, each failing before the change and passing after:

1. **Worker gate, disbanded Space with an orphan member row** → cleanup **runs**
   (today: silently finished as `skipped_rejoined`). The regression test for the live bug.
2. **Worker gate, member rejoined an active Space** → cleanup **skips** (unchanged).
3. **Cascade step, member active in a banned Space** → cleanup **skips**
   (today: tears them out of every group).
4. **Cascade step, disbanded Space** → cleanup **runs** (unchanged).
5. Predicate unit table over `{disbanded, normal, banned} × {active member, removed member}`,
   plus nil/empty-arg guards matching the existing `pkg/space` test style.

Must stay green:

- `go test ./pkg/space/... ./modules/space/... ./modules/group/...`
- `TestGroupCascadeStillRunsAfterSpaceDisbanded`, `TestGroupCascadeSkipsRejoinedMember`
- A source assertion that `CheckMembership`'s SQL is unchanged.

Gates: `golangci-lint run ./...`, `make i18n-extract-check`, `make i18n-lint`
(no new error responses expected — both gates return errors internally, they do not respond).
