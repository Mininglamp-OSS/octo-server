---
type: Learning
title: Synchronize asynchronous cleanup tests on completion evidence
description: Claim-time counters do not prove callback or cascade completion
tags: [testing, concurrency, async, cleanup]
timestamp: 2026-09-08T09:34:00+08:00
task: my-ai-team-sessions
source: self
---

# Synchronize asynchronous cleanup tests on completion evidence

For asynchronous job tests, do not treat a claim-time attempt counter as proof
that callbacks or downstream effects completed. Wait on evidence written after
the complete execution phase, and use atomics or channels for any callback state
observed across goroutines. Run the regression with both `-race` and shuffled
test order so the synchronization contract is executable.
