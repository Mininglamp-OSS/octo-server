---
type: Learning
title: "A schema bound copied from a producer's truncation length has zero headroom and breaks on units the producer never promised"
description: Pinning a server-side maxLength to a client's observed output (N + ellipsis = N+1) makes the contract exactly saturated, so it survives only as long as the client's truncation unit stays the same. JSON Schema maxLength counts Unicode code points; JS .length/.slice count UTF-16 code units; grapheme segmentation counts neither. Replace such a bound from the product side — but first find every ceiling the value must pass (validator, DB column, cache, envelope) and size against the smallest one in bytes at the worst-case encoding, measuring the value as the storage layer receives it (after any server-side finalization that enriches or denormalizes the payload) rather than as it leaves the producer, because a render-time gate is not a storage gate. If the payload is already over budget with the field at its minimum, the field ceiling is the wrong lever — enforce at the write boundary, where one check also covers versions whose bytes are frozen.
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
`DefaultCompileLimits().RequireBoundedSchema` anyway.

**But find the *binding* ceiling before declaring headroom — and make sure you are measuring the
artifact that gets stored.** This is the part the earlier versions of this learning got wrong, three
times in a row, each time in the unsafe direction.

*Round one* raised `thought` from 281 to 4001 on the grounds that the fully-saturated frame was only
19.5% of `cardmsg.MaxPayloadBytes` (512 KiB) — the only size gate a rendered frame passes — and cost zero
nodes, since `MaxNodes`/`MaxDepth` are structural limits text length does not touch. That looked like ~4×
headroom. It was not: the authoritative write stores the whole marshalled frame in
`message_extra.content_edit`, a MySQL **`TEXT` column — 65,535 bytes**, never widened by any later
migration.

*Round two* kept 4001 and proposed refusing oversized frames at the write boundary. That framing was
rejected for a reason worth stating separately: **a published contract that admits what the store cannot
hold is a defect regardless of how politely the write fails.** The schema is advertised to every bot
through a capability endpoint — one that also reports a 512 KiB payload allowance, actively suggesting
there is room. The realistic actor is not an attacker but the next integrator who reads `maxLength` and
trusts it.

*Round three* separated an **accept** ceiling from a **display** ceiling: accept 4001, clamp the rendered
value to 400 with an ellipsis. This answers round two's objection instead of evading it — 4001 really is
accepted, nothing is refused for length — and it has a property neither plain bound has: **frame size
stops depending on caller input.**

*Round four is the one that matters here, because it invalidated all three rounds' arithmetic.* Every
byte figure above had been measured on `json.Marshal` of the render output. That is not what gets stored.
A finalization step (`cardmsg.Finalize`) runs afterwards and adds a top-level `plain` derived from every
visible text node — **+47%** for this card. Re-measured on the persisted bytes, two conclusions reversed:

- **The field under discussion was never the dominant term.** At `thought = 1` the frame is still 107% of
  the column. Hold `thought` at 400 and shrink the *other* repeated strings instead and it drops to 87%.
  The frame is dominated by 13 array items × (81 + 192) code points plus the `plain` copy. So no value of
  the display ceiling makes the worst case fit, and 400 is **not** derived from the storage budget — it is
  a product decision. The earlier claim that the number "comes from the store" was false.
- **The previous published version was already over.** At its own bound, same encoding, it persists at
  121.6% of the column. Pre-existing, not introduced by the change under review, and unfixable in place
  because a published version's bytes are frozen.

Which relocates the whole problem: a storage budget that no single schema version can enforce has to be
enforced at the **write boundary**, once, for every version — a named check that all persisting paths
share, returning a typed error with the byte count. That is not what round two proposed. Round two would
have refused frames for *declared field length* while advertising that length; this refuses frames that
overflow for reasons **no field bound can express**, and it protects versions whose bytes are already
frozen.

Four things generalize:

- **Measure the value as *persisted*, not as produced.** If anything downstream of your measurement point
  enriches, normalizes, re-serializes, or derives a denormalized copy, your number is wrong — and wrong
  low. Find the exact call whose output the storage layer receives and measure *that*. Here the gap was a
  server-authoritative plain-text projection worth ~half the frame again.
- **A validator ceiling is not a storage ceiling.** The payload cap is checked at render; the column width
  is discovered at `INSERT`. Under `STRICT_TRANS_TABLES` the write fails (`Data too long for column`)
  *after* the frame validated; on a non-strict deployment it is silently truncated into invalid JSON.
  Trace the value to every place it is persisted, cached, or re-serialized — not just to the gate that
  rejects it.
- **Code points are not bytes, and one field's fixture is not the worst case.** JSON Schema counts code
  points; a column counts bytes; Go's encoder escapes `<`, `>`, `&`, U+2028 and U+2029 to six bytes each.
  A CJK fixture understated the maximum by 1.8×, and escaping only the field under discussion still
  understated it, because the *other* strings in the same frame escape too.
- **Check the floor before negotiating the ceiling — and if the floor is already over, stop tuning the
  ceiling.** When the baseline with the field set to one character is 107% of the budget, the field is not
  the problem and no value of it is a fix. That is the signal to move the enforcement point, not to keep
  searching for a number.

**Rule of thumb**: a `maxLength` equal to `producer_cap + 1` is a coupling, not a bound. Before replacing
it: find every ceiling the value must pass (validator, column, cache, envelope); measure the value **as
the storage layer receives it**, after all server-side finalization, in bytes at the worst-case encoding;
and check how much of that budget the rest of the payload already spends with the field at its minimum.
If that floor is already over budget, the field ceiling is the wrong lever — bound the payload at the
write boundary instead, where one check covers every already-frozen version. Then let the string ceiling
be a product decision, and separate *accept* from *display* so a long value degrades instead of failing.
