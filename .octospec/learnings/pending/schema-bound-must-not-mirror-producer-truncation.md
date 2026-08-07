---
type: Learning
title: "A schema bound copied from a producer's truncation length has zero headroom and breaks on units the producer never promised"
description: Pinning a server-side maxLength to a client's observed output (N + ellipsis = N+1) makes the contract exactly saturated, so it survives only as long as the client's truncation unit stays the same. JSON Schema maxLength counts Unicode code points; JS .length/.slice count UTF-16 code units; grapheme segmentation counts neither. Set the ceiling from the product, keep it bounded, and leave headroom.
tags: ["cardtmpl", "wire-contract", "trust-boundary", "schema", "cross-repo"]
timestamp: 2026-08-07T12:00:00Z
# --- octospec extension fields ---
source: self
origin_task: cardtmpl-reasoning-phase-tools-successor
origin_pr: "Mininglamp-OSS/octo-server (ai.reasoning-process@0.4.0)"
status: pending
candidate_rule: trust-boundary
---
# A schema bound copied from a producer's truncation length has zero headroom and breaks on units the producer never promised

When `ai.reasoning-process@0.2.0` first made the data contract fail-close, several
`maxLength` values were read off the one live producer: `THOUGHT_MAX = 280` plus a `…`
became `thought: 281`, `TOOL_NAME_MAX = 80` became `tool: 81`, `ERROR_MAX = 120` became
`errorMessage: 121`. That brief was honest about it — it explicitly declined to present
these as product contracts — but the numbers still shipped as the enforced ceiling.

Two problems are baked into `N + 1`:

**It is exactly saturated.** There is no slack for the producer to change *how* it
truncates. Three ordinary edits overflow it with no warning at authoring time:

- switching to grapheme-aware truncation (`Intl.Segmenter`) — one grapheme can be many
  code points: `é` as `e` + U+0301 is 2, a ZWJ family emoji is 5, flags and skin-tone
  modifiers more. 280 graphemes can be well over 1000 code points.
- changing the ellipsis from `…` (U+2026, 1 code point) to `...` (3).
- appending anything else — a marker, a space, a direction mark.

**The two sides count in different units.** JSON Schema `maxLength` counts Unicode **code
points**. JavaScript `.length` / `.slice` count **UTF-16 code units**. Grapheme
segmentation counts neither. Measured against this server's validator: 281 CJK runes
(843 bytes) passes, 281 BMP-outside emoji (562 UTF-16 units, 1124 bytes) passes, 282 of
either is rejected — so it really is code points, not bytes and not code units. Today's
`slice(0, N)` direction happens to be safe because code points ≤ code units, but that is
a property of the *current* implementation, not of the contract. Nothing in either repo
states the unit, and no test on either side would catch the flip.

The failure mode is also worse than it looks: an over-limit field is rejected with
`ErrFieldsInvalid` **before template expansion**. The server does not truncate or degrade
— the whole card fails to send. A display regression would be tolerable; a silent send
failure is not.

**What to do instead.** Set the ceiling from what the product should allow, not from what
one client currently emits, and leave it comfortably above every known producer. Keep it
explicitly bounded — removing the bound is not the fix, and an unbounded string fails
`DefaultCompileLimits().RequireBoundedSchema` anyway. Check the real budget before
worrying: for this card, widening `thought` from 281 to 4001 moved the fully-saturated
payload from 6.69% to 19.46% of `cardmsg.MaxPayloadBytes`, and cost zero nodes because
`MaxNodes`/`MaxDepth` are structural limits that text length does not touch. The bound was
never protecting a real resource.

If a bound genuinely must track a client constant, then write the unit down on both sides
and put a test on the arithmetic — otherwise it is an undocumented coupling that looks
like a safety limit.

**Rule of thumb**: a `maxLength` equal to `producer_cap + 1` is a coupling, not a bound.
Bound the payload for real (structural caps, byte caps, node caps) and let the string
ceiling be a generous product decision with room above every producer.
