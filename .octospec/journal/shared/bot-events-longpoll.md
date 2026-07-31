---
type: Journal
title: "Journal: bot-events-longpoll (card-message-interaction D5 / P3-2)"
description: Record of the opt-in long poll on POST /v1/bot/events — a per-bot Redis doorbell rung at both enqueue chokepoints, a BLPOP hold on an isolated connection pool, and a `wait` field that defaults to 0 so existing bots are untouched. The sorted set stays the sole authority for what is returned, so a lost bell can only cost latency. No new errcode, i18n entry, endpoint or migration.
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
- **`modules/robot/api.go`** — ring after a successful ZADD in
  `enqueueBotEventGeneric` and `enqueueBotTypedEventGeneric`. Those two helpers
  were already the single chokepoint for the queue write, so covering both is
  sufficient by construction; `card_action` rides the typed one.
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

## Known bounds (deliberate, recorded rather than hidden)

- `register.Module` has no shutdown hook and `main.go` does not gracefully stop
  the API server, so an in-flight hold can extend process drain by at most one
  5s chunk. Adding an uncalled `Stop()` would have been dead code; the chunk
  size is the bound.
- Producers discard `Ring`'s error and do not log it, matching the neighbouring
  best-effort `Expire` refresh. A persistently failing bell degrades every hold
  to "wait out the full timeout" **silently**. Closing that belongs to the card
  ingress observability work (G1), which owns the metric namespace.
- Hold budgets are per process. With N replicas one bot can park a hold on each,
  so the fleet ceiling is `maxEventHolds × N` — bounded and intended, but not a
  distributed invariant.

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

## Review follow-ups applied

- Removed a second copy of the legacy raw `status:0` error response that the
  long-poll branch had introduced; there is now a single error exit. The repo
  lint only scans `AbortWithStatusJSON`, so it would not have caught this.
- Corrected `Ring`'s doc comment, which claimed the error was "returned for
  logging only" while both callers discarded it without logging.
- Scoped the "one hold per bot" comment to the process, so it is not read as a
  cluster-wide guarantee.

## Out of scope

- **openclaw-channel-octo** carries the consumer half (send `wait`, raise the
  client timeout above the hold, drop the idle `intervalMs` in long-poll mode,
  add the `eventWaitSeconds` config knob, abort in-flight holds on stop). Server
  alone changes nothing observable: `wait` is opt-in, so an upgraded server with
  an unchanged plugin behaves exactly as before.
- Real push / WebSocket delivery — the other half of D5, untouched here.
- Metrics (G1), `expires_at` / approval timeout (P3-1).
