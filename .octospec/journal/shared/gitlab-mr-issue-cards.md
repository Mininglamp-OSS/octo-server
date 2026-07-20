---
type: Journal
title: "Journal: gitlab-mr-issue-cards"
description: GitLab merge_request/issue cards gained a Source/Target branch + Labels FactSet; the adapter stopped filtering MR/Issue actions and pipeline statuses per explicit product decision. Three independent review passes each caught a real trust-boundary escaping gap in the same file, two introduced by the filter removal and one pre-existing — all three the same "whitelist-gate-as-implicit-sanitizer" bug class.
tags: ["incomingwebhook", "adapter", "gitlab", "card", "trust-boundary", "external-content", "markdown", "code-review"]
timestamp: 2026-07-20T00:00:00Z
# --- octospec extension fields ---
task: gitlab-mr-issue-cards
upstream: self
source: self
---
# Journal: gitlab-mr-issue-cards

## What was done

Five commits on `feat/gitlab-mr-issue-cards`:

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

4. **Fix: escape the pipeline status too (caught by PR review).** After
   the PR was opened, a reviewer (lml2468) found that commit 2 removed
   the *pipeline* status whitelist gate but the raw `ev.ObjectAttributes.Status`
   was still interpolated unescaped in `renderGitLabPipeline`'s text path (the
   pipeline card path was already correctly escaped via `escapeCardText`,
   only the text path had the gap). This is the **identical bug class** as
   commit 3, on the sibling field the earlier review didn't examine — the
   first review had only seen the diff up to commit 2, and my own commit
   3 fix pattern-matched on `glActionVerb` specifically without checking
   whether the same "gate removed, escaping not added" gap existed anywhere
   else the same PR touched. Fixed identically: `mdInertText(status, glActorMax)`
   at both `renderGitLabPipeline` branches, with regression tests for both
   (web_url present/absent).

5. **Fix: escape `glActor`'s `username` branch (pre-existing, folded in on
   re-review).** A second reviewer (yujiawei) re-reviewed after commit 4 and
   found a *third* instance of the same bug class — this one pre-existing,
   byte-identical to `main`, not introduced by this task. `glActor` assumed
   GitLab's restricted username charset (`[a-zA-Z0-9_.-]`) made the `username`
   branch safe to interpolate raw; that assumption does not hold at this
   endpoint's actual trust boundary (it only verifies a shared secret token,
   not that the payload genuinely came from GitLab), so a token holder could
   set `username` to arbitrary markdown-bearing text. Folded into this PR
   (rather than filed separately) since it's the same file, same pattern, and
   the fix is the one-line change already applied twice above:
   `mdInertText(username, glActorMax)`, matching `glActorCard`'s card-path
   equivalent which was already correct. Also addressed two non-blocking
   review nits picked up in the same pass: `formatPipelineDuration` now
   prefixes `>` when it clamps a hostile duration (so a clamped value reads
   distinctly from a genuine ~100h pipeline), and `glCappedFactValue` uses a
   new dedicated `cardFactItemMax` constant instead of reusing the
   actor-name-sized `cardActorMax` for job/label name truncation.

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
- **Escape at the boundary that's actually load-bearing, not by convention
  alone.** `verb` is escaped at each of its 4 interpolation sites (2 text, 2
  card) rather than inside `glActionVerb`, because callers need it as a plain
  string for both a `mdInertText`- and an `escapeCardText`-shaped context.
  `glActor`/`glActorCard`, by contrast, escape *inside* the helper (as of
  commit 5) — there both call sites want the same one string back, so there's
  no reason to push the escaping decision out to callers. Same principle
  ("escape once, correctly, at whichever point makes every caller safe by
  construction"), different shape depending on how many distinct contexts a
  value flows into.
- **Filtering removal is a product decision, not a technical default.** The
  user was shown the concrete consequence before confirming; this is recorded
  here so a future reader doesn't mistake the wide-open behavior for an
  oversight and "fix" it back to filtered without checking history first.

## Process note: three independent review passes, three instances of the same bug class

A first review pass (before commit 3 existed) caught the `action` escaping
gap as HIGH severity, correctly identified that the existing "unknown action"
test didn't actually exercise the raw-passthrough branch (it used
`"approved"`, which is an explicitly-mapped case), and flagged the Jobs/Labels
duplication. All three were fixed in commit 3.

A PR reviewer (lml2468) then found that the fix in commit 3 was *incomplete*:
it treated the bug as specific to `glActionVerb`/`action` and didn't check
whether the same "gate removed → escaping assumption broken" pattern applied
to `status`, which the very same commit-2 change had also un-gated. It had.
This is a direct, concrete instance of the pending learning this task itself
filed (`gitlab-mr-issue-cards-whitelist-gate-sanitizer.md`) — point 4 of that
learning ("when reviewing a widen-this-gate change, ask whether the
restricted output range was load-bearing for escaping anywhere downstream")
should have been applied to *both* fields removed by commit 2, not just the
one a first review happened to flag. Fixed in commit 4.

A second reviewer (yujiawei) re-reviewed after commit 4 and found a *third*
instance — pre-existing in `glActor`, not introduced by this PR, but the same
"an assumption made the field implicitly safe, and the assumption was never
actually enforced by code" shape (here: assumed GitLab's username charset,
rather than a removed whitelist, made escaping unnecessary). Fixed in
commit 5, along with two smaller non-blocking review nits (mochashanyao:
duration-clamp indicator; yujiawei: dedicated fact-item-length constant).
Three passes, three real findings, zero false positives — worth noting for
calibrating how much a single review pass should be trusted on
trust-boundary-classified changes.

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
- **Deferred (yujiawei, PR #610 review): text-path markdown-breakout family
  in GitLab/GitHub adapters.** Two related, pre-existing gaps in the same
  spirit as the bugs this PR fixed for `action`/`status`/`username`, both
  unchanged from `main` and both out of scope to fix in this PR (see
  reasoning below):
  - **Ref/branch code-span backtick breakout.** `glShortRef` doesn't strip
    backticks, and its output goes raw into a `` `%s` `` text-path code span
    at 6 sites: GitLab push branch create/delete, push commit-count line, tag
    push (2 sites), and pipeline (2 sites — the only ones this PR's changes
    actually widen exposure to, by rendering non-terminal statuses that
    previously never reached this code path). A ref/branch name containing a
    literal backtick is not rejected by git's ref-name rules, so this is
    real, not just theoretical. The card path is already safe (`cardCodeSpan`
    strips backticks via `mdCodeSpanText`).
  - **Raw URL destinations in markdown link syntax.** `renderGitLabMergeRequest`/
    `renderGitLabIssue`/`renderGitLabNote` place `object_attributes.url` (and
    the note URL) directly as a link destination `](%s)`; a `)` in that
    token can close the link early and let the rest of the string inject
    forged markdown after it. Same class of bug, different sink — flagged by
    yujiawei's second review pass as worth closing in the same follow-up
    rather than treating as unrelated.
  - **Not fixed here**: a correct fix for either needs to touch
    `renderGitLabPush`/`renderGitLabTagPush`/`renderGitLabNote` (functions
    this PR never modified) and, per this repo's adapter-parity rule, the
    equivalent sinks in `adapter_github.go` — a partial, pipeline-only patch
    here would leave push/tag/note/GitHub with the identical gap, a worse,
    inconsistent posture than not touching it. **Tracked as one follow-up
    task**: harden every GitLab *and* GitHub text-path ref/branch code span
    (route through `mdCodeSpanText`, mirroring the card path) *and* every
    raw URL-destination interpolation (needs a `safeMarkdownURL`-style
    destination validator/escaper on the text path — `adapter.go` already
    has `safeMarkdownURL` for a different purpose, worth checking if it
    applies directly).

- **Noted, not tracked as a task (yujiawei, PR #610 review, both explicitly
  "awareness only" / cosmetic, no panic or injection risk):**
  - `int(ev.ObjectAttributes.Duration)` on an absurd external JSON float
    (e.g. `1e100`) can saturate before `formatPipelineDuration`'s upper
    clamp runs, silently dropping the duration fact instead of showing
    `>100h 0m`. Safe (no panic, no absurd string), just a display gap for a
    payload no real GitLab instance would ever send.
  - The text path still clamps `verb`/`status` with `glActorMax` while the
    card path has a dedicated `cardFactItemMax` — harmless today (same
    value), just a naming/domain mismatch if either constant's value
    diverges later.
