---
type: Task
title: "Task: project-provisioning-followups"
description: The decisions and review findings that survived PR #850 but not its squash — one design decision about the fleet container id, and six P2 items that only bite after a provisioning target is enabled.
tags: [project, auth, wire-contract, data-integrity, trust-boundary, testing, commit]
timestamp: 2026-09-08T01:15:00+00:00
# --- octospec extension fields ---
slug: project-provisioning-followups
upstream: Mininglamp-OSS/octo-server#850 (merged 2026-09-08, squash 5dd80d46)
source: user
---

# Task: project-provisioning-followups

## Goal

Keep two categories of knowledge from being lost, and make them actionable.

PR #850 landed as **one squashed commit whose message describes the slice, not
the review**. Eleven commits and seven review rounds collapsed into it. Two
things therefore now exist only in a closed PR thread:

1. a **design decision** about the fleet container id, agreed across two
   workstreams but recorded in neither repository's merged code, and
2. **six review findings** left open on purpose, each rated P2 because none can
   bite before a provisioning target is enabled — and enabling is itself gated
   behind a precondition on another repository.

Both are safe today for the same reason (`OCTO_PROJECT_PROVISION_TARGETS` is
empty by default), and both become live at the same moment (the first target is
turned on). That coincidence is the point: this brief is what someone reads
BEFORE flipping that switch.

Every item below was verified against the merged tree at `origin/main`
(`5dd80d4`) rather than restated from the review.

## Background

### D-1 — the fleet container id is a PROVISIONAL form

Not a defect. A recorded decision that has no home in code yet.

`modules/project/provisioning.go` mints an opaque container id that is
deliberately **not derivable** from `project_id`, and #850 calls that
non-derivability the load-bearing property of the slice. The reasoning is
concrete: `listVisibleInSpace` lets any active Space member read the
`project_id` of every space_listed project in their Space, while the fleet
workspace gate admits on octo Space membership alone and then materialises the
caller with `UpsertMember{Role:"member"}` in a row nothing ever deletes. A
derivable id would complete the chain from "can list projects" to "is
permanently a member of every project's workspace".

The Loop integration design (`.octospec/tasks/loop-project-fleet-integration/`)
requires the opposite: `project_id == workspace_id`, one canonical UUID, no
mapping table.

The agreed resolution is that these are **two phases of one problem, not two
positions**. The opacity mitigates a peer-side authorization defect; the Loop
design fixes it (the fleet side stops reading `workspace_member` for
octo-server-owned projects and calls octo-server's verify instead, requiring
Space member AND Project member). #850 itself says its P-2/P-3 preconditions are
not met and that no target may be enabled until they are.

So, recorded here because it is nowhere else:

> **When the peer-side narrowing lands, the fleet target's container id becomes
> `project_id`, and the non-derivability guard narrows to apply to DRIVE ONLY.**
> Drive's threat model is separate and #850's argument may still hold there; it
> must be re-decided on its own evidence rather than carried along.

The two inbound endpoints that make that narrowing possible are in flight as
PR #852 — without an authoritative way to ask octo-server "is this specific
person still in this specific project", the opacity can never be retired.

### The six open findings

All verified at `origin/main` 5dd80d4.

**A-4 — the secret is not trimmed while the URL is.**
`config_provisioning.go:309` reads the ensure URL through `strings.TrimSpace`;
`:305` reads the secret with a bare `getenv`. A secret mounted from a file
carries a trailing newline, every request then signs with a value the peer
rejects, and the whole ~23.5-minute retry budget burns as indistinguishable
401s before the row reaches `abandoned` — which has no automatic re-drive.
Preferred fix is to **reject** leading/trailing whitespace rather than trim it
silently: trimming hides a broken mount that will surprise someone again.

**A-5 — the empty-fragment case regressed a fix a sibling already documented.**
`internal/projectprovision/client.go:351` checks `parsed.Fragment != ""`, so
`https://host/ensure#` passes. `internal/cardactiondispatch/registry.go:338-341`
carries the comment explaining that `url.Parse` clears `Fragment` for an empty
one and uses `strings.ContainsRune(raw, '#')` instead. The provisioning client
claims alignment with that validator and does not have it.

**A-3 — an empty `container_id` echo is treated as permanent.**
`client.go:475` compares `out.ContainerID != req.ContainerID`, so a peer that
answers `{}` or omits the field lands in `container_id_mismatch`, which
`isPermanentProvisioningOutcome` gives up on at the FIRST attempt. A missing
field is a malformed response, not proof the peer owns a different container;
it belongs in `invalid_response`, which retries. The distinction matters most
during the peer's own rollout, which is exactly when the first target gets
enabled.

**A-1 — the `Enabled()` comment says "completely inert".**
`config_provisioning.go:227-229`. `plan.md` establishes that gating the DISBAND
write on `Enabled()` would be a real container leak, so the disband path is
deliberately unconditional. The comment is what a future reader consults right
before "tidying up" by adding that gate.

**A-6 — four provisioning timers have no jitter.**
`provisioning_worker.go:164, 165, 166, 184` schedule without it, while
`reconcile.go:213-214` jitters both of its timers — including
`p.cfg.MetricsInterval`, the SAME interval `provisioning_worker.go:184` uses
unjittered. The census runs on every deployment regardless of enablement, so
this one is live now rather than after enablement.

**P2-5 — `plan.md` §3.5 points the operator at a path with no caller.**
It names `pkg/db/mysql.go` as the startup migration entry. That file does call
`migrate.Exec` (`:45`), but only from `Migration()`, which has **no non-test
caller in this repository** — and `testutil.NewTestServer` sets
`cfg.DB.Migration = false`. Module SQL is applied by `module.Setup`, i.e.
octo-lib `module/module.go:88`. The rollback instructions are correct; the
pointer sends whoever follows them to dead code.

## Load-bearing list

- **The enablement gate is what makes all six safe.** Any change here must keep
  `OCTO_PROJECT_PROVISION_TARGETS` empty-by-default semantics, and must not be
  read as permission to enable a target — that still waits on the peer-side
  precondition #850 records.
  `touches: auth, wire-contract`
- **`abandoned` has no automatic re-drive.** A-4 and A-3 both end there, which is
  why a misclassification costs an operator action rather than a retry.
  `touches: data-integrity`
- **The outbound client's URL validation is a trust boundary**, and its stated
  contract is parity with `internal/cardactiondispatch`'s validator (A-5).
  `touches: trust-boundary, url-destination`
- **`plan.md` is an operator runbook**, so a wrong pointer in it is a production
  hazard rather than a typo (P2-5).
  `touches: data-integrity`

## Out of scope

- Anything that enables a provisioning target, or that relaxes the default.
- The container-id change itself (D-1) — it is blocked on the peer-side
  narrowing and on PR #852, and doing it early would ship a derivable id into
  exactly the window the opacity exists to cover.
- Re-opening #850's merged design decisions. This brief records and repairs; it
  does not relitigate.
- `modules/bot_task`'s per-source tokens, which sit outside `main.go`'s
  credential-collision registry. Real, separate, and recorded in that
  registry's own comment.

## Acceptance

- [ ] A-4: whitespace around a provisioning secret is REJECTED at config load
      with a message naming the env, and a test covers a trailing newline.
- [ ] A-5: `https://host/ensure#` is refused, by the same mechanism
      `internal/cardactiondispatch` uses; a test pins the empty-fragment case.
- [ ] A-3: an empty or absent `container_id` in the echo classifies as
      `invalid_response` (retryable); a genuinely different id stays
      `container_id_mismatch` (permanent). Both pinned.
- [ ] A-1: the `Enabled()` comment states that the disband write is
      unconditional and why, so "add a gate here" fails review rather than
      passing it.
- [ ] A-6: all four provisioning timers are jittered, matching `reconcile.go`.
- [ ] P2-5: `plan.md` §3.5 names `module.Setup` / octo-lib `module/module.go`,
      and says the rollback ordering it already gets right.
- [ ] D-1 is discoverable from the merged tree: a reader of
      `modules/project/provisioning.go` finds the pointer without reading a
      closed PR thread.
