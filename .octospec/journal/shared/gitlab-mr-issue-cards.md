---
type: Journal
title: "Journal: gitlab-mr-issue-cards"
description: GitLab merge_request/issue cards gained a Source/Target branch + Labels FactSet; the adapter stopped filtering MR/Issue actions and pipeline statuses per explicit product decision. A follow-up review caught and fixed a real trust-boundary bug introduced by the filter removal.
tags: ["incomingwebhook", "adapter", "gitlab", "card", "trust-boundary", "external-content", "markdown", "code-review"]
timestamp: 2026-07-20T00:00:00Z
# --- octospec extension fields ---
task: gitlab-mr-issue-cards
upstream: self
source: self
---
# Journal: gitlab-mr-issue-cards

## What was done

Three commits on `feat/gitlab-mr-issue-cards`:

1. **Add source/target branch + labels to GitLab MR/Issue cards.** The
   `merge_request`/`issue` InteractiveCards (shipped in #596) only carried a
   bare "actor verb an MR/issue" headline + numbered title. Added a
   Source/Target branch FactSet (MR only, each row independent so a payload
   missing one still shows the other) and a shared `Labels (N)` FactSet row
   (both MR and issue), parsing `object_attributes.source_branch`/
   `target_branch` and the top-level `labels[]` array — all previously
   unparsed. Card-only, same convention as the pipeline card's Duration/Jobs:
   the plain-text degrade path is untouched, so flag-off bytes are identical
   to history.

2. **Stop filtering GitLab MR/Issue actions and pipeline statuses.** Per
   explicit, twice-confirmed instruction from the requesting user (after being
   shown the concrete spam tradeoff — an active MR fires `update` per push; a
   pipeline fires `pending`→`running`→terminal): `glActionVerb`'s `default`
   case now falls back to the raw action string instead of returning `""`
   (which signaled skip), and `renderGitLabPipeline`/`buildGitLabPipelineCard`
   render for any non-empty status instead of gating on a fixed
   success/failed/canceled switch. The only remaining skip is a genuinely
   missing action/status field.

3. **Fix: escape the action verb before interpolation.** A follow-up code
   review (see below) found that commit 2's raw-passthrough fallback
   interpolated the *unescaped* external `action` field into both the
   text-path markdown and the card headline. Fixed by escaping `verb` at
   every call site (`mdInertText` text path, `escapeCardText` card path), and
   documented the contract on `glActionVerb` itself. Also extracted the
   duplicated "cap + escape + join" logic (pipeline Jobs fact, new Labels
   fact) into a shared `glCappedFactValue` helper, which incidentally fixed a
   minor bug where a blank label title would inflate the `Labels(N)` count
   with an empty slot.

## Load-bearing decisions

- **A whitelist gate doubles as an implicit sanitizer — removing it does not
  remove the need to escape.** Before commit 2, `glActionVerb` only ever
  returned one of four hardcoded, injection-free literals
  (`opened`/`closed`/`reopened`/`merged`); the fixed whitelist made explicit
  escaping unnecessary in practice. Widening the function to fall through to
  raw external input silently deleted that guarantee without anyone changing
  the render call sites — the bug shipped in the same commit as the filter
  removal and was only caught by an independent review pass. See the pending
  learning below.
- **Escape at the leaf, not at the source-of-truth function.** The fix escapes
  `verb` at each of the 4 interpolation sites (2 text, 2 card) rather than
  inside `glActionVerb` itself, matching this file's existing convention
  (`glActor`/`glActorCard` are also un-escaped and escaped by their callers)
  and keeping the text/card paths' different escapers (`mdInertText` vs
  `escapeCardText`) correctly separated.
- **Filtering removal is a product decision, not a technical default.** The
  user was shown the concrete consequence before confirming; this is recorded
  here so a future reader doesn't mistake the wide-open behavior for an
  oversight and "fix" it back to filtered without checking history first.

## Process note: delegated review caught a real bug

The user asked to delegate a code review to a fresh Opus 4.8 subagent (via the
`Agent` tool, `code-review` skill, high effort) against the diff at that point
(commits 1+2, before commit 3 existed). It correctly identified the escaping
gap as HIGH severity, correctly identified that the existing "unknown action"
test didn't actually exercise the raw-passthrough branch (it used `"approved"`,
which is an explicitly-mapped case), and flagged the Jobs/Labels duplication.
All three were fixed in commit 3, with new regression tests using the review's
own example payload (`"**pwn** [x](http://evil.example)"`) on both the text and
card paths, for both MR and issue events.

## Verification

- `go test ./modules/incomingwebhook/... -run '<adapter/card subset>'` green
  (including the new injection regression tests).
- `golangci-lint run ./modules/incomingwebhook/...` = 0 issues; `gofmt` clean.
- Manual render check (throwaway test, not committed) confirmed the actual
  rendered text for a realistic MR/pipeline payload before/after each change.

## Follow-ups / notes

- GitHub adapter's PR/issue cards remain unenriched (no branch/labels
  FactSet) and still gate on a fixed action whitelist — out of scope here,
  the user scoped this task to GitLab only.
- If message volume from unfiltered MR `update`/pipeline non-terminal statuses
  turns out to be a real problem in production, the filter can be
  reintroduced at the same two gate points (`glActionVerb`'s default case,
  `renderGitLabPipeline`/`buildGitLabPipelineCard`'s status check) — now with
  the escaping fix in place regardless of which way that goes.
