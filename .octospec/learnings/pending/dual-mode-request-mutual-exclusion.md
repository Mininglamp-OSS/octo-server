---
type: Learning
title: "Dual-mode request struct must reject both-present, not just both-absent"
description: When one request struct carries two mutually-exclusive modes (legacy field vs new structured field), enforce the both-set → 400 guard too, or one mode is silently dropped.
tags: ["api", "validation", "trust-boundary", "review"]
timestamp: 2026-07-13T15:44:34Z
# --- octospec extension fields ---
source: self
origin_task: card-message-internal-dispatch
origin_pr: dmwork-org/octo-server (summary-notify pilot)
status: pending
candidate_rule: error-handling
---
# Dual-mode request struct must reject both-present, not just both-absent

When an existing ingress struct is extended so one request can arrive in two
mutually-exclusive shapes — here `NotifyReq.Payload` (legacy text) XOR
`NotifyReq.Card` (new structured card) — the handler naturally grows a
"neither is set → 400" guard. It is easy to forget the symmetric case: if a
caller sets BOTH, the dispatch branch picks one (`Card != nil` wins) and
silently discards the other, which diverges from the written contract and hides
caller bugs.

**Rule candidate:** for any dual-mode request field pair, add an explicit
both-present → 400 rejection (a registered `errcode`, not a raw error) next to
the both-absent guard. A `switch { case A && B: reject; case !A && !B: reject }`
makes the exclusivity total. Caught in review on the summary-notify pilot
(`err.server.notify.card_invalid`).
