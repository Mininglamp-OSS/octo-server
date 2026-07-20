---
type: Journal
title: "Journal: github-webhook-parity"
description: GitHub pull_request/issues cards gained Source/Target branch + Labels FactSet rows, mirroring GitLab; the adapter stopped filtering pull_request/issues/issue_comment/release actions. Applied the whitelist-gate-as-implicit-sanitizer lesson from the GitLab task up front — every widened field was escaped in the same commit, plus a pre-existing actor/repo-name escaping gap was closed for parity with GitLab's already-fixed glActor.
tags: ["incomingwebhook", "adapter", "github", "card", "trust-boundary", "external-content", "markdown"]
timestamp: 2026-07-20T00:00:00Z
# --- octospec extension fields ---
task: github-webhook-parity
upstream: self
source: self
---
# Journal: github-webhook-parity

## What was done

One commit on `feat/github-webhook-parity` (following the GitLab task's
retrospective, applied proactively this time rather than across several
review-driven follow-ups):

1. **Stop filtering GitHub events.** Replaced the closed-whitelist switches
   in `renderGitHubPullRequest`/`renderGitHubIssues`/`renderGitHubIssueComment`/
   `renderGitHubRelease` (and their card-path twins) with four new mapper
   functions — `ghPRVerb`, `ghIssueVerb`, `ghCommentVerb`, `ghReleaseVerb` —
   each documented with the same "raw-passthrough fallback, caller must
   escape" contract `glActionVerb` carries. Known actions map to readable
   English verbs (e.g. `synchronize` → "pushed new commits to",
   `ready_for_review` → "marked ready for review"); genuinely unknown/future
   GitHub action values pass through raw. The only remaining skip is a
   missing `action` field.

2. **Add Source/Target branch + Labels FactSet to the PR card, Labels to the
   Issue card.** `ghPullRequestEvent`/`ghIssuesEvent` now parse
   `pull_request.base.ref`/`head.ref`/`labels[]` and `issue.labels[]` (all
   previously unparsed — nested under `pull_request`/`issue` rather than
   top-level like GitLab's `labels[]`, hence the separate `ghLabel` type and
   `ghLabelsFact` helper, which is otherwise a straight port of GitLab's
   `glLabelsFact`). Card-only; text degrade path unaffected.

3. **Escape every field the filter removal exposed, in this same commit.**
   All 8 verb-mapper call sites (2 events × {text, card} × ... — actually 4
   render functions × {text via `mdInertText`, card via `escapeCardText`} =
   8 sites) escape the verb before interpolation. Verified by grep + manual
   read of every call site before committing (see Verification) rather than
   relying on a later review pass to find the gap, which is what happened
   three times on the GitLab task.

4. **Fold in the pre-existing `ghLogin`/`ghWithRepo` escaping gap.** These
   are GitHub's structural twins of GitLab's `glActor`/`glWithRepo`, which
   were already fixed in `gitlab-mr-issue-cards` (`glActor`'s `username`
   branch was unescaped on the same "restricted charset" assumption that
   doesn't hold once you account for this endpoint only checking a shared
   token, not verifying genuine GitHub origin). `ghLogin`/`ghWithRepo` had
   the identical gap, entirely unfixed until now. Folded into this task
   rather than deferred, because (a) it's the exact same fix pattern already
   established and reviewed for GitLab, (b) it's a genuine asymmetry-closer
   (GitLab already has this fix; GitHub didn't), not a new asymmetry, and
   (c) `renderGitHubPush`/`buildGitHubPushCard` — untouched by the action-
   filter work — get the fix for free since `ghLogin`/`ghWithRepo` are
   shared helpers.

5. **Renamed `glCappedFactValue` → `cappedFactValue`.** It's shared
   `adapter_card.go` infra used by both adapters' Jobs/Labels facts; leaving
   the GitLab-specific prefix on a function GitHub's `ghLabelsFact` now also
   calls would be actively misleading to a future reader. Mechanical rename
   only, no behavior change (`sed` across the 3 files that referenced it).

## Load-bearing decisions

- **Apply a filed learning proactively, not just reactively.** The GitLab
  task's pending learning
  (`gitlab-mr-issue-cards-whitelist-gate-sanitizer.md`) exists specifically
  so this exact situation — widening another action-based gate — doesn't
  repeat the same discovery-by-review cycle. Concretely: before writing any
  code, enumerated every field the four whitelist gates were about to stop
  protecting (the 4 actions themselves) and escaped all 8 interpolation
  sites in the same commit, then separately grepped afterward to confirm no
  site was missed (see Verification). This is the "enumerate every field the
  same commit un-gates, not just the one already flagged" step the learning
  calls for, done at write-time instead of via a third review round.
- **Fold in a known-elsewhere-fixed pre-existing gap; defer known-nowhere-
  fixed ones.** `ghLogin`/`ghWithRepo` (GitLab already has the fix) got
  folded in. The ref/branch code-span backtick issue and the URL-destination
  breakout (neither adapter has ever had the fix) stayed deferred — fixing
  those properly needs `renderGitHubPush`/`renderGitLabPush`/tag/note, none
  of which this task touches, plus doing both adapters in the same pass per
  the repo's adapter-parity rule. Same boundary the GitLab task drew; see
  its journal for the reasoning this mirrors.
- **`ghCommentVerb`'s known values are full phrases (`"commented on"`,
  `"edited a comment on"`), not bare verbs**, unlike the other three
  mappers. This keeps the single `"**actor** %s [#N title]"` / `"%s %s an
  issue"` template working uniformly across all four event types without a
  special-cased sentence structure for comments — the phrase already
  includes the preposition the template needs.

## Verification

- Grepped every `ghPRVerb(`/`ghIssueVerb(`/`ghCommentVerb(`/`ghReleaseVerb(`
  call site (8 total) and manually confirmed each one's `verb` value is
  wrapped in `mdInertText`/`escapeCardText` before reaching a `fmt.Sprintf`
  destined for rendered output — done *before* running the hostile-payload
  tests below, as a design check, not just a test-driven discovery.
- `go test ./modules/incomingwebhook/... -run '<adapter/card subset>'` green
  on the first run after implementation (no red-green-red cycle this time,
  unlike the GitLab task's multi-round review churn) — includes hostile-
  action regression tests (text + card, PR/Issues/Release) and hostile-
  login/repo-name tests (text path, mirroring GitLab's equivalent).
- `golangci-lint run ./modules/incomingwebhook/...` = 0 issues; `gofmt`
  clean; `make i18n-lint` OK (no error-code changes).
- Manual render check (throwaway test, not committed) confirmed realistic
  PR/issue payloads render sensibly on both paths (see PR description for
  the actual output).

## Review round (PR #611)

Three independent reviewers (lml2468, Jerry-Xin, yujiawei) approved with no
P0/P1 findings — the proactive escaping (see "Load-bearing decisions" above)
meant none of them found a repeat of the GitLab task's "widened gate, missed
an escape site" bug. One P2 non-blocking finding, folded in directly rather
than deferred (all three reviews were in hand, so this was a single batched
decision, not a one-at-a-time back-and-forth):

- **yujiawei**: `ready_for_review`/`converted_to_draft` are complete
  predicate phrases ("marked ready for review", "converted to draft"), not
  transitive verbs — running them through the generic `"%s %s pull request
  [#N]"` / `"%s %s a pull request"` templates put the object in the wrong
  place ("actor marked ready for review a pull request"). Fixed by special-
  casing these two known-literal actions in both
  `renderGitHubPullRequest`/`buildGitHubPullRequestCard` to place the PR
  reference mid-phrase ("actor marked pull request [#N] ready for review" /
  "actor converted a pull request to draft"); every other action keeps the
  uniform template unchanged. No escaping impact — both branches match on
  the literal `ev.Action` string, not on interpolated content.
- Jerry-Xin's and lml2468's findings were both "no action needed, already
  tracked" — restating the same deferred URL-destination / comment-body
  items this journal already lists below.

## Follow-ups / notes

- The already-tracked deferred hardening item (ref/branch code-span
  backtick breakout + URL-destination breakout — see
  `gitlab-mr-issue-cards.md`'s Follow-ups section) now has its GitHub-side
  exposure widened the same way GitLab's pipeline widening did: previously
  only 4 PR actions + 3 issue actions + `created` comments + `published`
  releases reached the affected `html_url`/`comment.html_url` sinks; now
  every action does. Still not fixed here, for the same reasons (needs
  `renderGitHubPush` + both adapters in one consistent pass).
- Comment-body blockquote escaping (`issue_comment`'s `comment.body`) is
  still unescaped on the text path, matching GitLab's Note `note` field —
  deliberately left symmetric. Worth folding into the same deferred
  follow-up rather than fixing one adapter first.
- GitHub's `push` event text/card rendering is otherwise untouched — no
  action whitelist existed there to remove.
