---
type: Learning
title: "dbr Load into a *time.Time destination silently yields the zero time — select UNIX_TIMESTAMP into an int64 instead"
description: A scalar timestamp query returned rows=1 with a nil error while leaving the time.Time destination at its zero value, because dbr treats time.Time as a struct and maps columns to fields.
tags: [dbr, mysql, time, testing, false-green, silent-failure]
timestamp: 2026-07-30T11:25:47+00:00
status: pending
---

# `Load(&time.Time)` in dbr silently returns the zero time

## Context

A test helper for `space-join-apply-resubmit` (#683) read an application's
timestamp:

```go
var createdAt time.Time
_, err := ctx.DB().SelectBySql("SELECT created_at FROM space_join_apply WHERE id=?", id).Load(&createdAt)
```

It reports success — `rows=1`, `err=nil` — and leaves `createdAt` at
`0001-01-01 00:00:00 UTC`. dbr inspects the destination, sees a struct, and runs
its column→field mapper; `time.Time` has no exported fields to map onto, so
nothing is assigned and nothing complains. Verified side by side against the
same row:

```
time.Time dest   -> rows=1 err=<nil> value="0001-01-01 00:00:00 +0000 UTC" zero=true
int64 epoch dest -> rows=1 err=<nil> value=1785412397
```

(Loading a timestamp into a **struct field** of a row model is fine — this is
specifically about a bare scalar destination.)

## Why it is worse than an ordinary bug

Zero values compare equal to each other. In the test suite this produced a
**mixed** result that pointed at the wrong culprit: two `After()` assertions
failed against a *correct* implementation, while an equality assertion between
two reads passed vacuously. An assertion of the form "these two timestamps are
the same" is permanently green under this bug — which is precisely the assertion
written to catch "the timestamp was not refreshed".

## Learning

For a bare scalar timestamp, select `UNIX_TIMESTAMP(col)` into an `int64`. It
avoids the struct-mapping trap, and independently avoids the session-timezone vs
`NOW()`-written-column offset covered by
`app-time-vs-now-column-epoch-compare`.

Assert the value is non-zero at the point of reading. A helper that can silently
return a zero clock value should refuse to, rather than hand it to a comparison
that will quietly interpret it as a legitimate timestamp.

## Applies to

Any `Load` / `LoadOne` with a non-struct-row destination in this repo — scalar
`time.Time` reads in tests and in helper queries alike.
