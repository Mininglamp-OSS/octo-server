---
type: Journal
title: "Journal: ios-apns-mute-of-app"
description: The iOS APNs offline-push payload now honors 「手机静音」 (user.mute_of_app) by omitting aps.sound entirely, gated on a live PC/Web session so a stale DB value cannot mute a user forever. Server-only; octo-web does not participate and octo-ios needs no change.
tags: ["webhook", "push", "apns", "ios", "user-setting", "wire-contract", "notification"]
timestamp: 2026-09-11T00:00:00Z
# --- octospec extension fields ---
task: ios-apns-mute-of-app
upstream: self
source: user
---
# Journal: ios-apns-mute-of-app

## What was done

With 「手机静音」 on, offline messages reaching iOS through APNs used to ring
anyway. Two paths diverged: the **foreground** path (socket up, local
notification) reads the flag and sets `sound = nil`
(`WKLocalNotificationManager.m`), while the **background/terminated** path
(APNs) hardcoded `"sound": "default"`.

The fix is server-side only:

- `modules/user/service.go` — `user.Resp` gains `MuteOfApp`, mapped in
  `newResp`. Before this, the push pipeline was *structurally* unable to read
  the setting: `toUser.MuteOfApp` did not compile.
- `modules/webhook/app_mute.go` (new) — `resolveEffectiveAppMute` decides, per
  recipient, whether the mute is **currently in effect**, plus the `silenceable`
  optional interface.
- `modules/webhook/api.go` — the resolution runs in the batch stage next to the
  account-level pause lookup; the per-recipient flag rides the PushPool job and
  reaches `push`, which applies it only to payloads implementing `silenceable`.
- `modules/webhook/push_iosapns.go` — `IOSPayload.Silence()`; when silent, the
  `sound` key is **omitted from `aps` entirely**.

## Load-bearing decisions

**The mute must be gated on a live PC/Web session, and that gate is the
actual fix — not a nicety.** `user.mute_of_app` and the iOS local flag are two
independent states that reliably drift: `WKOnlineStatusManager.m` clears only
the local flag when PC/Web goes offline and never writes back, so the column
keeps `1` indefinitely. Reading the column unconditionally would have converted
"mute does not work" into "push is permanently silent after the user closes
Web" — strictly worse, and far harder to diagnose. The column's own contract
already said 当pc登录后有效. Waiting for a client write-back is not an option
either: at the moment Web disconnects the app may be killed or backgrounded, so
the check has to be server-side at push time
(`IOnlineService.DeviceOnline`, same order as `api_online.go`: PC, then Web).

**Omit `sound`, never set it to `""`.** Some iOS versions treat an empty string
as "sound file not found" and fall back to the default tone — which would look
exactly like the bug being fixed. A mutation test pins this: writing `""`
instead of omitting the key turns two tests red.

**`silenceable` as an optional interface, rather than widening `GetPayload`.**
Only `IOSPayload` implements it; the other five vendor payloads are untouched
and unchanged on the wire. Adding a vendor later means implementing `Silence()`
on its payload — no signature churn across six implementations, and no chance
of silently altering a vendor whose behavior was not reviewed here.

**Fail-open to audible.** An online-lookup error yields an audible push and a
logged error. A notification the user never hears is a lost message; this
mirrors `filterPausedUIDs`.

**No N+1.** The online lookup runs only for recipients whose `mute_of_app` is
already `1`, resolved before PushPool fan-out. A test pins zero lookups for an
unmuted batch.

**RTC pierces mute.** Call pushes keep `sound: "default"`, consistent with
`allowPush` exempting `isVideoCall`.

## Deviation from the brief

The brief sketched the flag travelling through `PayloadInfo` / `ParsePushInfo`.
Implementation used the `silenceable` interface instead: `ParsePushInfo` is a
package-level function without access to the online service, and threading the
flag through it would have meant either an N+1 lookup or a wider signature
change. Goal, wire contract and acceptance are unchanged.

## How it was verified

The bug was first reproduced by injecting a stub APNs gateway into
`IOSPush.client` and capturing the real request body — apns2 sends a `[]byte`
payload verbatim, so these are the actual bytes sent to Apple, not a
reconstruction. Muted and unmuted payloads were byte-identical. That harness was
then inverted into the regression tests.

Two mutations confirmed the tests are not vacuous: removing the `Silence()` call
in `push` reddens the connection-point test, and writing `sound: ""` instead of
omitting reddens the payload tests.

Coverage spans four levels: the payload bytes (stub APNs gateway), the
resolver (stubbed lookup), the SQL (`DesktopOnlineUIDs` against real MySQL), and
the assembly (`pushTo` through PushPool to a capturing pusher). The SQL and
assembly levels exist because the stub levels demonstrably could not catch the
Error 1064 defect described above.

Gates: `go build ./...`, `go vet`, `gofmt`, `golangci-lint` (0 issues),
`make i18n-extract-check`, `make i18n-lint`, full `modules/webhook` and
`modules/user` suites against local MySQL + Redis + WuKongIM.

## Review round (code-review findings)

A review pass raised ten findings; six were real and are fixed here.

**The online lookup was an N+1, and the code comment claimed it was not.**
The first version queried per muted recipient (PC, then Web). The defense —
"almost nobody enables mute" — contradicted this change's own premise: the
server never clears `mute_of_app`, so the muted population only grows. Replaced
with `user.IService.DesktopOnlineUIDs`, one `uid IN (...) AND device_flag IN
(PC, Web) AND online=1` for the whole batch, still skipped entirely when no
recipient is muted. A test pins one query for a 1000-recipient batch.

**A redundant dependency.** `user.IService` already exposed `GetDeviceOnline`
and is already an interface. The first version added a `deviceOnlineChecker`
interface, an `onlineService` field and a second `OnlineService` instance. All
three are gone; the batch method was added to the interface the struct already
holds.

**Error logging was per recipient.** A single DB blip during a broadcast would
emit one Error line per muted user. Now one line per batch carrying the count,
matching `filterPausedUIDs`.

**The RTC test covered dead code.** `PayloadInfo.IsVideoCall` is never assigned
in production, so the payload-layer RTC branch is unreachable and a test that
set the field by hand proved nothing. The real guarantee is `if !isVideoCall` in
`pushTo`, driven by the payload `cmd`. The test now drives `pushTo`; deleting
that line turns it red.

**Redis leaked between tests.** Device-token hashes were written and never
deleted, and `CleanAllTables` is SQL-only — a fact this repo's own testing rule
warns about.

### The finding that mattered most

The review also flagged that `pushTo`'s assembly — the only place the feature is
actually switched on in production — had no coverage: three layers each unit
tested, nothing covering the wiring between them. Writing that test immediately
failed, and the cause was in the code written to fix the N+1 finding:

```go
[]uint8{config.PC.Uint8(), config.Web.Uint8()}   // []uint8 IS []byte
```

dbr binds `[]byte` as a blob rather than expanding an `IN` list, so the `?` was
never expanded and MySQL returned Error 1064 on every call. The fail-open path
then swallowed it and every muted user got an audible push — **the original bug,
reproduced exactly, by the code meant to fix it**. Stub-based unit tests stayed
green throughout, because the stub never touched SQL.

Two defenses were added: a DB-level test for `DesktopOnlineUIDs` against real
MySQL (device-flag matrix: PC / Web / APP-only / offline / multi-device), and
the `pushTo` end-to-end tests. Mutation-verified — reverting to `[]uint8`
reddens the DB test, deleting the RTC exemption reddens the pushTo test.

## Known gaps left open

- **Android has the same defect.** `push_hms.go` (`sound` / `default_sound`) and
  `push_mi.go` (`sound_uri`) are equally hardcoded and equally blind to
  `mute_of_app`. Deferred by owner decision; the `silenceable` seam is where
  that fix would attach. Not a trust-boundary parity violation — mute is a user
  preference, not an escaping defense — but it is a real user-visible gap.
- **`NotificationService/NotificationService.m` (octo-ios) is still Xcode
  boilerplate** that appends `[modified]` to every title. It is dormant only
  because no payload sets `mutable-content: 1`. Anyone adding that flag inherits
  a visible bug.
