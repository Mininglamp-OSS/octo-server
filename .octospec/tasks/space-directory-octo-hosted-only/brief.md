---
type: Task
title: "Task: space-directory-octo-hosted-only"
description: GET /v1/space/directory lists only octo_hosted bots that belong to an eligible human in the same Space.
tags: ["space", "isolation", "wire-contract", "testing", "commit"]
timestamp: 2026-09-09T00:00:00+08:00
slug: space-directory-octo-hosted-only
upstream: product follow-up to space-cloud-agents-by-owner
source: user
---

# Task: space-directory-octo-hosted-only

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Change only `GET /v1/space/directory`.

A bot appears in `agents` (and counts toward `agent_count`) if and only if
**both** are true:

1. **Platform-hosted** — `robot.agent_hosting = 'octo_hosted'`.
2. **Owned by an eligible human in this Space** — `robot.creator_uid` is a
   non-empty uid of an active human Space member (`user.robot = 0`,
   `user.status = 1`, `is_destroy <> 2`, `space_member.status = 1`, not a
   system account). Otherwise the bot is dropped: no owner row, no hanging
   agent on someone else.

Empty hosting, retracted hosting, `self_hosted`, and third-party
`<vendor>_hosted` slugs must not appear.

This supersedes the hosting clause of `space-cloud-agents-by-owner`
(`agent_hosting <> 'self_hosted'`). The human-owner rule is not new: the
existing agent query already INNER JOINs the creator as a human in the same
Space and the handler fail-closes if the owner is missing from the human list.
This task keeps that rule, pins the missing "creator is itself a bot" case, and
makes the keyword subquery use the same owner predicate.

## Background

The live directory returned test bots with `hosting: ""` (never reported)
alongside `octo_hosted` clones. Product intent is now: only hosted bots, meaning
`octo_hosted`, and only when they hang off a real human.

Hosting is still self-reported via `POST /v1/bot/register`. Create paths do not
write the column, so a newly minted platform bot is invisible until its first
hosting report. That disappearance is accepted.

`agent_hosting` remains a display filter, never an authz or tenancy signal.

### Human-owner filter (already in `queryDirectoryAgents`)

```text
r.creator_uid <> ''
INNER JOIN space_member owner_sm
  ON owner_sm.space_id = bot_sm.space_id
 AND owner_sm.uid = r.creator_uid
 AND owner_sm.status = 1
INNER JOIN user owner_u
  ON owner_u.uid = owner_sm.uid
 AND owner_u.robot = 0
 AND owner_u.status = 1
 AND COALESCE(owner_u.is_destroy, 0) <> 2
AND owner_sm.uid NOT IN SystemBotList
```

Go assembly then attaches an agent only when `creator_uid` is in the human
owner index; otherwise it is discarded (no ownerless top-level row).

That already drops:

| Case | Why it is dropped |
|---|---|
| `creator_uid = ''` | `creator_uid <> ''` |
| Creator not in this Space | `owner_sm` INNER JOIN |
| Creator is a bot (`user.robot = 1`) | `owner_u.robot = 0` |
| Creator inactive / destroyed | `owner_u.status` / `is_destroy` |
| Creator is a system account | `NOT IN SystemBotList` |
| Owner missing from the human query | handler fail-close |

The keyword owner-match subquery must use the same joins. Today it only checks
`creator_uid <> ''` and relies on the outer human list; this task makes the
visible-bot definition identical in both SQLs.

## Load-bearing list

- **`space` / `isolation`** — directory is a Space-scoped cross-member read.
  Auth chain, `space_id` query selector, and Space middleware stay unchanged.
- **`wire-contract`** — `agents` / `agent_count` / `agents_truncated` /
  `only_with_agents` / `keyword` now operate on the `octo_hosted` + eligible
  human-owner set. Response shape is unchanged. Bots still never appear as
  top-level rows.
- **`testing`** — rewrite assertions that currently require unreported /
  withdrawn / vendor bots to appear; pin creator-is-bot and empty-creator
  exclusions on both the default listing and `keyword`.
- **`commit`** — English Conventional Commits.

## Out of scope

- Project agent eligibility (`classifyAgentsTx` / `members/add`).
- AI Team grouping (`cloud_clone` is already `octo_hosted`).
- Authoritative provision-source column.
- New error codes, migrations, or rate-limit changes.
- Cleaning leftover Space members such as `ProjectApiTest`.
- `GET /v1/space/:id/members` and `/v1/robot/space_bots`.
- Frontend.

## Acceptance

**Visible bot predicate (`modules/space` only)**

- `octo_hosted` + eligible human owner in this Space → appears, including when
  `agent_reported_hosting_at` is NULL (JSON `null`, not omitted).
- `agent_hosting = ''` (never reported) → hidden.
- `agent_hosting = ''` with a reported timestamp (retracted) → hidden.
- `self_hosted` → hidden.
- `vendor_hosted` (any other slug) → hidden.
- `creator_uid = ''` → hidden, even if `octo_hosted`.
- Creator is not a Space member → hidden (no ownerless row).
- Creator `user.robot = 1` → hidden.
- Creator inactive, destroyed, or a system account → hidden.
- `agent_count` / `agents_truncated` / the per-owner 50-cap count only the
  visible set.
- `only_with_agents=true` keeps a human only when that human owns at least one
  visible bot.
- `keyword` matches visible Bot names only. Names of hidden bots (wrong
  hosting or no eligible human owner) must not surface anyone.

**Gates**

- `go test ./modules/space/ -count=1 -run 'TestSpaceDirectory|TestQueryDirectory|TestDirectory'`
- `golangci-lint run ./modules/space/...` if available.
- Diff does not touch `modules/project/**`.
- No new SQL files under `modules/*/sql/`.
- No new error codes.
