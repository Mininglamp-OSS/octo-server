---
type: Journal
title: "Learning: the project group list, and a predicate that is only interesting in the one case nobody wrote down"
description: "A membership-scoped read whose access control IS its WHERE clause; the discovery that the repo's two group-membership predicates differ in exactly one reachable state — the blacklist — which no comment on either side mentioned; a Q2 I resolved from a config flag that turned out to gate nothing, refuted by actually running the suite; and two documentation defects that only a client team would ever have hit."
tags: ["octospec-learning", "space", "isolation", "acl", "wire-contract", "testing", "mysql", "documentation"]
timestamp: 2026-09-08T10:00:00Z
source: self
---

# Learning: project-p2-product-surfaces (PR-1)

## What changed

`GET /v1/projects/:project_id/groups` — the caller's own live groups within one
project. Before it, nothing in the tree could answer "which groups does this
project have": `modules/project` registered nine routes and none returned a
group, `GET /v1/group/my` filters on `space_id` alone, and the query that answers
it already existed unexported, reachable only from the member-removal cascade
worker.

The task brief (`project-p2-product-surfaces`) covers four surfaces that P0, P1
and P2 all deferred to "a sibling brief" nobody had written. This is PR-1 of it.

## What we learned

### A read whose access control is its WHERE clause needs no gate — and adding one makes it worse

The list is scoped to the caller's own group membership. That is not a filter
applied after an authorization decision; it **is** the decision, and the
consequence is counter-intuitive enough to be worth stating: this endpoint
deliberately has **no** role check beyond `projectMiddleware`.

`listMembersHandler` next to it needs `canViewMembers`, because the roster is a
fact *about other people*: a `space_listed` project shows its metadata to any
Space member, but who is in it is not part of that. This endpoint returns only
rows the caller is already a member of, so there is nothing to withhold. A Space
admin who never joined gets `[]`.

Adding a 403 there would be worse than redundant. It would answer a refusal to a
caller whose correct answer is an empty list, and in doing so make "you are not a
member of this project" **observable** on a route that currently discloses
nothing — reintroducing, on a new route, exactly the oracle
`projectMiddleware`'s three folded refusals exist to close.

### The repo has two group-membership predicates, and they differ in exactly one reachable state

`is_deleted = 0` versus `is_deleted = 0 AND status = GroupMemberStatusNormal`.
Both are in wide use. Neither side's comments said where they disagree, and the
abstract framing ("a read must not over-select") hides it.

Removal sets `is_deleted = 1`, so the two agree there. The **only** reachable
state where they disagree is the group blacklist, which sets `status = 2` and
deliberately leaves `is_deleted = 0`. So the choice of predicate is, in practice,
exactly one question: *does a group that blacklisted you still appear in your
list?*

Written that way it answers itself — `ExistMemberActive` is the hardening line
(#343/#345) in front of group and thread reads for precisely that uid, so listing
the group advertises a room the caller cannot open — but nothing on the page
posed the question until a review did. Two older surfaces (`GET /v1/group/my`,
the group detail) still show it, and `member_count` differs by one for the same
reason. Both divergences are now argued in the source and pinned by a test that
asserts **both halves**, so the day the older surfaces are hardened the comment
cannot quietly become wrong.

**Generalisable:** when two predicates for one concept coexist, the useful
question is not which is stricter but *enumerate the states where they disagree*.
There is usually exactly one, and it is usually a product decision wearing a SQL
costume.

### A config flag that gates nothing, believed for three commits

Resolving an open question about a supposed `group` stub table, I read
`testutil.NewTestServer` setting `cfg.DB.Migration = false` and concluded "tests
never run migrations". Wrong: `module.Setup` calls `executeSQL` unconditionally
(octo-lib `module/module.go:29`). The flag gates nothing on this path.

It survived a self-review and two commits because it was *load-bearing for
nothing yet* — the claim only had to be right the day someone acted on it.
Running the suite refuted it within a minute, and did so in the most direct way
available: `./modules/group/` against a database `./modules/project/` had just
migrated dies at startup with `unknown migration in database`, because
`modules/project`'s test binary registers all 40 modules (its external test
package blank-imports `octo-server/internal`) and `modules/group`'s registers
fewer. **Each package needs its own fresh `test` database** — worth knowing
before you burn twenty minutes on a "failure" your change did not cause.

The conclusion held and got better evidence: that divergence produces a
**missing** table, not a differently-shaped one, so a binary without
`modules/group` fails loudly with 1146 rather than quietly loading a NULL into a
Go bool. No stub exists in this repo; if one exists at all it lives in
`octo-deployment`'s `init-db.sql`.

### Two documentation defects that only a client team would ever have hit

Both survived the implementation and were caught in review. Both are the kind
where the code is right and the prose sends someone else somewhere wrong.

- **`is_named` documented with the meaning the column used to have.**
  `20260629000002_refresh_avatar_comments.sql` retired 「用户显式起名」 for 「改版前老群」,
  and `modules/group` hardcodes `0` on every create — so on this endpoint the
  value is *always* 0. A client implementing to the old text branches on a
  condition that is never true and expects an avatar fallback that never fires.
  A field whose doc describes a superseded meaning is worse than an undocumented
  field: it is confidently wrong.
- **An ordering comment that claimed "always".** `ORDER BY g.id ASC` usually puts
  the all-member group first because #855 provisions it with the project — but
  `ensureAllMemberGroup` rebuilds it on a later write path when the first
  provisioning failed, and the rebuild takes a fresh, higher id. The ordering
  stays (it is the only *total* order available, and a non-total ORDER BY under
  OFFSET pagination drops and duplicates rows between pages); the claim about
  position became "a convenience, never the contract".

### A pagination bounds test is not a pagination test

The first cut had `?page=9223372036854775807` and an empty far page, and no case
that read page 2. A swapped `LIMIT ? OFFSET ?` pair — adjacent ints, no compiler
check — reads **correctly on page 1** and only breaks from page 2 on, so every
existing assertion would have passed. The overflow regression and the
correctness property are different tests; having the first does not imply the
second.

### The house rule beat the brief's own acceptance list

The brief asked for `TestListProjectGroupsIsUIDRateLimited`. `TestAuthChainOrder`
in the same module argues *against* exactly that: `SharedUIDRateLimiter` fails
open without a uid, so a route mounted in the wrong order looks rate-limited and
is not, and a hammer test passes either way. The chain is checked where it is
declared; this PR adds only what that guard cannot see — that the new route went
onto the authenticated group rather than a new bare one. **An acceptance item
written before reading the module's own guards is a hypothesis, not a
requirement.**

## Gotchas worth remembering

- `modules/project` may read the `group` **table** but must never import
  `modules/group` (`pkg/project/import_guard_test.go` pins it at zero;
  `modules/group` imports project, so there is only one legal direction).
  Reverse-registration is the right shape for **writes** and the wrong shape for
  a synchronous **read**: an unregistered read provider answers an empty list,
  indistinguishable from "no rows".
- A statement joining `group` ⋈ `group_member` needs **no** COLLATE (both legacy);
  coercing either side gives up the index on `group_member.group_no` for a
  mismatch that does not exist. Say so in the source — "has no COLLATE" and
  "forgot its COLLATE" look identical in a diff — and register the statement in
  `TestP2StatementsSurviveCollationDrift` so the claim is not free.
- Running the integration suite locally needs MySQL 8, Redis and WuKongIM
  (`WK_TOKENAUTHON=false`, health on `:5001`), plus a **fresh `test` database per
  package**.
