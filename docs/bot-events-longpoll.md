# Bot events long-poll (`POST /v1/bot/events`)

`POST /v1/bot/events` reads a bot's event queue (`robotEvent:{robotID}`, a Redis
sorted set scored by `event_id`) from the caller's cursor. It accepts an optional
`wait` field that turns the read into a long poll.

Related: [card-protocol.md](./card-protocol.md),
[card-action-callback-dispatch.md](./card-action-callback-dispatch.md).

## Wire contract

```json
{ "event_id": 0, "limit": 20, "wait": 25 }
```

| Field | Default | Behaviour |
|---|---|---|
| `event_id` | `0` | Exclusive cursor. The read returns events with a strictly greater id. |
| `limit` | `20` | Clamped to `[1, 100]`. |
| `wait` | `0` | Hold, in seconds. Clamped to `[0, 30]`. Out-of-range values are clamped, never rejected. |

`wait` is **opt-in and defaults to 0**. A request that omits it behaves exactly
as it did before long-poll existed: one read, immediate response, empty batch
included. That default is load-bearing rather than cautious — the OpenClaw
channel plugin historically capped every request at a hard 10s client timeout,
so a server that held by default would make existing bots abort and log on every
poll.

The response shape never changes. An expired hold returns the ordinary
`{"status":1,"results":[]}` — a timeout is "nothing happened this round", not an
error, so there is no 408 and no error envelope.

The server always **reads before waiting**: a caller with a backlog is answered
immediately, whatever `wait` it sent.

## How the wake-up works

Every producer that ZADDs into `robotEvent:{robotID}` also rings a per-bot
doorbell (`robotEventBell:{robotID}`) — one `EVALSHA` doing
`LPUSH` + `LTRIM 0 0` + `EXPIRE`. The waiter blocks on `BLPOP`, so the blocking
happens inside Redis and works across replicas.

The doorbell is a **hint, never the event**. Every wake-up re-reads the
authoritative sorted set, so a bell that was lost, stolen by a waiter on another
replica, or left over from an already-consumed event costs at most one wasted
wake-up. The hold is also chunked (5s), which gives a bounded fallback poll: if
the bell never rings at all, the event still surfaces within one chunk rather
than at the hold deadline.

`pkg/botevent`'s `TestEveryBotEventQueueWriterRingsTheDoorbell` is a source guard
that fails the build if a new queue writer forgets to ring.

## Operational knobs

| Env | Default | Effect |
|---|---|---|
| `OCTO_BOT_EVENTS_MAX_HOLDS` | `64` | Concurrent long-poll holds **per process**. Clamped to `[1, 4096]`; a non-positive or unparseable value falls back to the default and logs a Warn at startup. |

The value is resolved and validated once at boot (`NewBotAPI`), not on the first
long-poll request, so a typo shows up in the startup log rather than in traffic.
It sizes two things that must agree: the hold semaphore and the dedicated Redis
pool.

### Connection budget

Each replica opens **three** Redis pools:

| Pool | Size | Why |
|---|---|---|
| shared (`ctx.GetRedisConn()`) | go-redis default `10 × NumCPU` | everything else in the process |
| wait (`modules/bot_api`) | `OCTO_BOT_EVENTS_MAX_HOLDS + 4` (default **68**) | `BLPOP` pins a connection for the whole hold, so holds must not park on the shared pool |
| ring (`pkg/botevent`) | **10** | a ring is one `EVALSHA` and must never queue behind a parked hold |

At the default cap that is **78 connections per replica** before the shared pool.
Check this against Redis `maxclients` at the planned replica count — a hold cap
of 4096 would ask for a 4100-connection wait pool on a single replica.

The budgets are per process. With N replicas one bot can park a hold on each, so
the fleet ceiling is `maxEventHolds × N`. That is bounded and intended; it is not
a distributed invariant.

### Degradation

Refusing to hold is never an error. Three degradations, all fail-open:

- **Hold refused** (at capacity, or this bot is already parked on this replica):
  the request is answered with an empty batch after a bounded pause of up to one
  chunk, so a refusal cannot be answered with an instant re-request. That pause
  is itself budgeted (4× the hold cap); past it, the answer is instant.
- **Doorbell failing**: the request does not fail — the sorted set is the
  authority. The hold falls back to chunk-paced authoritative re-reads and logs
  once per hold.
- **Doorbell never rung** (a producer's ring failed): same fallback, so the
  worst case is one chunk of extra latency. Ring failures are logged at Warn by
  each producer.

## Deployment notes

- The 30s cap sits under the 60s idle timeout common to reverse proxies. A hold
  may overshoot its deadline by **less than one second** (the BLPOP timeout is
  whole seconds and must be rounded up — rounding down degenerates into a 0s
  argument, which Redis reads as "block forever"). If your proxy's idle timeout
  is shorter, lower the cap; there is no contract impact.
- `WKHttp.Run` uses a zero-value `http.Server`, so there is no server-side
  `ReadTimeout`/`WriteTimeout` backstop. The handler carries its own deadline.
- There is no graceful-shutdown hook, so an in-flight hold can extend process
  drain by at most one chunk (5s), not by a full 30s hold.
