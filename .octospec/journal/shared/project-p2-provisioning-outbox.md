---
type: Journal
title: "Learning: one boolean for two subsystems, and the index that made two goroutines fight"
description: "The P2 provisioning outbox: why a process-global reclaim gate over a target-less DELETE is an irreversible-loss bug rather than a tidiness nit, why a rollback runbook has to name its ordering, and the FOR UPDATE SKIP LOCKED starvation a flaky test found that seven review passes did not."
tags: ["octospec-learning", "project", "provisioning", "outbox", "mysql", "skip-locked", "runbook", "rollback", "hmac", "testing", "mutation-testing"]
timestamp: 2026-09-07T16:00:00Z
source: self
---

# Learning: project-p2-subsystem-integration (PR #850)

## What changed

Each Project now gets one fleet workspace and one drive space, provisioned
through a transactional outbox (`octo_project_provisioning`, written inside the
project-create transaction) and a leased worker that calls one `ensure` endpoint
per subsystem over v1 HMAC. Teardown is pull-only: disband moves rows to
`disband_pending` and the subsystem polls a status endpoint that ships in a
later PR. Container ids are opaque and non-derivable, and no path discloses one.

The slice is inert by default — the enabled-target list is empty — which is why
seven review passes converged on documents and predicates rather than on the
state machine.

## What the last round actually fixed

Both remaining blockers were the same shape: **an authority whose scope was
wider than the state it was allowed to speak for.**

- **A process-global reclaim gate over a DELETE with no `target` predicate.**
  Everything else per-subsystem in this slice is per target — enablement, URL,
  secret, narrowing declaration — because fleet and drive land on different
  teams' schedules. The reclaim *consumer* lands separately too. So the first
  subsystem to ship one, with one global boolean, silently authorized deleting
  the other's reclaim records 90 days later: the D9 status answer becomes
  permanently `unknown`, D9 makes `unknown` indistinguishable from "outside your
  grant", and the container can never be reclaimed. Deleting a row is the only
  irreversible operation in the slice.

  The gate is now a per-target set threaded into the DELETE as `target IN ?`,
  resolved for every *known* target rather than every *enabled* one — because
  enablement governs what we create and this governs what we may forget, and a
  target disabled after use still has containers to reclaim.

- **A rollback runbook whose step 2 broke a path unrelated to the rollback.**
  `markProvisioningDisbandPendingTx` is called unconditionally inside the
  disband transaction, and that is *correct* — gating it on `Enabled()` would
  make a target that was enabled, produced rows and was then disabled stop
  marking its containers reclaimable, which is a real leak. The consequence is
  that step 1 (clear the target list, restart) does not stop the write, so a
  hand-run `DROP TABLE` turned every disband into `Error 1146` → 500. And it
  never healed: sql-migrate's ledger row survives a manual drop, so no restart
  recreates the table. The runbook now names the ordering (binary first, then
  table) and specifies `sql-migrate down`.

## The finding review did not produce

Neither seven review passes nor my own mutation runs found this; a **flaky test
did**, and only because the assertion happened to be on `attempts`, which is
written at claim.

Every claim/sweep/purge statement filters `target IN (...)`. While both scan
indexes led with `status`, each per-target scan walked an index range containing
the *other* target's rows, locked them, and only then filtered them out at the
server layer — and `FOR UPDATE SKIP LOCKED` makes the sibling scan skip a row a
peer transaction is holding. Measured on MySQL 8.0.46: with both targets due in
the same tick, the loser claimed **nothing** and waited silently for the next 15s
tick. That is exactly the mutual starvation the one-goroutine-per-target fan-out
was written to remove, so the fix belongs in the index, not the fan-out. Both
scan indexes now lead with `(target, status)`; as a side effect the `(target,
status)` census became a covering-index read instead of a whole-table scan.

Two things worth keeping from how it was found:

- **A correctness-shaped defect can present only as latency.** No row was lost,
  no state was wrong; the only symptom was a tick that did nothing. Under the
  runbook's own reading, a rising `pending` count means "the target is not
  responding".
- **A probabilistic guard is not a guard.** One tick caught the reintroduced
  defect in 7 of 8 runs — a test that reports green one time in eight while the
  defect is present. Five independent rounds took that to 8 of 8. Rounds, not
  rows in one wider tick: rows in one tick correlate.

## Two ways my own tests were vacuous

Both found by mutation, not by reading:

- **A fixture keyed by the constant the code reads is a tautology.** The
  reclaim-env test set `map[string]string{envProvisionDriveReclaimConsumerLive:
  "true"}`. Mistyping that constant renames it on *both* sides, so the test
  stayed green while the env the process actually reads no longer existed — and
  the failure direction is silent (purge permanently off, which is precisely the
  state an operator would believe they had switched on). Env names are a deployed
  contract, not a Go identifier, so the assertion has to be made of the literal.
- **Cardinality is not identity.** `TestPurgeOnlyDeletesDisbandPendingRows`
  asserted "1 deleted, 2 survive". A purge that deleted the `ready` row instead
  yields the same two numbers. It now reads back *which* statuses survived.

## Also worth recording

- **`[]uint8` is `[]byte`.** dbr scans a `*[]uint8` as a byte string, so status
  values came back as their ASCII codes (`49` for `1`) and the comparison quietly
  compared the wrong thing. Use `[]int` for small integer columns.
- **Published example credentials pass a length floor.** The conformance vectors
  ship two working secrets in a non-test file of a public repo, both 33 bytes, so
  both cleared the 32-byte bar: copying one into the production env booted clean
  holding a published HMAC key, and one of the shipped vectors is literally the
  forgery that key authorises. `ValidateTarget` now rejects them by value, and the
  check lives next to the literals so a fifth vector cannot leave it behind.
- **Mechanical renames damage prose.** A `sed` over an import path minted two
  paths that do not exist, in the very file other repositories are told to copy
  the wire contract from. Grep the prose after a path rename, not just the code.

## The follow-up round: a field that was preserved and empty

Both approving reviews independently named the same item as the one to fix before any target
is enabled, and it is worth recording why a *cosmetic-looking* defect earned that.

`last_error` for a target that is down read `transport_failed: projectprovision:
transport_failed` — the outcome label, twice. The real reason (connection refused / DNS / TLS
/ deadline) was captured in the error's unexported `cause`, `Unwrap()` existed, and **nothing
in the repository ever called it.** So the module had gone to real trouble to protect that
field — the sweep was deliberately changed to *append* to it rather than overwrite, on the
argument that it is "the only durable per-row evidence of why provisioning failed" — while the
field was empty for the first failure an operator would ever meet. Preserving a container
carefully and leaving it empty is its own failure mode, and it is invisible to every test that
only checks the field is *present*.

Two things about the fix that are the actual content:

- **The reason it was empty in the first place was a correct constraint.** `Error()` was built
  from category and status only *because* that string lands in `last_error` and the container
  id is a capability. Folding `cause` in would have satisfied the review and quietly broken
  that: at the `encode_failed` site `cause` is a `json.Marshal` error over an `EnsureRequest`,
  which carries the container id. `json.Marshal` of an all-string struct cannot realistically
  fail — and "cannot realistically" is not the bar for a capability. So the detail is drawn
  *only* from the transport error's **inner** error, which describes the network and
  structurally cannot contain the request. The guarantee stays a property of construction
  rather than an argument about how the standard library formats things.
- **The fix did not reach the row that needed it most until it was traced.** `finishProvisioning`
  *SET*s `last_error`, so the abandon path replaced the accumulated detail with "retries
  exhausted" — on the one row with no automatic re-drive. Exactly the defect the sweep's append
  had already fixed one layer away. Fixing the write and not the terminal write would have
  shipped a green test suite and an empty field.

## Making the doc-truth class mechanical

Five consecutive rounds produced the same finding shape: **a comment or document asserting
behaviour the code does not have.** Two instances were minted by a mechanical import rename,
one by this slice's own index change invalidating untouched prose. Every one was caught by a
human reading carefully — which is the wrong use of a careful human, because the references
involved are ones a grep can settle.

So two guards now own that half:

- Every repository path named in the slice's comments or task documents must resolve in the
  tree.
- Every parenthesised column tuple made *entirely* of the table's own columns is read as a
  claim about an index, and must be a **prefix** of a key the migration actually declares.

Both found a real live instance on the first run — `brief.md` still enumerated
`(status, finished_at)` after the index change, which three reviewers and I had all missed.
Two implementation notes worth keeping:

- **The first version of the path guard was all false positives.** It matched HTTP route
  paths and URLs, because `/v1/internal/projects/status` contains a substring shaped exactly
  like a package path. RE2 has no lookbehind, so the fix is a capture group on the preceding
  character: a real reference starts at a non-path character. That one condition removed the
  entire class.
- **A guard that reads its own file cannot quote the defect it catches.** Both guards tripped
  on their own explanatory comments. Describing the bad shape in words instead of quoting it
  is not a workaround — it is the guard demonstrating that it works.

