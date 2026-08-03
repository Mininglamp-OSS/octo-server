---
type: Learning
title: "A cross-key system_setting guard must resolve empty payloads as reset-to-default, not keep-current"
description: settingTypeInt documents "" as reset-to-default and normaliseBool accepts "" too, so a prospective-merge validator that carries the current value forward for "" silently under-detects violations introduced by clearing a key.
tags: [system-setting, validation, correctness, error-response]
timestamp: 2026-07-29T13:58:15Z
status: pending
---

# Empty system_setting payloads are "reset", and merge guards must model that

## Context

`POST /v1/manager/common/system_setting` validates cross-key invariants by
merging the incoming batch onto the current snapshot and validating the result
— the pattern established by the onboarding space-welcome composite check and
reused by the thread archive-window ordering guard
(`ApplyThreadArchiveOrderingOverlay` + `ViolatesThreadArchiveOrdering`).

The merge has a non-obvious case. For `settingTypeInt` the write path documents:

```go
// Empty value means "reset to default" (the getter treats an empty
// snapshot entry as not-configured), so only validate non-empty payloads.
```

`normaliseBool("")` likewise returns `("", true)` — accepted, and the getter
then falls back through env → code default.

So `""` is **not** "leave this key alone". It is a write that changes the
effective value.

## The trap

A merge that skips `""` (or treats it as the zero value) validates a state that
will never exist after the commit:

- current `thread.auto_archive_days = 30`, `sidebar.recent_filter_thread_days = 14`
  — valid, `30 >= 14`.
- admin POSTs `auto_archive_days = ""` → resets to the code default `3`.
- a merge that keeps `30` sees `30 >= 14` and accepts. The state that actually
  lands is `3 < 14`, which is precisely what the guard exists to reject.

The boolean form is worse because it short-circuits the whole check: `"" == "1"`
is `false`, so the guard reads "archiving disabled" and skips validation even
when env resolves it back to enabled.

Neither case fails any test that only exercises explicit values. This was found
by reading the merge against the write path, with the suite green.

## Rule

When building a prospective-merge validator over `system_setting` keys, resolve
an incoming `""` through **the same fallback chain the getter will use after the
write** (DB → env → code default), rather than carrying the current value
forward or parsing it as a zero value.

Keys absent from the batch keep their current value. Keys present with `""` do
not.

## Test shape

Cover, per key that participates in the invariant:

- absent → keeps current
- explicit value → wins
- `""` → resolves to env when an env layer exists, else code default
- `""` landing in a violating state → guard rejects (end-to-end through the
  manager endpoint, not just the pure function)

See `modules/common/thread_archive_setting_test.go`
(`TestApplyOverlay_*`, `TestManagerSystemSetting_OrderingRejectsResetToDefaultViolation`).
