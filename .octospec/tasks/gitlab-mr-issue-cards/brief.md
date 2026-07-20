---
type: Task
title: "Task: gitlab-mr-issue-cards"
description: GitLab merge_request/issue InteractiveCards gain a Source/Target branch + Labels FactSet (mirroring the existing pipeline card), and the GitLab adapter stops filtering events by MR/Issue action or pipeline status — every action/status now renders, both text and card paths.
tags: ["incomingwebhook", "adapter", "gitlab", "card", "trust-boundary", "external-content", "markdown", "testing"]
timestamp: 2026-07-20T00:00:00Z
# --- octospec extension fields ---
slug: gitlab-mr-issue-cards
upstream: self
source: self
---

# Task: gitlab-mr-issue-cards

> Builds directly on `.octospec/tasks/webhook-cardmsg-adapter/brief.md` (#596),
> which shipped the GitHub/GitLab InteractiveCard rendering and gave the GitLab
> pipeline card a Branch/Status/Duration/Jobs FactSet. This task extends the
> same FactSet treatment to the merge_request/issue cards, then (per explicit,
> twice-confirmed product decision from the requesting user) removes the
> anti-spam action/status filtering the adapter had carried since #297/#423.

## Goal

1. GitLab `merge_request`/`issue` cards carry the same kind of structured
   metadata the pipeline card already has: Source/Target branch (MR only) and
   a Labels(N) FactSet row (both MR and issue), instead of a bare
   "actor verb an MR/issue" headline + numbered title.
2. Stop filtering GitLab events by which action/status they carry. Previously
   MR/Issue only rendered for `open`/`close`/`reopen`/`merge` and pipeline only
   for `success`/`failed`/`canceled` — everything else was silently skipped as
   "spam". Every action and every status now renders; the only remaining skip
   is a genuinely malformed payload with the action/status field itself
   missing (nothing to describe).

## Background

- Card anatomy (`vcsCardData`, `vcsFact`, `escapeCardText`, `cardCodeSpan`,
  `httpURLForCard`) lives in `modules/incomingwebhook/adapter_card.go`,
  shared by the GitHub and GitLab adapters — see the webhook-cardmsg-adapter
  brief for the full escaping/parity rationale.
- `buildGitLabPipelineCard` was already the precedent for a card-only FactSet
  (Branch/Status/Duration/Jobs) added without touching the plain-text degrade
  path, keeping flag-off bytes identical to history. This task's MR/Issue
  facts follow the same convention.
- The filtering-removal was a deliberate scope decision made mid-conversation
  with the requesting user, not a default choice — the user was shown the
  concrete spam consequence (an active MR firing an `update` per push; a
  pipeline firing `pending`→`running`→terminal) and explicitly said to remove
  the filter anyway.

## Load-bearing list

- `modules/incomingwebhook/adapter_gitlab.go` — `glActionVerb` (MR/Issue
  action → verb mapping, now a full passthrough instead of a whitelist gate),
  `renderGitLabMergeRequest`/`renderGitLabIssue` (text path),
  `buildGitLabMergeRequestCard`/`buildGitLabIssueCard` (card path),
  `renderGitLabPipeline`/`buildGitLabPipelineCard` (status gate removed).
- `modules/incomingwebhook/adapter_card.go` — shared card leaf escaping
  (`escapeCardText`) and the new `glCappedFactValue` helper (dedups the
  pipeline Jobs fact and the new Labels fact's cap/escape/join logic).
- Trust boundary: `glActionVerb`'s raw-passthrough fallback returns
  **unvalidated external input** (any URL-token holder can set `action` to
  arbitrary text — GitLab does not enumerate it on the wire, and even if it
  did, this endpoint doesn't verify the payload is genuinely from GitLab
  beyond the shared secret). Every interpolation site must escape it
  (`mdInertText` text path, `escapeCardText` card path) — see Follow-ups.

## Out of scope

- GitHub adapter (`adapter_github.go`) — its PR/issue cards remain a bare
  headline + link with no branch/labels FactSet, and it still gates on a
  fixed action whitelist. Not touched; the user scoped this to GitLab only.
- GitLab `Note Hook` system-comment filtering (`ev.ObjectAttributes.System`)
  — unrelated filter, not discussed, left as-is.
- Unsupported GitLab event *types* (Job Hook, Wiki Page Hook, etc.) — still
  skip via `parseGitLabPush`'s default case; this task only removed filtering
  *within* the already-supported event types.

## Acceptance

- `go test ./modules/incomingwebhook/...` (adapter/card subset) green.
- `golangci-lint run ./modules/incomingwebhook/...` = 0 issues; `gofmt` clean.
- A hostile `action` value (`"**pwn** [x](http://evil.example)"`) renders as
  literal escaped text — never a live link/emphasis — on both the text and
  card paths, for both MR and issue events (regression test, not just manual
  verification: `TestParseGitLabPush_MergeRequest`/`_Issue` and
  `TestBuildGitLabMergeRequestCard_Facts`/`TestBuildGitLabIssueCard_Facts`).
