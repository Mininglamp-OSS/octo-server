---
type: Journal
title: "Journal: scanlogin-poll-binding"
description: Scan-login stops handing auth_code to any anonymous poller who knows the uuid — a poll_secret now gates the credential fields. This closes QR-observer hijack but NOT QRLJacking, which no server-side control can close; the server half of the real mitigation (requesting-device context on the confirm screen) ships here and is blocked on mobile rendering.
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
shown enough to notice. So the server half of the real mitigation ships here:
`ScanLoginOrigin` (requesting IP / device name / model / UA) is captured at
`loginuuid` and returned to the phone from `handleScanLogin` as
`HandlerTypeLoginConfirm.origin`. **Until iOS/Android render it in the confirm dialog,
QRLJacking still works.** That mobile work is the blocking follow-up.

## What was done

- `modules/user/scanlogin_poll.go` (new): `mintScanLoginPollSecret` (stores **SHA-256**
  under `scanlogin:poll:{uuid}`), `scanLoginPollSecretMatches` (fail-closed,
  `subtle.ConstantTimeCompare`), `deleteScanLoginPollSecret`,
  `filterScanLoginPublicFields`, and the `ScanLoginOrigin` capture/load pair. All of it
  is written against a small `pollSecretStore` interface so the gate is unit-testable
  without Redis.
- `modules/user/api.go`: `getLoginUUID` mints the secret and records the origin;
  `getloginStatus` reads the secret from the **`X-Scan-Poll-Secret` header** and routes
  every payload emission through one `respondStatus` gate; `loginWithAuthCode` revokes the
  secret on redemption. Both routes carry `StrictIPRateLimitMiddleware`.
- `modules/qrcode/api.go`: `authCode` TTL 10min → 5min; `handleScanLogin` returns `origin`.
- octo-web `login_vm.tsx`: stores `poll_secret`, replays it as a header, and falls back to
  re-minting the QR when `auth_code` is absent.

## Load-bearing decisions

- **Allow-list, not deny-list.** `qrcode:{uuid}` is written in three places
  (`getLoginUUID`, `handleScanLogin`, `grantLogin`). A deny-list of sensitive keys
  fails open the moment someone adds a field in any of them — and the first version's
  guard test only read `grantLogin`. `scanLoginPublicDataKeys` now lists the two keys
  that may leave (`status`, `app_id`); everything else is dropped by default.
- **The secret must never enter the qrcode payload**, which is exactly what
  `getloginStatus` echoes. Locked by a test that parses the `NewQRCodeModel(...)` literal.
- **Header, not query.** Putting the plaintext in the URL would have it written verbatim
  into nginx/CDN/WAF access logs, APM spans and browser history — cancelling out the
  reason for storing only a digest at rest.
- **Fail-closed everywhere**, including store errors. One fail-open branch restores the
  original hole.
- **Own your channel.** `removeQRCodeChan(uuid)` closes whatever is registered for the
  uuid, not the caller's. On an unauthenticated long-poll endpoint that let anyone who
  knows a uuid connect-and-abort in a loop to keep evicting the legitimate poller.
  `removeQRCodeChanOwned` only reclaims the channel this request registered.
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

**The full `./modules/user/` package cannot run in the dev sandbox** — many tests call
`testutil.NewTestServer()`, which needs MySQL on 127.0.0.1:3306; a clean `origin/main`
worktree fails identically, so this is environmental. **octo-web lint/test could not run
either**: `pnpm install` cannot complete because `cdn.sheetjs.com` (sheetjs' own CDN, not
the npm registry) is refused by the sandbox egress policy. CI is the first real gate for
the web change.
