# msgextra-version — #627 activation runbook

The transactional `message_extra` version allocator (#627) ships **inert**: the
schema (PR #644) and the writer cutover (PR-2) are behavior-neutral because the
DB-authoritative state row is seeded `mode=legacy`, so `ReserveTx` delegates to
the existing process-local `GenSeq`. This tool performs the one remaining step —
flipping the state row to `mode=transactional` — which is what actually fixes the
bug (commit-ordered, per-channel-unique versions across replicas).

Activation is a **runtime state-row flip, not a code deploy**: it takes effect
immediately on every replica the moment the `UPDATE` commits.

## Build

```
go build -o /tmp/msgextra-version ./tools/msgextra-version
```

## 1. Preflight (read-only, safe anytime)

```
/tmp/msgextra-version -config configs/tsdd.yaml -action preflight
```

Reports the current mode/epoch/floor and the recommended `cutover_floor` — the
max of `MAX(message_extra.version)`, the max legacy `seq.min_seq` for the
`messageExtra` keys (the upper bound on versions already handed out), and every
cached `messageExtraVersion:*` Redis hash value. Redis is scanned incrementally
with bounded `SCAN`/`HSCAN` batches (never `KEYS`/`HGETALL`); output includes only
aggregate key/field counts and the maximum, never user/source/channel identifiers.
The floor must be at least this maximum, or post-cutover versions could be
reissued or remain below a cached client cursor and therefore invisible.

## 2. Prepare

- Deploy the PR-2 writer-cutover build to **all** replicas and confirm it is
  stable in `mode=legacy` (behavior-neutral; `reserve_total{result=success}`
  climbing, no `invariant_violation_total`).
- Prove that no pre-cutover binary remains by checking the image digest/version
  on every replica. An old binary does not read the DB state row and can keep
  issuing versions from a cached legacy HiLo block after activation.
- Verify the `octo_message_extra_channel_seq.channel_id` collation matches
  `message_extra.channel_id` in production (the migration pins
  `utf8mb4_general_ci`; confirm the live table agrees).
- **Do NOT set `OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE=transactional` yet.** The
  expected-mode guard fails **every** `message_extra` writer closed
  (`ErrExpectedModeMismatch`) whenever a replica expects `transactional` while the
  state row is still `legacy`. Enabling it before the flip (§3) commits — e.g. a
  rolling deploy that ships the env in the same wave as activation, where an
  un-flipped replica reboots and picks up `transactional` first — is a
  self-inflicted write outage. The guard is the **last** step, only after
  activation is verified (§5).

## 3. Drain and activate

1. Drain or pause both writes to `message_extra` (edits, revokes, pins, read
   receipts, bot/card edits) **and `/message/extra/sync` requests**, then wait for
   in-flight requests to finish. The sync endpoint can advance
   `messageExtraVersion:*` directly from a client cursor and does not take the
   MySQL state-row lock, so it must remain drained through activation.
2. Rerun `preflight` after the drain and review the final recommended floor.
3. Activate while the write drain remains in place:

```
/tmp/msgextra-version -config configs/tsdd.yaml -action activate -yes
# or pin an explicit floor at/above the recommendation:
/tmp/msgextra-version -config configs/tsdd.yaml -action activate -floor <N> -yes
```

`activate` takes the state row `FOR UPDATE` — the **writer drain barrier**: it
waits for every in-flight writer (each holds the row `FOR SHARE` until it commits)
to finish under legacy, then flips while no writer can proceed. It recomputes the
DB maxima and performs a final bounded Redis cursor scan under that lock, and
refuses a floor below any source (`ErrFloorTooLow`). The external drain of
`/message/extra/sync` is what makes the Redis scan authoritative; the MySQL lock
cannot block Redis-only cursor updates. Activation fails closed if Redis cannot
be scanned. It is
idempotent (a second run is a no-op). `epoch` is bumped for observability. The
activation session uses a 3-second `innodb_lock_wait_timeout` as a fail-fast
backstop: if the drain is incomplete, activation fails instead of queueing behind
a long writer and stalling later writes. Fix the drain and retry; do not enable
the expected-mode guard after a failed activation.

## 4. Verify and resume

Run these steps in order:

1. Confirm `preflight` shows `mode=transactional` at the new epoch.
2. Confirm `message_extra.version` is strictly increasing per channel and
   delta-sync no longer skips terminal card frames. Verify metrics:
   `allocator_mode{mode="transactional"}=1`, `cutover_floor` set,
   `reserve_total{result=retry}` near zero, and no invariant violations.
3. Resume message-extra writes after the DB flip and verification succeed.
   Existing upgraded processes have no expected-mode assertion, but immediately
   use the transactional allocator because every transaction rereads the DB row.

## 5. Enable the durable expected-mode guard (only after §4 confirms transactional)

Once `preflight` reports `mode=transactional` (§4), set

```
OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE=transactional
```

on every replica (a normal config rollout / restart). This is the durable
read-side guard: if the state row is ever lost or reset to `legacy`, a replica
that expects `transactional` fails closed (`ErrExpectedModeMismatch`) instead of
silently reverting to the process-local `GenSeq` and re-opening the skip window.

**Ordering is mandatory and one-directional.** The DB flip (§3) must commit and be
confirmed (§4) *before* any replica boots with this env. Never ship the env in the
same rollout wave that performs the flip — an un-flipped replica that picks up
`transactional` fails every writer until activation lands (see the §2 warning).
The read-side proof is `TestRolloutOrdering_ActivateBeforeExpectedMode`: a legacy
state row with `expected=transactional` fails writers closed; after `Activate`
flips the row, the same writer succeeds.

Confirm every restarted replica reports transactional mode and serves a
message-extra write successfully. If activation succeeds but the guard rollout
is partial or fails, keep the DB mode transactional and repair the rollout. Do
**not** flip the DB back to legacy. Normal application rollback may use only a
binary that understands the transactional allocator; the allocator mode remains
transactional.

## 6. Rollback to legacy (coordinated, maintenance-window only)

**There is no online `deactivate` action, by design.** Rolling back to legacy
cannot be done safely by a single mode flip: octo-lib's `GenSeq` HiLo allocator
caches its block in **process memory** (`config/seq.go`, `seqMap`). A replica that
cached a legacy block during the pre-activation phase would, the instant mode
flips back, resume issuing versions from that stale low block — below the
transactional high-water clients' delta-sync cursors have already passed — which
is exactly the #627 skip window. A DB change cannot invalidate that in-memory
cache; only a process restart can. So rollback requires a coordinated,
maintenance-window procedure, run in this order:

1. **Drain / pause writes and `/message/extra/sync` requests** so both the
   transactional high-water and cached Redis cursors stop moving.
2. Run `preflight` and record its `recommended cutover_floor`, which already
   includes DB, legacy-seq, and Redis cursor maxima. **Raise the legacy `seq`
   boundaries above that complete watermark** so a restarted replica's first
   legacy `GenSeq` block starts above every issued version and cached cursor:

   ```sql
   -- Replace <PREFLIGHT_FLOOR> with the drained preflight recommendation.
   SET @watermark := GREATEST(
     <PREFLIGHT_FLOOR>,
     (SELECT COALESCE(MAX(`last_version`),0) FROM `octo_message_extra_channel_seq`),
     (SELECT `cutover_floor` FROM `octo_message_extra_version_state` WHERE `singleton_id`=1)
   );
   -- bump existing legacy seq rows
   UPDATE `seq` SET `min_seq` = GREATEST(`min_seq`, @watermark)
     WHERE `key` LIKE 'seq:messageExtra:%';
   -- create legacy seq rows for channels that only ran transactional
   INSERT INTO `seq` (`key`,`min_seq`,`step`)
     SELECT CONCAT('seq:messageExtra:', `channel_id`), @watermark, 1000
       FROM (SELECT DISTINCT `channel_id` FROM `octo_message_extra_channel_seq`) c
     ON DUPLICATE KEY UPDATE `min_seq` = GREATEST(`seq`.`min_seq`, VALUES(`min_seq`));
   ```
3. **Flip mode to legacy:**

   ```sql
   UPDATE `octo_message_extra_version_state`
     SET `mode` = 0, `epoch` = `epoch` + 1
     WHERE `singleton_id` = 1 AND `mode` = 1;
   ```
4. **Restart every replica** so `seqMap` is flushed; each replica's first legacy
   `GenSeq(messageExtra…)` then reads the raised `min_seq` and issues values above
   the transactional high-water.
5. **Unset / relax `OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE`** (unset or `legacy`)
   as part of the same restart, or the expected-mode guard fails writes closed.

Steps 3–4 must both happen inside the write-drained window (step 1): between the
mode flip and the restart completing, an un-restarted replica would serve legacy
with its stale in-memory block. Keep writes drained until all replicas have
restarted.

Re-activating later **must rerun preflight**: `message_extra.version` now includes
versions issued during the transactional interval, so the floor is recomputed
above them.

## Notes

- The tool connects to MySQL and Redis via the normal DB config. Redis is required
  for cursor evidence; WuKongIM is not used.
- All DB logic lives in `internal/msgextraseq` (`Preflight` / `Activate`) and is
  covered by `activation_test.go` against live MySQL; the command here is a thin
  wrapper.
