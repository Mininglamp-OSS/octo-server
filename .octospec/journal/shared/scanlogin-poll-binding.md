---
type: Journal
title: "Journal: scanlogin-poll-binding"
description: Scan-login stops handing auth_code to any anonymous poller who knows the uuid — a poll_secret now gates the credential fields. Closes QR-observer hijack but NOT QRLJacking, which no server-side control can close. Also fixes an auth-code expiry inversion, a channel-displacement vector, and post-login state that outlived the session.
tags: ["auth", "scan-login", "security", "rate-limit", "wire-contract", "qrljacking"]
timestamp: 2026-08-07T00:00:00Z
# --- octospec extension fields ---
task: scanlogin-poll-binding
upstream: self (code audit; not covered by either pentest report)
source: self
---
# Journal: scanlogin-poll-binding

## What this does and does not close

**Closed — QR-observer hijack.** `GET /v1/user/loginstatus` used to return the whole
`qrcode:{uuid}` payload, `auth_code` included, to anyone presenting the uuid. The uuid is
visible to anyone who can see the QR: shoulder-surfing, a screenshot forwarded in a chat,
a screen-share, a recording. Any of them could poll and steal the login. A `poll_secret`
minted with the uuid and returned only in the response body now gates the credential
fields, so seeing the QR is no longer enough.

**NOT closed — QRLJacking.** The first version of this task claimed it was. That claim was
wrong and code review caught it. In QRLJacking the attacker **mints the uuid themselves**
(`loginuuid` is unauthenticated), so `poll_secret` is handed straight to them along with
the uuid; they poll their own uuid with their own secret, the check passes, and
`auth_code` comes back. Binding the poll channel to the minter does nothing when the
minter *is* the attacker.

This is not fixable server-side. The server cannot distinguish "victim scanned the
attacker's QR" from "user scanned their own QR" — both are a legitimately authenticated
scan of a legitimately minted code. **Only the person confirming can tell**, and only if
shown enough to notice.

A first pass shipped the server half of that (`ScanLoginOrigin` — requesting IP / device /
UA returned to the phone at scan time). **Review pulled it back out, correctly.** Every one
of those fields is attacker-controlled: `device_name` / `device_model` come straight from
`loginuuid` query params, `User-Agent` is a request header, and the IP came from
`c.ClientIP()` — neither this repo nor octo-lib calls `SetTrustedProxies`, so gin trusts
`0.0.0.0/0` and takes the leftmost `X-Forwarded-For` entry, which the client can pre-seed.
The attacker who minted the QR could therefore make the confirm dialog display the
*victim's own* IP and device name. That turns weak evidence into false assurance — strictly
worse than showing nothing. Getting it right needs a trusted IP source, which in turn needs
ops to confirm whether the reverse proxy overwrites or appends XFF. Tracked in
octo-ios#71 / octo-android#116.

## What was done

- `modules/user/scanlogin_poll.go` (new): `mintScanLoginPollSecret` (stores **SHA-256**
  under `scanlogin:poll:{uuid}`), `scanLoginPollSecretMatches` (fail-closed,
  `subtle.ConstantTimeCompare`), `deleteScanLoginPollSecret`,
  `filterScanLoginPublicFields`. All of it is written against a small `pollSecretStore`
  interface so the gate is unit-testable without Redis.
- `modules/user/api.go`: `getLoginUUID` mints the secret and sets `Cache-Control: no-store`;
  `getloginStatus` resolves the secret through `scanLoginPresentedPollSecret` (header first,
  query fallback) and routes every payload emission through one `respondStatus` gate, and
  only registers a long-poll channel when authorized; `grantLogin` renews the auth code at
  confirm time; `loginWithAuthCode` revokes the secret and deletes `qrcode:{uuid}`.
  Both routes carry `StrictIPRateLimitMiddleware`. `removeQRCodeChan` is gone.
- `modules/qrcode/api.go`: auth-code TTL now `user.ScanLoginAuthCodeTTL` (5min).
- `modules/user/swagger/api.yaml`: documents `poll_secret` and both credential channels.
- octo-lib#116: registers `X-Scan-Poll-Secret` in the CORS allow-list.
- octo-web `login_vm.tsx`: stores `poll_secret`, sends the header (plus a query copy when
  the API base is cross-origin), and re-mints the QR when `auth_code` is withheld.

## Load-bearing decisions

- **Allow-list, not deny-list.** `qrcode:{uuid}` is written in three places
  (`getLoginUUID`, `handleScanLogin`, `grantLogin`). A deny-list of sensitive keys
  fails open the moment someone adds a field in any of them — and the first version's
  guard test only read `grantLogin`. `scanLoginPublicDataKeys` now lists the two keys
  that may leave (`status`, `app_id`); everything else is dropped by default.
- **The secret must never enter the qrcode payload**, which is exactly what
  `getloginStatus` echoes. Locked by a test that parses the `NewQRCodeModel(...)` literal.
- **Header first, query only where the header cannot travel.** Plaintext in a URL is
  written verbatim into nginx/CDN/WAF access logs, APM spans and browser history, which
  would cancel out storing only a digest at rest. But a custom header makes the poll a
  non-simple request, and octo-lib's `CORSMiddleware` hardcodes `Access-Control-Allow-Headers`
  and aborts `OPTIONS` with 204 the moment it is set — headers are flushed, so no downstream
  middleware can extend the list. A cross-origin preflight would reject the *real* request:
  the Tauri/Electron desktop build (absolute API origin) would lose scan-login entirely, and
  "degrade by stripping" cannot help because nothing reaches the handler. octo-lib#116 fixes
  the list; until it ships, cross-origin clients send the secret in the query and the server
  accepts either. Both sides carry a `SUNSET` comment naming the removal condition.
- **Renew the auth code at confirm, not just the QR state.** The code is minted at *scan*
  time and the user can sit on the confirm screen for its whole TTL. Refreshing only
  `qrcode:{uuid}` produced `status: authed` carrying a credential with a second left —
  redemption fails and the state machine has nowhere to go. The 10min→5min TTL change is
  what made this reachable: the old 5 minutes of slack had been hiding it.
- **Only authorized pollers get a channel.** `getQRCodeModelChan` overwrites unconditionally,
  so an unauthorized poller could keep capturing the push and leave the legitimate one
  waiting out the full 10s every cycle. Credentials were never at risk (the allow-list still
  applied), but it was a cheap sustained login-latency penalty on an unauthenticated
  endpoint. Unauthorized callers still wait the same 10s — returning early would leak a
  timing signal about whether the secret was right, and would let an attacker poll faster.
- **Fail-closed everywhere**, including store errors. One fail-open branch restores the
  original hole.
- **Own your channel.** `removeQRCodeChan(uuid)` closes whatever is registered for the
  uuid, not the caller's. On an unauthenticated long-poll endpoint that let anyone who
  knows a uuid connect-and-abort in a loop to keep evicting the legitimate poller.
  `removeQRCodeChanOwned` only reclaims the channel this request registered — and the old
  method is deleted rather than left around, since it kept the more obvious-looking name.
- **Never emit an empty 200.** `respondStatus(nil)` originally wrote nothing, which gin
  turns into a zero-length 200; the frontend then sets `loginStatus = undefined` and the
  state machine stops matching, freezing the page. It now falls back to the state read at
  request start.
- **Rate limits deliberately loose (120/600 per min) and env-tunable.** `qrcode:{uuid}`
  has a 60s TTL so every parked login page re-mints ~1/min; a 100-person office behind one
  egress IP is ~100 mints/min. And when a reverse proxy has no `X-Real-Ip`/`X-Forwarded-For`,
  `getClientIP` falls back to `RemoteAddr` — nginx's own address — collapsing the whole
  deployment into one bucket. The first version's 20/min would have locked buildings out.
- **Auth-gate exemption documented in place.** `space-isolation` wants every route behind
  `AuthMiddleware` or an explanation. These two cannot be: the QR renders before any token
  exists.

## Learning

**Binding a channel to "whoever minted it" is not an access control when minting is
unauthenticated.** The whole first version rested on that confusion, and it survived
because every individual check looked sound: the uuid is real UUIDv4 from `crypto/rand`,
`grantLogin` correctly verifies `scaner == loginUID`, the secret is hashed and compared in
constant time. None of that mattered, because the party being authenticated was the
attacker.

The question to ask of any mint→poll flow is not "is this token unguessable?" but
**"who ends up holding it, and is that the party I actually want to authorize?"** When the
answer is "whoever asked", the binding is a no-op against anyone who can ask — which, on
an unauthenticated endpoint, is everyone.

Staged in `.octospec/learnings/pending/`.

## Verification notes

`go build ./...`, `go vet`, `make i18n-extract-check`, `make i18n-lint` pass; the three
touched files are `gofmt`-clean (the repo has pre-existing drift elsewhere, tracked under
`.golangci.yml`'s `TODO(octo-lint-cleanup)`). The security gate now has real behavioural
tests via an injected in-memory store — correct / wrong / missing secret, store-error
fail-closed, digest-not-plaintext, revocation, and TTL-covers-worst-case.

A local integration environment was later stood up (MySQL 8.0.46 + Redis + WuKongIM 2.2.4 —
setup and the per-package-database requirement are recorded in `integration-test-env.md`
next to this task's brief), so `./modules/user/` and `./modules/qrcode/` now pass in full
against live infrastructure. Whole repo: 102 pass / 4 fail, with the four confirmed
pre-existing by running the same packages on a clean `origin/main` worktree and diffing the
failing test names (byte-identical).

octo-web's `@octo/login` suite passes (210 tests). `apps/web` shows 52 failing files both
on this branch and on `main` — verified by checking the one changed file back out to the
`main` version and re-running.
