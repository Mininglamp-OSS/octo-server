# botevent cutover — #697 activation runbook

The monotonic bot-event id allocator (#697) ships **inert**: the migration seeds
`octo_bot_event_seq_state` with `mode=legacy`, so `NextEventID` delegates to the
existing process-local `GenSeq` and a deploy changes nothing. The
`app cutover botevent` operator command performs the one remaining step —
flipping the state row to `mode=incr` — after which every replica allocates from
the monotonic Redis counter above the validated cutover floor.

This procedure previously lived only in `tools/botevent-seq`'s source comments
and CLI output; the command is now part of the server binary and ships inside
the image:

```
/home/app cutover botevent <preflight|activate|status> [flags]
```

Shared conventions — the state-table shape, the guard-env ordering invariant,
the evidence discipline — are documented in [cutover-framework.md](cutover-framework.md).

## Authority and mirror

The authority is the `octo_bot_event_seq_state` row in **MySQL**, not the Redis
key. The Redis key (`botEventSeq:mode`) is a **mirror** that lets the hot path
check the mode inside the same Lua script as the allocation; its value is
generation-stamped (`incr:{epoch}`) so a hand-written `SET` cannot open the
gate. A lost mirror can eventually be rebuilt from the authority row by an
allocator after its negative belief expires, but publishing the mirror is still
a required cutover step: `activate` fails (and writes must stay paused) if that
write does not complete. Production Redis runs `appendonly no`, so activation
state kept only in Redis would silently regress with an RDB rollback — dropping
every replica back to the legacy allocator, whose lower ids land beneath live
consumer cursors. That is #697 mirrored, caused by its own fix, and it is why
the authority lives in MySQL.

## 1. Preflight (read-only, safe anytime)

```
/home/app cutover botevent preflight -config configs/tsdd.yaml
```

Gathers the floor evidence from **three** sources — the maximum queue score
(`SCAN robotEvent:*`, bounded rank pages plus one atomic `ZREVRANGE 0 0` per
queue), the maximum legacy `seq.min_seq`, and the maximum durable high-water
mark, the latter two swept **table-wide** so a bot whose queue has fully
drained (and therefore has no Redis key) cannot hide a high cursor. It reports
the observed max, per-queue duplicate-score counts, and a recommended floor of
`observed max + 2000` (one reserved block + margin).

- `-sample N` inspects only the first N queues for a quick look. Sampled output
  is **not activation evidence** and `activate` refuses it.
- If any evidence source cannot be read, every total is a lower bound and the
  output says so; `activate` refuses that too.

`app cutover botevent status` is the lightweight variant: the state row, the
mirror value, and the expected-mode guard — no queue scan.

## 2. Preconditions the command cannot check

`activate` prints these on every run; `-yes` confirms them, it does not hide
them:

1. Mininglamp-OSS/octo-server#704 is closed, including an independently
   reviewed cutover floor that cannot inherit a regressed legacy `min_seq` and
   land below a cursor already held by a consumer. Merging or deploying #702 is
   not activation.
2. **Every** replica runs the post-#697 image. Activating while a pre-fix
   replica still allocates from GenSeq puts two id sources on one queue, which
   is the bug (#697), not the fix.
3. Bot-event writes are paused for a few seconds around the flip. This design
   has **no drain barrier** — a `robotEvent` writer is `INCR` + `ZADD` with no
   transaction, so unlike #627 there is nothing to hold a MySQL lock until — so
   a request that resolved legacy before the flip could otherwise publish a low
   id after it. The write pause is the procedural substitute for the barrier.

**Do NOT set `OCTO_BOTEVENT_EXPECTED_MODE=incr` yet.** A replica expecting
`incr` while the row is still `legacy` fails every enqueue closed. The guard is
the last step (§4).

## 3. Activate

```
/home/app cutover botevent activate -config configs/tsdd.yaml -yes
# or pin an explicit floor at/above the recommendation:
/home/app cutover botevent activate -config configs/tsdd.yaml -floor <N> -yes
```

`activate` re-gathers the full evidence, refuses sampled or incomplete
evidence, refuses a floor at or below the observed max (`ErrFloorTooLow` — the
floor must clear everything legacy could still hand out, strictly), refuses a
floor above `2^50` (above `2^53` distinct int64 ids stop having distinct
float64 sorted-set scores, recreating the pagination skip this change removes),
and refuses to flip while the mirror claims an activation the authority has not
granted (a forged/leftover mirror needs a human look first — the command tells
you exactly what to check and never deletes the key itself).

Then, in this order:

1. MySQL `FOR UPDATE` compare-and-set flip (`legacy → incr`, epoch+1). This
   lock serializes concurrent operators; it is **not** a drain barrier.
2. Redis mirror publication (`botEventSeq:mode = incr:{epoch}`). A failure here
   exits non-zero and the message says to **keep bot-event writes paused**: the
   authority is committed but replicas may retain a cached negative belief for
   up to the negative-belief TTL, so an incomplete mirror write is not a
   successful activation. Rerun `activate` (it is idempotent and reconciles the
   mirror) until it exits zero.

## 4. Verify, then arm the guard

**If `activate` reported a failure, re-run it before believing that.** A
connection dropped between the server's commit and the client's ack is
indistinguishable from a commit that never landed, so a flip that happened can
be reported as failed — and in that case the Redis mirror was not published
either. The re-run is idempotent: it finds the authority already activated,
publishes the mirror, and exits zero.

Immediately after a successful activate:

- no `botevent: seed event id counter` errors in logs (a failed seed refuses
  the enqueue rather than issuing an unsafe id);
- rerun `preflight`: the duplicate-score count must **stop growing** (it will
  not drop — existing duplicates are deliberately left alone, because an ack
  deletes every member sharing a score and there is no record of which of a
  pair was delivered);
- bot events still flowing (`POST /v1/bot/events` returning results);
- `dmwork_bot_event_seq_mirror_unauthorized_total` stays 0 (non-zero means some
  replica saw a mode mirror the authority did not confirm).

Only after all of that, roll out on every replica:

```
OCTO_BOTEVENT_EXPECTED_MODE=incr
```

so that a lost mirror **and** a lost authority row fail closed instead of
degrading to GenSeq. The pre-guard window is open by design during the flip
itself; the guard closes it afterwards. As with every cutover guard: never ship
it in the same rolling wave as the flip.

## 5. Rollback

**There is no online deactivate, and unlike #627 there is no coordinated
rollback procedure either — roll forward.** Going back means every
counter-issued id is above what GenSeq would hand out next, so legacy ids would
land below consumer cursors: the same loss, in reverse.

The migration's Down enforces this: with the allocator activated (`mode=1`) it
deliberately violates the table's singleton CHECK constraint, aborting with
MySQL error 3819 instead of dropping the authority
(`modules/robot/sql/20260805000001_bot_event_seq_state.sql`). A Down only
succeeds while the allocator has never been activated.

## Notes

- The command connects to MySQL and Redis via the normal DB config, and prints
  both endpoints it resolved before doing anything. The MySQL line is
  host:port/schema — the DSN's credential is deliberately not echoed.
- `status` reports the expected-mode guard from **the environment of the process
  running the command**, not the fleet's. Confirm the fleet's guard where it is
  set.
- Interrupting is two-stage: the first Ctrl-C / SIGTERM cancels the MySQL work
  (the `seq` sweeps and the flip), and a second terminates the command — an
  in-flight Redis queue scan takes no per-command deadline and runs to
  completion. Interrupting `activate` before the flip commits leaves nothing
  behind; re-run `preflight` before retrying.
- Exit codes: `0` success, `1` a refusal, `130` interrupted by Ctrl-C, `143`
  terminated by SIGTERM — the last usually means the platform reclaimed the pod
  rather than an operator deciding to stop.
- The flip primitives live in `pkg/cutover` (shared with #627); the state row
  and allocator semantics live in `pkg/botevent` (`state.go`, `seq.go`,
  `mode.go`); the evidence gathering and mirror judgement live in
  `cutover_botevent.go` with the operator-command tests
  (`cutover_botevent_test.go` — the `judgeMirror` matrix and the
  mirror-write-failure contract).
