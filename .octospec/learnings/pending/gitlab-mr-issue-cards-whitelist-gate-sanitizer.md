---
type: Learning
title: "Removing a whitelist gate on external input silently removes its implicit escaping guarantee"
description: A switch that only ever returns hardcoded safe literals doubles as an implicit sanitizer; widening its default case to pass through raw external input reopens markdown/link injection at every call site that relied on the old guarantee, unless each site is updated to explicitly escape.
tags: ["trust-boundary", "escaping", "markdown-injection", "adapter", "webhook", "code-review"]
timestamp: 2026-07-20T00:00:00Z
# --- octospec extension fields ---
source: self
origin_task: gitlab-mr-issue-cards
origin_pr: self
status: pending
candidate_rule: trust-boundary
---

# Removing a whitelist gate silently removes an implicit escaping guarantee

## Context

`modules/incomingwebhook/adapter_gitlab.go`'s `glActionVerb` mapped GitLab's
`object_attributes.action` (an external, unvalidated string — any holder of
the webhook URL token can set it to anything) to a render verb. Originally it
was a strict whitelist: `open`/`close`/`reopen`/`merge` → four hardcoded
literals, anything else → `""` (skip). Because the only values it could ever
*return* were those four safe literals, none of its callers escaped the
result before interpolating it into markdown text or a card headline — there
was nothing to escape.

## The trap

A later change (per an explicit product decision to stop filtering GitLab
events by action) widened the `default` case from `return ""` to
`return action` — i.e., "unknown actions render too, using their raw name."
This is a reasonable product change on its own. But it silently deleted the
whitelist's second, unstated job: every caller's assumption that this
function's output was always a safe literal became false, and none of the
four call sites (2 text-path, 2 card-path) were updated to escape the now-
possibly-hostile value. The result: `action: "**pwn** [x](http://evil)"`
rendered as forged bold + a live link in the delivered message. This shipped
in the same commit as the filter-removal and was only caught by an
independent code-review pass — the "unknown action" test that already existed
didn't catch it either, because it exercised `"approved"` (an explicitly
mapped, safe case), not a genuinely unmapped value.

## It recurred in the same PR, on the sibling field

The same commit that widened `glActionVerb` *also* removed the equivalent
whitelist gate on GitLab pipeline `status` (`success`/`failed`/`canceled` →
any non-empty value), in `renderGitLabPipeline`. The fix for `action` shipped
in a follow-up commit — but that fix was scoped to `glActionVerb` specifically
and didn't re-check `status`, which had the identical shape of bug: raw
`ev.ObjectAttributes.Status` interpolated unescaped into the text-path
markdown once its gate was gone. It took a **second**, independent review (a
human PR review, after a first AI-delegated review had already caught and
"fixed" the `action` half) to catch it. Point 4 below is not hypothetical —
it's exactly the check that would have caught this the first time, and it
needed to be applied to *every* field a gate-removal commit touches, not just
the one an initial finding happened to name.

## The rule

When a function's return value has been implicitly safe only because its
domain was a small hardcoded whitelist, and a change widens that domain to
include (or pass through) external input:

1. Treat the return value as untrusted from that point on, at **every**
   existing call site — not just new ones.
2. Escape at the interpolation site (the boundary the caller can't cross),
   using the same escaper already used for other external fields in that
   context (`mdInertText` for GitLab adapter text-path markdown,
   `escapeCardText` for the octo/v1 card leaf) — see
   `.octospec/rules/trust-boundary.md`.
3. Write a regression test with a value that is genuinely outside the old
   whitelist and contains markdown metacharacters — a test using a value the
   new code *happens* to map explicitly (like `"approved"` here) proves
   nothing about the new raw-passthrough branch.
4. When reviewing a "widen this gate" change, explicitly ask: was this gate's
   restricted output range load-bearing for escaping anywhere downstream? —
   and enumerate **every** field the same commit un-gated, not just the one
   already flagged. A gate-removal commit that touches N fields needs this
   check done N times, independently; fixing the first one found does not
   imply the others were checked.

## Candidate rule promotion

Proposing to fold this into `.octospec/rules/trust-boundary.md` as a named
sub-case of "escape at the right boundary": *whitelist-gates-as-implicit-
sanitizers* — call out that narrowing a function's possible outputs to a
fixed safe set is a common, easy-to-miss way code becomes implicitly
unescaped, and that widening such a gate is itself a trust-boundary-relevant
change requiring the same review depth as adding a new external field.
