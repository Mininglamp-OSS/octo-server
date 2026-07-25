---
type: Task
title: "Task: message-extra-transactional-version"
description: Make message_extra delta-sync versions unique and commit-ordered per storage channel across octo-server replicas.
tags: ["message", "delta-sync", "wire-contract", "database", "migration", "concurrency", "multi-replica", "card", "bot-api", "observability", "testing"]
timestamp: 2026-07-21T17:38:17+08:00
# --- octospec extension fields ---
slug: message-extra-transactional-version
upstream: "Mininglamp-OSS/octo-server#627"
source: self
---

# Task: message-extra-transactional-version

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Make `message_extra.version` a unique, commit-ordered cursor within each
persisted channel so `/v1/message/extra/sync` cannot permanently skip a later
committed mutation in a multi-replica deployment.

The implementation introduces a message-extra-specific transactional sequence
in octo-server. Every authoritative `message_extra` mutation reserves its
version from the channel sequence and writes the business row in the **same
MySQL transaction**, while holding the sequence-row lock until commit. This
preserves the existing client wire contract (`extra_version`, ascending
pagination, and `CMDSyncMessageExtra`) while fixing the ordering invariant at
its source.

## Background

- Delta sync reads one storage channel with
  `WHERE channel_id=? AND channel_type=? AND version>? ORDER BY version ASC
  LIMIT ?` (`modules/message/db_message_extra.go`). Once a client advances its
  cursor, a row later committed with `version <= cursor` is invisible to that
  incremental path.
- Current writers allocate versions through octo-lib `Context.GenSeq`. That
  allocator keeps a process-local `seqMap` and reserves blocks of 1000 values;
  the database stores only the block ceiling. Therefore allocation order is
  not globally ordered across octo-server replicas.
- The current block reservation is also a read-then-overwrite operation. Two
  processes initializing the same key concurrently can reserve the same block,
  so cross-process uniqueness is not guaranteed either. Duplicate versions are
  unsafe for `version > cursor` pagination when a page boundary splits rows
  sharing the same value.
- Card edits already serialize competing writes to the same `message_id` and
  allocate after taking that row lock. This closes the single-process race but
  cannot close the cross-process HiLo range problem. `card_seq` protects which
  content wins; it does not make the channel delta-sync cursor monotonic.
- A concrete failure is: replica B commits an intermediate card frame with a
  higher HiLo value; the client syncs it and advances its cursor; replica A then
  commits the accepted terminal frame from an older block. The database stores
  the correct terminal content, but the generic CMD carries neither content nor
  version and the subsequent `version > cursor` read cannot return that row.
- The defect is broader than cards. User/bot/robot edits, callback card
  mutation, revoke/delete, pin changes, manager deletion, and read-receipt count
  materialization all write the same cursor domain.
- Merely replacing HiLo with MySQL `AUTO_INCREMENT` or Redis `INCR` is not a
  root fix. Those primitives order allocation, not transaction commit: a
  transaction holding a lower allocated value can still commit after a higher
  value and be skipped.

## Decisions

### D1. Cursor invariant and scope

For each persisted storage-channel key `(channel_id, channel_type)`:

1. Every committed authoritative `message_extra` mutation has one version
   strictly greater than every mutation that committed before it in that key.
2. No two committed mutations in that key share a version.
3. A transaction that rolls back exposes neither its business mutation nor its
   sequence advance. Reuse of a rolled-back value is allowed because it was
   never visible; gaps are also allowed and carry no protocol meaning.
4. Ordering is per storage channel, not global. Personal chats use the existing
   fake channel ID derived before persistence; group/topic channels use their
   persisted channel ID and current `channel_type` unchanged.

This is the invariant required by the existing channel-scoped delta cursor. A
per-message monotonic value is insufficient because a client cursor can advance
past that message due to a mutation on another message in the same channel.

### D2. Transactional channel-sequence and allocator-state tables

Add a message module migration creating the per-channel sequence table and one
database-authoritative allocator-state row:

```sql
CREATE TABLE `octo_message_extra_channel_seq` (
  `channel_id`   VARCHAR(100)     NOT NULL DEFAULT '' COMMENT '存储行频道ID(person=fakeChannelID)',
  `channel_type` SMALLINT         NOT NULL DEFAULT 0  COMMENT '频道类型',
  `last_version` BIGINT           NOT NULL DEFAULT 0  COMMENT '该频道已分配的最大 message_extra 版本',
  PRIMARY KEY (`channel_id`,`channel_type`)
) ENGINE=InnoDB DEFAULT CHARSET=<match message_extra.channel_id> COLLATE=<match> COMMENT='message_extra 每频道事务序列(#627)';

CREATE TABLE `octo_message_extra_version_state` (
  `singleton_id`  TINYINT UNSIGNED NOT NULL COMMENT '恒为1的单例键',
  `mode`          TINYINT          NOT NULL DEFAULT 0 COMMENT '0=legacy,1=transactional',
  `epoch`         BIGINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '换代计数(operator CAS 递增)',
  `cutover_floor` BIGINT           NOT NULL DEFAULT 0 COMMENT 'cutover 校验后的版本下界',
  PRIMARY KEY (`singleton_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='message_extra allocator 全局状态单例(#627)';

INSERT INTO `octo_message_extra_version_state`
  (`singleton_id`, `mode`, `epoch`, `cutover_floor`)
VALUES (1, 0, 0, 0);
```

- Table names use the `octo_` prefix required for all new tables in this repo
  (existing prefix-less tables such as `message_extra` are not touched); the
  migration file follows `modules/message/sql/<yyyyMMdd>-<seq>_<name>.sql`,
  embedded via `//go:embed sql`, and matches the repo DDL style of the most
  recent new table (`octo_message_card_revision`): inline `PRIMARY KEY`/`KEY`,
  per-column `COMMENT`, and an `ENGINE=InnoDB DEFAULT CHARSET=... COLLATE=...
  COMMENT=...` suffix.
- `octo_message_extra_channel_seq.channel_id` charset **and** collation must match
  `message_extra.channel_id`, and `channel_type` its persisted domain. Note
  `message_extra` declares no explicit charset/collation (it inherits the schema
  default), so the implementation must read the **effective** charset/collation
  of `message_extra.channel_id` from the live schema and pin the new table to it
  — do not assume `utf8mb4_general_ci`. The state table carries no channel key,
  so it may use the standard `utf8mb4`/`utf8mb4_general_ci`.
- The channel-sequence table contains no business content and has one hot row
  per active storage channel, avoiding a process-wide or deployment-wide
  sequence bottleneck. Both new tables are empty/tiny at creation, so their
  `CREATE TABLE` statements are cheap and need no online DDL tooling.
- `octo_message_extra_version_state` is the sole allocator-mode/floor source of
  truth for every upgraded binary. Exactly one row (`singleton_id=1`) must
  exist. A **missing** row defaults to `legacy` — the safe pre-migration
  behavior (existing `GenSeq`), which a deploy also sees before the migration
  seeds the row and which test setups leave after `CleanAllTables`; this is not
  fail-closed. An unrecognized `mode` value or an unreadable row **does** fail
  closed. `epoch` is never a write-abort condition (see D3). A row deleted while
  in transactional mode is guarded by the **expected-mode check** on the state
  read (`OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE`): when a deployment declares
  `transactional`, a resolved mode that is not transactional (including a missing
  row → legacy) fails closed. Unset makes no assertion (pre-migration deploys and
  test setups keep the legacy default). Environment variables assert an expected
  mode but must never select an allocator independently of this DB row.
- Separately, add on the **existing `message_extra` table** the composite index
  the delta-sync read path needs: `(channel_id, channel_type, version)`. Today
  `message_extra` carries only `channel_idx (channel_id, channel_type)`, so the
  `WHERE channel_id=? AND channel_type=? AND version>? ORDER BY version ASC`
  scan cannot resolve the `version` range/ordering from the index. This index is
  the one with real DDL cost: because `message_extra` is a large, live table, the
  migration must use the deployment's approved online-schema-change path when the
  target MySQL version cannot add it online without a table rebuild or blocking
  writes. (This is a read-path optimization independent of the sequence rework;
  land it in the same task since both touch the cursor domain.)
- Do not modify octo-lib `GenSeq` in this task. Its other callers have separate
  compatibility and ordering contracts. During the Prepare phase, the one
  centralized version store may retain a transitional legacy adapter; business
  writers must no longer call `GenSeq(MessageExtraSeqKey, ...)` directly, and
  transactional mode must never call it.

### D3. Reservation API and transaction ownership

Introduce one shared message-extra version store, usable from `modules/message`,
`modules/bot_api`, `modules/robot`, and `internal/carddispatch`, with semantics
equivalent to:

```go
ReserveTx(tx *dbr.Tx, storageChannelID string, channelType uint8, count int) (versions []int64, err error)
```

`versions` always has `len == count`, in assignment order. In transactional mode
it is a contiguous ascending range; in legacy mode it is `count` distinct values
from `GenSeq` (contiguity is not guaranteed under concurrency — matching pre-task
per-message behavior). Returning explicit per-item values, rather than a single
`first`, keeps callers mode-agnostic and behavior-preserving across the Prepare
window (a `first`+`first..first+N-1` assignment would be unsafe in legacy mode).

The store must live in a neutral package that all four callers can import
without an import cycle (e.g. `internal/msgextraseq` or a `pkg/` package), not in
`modules/message` itself if that would clash with existing dependency direction
between `modules/message`, `modules/bot_api`, `modules/robot`, and
`internal/carddispatch`. Confirm the dependency direction before placing it.

- `count` must be positive and must not exceed a fixed upper bound of **1000**
  (aligned with this module's existing chunk magnitude). `ReserveTx` rejects
  `count <= 0` or `count > 1000` before touching any row. Callers handle the cap
  by semantics: client-supplied batches (e.g. manager delete `req.List`) reject
  oversized input through the existing localized error contract; internal batch
  paths that must not drop work (e.g. read-receipt materialization) reserve in
  `<=1000` chunks — repeated reservations on the same channel stay monotonic.
  This keeps a pathological batch from reserving a huge range while holding the
  hot sequence-row lock.
- The caller starts and owns the business transaction. The allocator must not
  open or commit an independent transaction. The design must be correct under
  the repo's ambient isolation level (MySQL default `REPEATABLE READ` on pooled
  dbr connections); the race-safe initializer below removes the need for a
  gap-lock-avoiding level. Do **not** silently `SET TRANSACTION ISOLATION
  LEVEL` per call: that leaks to the next user of a pooled connection. If a
  future need for `READ COMMITTED` is proven, it must be set at transaction
  granularity by every migrated caller and reset before the connection returns
  to the pool, and stated explicitly — not assumed.
- Before choosing an allocator, every upgraded writer performs a current locking
  read of `octo_message_extra_version_state(singleton_id=1)` in the same business
  transaction (`SELECT ... FOR SHARE`) and holds that shared lock until
  commit/rollback. Shared locks are compatible across writers, so this does not
  serialize normal traffic; the exclusive mode flip waits for all upgraded
  in-flight writers to finish. This per-transaction locking read is the drain
  barrier, so the state row is deliberately not cached across transactions.
  `mode=0` is the only state that may call the centralized legacy adapter;
  `mode=1` must use the transactional channel sequence. A **missing** state row
  defaults to `legacy` (safe pre-migration behavior; see D2). A write aborts and
  marks readiness unhealthy only on: more than one row, a `mode` value outside
  the known set, or a state read failure. An optional expected-mode config
  mismatch also fails closed. `epoch` is not validated by writers and is never a
  write-abort condition.
- `epoch` is a monotonic generation counter owned exclusively by the operator
  activation/emergency tool. It is bumped only inside the guarded exclusive state
  transition via compare-and-set (assert expected epoch `N`, set `N+1`), which
  prevents a stale or double activation from racing operators or a reran tool.
  Business writers record the epoch they read for observability (D8) and never
  compare it against a compiled-in value; the allocator is selected solely by
  `mode`. Cross-generation safety is provided by the per-allocation floor raise
  below plus the preflight floor bound (D9), not by epoch.
- The first allocation for a channel initializes `last_version` to the maximum
  of:
  1. the deployment's verified `cutover_floor`;
  2. the current maximum `message_extra.version` for that channel/type; and
  3. the legacy octo-lib `seq.min_seq` value for the corresponding
     `MessageExtraSeqKey` channel key, when present.
  The legacy `seq.min_seq` value is evidence, not a sufficient safety boundary:
  its current read-then-overwrite implementation can move backward when an old
  replica extends a stale block. `cutover_floor` is the authoritative boundary
  derived by the deployment preflight in D9.
- On every transactional allocation, including for an already-existing channel
  row, the locked `last_version` is first raised to at least the current state
  row's `cutover_floor`. This makes a later transactional epoch/cutover safe even
  when the per-channel row survived an emergency legacy interval.
- Initialization must be concurrency-safe through the table's primary key. A
  bare `SELECT ... FOR UPDATE` on a not-yet-existing row does **not** block, and
  two initializers that both read empty then `INSERT` will collide: the loser
  gets MySQL error 1062 (duplicate key), not a wait. The implementation must use
  a race-safe upsert — e.g. `INSERT ... ON DUPLICATE KEY UPDATE
  last_version=last_version` (a no-op placeholder to materialize the row), then
  `SELECT ... FOR UPDATE` to reload and lock the winning row — or explicitly
  catch 1062 and re-`SELECT ... FOR UPDATE`. A duplicate initializer must
  reload the winning row, never proceed on a private baseline.
- Reservation locks the sequence row with `SELECT ... FOR UPDATE`. Acquiring the
  lock and advancing the counter are distinct steps: the writer takes the lock,
  performs any accept/reject decision that depends on the message row (e.g. a
  card CAS check), and only advances `last_version` by `count` when the mutation
  is actually going to be written. On accept it returns the contiguous range
  `old+1 ... old+count`; a rejected/stale path releases the lock on rollback
  without consuming any version (gaps are allowed by D1.3). The lock remains held
  until the caller commits or rolls back the business transaction.
- Reservation fails before mutation when `last_version+count` would overflow
  `int64` or exceed `2^53-1`; this preserves exact cursor representation for
  existing JavaScript clients instead of silently emitting lossy JSON numbers.
- Batch writers reserve once and assign values deterministically. They must not
  perform one sequence round trip per message.

### D4. Lock ordering and retries

- Every migrated writer acquires locks in this order:
  1. the singleton allocator-state row in shared mode;
  2. channel-sequence rows, sorted by `(channel_id, channel_type)` if a
     transaction touches more than one channel;
  3. `message_extra` rows, sorted by `message_id` for batches;
  4. any secondary rows already required by that operation.
  This order is uniform across all writers, and the mode flip takes only the
  exclusive state-row lock (it never acquires sequence or `message_extra` locks),
  so no writer/writer or writer/flip lock cycle can form.
- The current card CAS/LWW paths must be reordered from "message row then
  GenSeq" to "channel sequence then message row". A stale `card_seq` conflict
  rolls back without consuming a committed version.
- Map iteration order must never determine lock order. In particular, the
  read-receipt event path groups work by channel today and must sort the keys
  before reserving ranges.
- MySQL errors 1213 (deadlock) and 1205 (lock wait timeout) receive one bounded
  retry policy around the **whole business transaction**. Do not nest allocator,
  card-mutator, and handler retry loops. Exhaustion fails the operation using
  its existing localized error/logging contract; it must not send a CMD for an
  uncommitted mutation.

### D5. Complete writer cutover

The following production paths, including any helper they call, move to the
shared transactional allocator:

- user message edit, mutual/global delete, revoke, and other
  `modules/message/api.go` mutations;
- pin/unpin and bulk pin removal in `modules/message/api_pinned.go`;
- read-receipt count materialization in `modules/message/event.go`;
- manager message-extra deletion in `modules/message/api_manager.go` — this
  path currently accepts a client-supplied `req.List` with only an empty check
  and no upper bound; add a `len(req.List) <= 1000` guard (existing localized
  error contract) as part of the cutover so the batch reservation is bounded;
- bot edit with `card_seq`, bot edit without `card_seq`, and non-card bot edits
  in `modules/bot_api/send.go`;
- internal callback terminal-card mutation in
  `internal/carddispatch/mutation.go`;
- legacy robot edit in `modules/robot/api.go`.

Implementation-time source search is authoritative: no business writer may
call `GenSeq` with `common.MessageExtraSeqKey`. At most one call may remain in
the centralized, explicitly named transitional legacy adapter used only during
the Prepare/rollback mode; transactional mode and all test production paths
must bypass it. A source-guard test must enforce this allowlist so a new writer
cannot silently reintroduce the incompatible allocator.

All affected operations must write `message_extra` transactionally. A path that
currently performs a single autocommit upsert must be converted to begin →
reserve → mutate → commit before CMD fan-out.

### D6. Existing business and wire semantics remain unchanged

- `/v1/message/extra/sync` request/response fields, ordering, default/max limit,
  membership checks, and fake-channel derivation are unchanged. Its Redis cursor
  cache preserves the existing monotonic `max(cache, request)` behavior for valid
  values, but untrusted request/cache cursors are constrained to the storage
  channel's persisted `MAX(message_extra.version)` so they cannot poison the
  activation floor; historical poisoned entries self-heal on read.
- `CMDSyncMessageExtra` remains a best-effort notification sent only after the
  authoritative transaction commits. It does not gain card content, message ID,
  or version in this task.
- `card_seq` conflict/replay semantics, content hash deduplication, card revision
  history, edit ownership, revoke/delete gates, and user-facing i18n envelopes
  remain unchanged.
- Existing Space/Auth/Bot ownership checks remain before mutation. Moving the
  version allocator must not create a bypass or a cross-Space lookup.
- Sequence allocation failure is an internal storage failure. Existing endpoint
  error codes are reused; this task does not introduce raw error responses or
  expose DB/lock details to clients.

### D7. Performance and bounded contention

- Serialization is deliberately per storage channel because that is the cursor
  consistency domain. Different channels must allocate independently.
- Batch paths reserve a range once. The implementation must keep transactions
  free of network calls and other unbounded work while holding the sequence-row
  lock; WuKongIM/CMD calls and search event commits happen after DB commit as
  their current contracts require.
- The sequence lock adds a write hotspot for very active channels. Before
  rollout, run a concurrency test representative of large-group read receipts
  and card edits; record p50/p95/p99 sequence-lock wait and transaction latency.
  No fixed throughput SLA is invented by this brief, but an unexplained
  regression relative to the legacy path blocks rollout.
- The per-write shared lock on the singleton state row is part of the measured
  path. It is compatible across writers (no steady-state serialization) and
  costs one extra small primary-key lookup per business transaction — amortized
  across a batch writer, not paid per message. The concurrency test records
  state-row shared-lock wait (p50/p95/p99) and the added per-transaction latency
  versus the legacy path alongside the sequence-row metrics.

### D8. Observability

Add low-cardinality metrics for the shared allocator:

- `message_extra_version_reserve_total{result=success|retry|failure}`;
- `message_extra_version_reserve_seconds` histogram;
- `message_extra_version_lock_wait_seconds` histogram;
- `message_extra_version_state_lock_wait_seconds` histogram (shared state-row
  wait, kept separate from the sequence-row lock);
- `message_extra_version_batch_size` histogram;
- `message_extra_version_allocator_mode{mode=legacy|transactional}` gauge;
- `message_extra_version_allocator_epoch` gauge;
- `message_extra_version_cutover_floor` gauge;
- `message_extra_version_invariant_violation_total` counter, incremented when a
  defensive check observes a candidate version not greater than the currently
  locked row version or a duplicate result.

Structured failure logs include `channel_id`, `channel_type`, operation,
attempt, MySQL error number, and stage
(`state|initialize|lock|reserve|write|commit|activate`). They must not include
message content, card payload, credentials, or raw request bodies.

### D9. Deployment, rollback, and historical state

The allocator change is not safe to activate while any pre-task binary can
still write from a cached legacy HiLo block. The DB state row is authoritative
for upgraded binaries, but old binaries do not know it exists; the deployment
must prove their replica count is zero before activation.

Deployment is two-phase:

1. **Prepare** — apply both new tables and the `message_extra` index. The
   migration seeds state `(mode=legacy, epoch=0, cutover_floor=0)`. Deploy the
   dual-capable binary everywhere; every upgraded writer reads the DB state row
   and therefore continues through the one centralized legacy adapter. Confirm
   image digests/versions, readiness, migrations, and focused tests on every
   replica.
2. **Cut over** — drain message-extra write traffic **and the
   `/message/extra/sync` requests that can advance Redis cursors**, wait for
   in-flight requests, and confirm no pre-task binary remains. Run the final
   preflight, then use the operator tool to take an exclusive lock on the state
   row, assert its expected mode/epoch, and atomically set
   `cutover_floor=<verified floor>`, increment `epoch`, and set
   `mode=transactional`. The activation transition runs with a
   short `innodb_lock_wait_timeout` on its own session: if it is issued before
   the write drain completes, the exclusive state-row lock fails fast and reports
   the in-flight writers instead of stalling live message-extra writes. Because
   the flip only ever runs after the documented drain, steady-state writers are
   not affected. Upgraded writers hold a shared state lock for their transaction,
   so this flip waits for any upgraded in-flight legacy writer and no later
   upgraded writer can independently choose legacy. Resume traffic only after
   readiness and allocator metrics report the new DB mode/epoch/floor on every
   replica.

The DB state row, not an environment variable, selects the allocator. An
optional deployment setting may declare an **expected** mode/epoch and fail
startup/readiness on mismatch, but it cannot override DB state. Missing,
duplicate, or unreadable state and an unrecognized `mode` value fail
message-extra writes closed; `epoch` never fails a write and is validated only
by the operator tool's compare-and-set during a transition. The transactional
`cutover_floor` must be positive and no greater than `2^53-1` so existing
JavaScript clients can continue to represent JSON integer cursors exactly.

An operator first runs the preflight before draining to estimate the floor and
validate scan cost. After write traffic, `/message/extra/sync`, and their
in-flight requests are drained, the operator reruns it authoritatively; no
message-extra writer or Redis cursor updater may resume between that final scan
and transactional activation. The tool uses Redis
`SCAN`/`HSCAN` (never `KEYS`) and bounded DB queries to find the observed
maximum across:

1. `message_extra.version`;
2. matching legacy `seq.min_seq` rows; and
3. every cached `messageExtraVersion:*` hash field. Non-negative values at or
   below the DB/legacy issued ceiling participate in the floor. Malformed,
   negative, or above-issued values cannot be trusted as server-issued:
   preflight counts them as poisoned and excludes them, while the upgraded sync
   handler repairs their per-channel cache entry on its next request.

The task delivers an operator command under `tools/`: its default/read-only
preflight computes these maxima and validates an operator-supplied floor; an
explicit activation action performs only the guarded state-row transition
described above. The chosen `cutover_floor` is strictly greater than the
observed maximum, with documented
headroom for future increments while remaining below the safe integer limit. If
the observed maximum leaves no safe headroom, cutover fails closed and requires
a separately approved cursor-wire migration. Preflight output records counts,
maxima, and the selected floor without printing UIDs, source identifiers,
channel IDs, or other Redis values. Startup logs and low-cardinality gauges
expose the DB-authoritative mode, epoch, and floor.

**Allocator cutover is one-way for normal rollback.** Business/application code
may roll back only to a binary that still understands the state row and
transactional allocator; DB mode remains `transactional`, and the version
allocator is not rolled back. A pre-task binary is not a supported normal
rollback target. This is intentional: returning a multi-replica deployment to
the old HiLo allocator necessarily reopens the duplicate-range and commit-order
defects fixed by this task.

If a defect in the transactional allocator itself forces an emergency return to
a pre-task binary, the runbook must use this explicitly degraded procedure:

1. drain all message-extra writes, wait for in-flight requests, and scale every
   octo-server replica to zero;
2. for each legacy key (which is keyed by storage `channel_id` without
   `channel_type`), compute one watermark equal to the maximum of legacy
   `seq.min_seq`, `message_extra.version`, cached Redis client cursors, the state
   row floor, and every `octo_message_extra_channel_seq.last_version` across all
   channel types sharing that channel ID;
3. upsert legacy `seq.min_seq` to at least that watermark, record the emergency
   in the state row as `mode=legacy` with an incremented epoch, and verify no
   watermark exceeds the JavaScript safe-integer boundary;
4. start the pre-task binary at **exactly one replica**. Multi-replica legacy
   mode is forbidden because watermark reconciliation cannot repair HiLo's
   ongoing duplicate-block/commit-order behavior;
5. before returning to transactional mode, drain and scale to zero again, rerun
   the complete preflight, choose a new higher floor, increment the state epoch,
   and perform the full transactional cutover. Existing channel rows adopt the
   new floor per D3.

Emergency legacy operation may still require targeted stale-client recovery and
is not a normal availability-preserving rollback. The allocator must never be
alternated while traffic is live.

Existing `message_extra` rows remain valid and are not rewritten by the schema
migration. The first transactional value is above the preflight-observed DB,
legacy-seq, and cached-client maxima, so future mutations are visible to
existing cursors. Automatically rewriting every historical row and fanning out
every channel is deliberately excluded from this task because it can create an
unbounded DB/WS storm and personal fake channels do not retain enough
addressing data for a safe generic broadcast. The rollout runbook must state
that already-stale clients require a separately approved recovery action
(client full refresh/source reset, a targeted row bump through the new
allocator, or a future exact-refresh protocol).

## Load-bearing list

- **Delta-sync cursor contract** — `version > cursor`, ascending pagination,
  and client cursor advancement require unique commit order per storage
  channel.
- **Cross-replica transaction ordering** — sequence reservation and business
  write are atomic and serialized through one InnoDB row per channel.
- **All message-extra producers** — no writer may bypass the shared allocator;
  a single legacy path can poison the cursor domain for every client.
- **DB-authoritative allocator state** — every upgraded writer holds the
  singleton state row in shared mode; a missing row defaults to legacy while an
  invalid/unsupported/unreadable row fails closed, and only a guarded exclusive
  transition changes mode/epoch/floor.
- **Card correctness** — accepted terminal `card_seq` content remains
  authoritative and becomes incrementally visible; CAS/replay behavior is not
  weakened.
- **Batch and lock discipline** — deterministic multi-channel/message ordering,
  one range reservation per batch, and bounded whole-transaction retry prevent
  deadlock amplification.
- **Wire compatibility** — no request/response/CMD shape change and no required
  client release.
- **Multi-tenant boundaries** — storage-channel derivation and existing
  Auth/Space/Bot ownership gates stay authoritative.
- **Production migration** — online DDL assessment, no mixed allocator mode,
  explicit drain/cutover/rollback, and a verified cutover-floor bootstrap.
- **Operational visibility** — lock contention, retries, failures, batch sizes,
  and invariant violations are observable without logging message content.

## Out of scope

- Changing octo-lib `GenSeq` or auditing every non-message-extra sequence user.
- Replacing delta sync with an append-only change log, binlog/CDC stream, or a
  composite cursor.
- Adding content/message/version to `CMDSyncMessageExtra` or changing web,
  desktop, iOS, or Android refresh behavior.
- Redesigning Redis cursor ownership or defining reconnect/foreground
  source-reset semantics beyond the bounded validation/self-healing required for
  cutover safety.
- Automatically re-versioning all historical `message_extra` rows during
  migration or broadcasting a fleet-wide forced resync.
- Changing card rendering, callback delivery, revision history, `card_seq`,
  edit permissions, rate limits, or user-facing error codes.
- Introducing a global distributed lock or leader election.
- Treating a pre-task binary or the legacy HiLo allocator as a supported normal
  rollback target after transactional activation.

## Acceptance

- A deterministic two-session MySQL integration test proves that two concurrent
  transactions for the same storage channel commit with unique increasing
  versions. The second allocator blocks behind the first sequence lock and
  receives the next value only after the first commits.
- A rollback test proves a failed business transaction leaves neither a
  `message_extra` mutation nor a committed sequence advance visible.
- A different-channel concurrency test proves sequence rows do not introduce a
  global lock.
- A personal-chat test proves the sequence key `channel_id` derived by the
  allocator is byte-identical to the fake `message_extra.channel_id` persisted on
  the same mutation, so the seq row and the delta-sync read path never diverge.
- A batch test reserves N values once, assigns a deterministic contiguous range,
  and paginates all resulting rows through repeated `version > cursor` reads
  without duplicates or omissions.
- A regression test reproduces the card scenario with two independent writer
  contexts: after the client observes the intermediate frame, the accepted
  terminal frame is returned by the next delta sync and has a greater version.
- Card CAS tests continue to prove stale `card_seq` rejection, identical-frame
  replay, mixed CAS/LWW behavior, and final-content correctness.
- Focused tests cover user edit, bot edit, callback mutation, robot edit,
  revoke/delete, pin, manager delete, and batched read-receipt writers using the
  shared transactional allocator.
- A source guard fails if any business writer calls `GenSeq` with
  `common.MessageExtraSeqKey`, or if more than the single allowlisted
  transitional legacy adapter contains that call.
- First-use bootstrap tests cover: legacy seq row present, legacy row absent,
  existing message-extra maximum above the legacy seq value, and two concurrent
  initializers. The concurrent-initializer test asserts the loser reloads the
  winning row (no 1062 leaking out, no private baseline, no init deadlock under
  the repo's ambient isolation level).
- Boundary tests prove allocation rejects `count <= 0`, `count > 1000`,
  `int64` overflow, and crossing `2^53-1` without mutating the sequence or
  business row. A test proves manager delete rejects an oversized `req.List`
  (> 1000) through the existing localized error contract, and that read-receipt
  materialization chunks a > 1000-message channel group into monotonic
  reservations without gaps or duplicates.
- Deadlock/lock-timeout tests prove one bounded whole-transaction retry policy
  and no CMD before a successful commit.
- Migration validation covers a clean database and a populated database,
  verifies both new `octo_` tables plus the singleton legacy-mode seed row, and
  verifies the new `(channel_id, channel_type, version)` index exists on
  `message_extra`; the production DDL plan documents whether native online DDL
  is supported or an external online-schema-change tool is required for that
  index on the live table.
- State-control integration tests prove concurrent upgraded writers can hold
  compatible shared state locks, an exclusive cutover waits for in-flight
  writers, and every write beginning after the flip selects transactional mode.
  A missing state row defaults to legacy (write succeeds via GenSeq, transactional
  table untouched); a duplicate row, an unrecognized `mode` value, a state read
  failure, and an optional expected-mode mismatch all fail writes closed and mark
  readiness unhealthy; a bumped `epoch` alone never fails a write; and no local
  environment value can override DB mode.
- Cutover-preflight/activation tests cover DB/legacy-seq/Redis maxima, poisoned
  Redis cursor classification without floor inflation, per-channel sync-cursor
  normalization/self-healing, stale or regressed `seq.min_seq`, safe-integer
  overflow rejection, redacted output, expected mode/epoch compare-and-set,
  atomic DB mode/floor/epoch transition, and transactional refusal when DB state
  is missing or malformed.
- Rollback tests prove normal application rollback keeps DB mode transactional
  and accepts only a transactional-capable binary. Emergency-tool tests merge
  all `channel_type` rows into the legacy channel-ID key, reconcile the maximum
  DB/Redis/legacy/new-sequence watermark, require zero running replicas before
  mutation, and require the next transactional activation to use a higher floor
  and epoch. The runbook pins emergency legacy operation to one replica.
- Allocator metrics and structured logs are asserted without message/card
  content or credentials.
- At minimum, focused suites pass for `./modules/message/...`,
  `./modules/bot_api/...`, `./modules/robot/...`, and
  `./internal/carddispatch/...`; broader `go test ./...` is run when the
  MySQL/Redis/WuKongIM test environment is available.
- The rollout artifact includes Prepare, drain, DB-state activation,
  verification, normal transactional-capable application rollback, targeted
  stale-client recovery, emergency single-replica legacy rollback, and full
  re-cutover steps. Verification queries assert that no newly committed row
  version is at or below the transactional channel's previous committed value,
  every upgraded replica reports the DB mode/epoch/floor, no pre-task binary is
  running, and invariant-violation metrics stay zero.
