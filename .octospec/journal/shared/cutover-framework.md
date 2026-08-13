---
type: Journal
title: "Journal: cutover-framework"
description: Record of extracting the shared control plane of the three one-way cutover mechanisms into pkg/cutover and folding the two standalone operator tools into the server binary
tags: [cutover, operability, refactor, cli, botevent, msgextra]
timestamp: 2026-08-13T00:00:00+08:00
task: cutover-framework
source: self
---

# Journal: cutover-framework

## What was done

- **`pkg/cutover`** (new leaf package): the control plane the three cutovers had
  hand-written separately — `ReadState` (missing row and missing table both →
  `ErrStateMissing`; anything else surfaced so "not migrated" stays
  distinguishable from "authority unreachable"), `Flip` (FOR UPDATE CAS,
  idempotent re-run, observe-under-lock closure, inclusive/strict floor policy,
  optional MaxFloor, optional pinned-connection `innodb_lock_wait_timeout` with
  restore-or-discard — #627's activationTransaction machinery, generalized), and
  `ExpectedMode` (guard env parse/assert: unset asserts nothing, malformed fails
  closed). Error *wording* deliberately stays in the domains: those messages
  explain domain consequences and are read mid-incident.
- **`internal/msgextraseq`**: `Activate` now calls `cutover.Flip` with the
  evidence recompute as the under-lock closure; drain-barrier semantics, floor
  bounds, sentinel errors (`ErrFloorTooLow/TooHigh/StateRowMissing/
  ErrInvariantViolation`) and messages preserved. Guard parse swapped to the
  shared parser (env name and error text unchanged). Added `CurrentState()` for
  the status verb. Runtime paths (`ReserveTx`, `readStateForShare`, `Mode`)
  untouched.
- **`pkg/botevent/state.go`**: `ReadStateContext`/`Activate` delegate to the
  framework; strict floor (`>`) semantics and sentinels preserved; `mode.go`,
  `seq.go` untouched.
- **`app cutover <domain> {preflight,activate,status}`** (root package,
  pre-flag.Parse dispatch like `app session-rollout`): domains `msgextra` and
  `botevent`. Ships in the image — no more cross-compiling and `kubectl cp`ing
  a 43MB binary into a prod pod. `status` is new: state row + guard env, no
  evidence scan. Every invocation prints the MySQL (and Redis, where used)
  endpoint first. `tools/msgextra-version` and `tools/botevent-seq` deleted;
  the botevent CLI's tests (judgeMirror matrix, mirror-write-failure contract,
  scan-paging guards) moved to `cutover_botevent_test.go`.
- **Docs**: `docs/cutover-framework.md` (state-table template, guard-env naming
  `OCTO_<DOMAIN>_EXPECTED_MODE`, the flip-then-arm ordering invariant, evidence
  discipline, Down 3819 pattern, no-online-deactivate, new-domain checklist),
  `docs/msgextra-cutover-runbook.md` (moved from the tool README, commands
  updated), `docs/botevent-cutover-runbook.md` (first standalone runbook — it
  previously lived in source comments and CLI output), cross-reference in the
  token-session runbook.

## What deliberately did NOT move

- **#733 session rollout**: five-phase evidence ladder with a reconciler, not a
  two-state flip; already in-image with its own 7-verb surface. It shares the
  documented conventions, not the code.
- Floor evidence gathering (per-domain by nature), #697's mirror
  judgement/publication, and every runtime read path.
- Existing guard env names (deployments may already reference them); the naming
  convention binds new domains only.

## Accepted small semantic deltas (all fail-closed-preserving)

- Guard env values are now whitespace-trimmed in both domains (botevent already
  trimmed; msgextra previously treated `"legacy\n"` as malformed → fail closed;
  now it is a valid assertion — same assertion, fewer ConfigMap surprises).
- msgextra `Preflight`/`CurrentState` on a missing *table* now report
  `ErrStateRowMissing` ("run the migration first") instead of a raw MySQL 1146.
- botevent `Activate` on a corrupt mode value (neither 0 nor 1) now fails with
  the framework's `ErrUnknownMode` instead of "flip matched 0 rows"; both
  refuse, the new message names the cause.

## Load-bearing details

- `pkg/botevent/genseq_guard_test.go` forbids naming `RobotEventSeqKey` outside
  `pkg/botevent/seq.go` and exempted `tools/`. The evidence sweep moving to the
  root package would have tripped it; the guard now allowlists
  `cutover_botevent.go` explicitly (reader, not allocator) instead of relying
  on a directory exemption. The delegation assertion still targets seq.go only.
- The moved source guard `TestPreflightPagesQueueMembers` reads its own file;
  its target changed from `main.go` to `cutover_botevent.go` with the move.
- `cutover.Flip` returns a typed `*FloorError` rather than formatted sentinel
  errors so each domain re-renders the numbers with its own sentinel and
  wording — existing `errors.Is` contracts in tests and callers survive.

## Verification

- Local (no MySQL/Redis in this sandbox): `go build ./...`, `go vet ./...`,
  gofmt on touched files, `lint-direct-error-response`, `lint-unregistered-code`
  all clean; pure-source tests pass (both genseq/source guards, the moved
  judgeMirror matrix + paging + precondition-text tests, mirror-write-failure
  against a dead Redis, `pkg/cutover` guard parser).
- CI carries the MySQL-backed proof: `internal/msgextraseq` activation/store
  suites, `pkg/botevent` integration suite, new `pkg/cutover` flip/state tests.

## Review round (12 findings, all fixed)

A review pass over the first commit found twelve issues; none touched the
architecture, four were operationally material:

- **The endpoint print leaked the DB password.** `cfg.DB.MySQLAddr` is a full
  DSN, so the "say where we are before doing anything" line — copied from
  session-rollout, which prints a bare `host:port` — put the credential into
  operator scrollback, `kubectl logs`, and any audit capture. Now redacted to
  `host:port/schema` via `mysql.ParseDSN`, with an unparseable DSN yielding a
  placeholder rather than falling back to the raw string. Pinned by
  `TestRedactedMySQLEndpointNeverLeaksTheCredential`.
- **A committed flip could be reported as a failure.** `Flip` returns
  `flipped=true` alongside a joined connection-cleanup error, and both domains
  checked `err` first — so a post-commit cleanup failure produced
  "not flipped", no metrics, and (for botevent) a skipped mirror publication on
  a committed authority. The contract is now documented on `Flip` and both
  wrappers check `flipped` before mapping errors, restoring pre-refactor
  behavior.
- **msgextra never printed the Redis endpoint** even though its floor evidence
  comes from a `messageExtraVersion:*` scan — the same failure mode the design
  cites as its reason for printing endpoints at all. Added for
  preflight/activate, and for `botevent status` which reads the mirror.
- **`msgextra status` hard-failed on a missing state row**, returning before the
  guard readout — precisely the missing-row + armed-guard combination that fails
  every write closed. It now reports MISSING and continues, matching botevent.

The rest: `flag.ErrHelp` handling and `SetOutput(io.Discard)` (so `-h` exits 0
and a typo prints once); guard readout scoped to "this process's env" (the
runbooks endorse off-replica runs, where the local env says nothing about the
fleet); guard spellings exported from each domain via `ExpectedModeSpellings()`
so the CLI cannot disagree with the allocator; `CurrentState` returns
`msgextraseq.State` instead of leaking `cutover.State`; `Flip` takes a context
(a wedged MySQL previously hung the activation with writes drained) and the CLI
supplies a signal-aware one; `gatherBoteventEvidence` writes collisions to `out`
instead of stdout; and the genseq guard's file exemption narrowed from "skip the
file" to "waive the key-name check only", verified by injecting a GenSeq call
and watching the guard fail.

`cutover_cmd_test.go` covers the dispatch guards the file's own comment calls
load-bearing — unknown domain/action rejected before the config is read,
positional args, `-h`, every registered domain fully wired, plus the redaction
and guard-scope assertions.

## Learning

Operator surfaces that live in `tools/` do not ship: the image builds only the
root package, so a standalone cutover tool is undeliverable exactly where it is
needed (production, private deployments). #733 learned this for
session-rollout; this task applies it to the remaining two and records the rule
in docs/cutover-framework.md — `tools/` is for dev-only utilities (repro
harnesses, linters), operator surfaces mount on the server binary.

A second, sharper one from the review: **copying a precedent copies its shape,
not its safety.** The endpoint print, the `flag.ContinueOnError` setup and the
"validate the subcommand before dialing" ordering all came from
`session_rollout_cmd.go` — but the credential-free data source, the
`SetOutput(io.Discard)` line, and the tests pinning the guards did not come with
them. When lifting a pattern from a sibling, diff against the original rather
than re-deriving from memory.
