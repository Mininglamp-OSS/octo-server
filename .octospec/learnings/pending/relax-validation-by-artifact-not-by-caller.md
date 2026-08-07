---
type: Learning
title: "Express a validation relaxation as a property of the artifact, not of the caller"
description: A gate relaxed via a trusted-caller flag ("this call site may do X") must be kept in sync across every path that re-validates the same artifact — and nothing in the type system enforces that. When the render path opted in but the three persist paths did not, server-rendered frames passed validation on creation and were rejected by the server's own validator before the write, so cards sent once and then froze on first edit. Prefer relaxations keyed on the artifact itself (these exact bytes are vetted): they cannot drift, they make sanitizer bypasses unreachable, and they force interpolated values out of the relaxed position by construction.
tags: ["trust-boundary", "validation", "cardmsg", "cardtmpl", "security"]
timestamp: 2026-08-07T12:00:00Z
# --- octospec extension fields ---
source: review
origin_task: cardtmpl-reasoning-phase-tools-successor
origin_pr: "Mininglamp-OSS/octo-server#712"
status: pending
candidate_rule: trust-boundary
---
# Express a validation relaxation as a property of the artifact, not of the caller

A card template needed to inline two vector icons, which meant relaxing a URL allowlist that
admitted only absolute `http`/`https` so it would also accept `data:image/svg+xml`. The relaxation
could not be unconditional: the same validator also gates bot-authored and webhook-authored cards,
where the content is caller-controlled.

The natural-looking design was a **trusted-caller** flag — a `ValidateOption` the server's own render
path passes and untrusted entry points do not — plus a content filter for the SVG bytes it let through.
Both halves failed, and the two failures are different lessons.

## The caller-keyed flag was applied at one call site out of four

The option went on the render call. But a rendered frame is re-validated by every path that persists
it: the mutation gate before the authoritative write, and two updater paths that re-validate a frame
read back from storage. None of them passed the option, so the server's own validator rejected the
server's own output — after the frame had already rendered and been sent successfully.

The user-visible shape is worse than a plain rejection. Sending worked (that path finalizes without a
second strict validation), so cards were created normally; the card advanced by *edits*, and every
edit failed. Every card created after deploy would render its first frame and freeze mid-stream.

Two things hid it:

- **Nothing types the coupling.** A variadic option is invisible at the call sites that omit it. There
  is no compiler error, no lint, and no natural place for a reviewer to notice absence.
- **The second validation pass had no test coverage.** The edit tests injected a fake mutator that
  records the request and never re-validates, so the entire re-validation path was untested. A guard
  that only one of several equivalent paths applies is exactly the shape a fake collapses.

## The content filter was porous, because the content had escapable syntax

The filter was a substring denylist (`<script`, `<use`, `href`, `url(`, `@import`, `on*=`, …). Review
produced five accepted bypasses, all reproduced:

| payload | why it passed |
| --- | --- |
| `<svg xmlns:s="…"><s:script>` | `"<script"` is not a substring of `"<s:script"` |
| `<svg xmlns:s="…"><s:use/>` | same, for `"<use"` |
| `<style>path{fill:\75 rl(http://evil/x)}</style>` | CSS identifier escape — `"url("` absent |
| `<style>@\69 mport "http://evil/x.css";</style>` | same, for `"@import"` |
| `<handler type="text/javascript">` | SVG 1.2 Tiny event-handler element, not on the list |

SVG is XML *containing* CSS. Both are independently escapable, so `strings.Contains` cannot see
through either. A denylist over any syntax with escape mechanisms is structurally leaky — the count of
known holes is a measure of review effort, not of remaining holes.

## What replaced both: an exact-byte allowlist

The icons are a fixed, small, reviewable set of bytes. So the allowlist stores those exact strings and
matches on full equality — no decoding, no parsing, no normalization, no filtering. It is *smaller*
than what it replaced (the option plumbing and the ~60-line sanitizer both went away) and it removes
four problems at once:

- **Validation no longer depends on the call site**, so no path can disagree with the one that produced
  the artifact. The synchronization bug is not fixed — it is unrepresentable.
- **The sanitizer's holes stop being reachable.** Arbitrary SVG never matches, so all five bypasses and
  any sixth are excluded by the same mechanism, without anyone having to have thought of them.
- **Interpolated values are forced out of the relaxed position.** A template's image URL must now be a
  literal, because `${field}` cannot expand to a value equal to a constant. This closes "a
  caller-supplied data field becomes a `data:` sink" without a separate compile-time rule — and that
  invariant had previously been *assumed* by a comment with nothing enforcing it.
- **Widening the allowlist is a reviewable source change**, which puts a human in front of exactly the
  bytes that would be inlined into every client's DOM. That is the right thing to review; "is this
  caller trusted" is not.

The cost is real and worth naming: template authors cannot use arbitrary icons. For a relaxation this
security-sensitive, that gate is the feature.

## When this generalizes, and when it does not

Artifact-keyed relaxation works when the set of legitimate values is **small, fixed, and enumerable at
build time** — icons, well-known schema identifiers, a fixed set of embed hosts. It does not work when
the legitimate set is open-ended (user avatars, arbitrary attachment URLs); those need real
content/provenance validation, and then the discipline is different: put the relaxation *inside* the
validator as a named mode, have every re-validation path obtain it from the artifact under validation
rather than from an argument, and test each persisting path separately rather than through a fake.

**Rule of thumb**: before adding a "trusted caller may do X" flag to a validator, list every path that
re-validates the same artifact — especially before a write, and especially ones a test double currently
stands in for. If you cannot make all of them agree by construction, key the relaxation on the artifact
instead. `these exact bytes are safe` cannot drift out of sync; `this caller is trusted` will.
