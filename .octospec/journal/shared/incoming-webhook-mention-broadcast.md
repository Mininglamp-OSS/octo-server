---
type: Journal
title: "Journal: incoming-webhook-mention-broadcast (octo-server #448)"
description: Record of the broadcast-pill auto-compose (deferred half of #448) and the rules/decisions it honored.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary", "render-layer"]
timestamp: 2026-06-24T00:00:00Z
# --- octospec extension fields ---
task: incoming-webhook-mention-broadcast
upstream: Mininglamp-OSS/octo-server#448
source: self
---
# Journal: incoming-webhook-mention-broadcast (octo-server #448)

## What was done
Closed the **broadcast** half of #448 item ② (after #445 shipped the flags and
#449 the directed entities). When a native incoming-webhook push sets
`mention.all` (humans) or `mention.bots` (ais) **and** the per-webhook capability
bit is permitted, the server now prepends the canonical broadcast literal
(`@所有人` / `@所有AI`) + a trailing space to the **text** content, so all three
clients render a visible broadcast pill. Before this, those flags fired the
red-dot / notify / bot-summon but no pill appeared unless the caller hand-wrote
the literal into `content`.

- `modules/incomingwebhook/mention.go`:
  - New `broadcastTokenAll`/`broadcastTokenAIs`/`broadcastTokenSep` constants
    (the canonical render labels for `mentionrewrite.HumansKey`/`AIsKey`).
  - `composeBroadcastContent(content, wantAll, wantBots, allowAll, allowBots)
    (string, int)` — prepends gated, idempotent (`strings.Contains` on the
    canonical literal), humans-first; returns the new content and the prefix's
    **UTF-16 code-unit** length. No-op returns `(content, 0)`.
  - `shiftEntityOffsets(ents, by)` — shifts surviving directed-entity (#449)
    offsets by the prefix length so web/Android keep binding the right member.
  - `buildMention` now returns `(mention, content, ignored)`: computes
    `allowAll`/`allowBots` once (shared by compose + assemble), composes on the
    **text path only**, validates entities against the **original** `req.Content`
    then shifts the survivors.
- `modules/incomingwebhook/api.go`:
  - `handlePush` writes the composed content back via the new shared
    `payloadContentKey` const, **only when `content != req.Content`** — a
    load-bearing guard (see Lessons).
- Tests: `mention_broadcast_test.go` (pure `composeBroadcastContent` +
  `shiftEntityOffsets`, bare-`w` `buildMention` wiring with no infra, and one
  infra integration test for broadcast+entity offset-shift asserting
  `buildMention`'s return directly — no WuKongIM read-back).

## Cross-client grounding (re-verified against the three repos)
**All three clients require the literal in the content text to render a pill —
none fabricate one from the flag alone.** iOS `WKMentionService.parseMention`
regex-scans content; Android `WKUIChatMsgItemEntity` uses the flag only to *gate*
an `indexOf` highlight of a literal already in content; web `buildMessageMentions`
synthesizes a render entry but `MarkdownContent.segmentText` only styles it where
the name matches the content text. So server-prepending the canonical literal is
the correct, double-render-safe fix universally.

## octospec rules injected (see context.yaml)
- **trust-boundary** (load-bearing): the prepend is a server-controlled
  compile-time constant with no markdown/regex metacharacters (caller only flips
  a boolean) → no new injection/escape surface; native-only (siblings don't set
  the flags) so adapter parity holds; bounded (≤11 UTF-16 units).
- **error-handling** (load-bearing): no new error codes; compose is non-fatal
  render-layer text, never an error response; `NoLegacyResponseError` guard holds
  (no new handler file). `golangci-lint` (govet gate) clean.
- **space-isolation** (load-bearing): no new identity/data access; only content
  text + already-gated humans/ais flags are touched.
- **testing**: pure unit tests + bare-`w` wiring tests + one testutil integration.

## Verification
- `go test ./modules/incomingwebhook/ -count=1` → PASS (live MySQL/Redis/WuKongIM)
- `go vet ./modules/incomingwebhook/...` → clean
- `golangci-lint run ./modules/incomingwebhook/...` → 0 issues
- High-effort adversarial code-review (8 finder angles + verify) → see Lessons.

## Lessons
- **The `content != req.Content` write-back guard is load-bearing, not
  redundant.** A review angle flagged it as a removable no-op. It is not: the
  richtext payload stores a **blocks array** under the same `content` key
  (`richtext.go:84`); `buildMention` never composes for richtext
  (`content == req.Content`), so the guard is false and the array is preserved.
  Making the write unconditional would clobber the array with a string and
  corrupt every richtext message. Comment now spells this out so it is never
  "simplified" away.
- **Offset shift must be by the *actual* prepended prefix length.** With
  per-token idempotency, the prefix may be only one token (or none); the shift is
  `len(utf16.Encode([]rune(prefix)))` computed from exactly what was written, and
  a prepend at offset 0 keeps every entity's `'@'` anchor valid at
  `offset+prefixLen`. Deriving the length from `utf16.Encode` (not a hardcoded
  5/6/11) keeps it correct-by-construction if a literal ever changes.
- **Idempotency can only be canonical-literal-deep, by design.** Clients also
  recognize English aliases (`@all`, `@All AIs`) and a per-locale label the
  server cannot know, so the server cannot fully replicate client tokenization.
  Checking `strings.Contains` on the *canonical* literal is the safe choice:
  extending it to `@all` would false-positive on member names like `@allen` and
  *suppress* a legitimate pill (worse than the rare manual-alias double-pill,
  which is documented as out-of-scope).
- **`buildMention` is the reliable seam to test the wiring**, not a WuKongIM
  read-back — it returns content+mention directly, and broadcast-only cases never
  query the member gate, so a bare `*IncomingWebhook` (no infra) exercises the
  text-path/gate/return logic.
