---
type: Learning
title: "A setting the client clears locally is already stale on the server — gate it, don't just read it"
description: When a client double-writes a preference (local flag + server column) but clears only the local copy at end-of-life, the server column silently becomes permanently-on. Wiring the unread column into a new consumer then converts a feature that does nothing into a feature that never stops — a worse bug. The fix is a server-side liveness gate on the precondition the column's own contract already names, not a client write-back.
tags: ["user-setting", "push", "client-server-drift", "fail-open", "wire-contract"]
timestamp: 2026-09-11T00:00:00Z
# --- octospec extension fields ---
source: self
origin_task: ios-apns-mute-of-app
status: pending
candidate_rule: null
---

# A setting the client clears locally is already stale on the server

## Context

「手机静音」 is written by the iOS client to two places at once: a local
`NSUserDefaults` flag, and `user.mute_of_app` via `PUT /v1/user/my/setting`.
The offline-push path never read the column — `user.Resp` did not even carry the
field — so the setting did nothing once the socket dropped and delivery moved to
APNs.

The obvious fix is to plumb the column into the push payload. That fix is wrong
on its own, and the reason is only visible in the client:

```objc
// WKOnlineStatusManager.m — PC/Web went offline
weakSelf.muteOfApp = false;
[[WKMySettingManager shared] saveMuteOfAppLocally:NO];   // local only
                                                         // no PUT back
```

The client clears its own copy and never tells the server. So every user who has
ever toggled the switch carries `mute_of_app = 1` in the database **forever**.
Reading it unconditionally would have turned "mute does not work" into "push is
permanently silent after the user closes Web" — a bug that is worse, reported
less often (silence generates no notification to complain about), and much
harder to trace back to a setting the user flipped weeks ago.

## The transferable shape

A preference that is double-written but **single-cleared** is not a preference,
it is a high-water mark. The server copy records that the setting was *ever*
enabled, not that it is *currently* enabled.

This is invisible from the server code alone. Nothing in the column, the write
endpoint, or the schema says the value decays — the only evidence lives in a
client lifecycle callback in a different repository. The column comment here did
carry the precondition (当pc登录后有效), and reading it as documentation of an
**invariant the server must enforce**, rather than a note about when the client
happens to write it, is the whole difference.

## What to do instead

1. **Gate on the precondition at read time, server-side.** Here: the mute
   applies only while that user actually has a live PC/Web session
   (`DeviceOnline`). The stale `1` becomes harmless the moment the session ends
   — no migration, no cleanup job, no client cooperation.
2. **Do not ask the client to write back.** It cannot be relied on: at the
   moment the precondition lapses the app may be killed, backgrounded, or
   offline. A write-back is a best-effort optimization at most, never the
   correctness mechanism.
3. **Choose the fail-open direction by which failure is louder.** If the
   liveness lookup fails, treat the setting as *off*. A missed notification is
   silent and gets reported late, if ever; an unwanted sound is immediately
   visible and self-corrects on the next message.
4. **Pin the stale case in a test by name.** Not "muted user gets no sound", but
   "muted user with no live session still gets sound" — the assertion that would
   have caught the worse bug the naive fix introduces.

## Smell to look for

When adding the first real consumer of a column that has been written but never
read, treat its current stored values as untrusted history rather than state.
Ask what clears it, and find that answer in the writer's code — not in the
schema. If the only thing that clears it is a client-side branch, there is no
clearing at all.
