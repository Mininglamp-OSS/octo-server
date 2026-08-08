---
type: Learning
title: "Binding a channel to whoever minted it is not access control when minting is unauthenticated"
description: Session-binding a poll/callback channel to its requester only excludes third parties. If the mint endpoint is open, the attacker is the requester, so the binding is a no-op against them — ask who ends up holding the token, not whether it can be guessed.
tags: [auth, session-binding, capability, polling, callback, qrljacking, security, threat-model]
timestamp: 2026-08-07T00:00:00Z
status: pending
---

# Binding to the minter is not access control

## Context

Scan-login handed `auth_code` — directly redeemable for a full user token — to anyone
polling `GET /v1/user/loginstatus` with the right `uuid`. The fix attempt was to mint a
`poll_secret` alongside the uuid and require it on the poll, binding the channel to the
browser that requested the QR.

That fix was written to close QRLJacking (attacker mints a QR, puts it on a phishing page,
victim scans and confirms, attacker harvests the credential). **It does not.** The attacker
is the one calling `loginuuid`, so `poll_secret` is issued to *them*, along with the uuid.
They poll their own uuid with their own secret and the check passes.

What made this hard to see is that every individual control was sound:

- the uuid is UUIDv4 over `crypto/rand` — 122 bits, unguessable;
- `grantLogin` correctly verifies `scaner == loginUID`;
- the secret is stored as a SHA-256 digest and compared with
  `subtle.ConstantTimeCompare`.

None of it mattered. The party being authenticated was the attacker.

## The rule

Session-binding excludes **third parties**. It never excludes **the requester**. So for any
mint → read-back-state flow, the security question is not

> "can this token be guessed?"

but

> **"who ends up holding this token, and is that the party I actually want to authorize?"**

If the mint endpoint is unauthenticated, "whoever asked" includes every attacker, and the
binding buys you nothing against them. It is still worth having — it closes the weaker
*observer* threat (someone who saw the QR / URL / callback id but did not request it) — but
it must not be described as closing the impersonation threat.

When the requester and the victim are different people and only the victim can tell them
apart, **the control has to live in the victim's confirmation UI**: show the requesting
device, IP, and location, and make refusing easy. No server-side check can substitute,
because both branches look identical on the wire.

## Applies to

Any flow where an anonymous client mints an id and later reads state back keyed on it:
QR/device pairing, email- and SMS-confirmation polls, OAuth-ish callback handoffs, job
status endpoints for anonymously submitted work.

Corollaries worth keeping:

- Filter the echoed payload with an **allow-list**. `qrcode:{uuid}` has three writers; a
  deny-list of sensitive keys fails open the first time anyone adds a field.
- Carry the secret in a **header**, not the query string, or access logs, CDN/WAF logs,
  APM spans and browser history undo the at-rest hashing.

Reference implementation: `modules/user/scanlogin_poll.go`.
