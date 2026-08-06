---
type: Learning
title: "A fixed-400 error facade silently breaks status-code-driven client logic"
description: ResponseErrorL pins the wire status to 400, so rate limiting looked correct while telling clients \"bad request\" — which they are entitled to read as \"stop retrying\".
tags: ["error-response", "i18n", "wire-contract", "rate-limit", "review"]
timestamp: 2026-08-05T08:15:00Z
# --- octospec extension fields ---
source: self
origin_task: bot-api-per-bot-ratelimit
origin_pr: Mininglamp-OSS/octo-server#696
status: pending
candidate_rule: error-handling
---

# A fixed-400 error facade silently breaks status-code-driven client logic

## Context

`httperr.ResponseErrorL` pins the wire status to **400** for D14 compatibility;
the real status only appears inside `error.http_status`. That is the default
facade and the right choice for most legacy-bearing endpoints.

In #696 the new bot rate limiter used it. The implementation was otherwise
correct — the token bucket was genuinely rejecting requests — but the response was:

```json
{"msg":"Too many requests, please try again later.","status":400}
```

Two consequences, neither visible from the server side:

1. **Clients branch on the status code, not on `error.code`.** octo-lib's own
   three rate-limit middlewares return a real 429 (`TransportStatus: 429`). Having
   the bot channel answer 400 means one system with two representations of "you
   are being throttled".
2. **400 and 429 imply opposite client behaviour.** 429 means back off and retry;
   400 means the request is malformed, stop retrying. This is not hypothetical:
   the same incident had a bot hammering `register` ~4 rps because an
   authentication failure *also* surfaces as 400 (`queryRobotByBotToken` miss),
   so the client could not tell "stop, your credential is dead" from
   "back off and retry". Returning 400 for throttling adds a third meaning to the
   same code.

Nothing caught this: it compiles, `i18n-lint` passes (the facade *is* the approved
one), unit tests on the limiter pass, and the server logs look right. Only an
integration test asserting `w.Code == 429` failed.

## Rule of thumb

Before choosing the error facade, ask **what the client does with the status code**:

1. **If client behaviour is keyed off the status** (throttling → backoff,
   401 → re-auth, 409 → conflict resolution), the wire status must be real.
   Use `ResponseErrorLWithStatus`.
2. **Match sibling implementations of the same concern.** If another layer of the
   same system already returns a real status for this condition, diverging is a
   contract split, not a compatibility win — even though each side individually
   looks compliant.
3. **Assert the status code in tests, not just the body.** A test that only checks
   `error.code` passes happily while the transport status is wrong.
4. **Remember 400 is overloaded.** Under the fixed-400 facade, "bad parameter" and
   "authentication failed" are indistinguishable on the wire. Adding a third
   meaning makes client-side triage strictly harder.

Note: CLAUDE.md requires maintainer sign-off for `ResponseErrorLWithStatus`.
The point of this learning is not to bypass that, but to make the question
explicit at authoring time instead of discovering it from client behaviour.

## Why worth a rule

The failure mode is invisible to every automated gate in the repo and to
server-side observation; it only manifests as *client* misbehaviour (stopped
retrying, or logged a parse/validation error) far from the code that caused it.
A one-line question at authoring time — "does the client branch on this status?" —
prevents it.
