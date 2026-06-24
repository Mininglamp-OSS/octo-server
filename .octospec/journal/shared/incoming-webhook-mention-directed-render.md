---
type: Journal
title: "Journal: incoming-webhook-mention-directed-render (octo-server #448)"
description: Record of the opt-in server-side directed @mention name-resolution (#448 item ① b) and the adversarial-review hardening it received.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary", "render-layer"]
timestamp: 2026-06-24T00:00:00Z
# --- octospec extension fields ---
task: incoming-webhook-mention-directed-render
upstream: Mininglamp-OSS/octo-server#448
source: self
---
# Journal: incoming-webhook-mention-directed-render (octo-server #448)

## What was done
Implemented #448 item ① option (b): an incoming-webhook caller can render a
directed `@member` pill by sending **only the uid** + `mention.render:true`. The
server resolves the member display name, prepends `@<name> ` to the **text**
content, and **generates** the UTF-16 `mention.entities` element. Shipped in the
same PR as the broadcast half (#450), so together they close #448.

- `model.go`: `mentionReq.Render` — opt-in request flag (default off → #445/#449
  callers unchanged; request-only, not on the wire).
- `db.go`: `filterGroupMembers` now returns `map[uid]name` via `LEFT JOIN user`
  (`IFNULL(user.name,'')`), matching the group member-list resolution. Membership
  is still key-presence; the name is only used by render.
- `mention.go`: `composeBroadcastContent` refactored into the unified
  `composeMentionContent` (broadcast literals + directed `@name`s in one prepend
  pass; broadcast first, directed after). `buildMention` returns
  `(mention, content, ignored)`, computes the render set (member uids in caller
  order), and — `render` and caller-supplied `entities` being mutually exclusive
  (#449 authoritative) — attaches either the generated entities or the
  shifted caller entities.
- Tests: `mention_directed_render_test.go` (pure compose: order / non-member &
  empty-name skip / idempotency / budget / broadcast+directed combo / the
  forged-broadcast guard; integration via `buildMention` with seeded
  `group_member` + `user` rows).

## Grounding
Client send-path (`octo-web mentionSendParse.ts`): a user-typed @ emits `content`
with `@<displayName>` inline + `entities.push({uid, offset, length})` (UTF-16) +
`uids`. The server generates the **same** shape for a bare uid, so a
webhook-rendered directed @ is wire-identical to a client-sent one. Display name =
`user.name` (the group member-list source); `remark` (viewer-specific) and the
verified-`real_name` chain (#344) are out of scope for v1.

## Adversarial review (3 finder agents) → hardening applied
- **Forged broadcast pill (security + cleanup agents, CONFIRMED).** A member whose
  display name *is* a broadcast label (`所有人`/`所有AI`/`All People`/`All AIs`/
  `all`) would compose to `@所有人` etc. — which all three clients render as a
  broadcast pill **by scanning content text**, independent of `humans/ais`. That
  forges an all-hands broadcast that bypasses the `allow_mention_*` capability gate
  (directed @ is deliberately not capability-gated). **Fix:** skip render when the
  name hits `isBroadcastLabel` (the clients' own broadcast-label set) **or contains
  `@`** (the latter also defends the pre-existing WeChat-nickname path that doesn't
  strip `@`). The uid still routes via `mention.uids`; only the auto-pill is
  suppressed. Locked by a unit test.
- **O(n²) prefix recompute (cleanup agent).** The rune budget recomputed
  `prefix.String()` length each iteration. Switched to incremental `prefixU16`/
  `prefixRunes` accumulators (note: `strings.Builder.String()` does not allocate —
  the cost was CPU re-scans, not memory).
- **Content cap (correctness + security agents).** The rune budget keeps the
  composed content ≤ `maxContentRunes`; with utf8mb4 emoji names the byte size can
  be ~4× but stays far below the downstream serialization cap — documented, no
  byte cap added.
- **iOS spaced-name pill (cleanup agent).** A name with an internal space (e.g.
  "Bob Smith") renders fully on web/Android (entity-bound) but iOS — which ignores
  entities and re-parses `@\S+` positionally — shows a pill truncated at the first
  space; the tap still binds to the correct uid by position. Documented as the same
  accepted iOS behavior as #449, not a mis-binding.
- **Markdown-in-name injection (security agent).** The composed `@<name>`
  replicates exactly what clients already write into content for any @-mention, so
  it adds no novel surface; the render layer (`rehypeSanitize` + `isSafeUrl`) is the
  controlling boundary. Not escaped server-side (would diverge from how the same
  name renders everywhere else). The WeChat-nickname `@` non-sanitization is a
  pre-existing `modules/user` gap (out of scope here); the render `@`-skip guard
  defends this module regardless.
- Correctness agent: **clean** (`[]`) on the db JOIN/type change, the budget/offset
  math, and the render↔caller-entities mutual exclusion + routing.

## octospec rules injected (see context.yaml)
- **trust-boundary** (load-bearing): name from server-side member data; forged-
  broadcast guard; render-side sanitization is the boundary.
- **space-isolation** (load-bearing): names resolved only for current internal
  members of the group; non-members drop silently.
- **error-handling** (load-bearing): no new codes / raw responses; render is a
  request-only, non-fatal field.
- **testing**: pure + integration tests proportional to the trust-boundary risk.

## Verification
- `go test ./modules/incomingwebhook/ -count=1` → PASS (live MySQL/Redis/WuKongIM)
- `go vet` clean; `golangci-lint run ./modules/incomingwebhook/...` → 0 issues

## Lessons
- **A caller-influenced value spliced into content can re-enter a sibling
  protocol.** The broadcast literal was safe *as a constant*; routing `user.name`
  through the same prepend re-introduced the literal from attacker-selectable data
  and re-collided with the broadcast token scan. When generating content that
  another layer re-parses (clients scan text for broadcast tokens), guard the
  generated text against that layer's reserved vocabulary.
- **The cross-client render contract is the spec for server-generated content.**
  Because web binds by offset, Android validates, and iOS re-parses `@\S+`
  positionally, the server must emit a single-token `@name` (no internal-space
  guarantees) and UTF-16 offsets — the same constraints that made the broadcast
  literals space-free.
- `buildMention` (returning content+mention directly) is again the reliable test
  seam — broadcast-only cases need no infra; render/entity cases seed
  `group_member` + a `user` row for the `user.name` JOIN.
