---
type: Task
title: "Task: ios-apns-mute-of-app"
description: Honor the user's 「手机静音」 (mute_of_app) setting in the iOS APNs offline-push payload.
tags: [webhook, push, apns, ios, user-setting, notification]
timestamp: 2026-09-11T10:00:00+08:00
# --- octospec extension fields ---
slug: ios-apns-mute-of-app
upstream: "reported: Web 已登录状态下开启「手机静音」后 iOS 仍弹出带声音的消息通知"
source: user
---

# Task: ios-apns-mute-of-app

## Goal

When a user has turned on 「手机静音」 (`user.mute_of_app = 1`) while their Web/PC
client is signed in, offline messages delivered to iOS through APNs must arrive
**silently**. Today they always ring, because the APNs payload hardcodes
`"sound": "default"` and the setting never reaches the push layer at all.

Silent here means the `aps.sound` key is **omitted entirely** — not set to an
empty string, which some iOS versions treat as "sound file not found" and fall
back to the default tone.

## Background

The 「手机静音」 switch lives in the iOS client (`WKPCOnlineVC`, reachable only
while PC/Web is online). Toggling it double-writes: local `NSUserDefaults`, and
`PUT /v1/user/my/setting {"mute_of_app":1}` → `user.mute_of_app`.

Two delivery paths diverge:

- **Foreground** (socket connected, local notification):
  `WKLocalNotificationManager.m:155` reads the local flag and sets
  `notifContent.sound = nil`. Works correctly.
- **Background / terminated** (socket dropped, APNs):
  `modules/webhook/push_iosapns.go` hardcodes `"sound": "default"`. Broken.

Reproduced by injecting a stub APNs gateway into `IOSPush.client` and capturing
the real payload bytes (apns2 sends a `[]byte` payload verbatim as the HTTP
body): the payload is byte-identical whether or not the user has muted.

The root cause is two-layer, and the second layer is the load-bearing one:
`user.Resp` — the struct the push pipeline receives as `toUser` — has no
`MuteOfApp` field at all, so the push layer is structurally incapable of reading
the setting. `go vet` rejects `toUser.MuteOfApp` with "undefined". `modules/webhook`
references `mute_of_app` zero times.

**Staleness hazard (must be handled, not optional).** `user.mute_of_app` and the
iOS local flag are two independent states that reliably drift:
`WKOnlineStatusManager.m:121-127` clears only the local flag when PC/Web goes
offline and never writes back to the server, so the DB keeps `1` indefinitely.
Reading the column unconditionally would convert "mute does not work" into
"push is permanently silent after the user closes Web" — a strictly worse bug.
The column's own contract says 当pc登录后有效, so the server must gate it on a
live PC/Web session (`IOnlineService.DeviceOnline`) rather than trusting the
client to write back — a client write-back cannot be relied on anyway, since the
app may be killed or backgrounded at the moment Web disconnects.

## Load-bearing list

- **wire-contract**: the APNs payload shape consumed by the iOS client. Only the
  presence/absence of `aps.sound` may change; `alert`, `badge`, `space_id`,
  `channel_id`, `channel_type`, `message_seq` must stay byte-identical.
- **webhook / external-content**: `modules/webhook` offline-push dispatch
  (`pushTo` → PushPool job → `push` → `GetPayload` → `Push`).
- `user.Resp` is a shared struct read by `modules/user` (`api_friend.go`,
  `api_batch.go`) and by `modules/webhook`; adding a field must not alter any
  existing JSON/wire output.
- Effective-mute semantics: `mute_of_app = 1` alone is NOT sufficient. It takes
  effect only while that user has a live PC or Web session, mirroring the
  existing check at `modules/user/api_online.go:98-115`.
- Fail-open: an online-status lookup failure must fall back to **audible** push.
  A muted notification the user never hears is a lost message; consistent with
  the existing fail-open posture of `filterPausedUIDs` (`api.go:549`).
- No N+1: the online lookup runs only for users who actually have
  `mute_of_app = 1`, resolved in the batch stage before PushPool fan-out — same
  reasoning as the account-level pause lookup already documented at `api.go:547`.
- RTC / call pushes keep `sound: "default"`. Calls intentionally pierce mute,
  consistent with `allowPush` exempting `isVideoCall`.
- The other five push vendors (`GetPayload`/`Push` implementations) must keep
  compiling and behaving unchanged; the `Push` interface signature is preserved.

## Out of scope

- Android vendor sound fields (`push_hms.go:100` `sound`/`default_sound`,
  `push_mi.go:75` `sound_uri`) — same class of bug, separate change.
- `NotificationService/NotificationService.m` (octo-ios), still Xcode boilerplate
  appending `[modified]` to titles; dormant because no payload sets
  `mutable-content: 1`. Recorded as a learning, not fixed here.
- Any octo-ios change. The server-side online gate makes a client write-back
  unnecessary, and `octo-web` does not participate in this setting at all
  (`mute_of_app` and `user/my/setting`: zero hits in that repo).
- Changing how `mute_of_app` is written, its UI, or the `user_setting` schema.
- Per-channel mute, account-level notification pause, and `new_msg_notice` —
  all already handled by `allowPush`, all untouched.
- Suppressing the push itself. This change only controls sound; the banner and
  badge still arrive.

## Acceptance

- With `mute_of_app = 1` and a live PC/Web session, the captured APNs payload has
  **no** `aps.sound` key; every other field is byte-identical to the unmuted payload.
- With `mute_of_app = 0`, the payload is unchanged from today: `aps.sound == "default"`.
- With `mute_of_app = 1` but **no** live PC/Web session (the Web-signed-out
  staleness case), the payload is audible — `aps.sound == "default"`.
- An online-status lookup error yields an audible push (fail-open), and is logged.
- The RTC branch still emits `sound: "default"` under all mute states.
- The online lookup is skipped entirely for users with `mute_of_app = 0`;
  a test pins that no lookup happens for an unmuted batch.
- `aps.sound` is absent when muted, never `""`.
- The five non-iOS push implementations compile and their payloads are unchanged.
- Focused tests in `modules/webhook` capture real payload bytes through a stub
  APNs gateway (the reproduction harness, inverted into a regression test).
- `gofmt`, `go build ./...`, `go vet ./modules/webhook/ ./modules/user/`, and the
  focused tests pass. `make i18n-extract-check` / `make i18n-lint` are run if any
  error response is touched (none expected — this change adds no new error paths).
