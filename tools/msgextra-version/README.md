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
max of `MAX(message_extra.version)` and the max legacy `seq.min_seq` for the
`messageExtra` keys (the upper bound on versions already handed out). The floor
must be at least this, or the new sequence could reissue a version ≤ an existing
one and re-open the exact skip window #627 closes.

## 2. Prepare

- Deploy the PR-2 writer-cutover build to **all** replicas and confirm it is
  stable in `mode=legacy` (behavior-neutral; `reserve_total{result=success}`
  climbing, no `invariant_violation_total`).
- Set `OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE=transactional` on every replica
  **as part of the same rollout that activates** (see §5). This is the durable
  guard: if the state row is ever lost/reset, a replica that expects
  transactional fails closed instead of silently reverting to legacy.
- Verify the `octo_message_extra_channel_seq.channel_id` collation matches
  `message_extra.channel_id` in production (the migration pins
  `utf8mb4_general_ci`; confirm the live table agrees).

## 3. Activate

```
/tmp/msgextra-version -config configs/tsdd.yaml -action activate -yes
# or pin an explicit floor at/above the recommendation:
/tmp/msgextra-version -config configs/tsdd.yaml -action activate -floor <N> -yes
```

`activate` takes the state row `FOR UPDATE` — the **drain barrier**: it waits for
every in-flight writer (each holds the row `FOR SHARE` until it commits) to finish
under legacy, then flips while no writer can proceed. It recomputes the maxima
under that lock and refuses a floor below them (`ErrFloorTooLow`). It is
idempotent (a second run is a no-op). `epoch` is bumped for observability.

## 4. Verify

- `preflight` shows `mode=transactional` at the new epoch.
- `message_extra.version` is strictly increasing per channel; delta-sync no longer
  skips terminal card frames.
- Metrics: `allocator_mode{mode="transactional"}=1`, `cutover_floor` set,
  `reserve_total{result=retry}` near zero (the card retry loop is only a backstop).

## 5. Rollback to legacy (coordinated, maintenance-window only)

**There is no online `deactivate` action, by design.** Rolling back to legacy
cannot be done safely by a single mode flip: octo-lib's `GenSeq` HiLo allocator
caches its block in **process memory** (`config/seq.go`, `seqMap`). A replica that
cached a legacy block during the pre-activation phase would, the instant mode
flips back, resume issuing versions from that stale low block — below the
transactional high-water clients' delta-sync cursors have already passed — which
is exactly the #627 skip window. A DB change cannot invalidate that in-memory
cache; only a process restart can. So rollback requires a coordinated,
maintenance-window procedure, run in this order:

1. **Drain / pause writes** to `message_extra` (edits, revokes, pins, read
   receipts, bot/card edits) so the transactional high-water stops moving.
2. **Raise the legacy `seq` boundaries above the transactional high-water** so a
   restarted replica's first legacy `GenSeq` block starts above every version
   already issued transactionally:

   ```sql
   SET @maxtx := (SELECT COALESCE(MAX(`last_version`),0) FROM `octo_message_extra_channel_seq`);
   -- bump existing legacy seq rows
   UPDATE `seq` SET `min_seq` = GREATEST(`min_seq`, @maxtx)
     WHERE `key` LIKE 'seq:messageExtra:%';
   -- create legacy seq rows for channels that only ran transactional
   INSERT INTO `seq` (`key`,`min_seq`,`step`)
     SELECT CONCAT('seq:messageExtra:', `channel_id`), @maxtx, 1000
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

- The tool connects only to MySQL (via the config's `DB.MySQLAddr`); it does not
  need Redis or WuKongIM.
- All DB logic lives in `internal/msgextraseq` (`Preflight` / `Activate`) and is
  covered by `activation_test.go` against live MySQL; the command here is a thin
  wrapper.
