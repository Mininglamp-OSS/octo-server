---
type: Learning
title: "A loop that waits needs a stated progress invariant, not a fix per branch — four review rounds each closed one branch and opened the next"
description: When a request handler loops while waiting for something, its correctness is not a property of any one branch but of all of them at once — every iteration must either consume time or make progress. Answering review findings branch by branch produces a loop where each fix is locally right and the whole is still wrong, because the fixes are not checked against each other. State the invariant, enumerate the exits, and test the enumeration. Progress must also be derived from reads, never from writes whose errors are only logged.
tags: ["testing", "review", "redis", "concurrency", "long-poll"]
timestamp: 2026-07-31T08:00:00Z
# --- octospec extension fields ---
source: self
origin_task: bot-events-longpoll
origin_pr: Mininglamp-OSS/octo-server#685
status: pending
candidate_rule: testing
---

# A loop that waits needs a stated progress invariant

## Context

`POST /v1/bot/events` gained an opt-in long poll: the caller passes `wait`
seconds, and the handler loops — block on a Redis doorbell for a chunk, re-read
the authoritative sorted set, return when something is visible — until the
deadline.

Four review rounds each found a different branch of that loop where an iteration
cost nothing, and each round's fix was locally correct:

| Round | Branch that made no progress | Measured cost |
|---|---|---|
| 1 | three of five producers never rang the doorbell | ordinary messages waited a chunk |
| 2 | a refused hold answered instantly | instant re-request → load amplification exactly at capacity |
| 3 | a failing BLPOP retried with no pacing | **924 reads in 8s**, one log line each |
| 3 | a full page that advanced nothing skipped the block | **38,722 reads in 6s**, completely unpaced |

Round 3's two findings were *introduced by round 2's fix and round 3's own fix*.
The reviewer's process note named it exactly: "each round's fix has introduced a
fresh defect in the same function … that is the shape of a diff being patched
finding-by-finding rather than reasoned through once."

## Why branch-by-branch fixing fails here

Each finding arrives as "this branch returns too early" or "this branch retries
too fast", and each has an obvious local fix. What the local fix cannot see is
that the branches share one budget: the request's wall clock. A loop is safe only
if *every* path through it costs something, so a fix that makes one path cost
time while another still costs nothing has not improved the worst case — it has
moved it.

Round 2 is the clearest example. "A refused hold returns instantly, which invites
an instant re-request" was fixed by pausing. Correct. But the same review round
also asked BLPOP failures not to collapse the hold, and the obvious fix — fall
through and retry — created a *tighter* loop than the one just removed, on the
sibling branch, justified by a comment asserting a pacing property that could not
exist. The comment was written from intent, not from go-redis's behaviour: a
failing BLPOP returns after `MinRetryBackoff` (~8ms), not after the chunk,
because the chunk is only consumed when the block *succeeds* in timing out.

## What actually closed it

Writing the invariant down first, then checking every exit against it:

> **Every iteration either burns at least one chunk of wall clock, or advances
> the queue cursor. Never neither.**

- Block timed out → the chunk was spent.
- Block returned a token → a producer wrote, so the read that follows advances.
- Block **failed** → the error path pays out the chunk remainder explicitly,
  because nothing else will.
- Block was skipped → permitted only when the previous read actually advanced.

The cursor is monotonic and bounded by the queue's maximum id; the chunk count is
bounded by the deadline. Termination follows from the invariant rather than from
inspecting branches.

**And progress must come from a read, not a write.** The broken version reasoned
"the filtered events were removed from the queue, so we cannot be handed them
again" — but that removal only *warned* on failure, and "reads work, writes do
not" is an ordinary Redis state (MISCONF after a failed snapshot, a READONLY
replica, a write-denying ACL). Deriving the cursor from what the read observed
makes progress structural. It also covered a case nobody had raised: members that
fail to decode were never removed at all, so a corrupt queue head starved every
event behind it forever.

## How to test an invariant like this

Assert the *resource*, not the timing. Both regression tests read Redis's own
`INFO commandstats` and bound the `zrangebyscore` delta across one request:

```go
before := zrangeByScoreCalls(t, ctx)
got, err := ba.waitForEvents(...)          // 8s hold, doorbell pointed at a closed port
reads := zrangeByScoreCalls(t, ctx) - before
assert.LessOrEqual(t, reads, int64(wait/eventWaitChunk)+3,
    "a failing doorbell issued %d authoritative reads in %v", reads, elapsed)
```

That assertion is about what the server was asked to do, not what the test
believes the code did — and it holds regardless of how the pacing is implemented.
CPU time or a timer count would have been proxies; the command counter is the
thing itself. Both tests were verified by deleting the fix and watching the
count explode, which is where the 924 and 38,722 figures come from.

A wall-clock assertion alone would not have caught either defect: both spinning
versions still returned at the right time. They were correct in every respect
except the load they generated getting there.

## The generalization

Any loop that waits on an external signal has this shape: long polls, leader
election, queue drains, reconciliation loops, retry-until-deadline helpers,
watch-and-reconnect loops. In each, an error branch that "just retries" is a spin
unless something in it costs time, and library error paths return far faster than
the timeout the happy path uses.

## Proposed rule text (for `testing`)

> **Loops that wait need a stated progress invariant, and a test that bounds
> their work.** Before fixing a review finding in a waiting loop, write down what
> every iteration must cost — time, or progress — and check every exit against
> it, including the ones the finding did not mention. A fix that makes one branch
> cost time while a sibling branch still costs nothing has moved the worst case,
> not removed it.
>
> Derive progress from reads, never from writes whose errors are only logged:
> "reads succeed, writes fail" is an ordinary state for every datastore, and a
> loop whose advancement depends on a best-effort write will spin in it.
>
> Test the bound on work, not just the elapsed time — a spinning loop returns on
> schedule. Count what the dependency was actually asked to do (`INFO
> commandstats`, query counters, a request-counting fake) and assert an upper
> bound, then verify the test by deleting the fix.
