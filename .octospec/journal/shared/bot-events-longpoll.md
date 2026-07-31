---
type: Journal
title: "Journal: bot-events-longpoll (card-message-interaction D5 / P3-2)"
description: Record of the opt-in long poll on POST /v1/bot/events — a per-bot Redis doorbell rung by every one of the five queue writers (a source guard enforces it, after review found only two wired), a BLPOP hold on an isolated connection pool, and a `wait` field that defaults to 0 so existing bots are untouched. The sorted set stays the sole authority for what is returned, so a lost bell can only cost latency. No new errcode, i18n entry, endpoint or migration.
tags: ["bot-api", "wire-contract", "rate-limit", "redis", "testing", "card"]
timestamp: 2026-07-31T04:00:00Z
# --- octospec extension fields ---
task: bot-events-longpoll
upstream: card-message-interaction D5 (P3-2)
source: self
---

# Journal: bot-events-longpoll (card-message-interaction D5 / P3-2)

## What was done

Bot event delivery was cursor short polling: one `ZRangeByScore` per request,
returning immediately. Card interaction latency therefore equalled the bot's
poll cadence — the open item D5 named explicitly rather than leaving implied.

- **`pkg/botevent` (new leaf package)** — the doorbell key plus `Ring`
  (LPUSH → LTRIM 0 0 → EXPIRE). A leaf package because `modules/bot_api` already
  imports `modules/robot`; putting the key format in either module would mean an
  import cycle or a second copy free to drift.
- **Every queue writer rings** — five sites, not the two the first revision
  assumed: `enqueueBotEventGeneric`, `enqueueBotTypedEventGeneric`
  (`card_action`), `saveRobotMessage` (ordinary DM / @-mention, the
  highest-volume producer), and both `notifyBotJoinedGroup` variants in
  `modules/group`. The first revision trusted a docstring claiming
  `enqueueBotEventGeneric` was the shared chokepoint; it never was. Review
  caught it, and the invariant is now held by a source guard rather than a
  comment — see the Gotchas section.
- **`modules/bot_api/events.go`** — optional `wait` on `BotEventsReq`,
  read-before-wait, and the App Bot DM-only filter extracted into
  `filterAppBotEvents` so the immediate and long-poll paths share one copy.
- **`modules/bot_api/events_wait.go` (new)** — the hold loop, a dedicated Redis
  client, and the concurrency budget.

## Why the design landed where it did

- **The doorbell is a hint, never the event.** Every wake-up re-reads the
  authoritative sorted set from the caller's cursor. A bell that was lost,
  stolen by a waiter on another replica, or left over from an already-consumed
  event costs at most one wasted wake-up. This is what makes a best-effort
  Redis-side notification acceptable in a delivery path at all.
- **`wait` defaults to 0.** Not caution for its own sake: the OpenClaw channel
  plugin caps every `/v1/bot/events` request at a hard 10s client timeout
  (`src/api-fetch.ts`), so a hold enabled by default would have made existing
  bots abort and log on every poll, while *tripling* idle request volume. The
  cross-repo constraint decided the default, and it was only visible by reading
  the consumer.
- **An isolated Redis pool.** BLPOP occupies its connection for the whole block.
  The shared pool from `ctx.GetRedisConn()` is built with no explicit `PoolSize`
  (go-redis default `10*NumCPU`) and serves every other call in the process, so
  parking holds there would let a few dozen long-polling bots starve ordinary
  traffic. A dedicated instrumented client sized to the hold cap keeps the worst
  case structurally confined.
- **Not the in-process channel map.** `modules/robot`'s `inlineQuery` long poll
  already exists, but it is single-replica-correct only; this queue is consumed
  by whichever replica the bot reaches. Blocking had to happen inside Redis.

- **The hold loop's progress is an invariant, not an accident.** Every iteration
  either burns a chunk of wall clock or advances the queue cursor, never neither.
  That sentence is the whole design of `waitForEvents`, and it took three review
  rounds to arrive at because each round answered one branch of it in isolation.
  See "How this loop terminates" below.

## Gotchas worth remembering

- **`BLPOP 0` means block forever, and go-redis truncates toward zero.**
  `formatSec` is `int64(dur / time.Second)`, so any sub-second timeout becomes an
  unbounded block. Chunks must round **up**.
- **Rounding down silently halves the hold.** The first implementation used
  `remaining.Truncate(time.Second)`. Entering the loop with ~1.999s left
  truncated to 1s, and the 0.999s remainder then failed the `< 1s` guard, so a
  `wait=2` request returned in ~1.2s — half of what the caller asked for, while
  looking entirely successful. Only a wall-clock **lower-bound** assertion in an
  integration test caught it; every unit test still passed.
- **go-redis does not let the pool read timeout truncate a blocking command.**
  `BLPop` calls `cmd.setReadTimeout(timeout)` and the pool uses `t + 10s`, so the
  default `ReadTimeout` is not a constraint on hold length. Worth knowing before
  reaching for a custom timeout.
- **Reusing one MySQL database across package test binaries fails legitimately.**
  `bot_api` and `robot` carry different migration sets; sharing `test` produces
  "unknown migration in database". Reset between packages.

## How this loop terminates (and why it needed four rounds)

The endpoint's stated purpose is to stop bot delivery latency tracking the poll
cadence. Its stated *risk* is that a hold, unlike a short poll, keeps running
while things go wrong — so every branch has to cost something. Four review rounds
found four branches where it did not, all in the same function:

| Round | Branch that made no progress | What it cost |
|---|---|---|
| 1 | three of five producers never rang | ordinary messages waited a chunk |
| 2 | a refused hold answered instantly | instant re-request, load amplification at capacity |
| 3 | a failing BLPOP retried unpaced | **924 reads in 8s**, one log line each |
| 3 | a full page that advanced nothing skipped the block | **38,722 reads in 6s**, completely unpaced |

Both numbers are measured, by deleting the fix and re-running the regression
test: `TestWaitForEventsPacesAFailingDoorbell` and
`TestWaitForEventsDoesNotSpinOnAPageItCannotAdvancePast` count Redis's own
`INFO commandstats` `zrangebyscore` calls, so the assertion is about what the
server was asked to do rather than what the test believes the code did.

The fix was to stop answering branches and state the invariant instead:

> **Every iteration either burns at least one chunk of wall clock, or advances
> the queue cursor. Never neither.**

- BLPOP timed out (`redis.Nil`) → the chunk was spent.
- BLPOP returned a token → a producer rang, so a ZADD landed, so the read
  advances.
- BLPOP **failed** → go-redis returns dial/protocol errors after
  `MinRetryBackoff` (~8ms), *not* after the chunk, because a read timeout is the
  one error class excluded here. So the error path pays out the chunk remainder
  explicitly, and logs once per hold rather than once per iteration.
- The block was skipped → only permitted when the previous read actually moved
  the cursor.

The cursor is monotonic and bounded by the queue's maximum event id; the chunk
count is bounded by the deadline. Nothing in that argument depends on a write
succeeding.

**The cursor comes from the read, never from the ACK.** `readEventPage` advances
to the highest `event_id` it observed *before* the App Bot filter runs. The
earlier revision reasoned "the filtered events were auto-ACK'd out of the queue,
so we cannot see them again" — but `filterAppBotEvents` only *warns* when its
`ZRemRangeByScore` fails, and "reads work, writes do not" is an ordinary Redis
state (MISCONF after a failed RDB save, a READONLY replica, a write-denying ACL).
Deriving progress from the read makes it structural. It also covers members that
fail to decode, which the ACK never touched at all: without it, a corrupt queue
head starves every event behind it — the read keeps returning the same `limit`
bad members forever.

`readEventPage` is now the single seam both paths go through, so the immediate
read and the hold cannot drift in what they read, what they filter, or how far
they advance. Threading the entry page into `waitForEvents` is what closes the
first iteration too: a backlog that was already in the queue when the request
arrived will never get a *new* bell, so blocking for one first is pure latency.

## Known bounds (deliberate, recorded rather than hidden)

- `register.Module` has no shutdown hook and `main.go` does not gracefully stop
  the API server, so an in-flight hold can extend process drain by at most one
  5s chunk. Adding an uncalled `Stop()` would have been dead code; the chunk
  size is the bound.
- A persistently failing bell degrades every hold to chunk-paced authoritative
  re-reads — ≤5s of extra latency, not "wait out the full timeout" as an earlier
  version of this file claimed. Chunking *is* the fallback poll. Producers now
  log a Warn when `Ring` fails, which matters because the ring moved onto its own
  pool: a ZADD can succeed on a warm shared connection while the ring pool cannot
  get one at all. A counter still belongs to G1, which owns the namespace.
- Hold budgets are per process. With N replicas one bot can park a hold on each,
  so the fleet ceiling is `maxEventHolds × N` — bounded and intended, but not a
  distributed invariant.
- Three Redis pools per replica now: shared, wait (`maxEventHolds + 4` = 68),
  ring (10). 78 connections before the shared pool, `× N` replicas — the number
  to check against `maxclients`, and the one the brief originally omitted.
  `docs/bot-events-longpoll.md` carries it for operators.
- A page in which *every* member fails to decode cannot advance the cursor, so
  that request is starved for its remaining hold. Bounded and non-spinning (the
  loop falls back to blocking), and every skipped member logs. Advancing past it
  would need the ZSet **score** rather than the payload's `event_id`; octo-lib's
  `redis.Conn` exposes no `ZRangeByScoreWithScores`, so that is a separate change.
- The refused-hold pause has a budget of `maxEventHolds × 4`, chosen rather than
  measured: enough that a bot fleet briefly running two pollers during a rolling
  restart is still paced, bounded so the refusal path cannot out-consume the
  success path. Past it the answer is the instant empty batch — the same thing
  every caller that omits `wait` already gets.

## Verification

Integration tests were run against real MySQL + Redis + WuKongIM, not skipped.
`go test -race` green on `modules/bot_api`, `modules/robot`, `pkg/botevent` and
`pkg/redis` (including the `TestNoRawRedisClientOutsideChokepoint` source
guard); `golangci-lint` 0 issues; `make i18n-extract-check` + `make i18n-lint`
unchanged, as expected for a change that adds no code.

Six long-poll cases run through the real HTTP route: `wait` absent is
field-for-field unchanged; read-before-wait; a full hold expiring into an empty
batch; doorbell wake-up under 2s; a deliberately dropped doorbell still not
losing the event; and a disconnected client releasing its slot within one chunk.
A real `robot.NewService` call asserts the producer chokepoint actually rings.

Cross-repo, the plugin's own `fetchBotEvents` was driven against a locally built
server from this branch: no `wait` returned in 105ms, `wait=3` in 3017ms with an
ordinary empty batch, and an event injected 1.5s into a 20s hold returned at
1551ms — about 50ms after the event landed, 18s before the deadline.

Multi-replica behaviour was checked with two real replicas contending for one
doorbell token: A returned at 2014ms, B at 5031ms (its chunk boundary), and
**both received the event** — the chunked fallback poll doing exactly what the
safety argument claims.

Round 4 added five cases and verified each by deleting its fix:

| Test | Deleting the fix produces |
|---|---|
| `TestWaitForEventsPacesAFailingDoorbell` | 924 `zrangebyscore` in 8s |
| `TestWaitForEventsDoesNotSpinOnAPageItCannotAdvancePast` | 38,722 in 6s |
| `TestWaitForEventsDrainsAFilteredBacklogWithoutBlocking` | the DM waits a full chunk |
| `TestRingLeavesExactlyOneTokenHoweverOftenItRings` | `LTRIM 0 1` → 2 tokens |
| `TestRingExpiresTheBellKeyItPushedTo` | EXPIRE on another key → no TTL on the bell |

A mutation that a test does not catch is not covered, so each was run rather
than argued.

One pre-existing environment trap worth recording: creating the test database
with MySQL 8's default collation (`utf8mb4_0900_ai_ci`) makes
`20260308000002_space_legacy01.sql` fail with "Illegal mix of collations", and
the panic looks like a code failure. Create it `collate utf8mb4_general_ci`.

## Review follow-ups applied

Round 2:

- Removed a second copy of the legacy raw `status:0` error response that the
  long-poll branch had introduced; there is now a single error exit. The repo
  lint only scans `AbortWithStatusJSON`, so it would not have caught this.
- Corrected `Ring`'s doc comment, which claimed the error was "returned for
  logging only" while both callers discarded it without logging.
- Scoped the "one hold per bot" comment to the process, so it is not read as a
  cluster-wide guarantee.

Round 3:

- A refused hold pauses for up to one chunk instead of answering instantly.
- `botevent.Ring` became one Lua call through its own small pool, measured
  256µs → 69.6µs per ring.
- `maxAllowedEventHolds` clamps the operator override, which previously could
  ask go-redis for a million-connection pool.

Round 4 (this one — see "How this loop terminates" for the reasoning):

- The BLPOP error path pays out its chunk and logs once per hold.
- `readEventPage` gives both paths one seam, and the cursor advances from the
  read rather than from the auto-ACK.
- The entry read's page is threaded into the hold, so the first iteration drains
  a pre-existing backlog instead of blocking on a bell that will never ring.
- The refused-hold pause got a budget of its own, so back-pressure cannot become
  the resource sink.
- `Ring` failures are logged at all five producers; `rd.NewScript` replaces raw
  `EVAL`; `OCTO_BOT_EVENTS_MAX_HOLDS` is validated at boot and documented in
  `docs/bot-events-longpoll.md`.
- `pkg/botevent`'s unit test went back to being behavioural: the fake now parses
  whatever `redis.call` lines the script contains and applies real Redis
  semantics, so `LTRIM 0 1` or an EXPIRE on the wrong key fails it. Both
  mutations were run to confirm. The previous revision had traded that for
  `strings.Contains` on the script text, which catches neither.

## Out of scope

- **openclaw-channel-octo** carries the consumer half (send `wait`, raise the
  client timeout above the hold, drop the idle `intervalMs` in long-poll mode,
  add the `eventWaitSeconds` config knob, abort in-flight holds on stop). Server
  alone changes nothing observable: `wait` is opt-in, so an upgraded server with
  an unchanged plugin behaves exactly as before.
- Real push / WebSocket delivery — the other half of D5, untouched here.
- Metrics (G1), `expires_at` / approval timeout (P3-1).
