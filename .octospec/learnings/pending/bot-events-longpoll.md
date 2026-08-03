---
type: Learning
title: "A duration you promised needs a lower-bound assertion — upper bounds and 'it returned' both pass while it silently under-delivers"
description: When code promises to wait, hold, retry within, or back off for a duration, tests that assert only completion or an upper bound cannot distinguish correct behavior from behavior that delivers a fraction of it. A hold that returns early looks identical to success at every layer — same shape, same status, no error. Assert the lower bound in wall clock, and prefer an integration test, because the arithmetic that truncates the duration usually lives below the constants a unit test can reach.
tags: ["testing", "timing", "integration", "review"]
timestamp: 2026-07-31T04:00:00Z
# --- octospec extension fields ---
source: self
origin_task: bot-events-longpoll
origin_pr: Mininglamp-OSS/octo-server#685
status: pending
candidate_rule: testing
---

# A duration you promised needs a lower-bound assertion

## Context

`POST /v1/bot/events` gained an opt-in long poll: the caller passes `wait`
seconds and the server holds an empty queue open for that long. Because BLPOP's
timeout argument is whole seconds, the hold is served as a loop of chunks.

The first implementation sized each chunk with `remaining.Truncate(time.Second)`
and stopped once fewer than one second was left. Entering the loop with ~1.999s
remaining, that truncates to a 1s block; the ~0.999s left afterwards then fails
the `< 1s` guard and the loop exits. **A `wait=2` request returned in ~1.2s** —
roughly half the hold the caller asked for.

Nothing about the response revealed it. Same `{"status":1,"results":[]}`, same
HTTP status, no error, no log line. An idle hold and a hold cut in half are
byte-identical on the wire; the only difference is the clock.

## What did not catch it

- **Unit tests over the constants.** `eventWaitChunk` was asserted to be a whole
  number of seconds and at least one second. All true, all passing, all
  irrelevant — the defect was in the arithmetic that consumed the constant, not
  the constant.
- **"It returned successfully."** Every functional assertion held: correct shape,
  correct status, empty batch as expected for an idle queue.
- **An upper bound.** The integration test originally asserted
  `elapsed < 8s`, guarding against overrun. A hold that returns in half the time
  passes an upper bound trivially — the assertion was pointed the wrong way.

## What caught it

One wall-clock **lower** bound in an integration test through the real HTTP
route against real Redis:

```go
resp, elapsed := pollEvents(t, s, `{"event_id":0,"limit":20,"wait":2}`)
assert.GreaterOrEqual(t, elapsed, 2*time.Second, "the full requested hold must be served")
assert.Less(t, elapsed, 4*time.Second, "must not overrun by more than one rounded chunk")
```

The fix was to round each chunk **up** to a whole second, accepting sub-second
overshoot: over-serving a hold by <1s is harmless, under-serving it by 50%
defeats the feature. `eventWaitChunkFor` was then extracted so the rounding
itself is unit-testable, with a regression test asserting no remainder can ever
produce a sub-second chunk (which would also mean `BLPOP 0` — block forever).

## The generalization

This is not specific to long polling. The same shape appears wherever code
promises an amount of time:

- retry/backoff windows that silently collapse to one attempt,
- lock or lease TTLs renewed on a truncating interval,
- debounce/throttle windows that fire early,
- graceful-shutdown drains that stop draining before the deadline,
- cache TTLs computed by integer division.

In each case the "too short" failure is invisible to functional assertions,
because returning early *is* the success path with less waiting. And in each
case the truncation tends to live one layer below whatever constant a unit test
can see, so an integration test measuring real elapsed time is what closes it.

## Proposed rule text (for `testing`)

> **Timing promises need lower bounds.** When behavior is specified as a
> duration — wait, hold, retry window, backoff, TTL, drain — a test asserting
> only that the operation completed, or only an upper bound, cannot detect it
> delivering a fraction of that duration. Assert the lower bound against the
> wall clock, and place that assertion in an integration test: duration
> arithmetic is usually below the level a unit test over the constants reaches.
> Where rounding is unavoidable, round so the promise is met or exceeded, and
> say so in the test's failure message.
