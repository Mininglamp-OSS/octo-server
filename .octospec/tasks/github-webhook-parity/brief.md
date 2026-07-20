---
type: Task
title: "Task: github-webhook-parity"
description: Bring the GitHub incoming-webhook adapter to parity with the GitLab adapter's gitlab-mr-issue-cards work — stop filtering pull_request/issues/issue_comment/release actions, add Source/Target branch + Labels FactSet rows to the PR/Issue cards, and close the corresponding trust-boundary escaping gaps up front.
tags: ["incomingwebhook", "adapter", "github", "card", "trust-boundary", "external-content", "markdown", "testing"]
timestamp: 2026-07-20T00:00:00Z
# --- octospec extension fields ---
slug: github-webhook-parity
upstream: self
source: self
---

# Task: github-webhook-parity

> Mirrors `.octospec/tasks/gitlab-mr-issue-cards/brief.md` — same two changes
> (stop filtering, add FactSet enrichment), same underlying trust-boundary
> reasoning, applied to the sibling GitHub adapter. Read that brief's journal
> for the full incident history (three review rounds each found an instance
> of the same "widen a gate, forget to escape" bug class); this task applies
> those lessons up front instead of discovering them one review round at a
> time.

## Goal

1. GitHub `pull_request`/`issues` cards carry the same kind of structured
   metadata the GitLab MR/Issue cards already have: Source/Target branch (PR
   only, from `pull_request.head.ref`/`base.ref`) and a Labels(N) FactSet row
   (both PR and issue, from `pull_request.labels[]`/`issue.labels[]`).
2. Stop filtering GitHub `pull_request`/`issues`/`issue_comment`/`release`
   events by which action they carry. Previously: PR rendered only
   `opened`/`closed`/`reopened`/`ready_for_review`; issues only
   `opened`/`closed`/`reopened`; issue_comment only `created`; release only
   `published`. Every action now renders (mapped to a readable verb where
   possible, raw-passthrough — escaped — otherwise); the only remaining skip
   is a malformed payload missing the `action` field itself.

## Background

- Card anatomy (`vcsCardData`, `vcsFact`, `escapeCardText`, `cardCodeSpan`,
  `cappedFactValue`) lives in `modules/incomingwebhook/adapter_card.go`,
  shared by both adapters — this task's Labels-fact code reuses the exact
  same `cappedFactValue` helper the GitLab task built (renamed from
  `glCappedFactValue` to drop the misleading GitLab-only prefix, since it's
  now a genuine two-adapter shared helper).
- The GitLab task (`gitlab-mr-issue-cards`) shipped the identical feature
  pair for GitLab and, across three independent review rounds, surfaced the
  same bug class three times: a whitelist gate that only ever returned safe
  literals was widened to pass through raw external input, and the escaping
  fix initially only covered the field the first review flagged (`action`),
  missing a sibling field the same commit also un-gated (`status`), and
  later a third, pre-existing instance in a shared actor-rendering helper
  (`glActor`'s `username` branch) that had never been escaped at all. See
  that task's journal and the resulting pending learning
  (`.octospec/learnings/pending/gitlab-mr-issue-cards-whitelist-gate-sanitizer.md`)
  for the full incident writeup.
- This task applies that learning proactively: every action-verb function
  (`ghPRVerb`/`ghIssueVerb`/`ghCommentVerb`/`ghReleaseVerb`) is designed from
  the start with the same "return value may be raw external input, caller
  MUST escape" contract documented in its own doc comment, and every one of
  its 8 call sites (4 text via `mdInertText`, 4 card via `escapeCardText`)
  was escaped in the same commit — not discovered later via review. The
  pre-existing `ghLogin`/`ghWithRepo` actor/repo-name escaping gap (the
  GitHub-side twin of `glActor`/`glWithRepo`, already fixed for GitLab) was
  also folded in here for the same "brings a known-fixed-elsewhere gap to
  parity" reason `glActor`'s fix was folded into the GitLab PR.

## Load-bearing list

- `modules/incomingwebhook/adapter_github.go` — `ghPRVerb`/`ghIssueVerb`/
  `ghCommentVerb`/`ghReleaseVerb` (new action → verb mappers, full
  passthrough instead of whitelist gates), `renderGitHubPullRequest`/
  `renderGitHubIssues`/`renderGitHubIssueComment`/`renderGitHubRelease`
  (text path), `buildGitHubPullRequestCard`/`buildGitHubIssuesCard`/
  `buildGitHubIssueCommentCard`/`buildGitHubReleaseCard` (card path),
  `ghLogin`/`ghWithRepo` (now escape internally).
- `modules/incomingwebhook/adapter_card.go` — `cappedFactValue` (renamed from
  `glCappedFactValue`; no behavior change) gains a second caller
  (`ghLabelsFact`) alongside GitLab's `glLabelsFact`.
- Trust boundary: this endpoint (like GitLab's) authenticates only a shared
  URL token — it does not verify the payload genuinely originates from
  GitHub (`X-Hub-Signature-256` HMAC verification is optional/unenforced,
  per the file's own header comment). Every field a removed gate exposes, or
  that was already reachable but unescaped, must be escaped at every
  interpolation site on both paths.

## Out of scope

- GitLab adapter — untouched by this task (only a mechanical rename of the
  now-shared `cappedFactValue` helper touches `adapter_gitlab.go`).
- `push` event — has no action-based whitelist to remove (its branching is
  create/delete/degenerate-ref logic, not an action string), so it's
  structurally unchanged; it benefits automatically from the `ghLogin`/
  `ghWithRepo` escaping fix since those are shared helpers.
- The already-tracked deferred follow-up (ref/branch code-span backtick
  breakout, raw URL-destination breakout in markdown link syntax) — this
  task does not fix either for GitHub. Fixing them requires touching
  `renderGitHubPush` (untouched here) and mirrors the same GitLab gaps
  already deferred in `gitlab-mr-issue-cards`; see that journal's follow-up
  section, now updated to note the action-filter removal here widens which
  GitHub actions reach the URL-destination sink (previously only 4 PR
  actions + 3 issue actions + `created` comments + `published` releases
  reached `object_attributes`-equivalent URLs; now all actions do).
- Comment-body blockquote escaping (`issue_comment`'s `comment.body`,
  GitLab's Note `note` field) — pre-existing on both adapters, deliberately
  left symmetric/unfixed to avoid creating a new cross-adapter asymmetry;
  noted as a candidate to fold into the same deferred follow-up.

## Acceptance

- `go test ./modules/incomingwebhook/...` (adapter/card subset) green.
- `golangci-lint run ./modules/incomingwebhook/...` = 0 issues; `gofmt` clean.
- A hostile `action` value (`"**pwn** [x](http://evil.example)"`) renders as
  literal escaped text — never a live link/emphasis — on both the text and
  card paths, for `pull_request`, `issues`, and `release` (regression
  tests, not just manual verification).
- A hostile `sender.login`/`repository.full_name` renders as literal escaped
  text on the text path (regression test — the `ghLogin`/`ghWithRepo` fix).
