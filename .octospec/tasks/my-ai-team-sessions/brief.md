---
type: Task
title: "Task: my-ai-team-sessions"
description: 为“我的 AI 团队”提供按 Space、用户与 User Bot 隔离的专用父群，每个 session 复用一个 thread。
tags: ["space", "isolation", "auth", "acl", "bot-api", "thread", "wire-contract", "error-response", "i18n", "rate-limit", "adapter", "trust-boundary", "test", "commit"]
timestamp: 2026-09-07T11:43:14+08:00
slug: my-ai-team-sessions
upstream: Mininglamp-OSS/octo-server#849
source: user
---

# Task: my-ai-team-sessions

> Canonical source supplied by the requester: `/Users/kense/Downloads/brief.md`.
> This in-repo copy records the implementation contract; the source brief remains
> the detailed product and cross-client acceptance document.

## Goal

Within one Space, give each human/User-Bot pair one private, server-managed parent
group and represent every independent conversation as a thread. Adding/removing an
AI is idempotent and reversible; the parent group is created lazily on the first
session. Ordinary user messages in these threads automatically target the bound Bot.

## Load-bearing list

- Space membership, active User-Bot ownership, App-Bot rejection, and object-level
  association/group/thread authorization are rechecked on every operation.
- `(space_id,user_uid,bot_id)` and `group_no` bindings are unique; session creation
  uses a caller-scoped idempotency ledger and converges after partial IM failure.
- Dedicated parents have `purpose=ai_session_container`; ordinary group membership
  and administration mutations cannot change their two-member invariant.
- Empty sessions create no source message or parent notification. Session history,
  archive state and channel IDs reuse thread/WuKongIM contracts.
- Bot auto-routing derives its target and stable runtime session key from persisted
  association + complete thread channel ID, rejects bot/system messages, and does
  not trust payload `robot_id` or `purpose`.
- Dedicated parents and sessions are excluded from ordinary group/sidebar surfaces;
  AI/session lists are bounded, stable and SQL-paginated.
- Session controls cover rename, pin/unpin, mute/unmute, archive/unarchive, soft
  delete and per-user history clearing without reopening ordinary thread mutation APIs.
- New API routes use Auth -> SharedUIDRateLimiter -> SpaceMiddleware, reject a missing
  Space explicitly, and return registered localized error envelopes.

## Out of scope

- Shared/third-party Bot entitlement, App Bot support, multiple humans/Bots per
  session, cross-Space sessions, participant changes, and conversion to normal groups.
- Front-end implementation, adapter implementation, old DM migration, cross-session
  memory, physical message deletion, cancellation, or a general workflow/outbox platform.
- Claiming end-to-end adapter isolation without verifying the external adapter build.

## Acceptance

- Add/re-add/remove is idempotent and preserves history; concurrent initialization
  creates one parent with exactly the human and bound Bot.
- Same idempotency key and parameters returns one thread; mismatched replay is rejected;
  different keys create different threads; IM failures never return chat-ready success.
- Missing/cross-Space, inactive/non-owned/App Bots, forged group/thread combinations,
  and non-owner reads/writes are rejected without side effects.
- Ordinary group/member behavior is unchanged, while every normal member/admin/Bot API
  mutation path rejects an AI container.
- A user chat body in a dedicated thread produces one authoritative Bot target and a
  stable `(space_id,bot_id,full_thread_channel_id)` session key; Bot/system messages do
  not loop. Ordinary group mention behavior remains unchanged.
- AI/session pagination, ordering, archive behavior, sidebar hiding, migrations,
  localized errors, and route middleware are covered by tests.
- AI session rename/mute/soft-delete use owner-scoped AI routes. Pinning reuses the
  Space-scoped channel pin contract and is reflected in AI list ordering. “Clear
  chat history” reuses the per-user message offset contract and never deletes the
  Bot's or another user's persisted history.
- The global effective thread auto-archive setting is disabled for rollout
  (`thread_auto_archive_enabled=false`); the code default is already false, and
  deployment verification must also check that no DB override enables it.
- Run focused and full Go tests, i18n extraction/lint, WuKongIM-backed integration tests,
  and API tests. Record exact commands/results in `verification.md`.
- External E1-E5 adapter/client acceptance from the source brief remains an explicit
  handoff if those repositories/runtimes are unavailable in this checkout.
