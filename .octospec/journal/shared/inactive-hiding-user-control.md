---
type: Journal
title: "inactive-hiding-user-control: groundwork for handing inactive-hiding control to users (Batch 1)"
description: One visibility predicate for archived threads across the two sync read paths that are conversation lists (the sidebar follow tab is deliberately excluded — it is the clients' follow-state source), thread auto-archive policy moved from env into system_settings with a two-stage-decay ordering guard, and unread/pinned/system-bot exemptions so a hiding window can never swallow unread.
tags: [thread, message, common, wire-contract, system-setting, space, isolation, error-response, i18n, observability, testing]
timestamp: 2026-07-29T13:58:15Z
---

# inactive-hiding-user-control (Batch 1)

Branch `claude/inactive-conversation-hiding-elvg66`. Started as two isolated
consistency fixes ("archived threads behave differently on two endpoints",
"archive config lives in env while the hiding window lives in the DB") and was
re-framed mid-plan into what those fixes are actually *for*: **handing the
"when does a quiet conversation leave my list" decision back to users and
thread creators, with global config demoted to a default.**

Batch 1 ships only the groundwork — no new user-facing config. Per-user windows
(Batch 2) and per-thread archive durations (Batch 3) sit on top.

## What was done

1. **P0 — one visibility predicate for archived threads, on the read paths that
   are conversation lists.** `/v1/conversation/sync` had dropped archived threads
   for a long time (`QueryActiveShortIDs`, `status=active`); `/v1/sidebar/sync`
   still returned them, so the same thread vanished on mobile while staying in
   the web sidebar. `dropArchivedThreadItems` converges the sidebar's **recent
   tab** on the same semantics, after `SidebarItem.Status` is backfilled. Only an
   explicit `archived` is dropped; `Status == 0` (query failed / row missing) is
   kept — the fail-open direction every thread read path here already takes.

   The **follow tab is deliberately excluded**, and that exclusion is the most
   important thing in this entry — see the follow-state gotcha below. A first
   revision dropped archived from both tabs and had to be reversed in review.
2. **P1 — archive policy env → `system_settings`.** New `thread.auto_archive_enabled`
   / `auto_archive_days` resolve DB → env → code default. **No row is written by
   the migration**, so the resolved value on rollout is byte-identical to what
   each deployment's env already produces. The worker re-reads the policy every
   tick (injected resolver, `cfg` fallback keeps all 15 existing worker tests
   untouched), so an admin change lands within one tick instead of needing a
   restart — and `effective_value` finally makes "is archiving on, with what
   window" a queryable fact.
3. **P1 — two-stage-decay ordering guard.** `archive_days >= recent_filter_thread_days`,
   enforced on **both** keys' write entry points and against the post-merge
   state (item order irrelevant). Guarding only the thread side would leave the
   sidebar side as a way around it.
4. **P2 — unread / pinned / system-bot exemptions.** Both endpoints share
   `keepDespiteRecentWindow`. `buildRecentItems` now resolves `pinned` *before*
   the cutoff check; it previously filtered first and tagged `IsPinned`
   afterwards, so a pinned-but-quiet channel was dropped before anything knew it
   was pinned.

## Structural learning: which axis a knob belongs on

The design went through two wrong turns worth recording.

First I argued the per-user hiding layer was **redundant** with archiving and
should be retired. Wrong premise: Discord — which this repo's 3-day window was
modelled on — has *both* layers (auto-archive **and** collapsed categories).

Then I argued auto-archive should be stretched to a monthly scale so the two
layers stop overlapping. Also wrong axis: Discord's `auto_archive_duration` is
**per-thread** (60 / 1440 / 4320 / 10080 minutes) with a per-channel default —
this repo copied the *number* 4320 and made it a global env constant, losing
the expressiveness that made 3 days acceptable there.

The rule that fell out, and that Batch 2/3 are built on:

| knob | correct axis | because |
|---|---|---|
| archive duration | **per-thread** (+ group default) | a property of the topic |
| hiding window | **per-user** (+ global default) | a property of the viewer |
| global config | **default only** | not the only truth |

The corollary that makes P2 non-optional: Discord's per-user layer always lets
**unread** through. Ours did not. A hiding mechanism that can swallow unread is
not safe to hand to users, so the exemptions ship *with* the groundwork rather
than after it.

Second corollary, for Batch 2: a per-user window **cannot** rescue a thread the
global archive window already removed, because archiving is a global write and
the window is a per-viewer read projection. The global archive window is a hard
ceiling on every user's window — which is exactly what the ordering guard
encodes.

## Gotchas worth remembering

- **`QueryActiveByGroupShortIDs` is misnamed.** Its predicate is
  `status != Deleted`, so it returns active **and** archived. Every "is this
  thread alive" judgement on the sidebar was built on it, and that is the direct
  source of the two endpoints diverging. Do not infer semantics from the name.
- **Empty `system_setting` payloads mean "reset to default", and a merge guard
  must resolve them.** `settingTypeInt` documents `""` as reset, and
  `normaliseBool("")` is accepted too. A cross-key guard that carries the
  *current* value forward for `""` under-detects: clearing `auto_archive_days`
  from 30 resets it to 3 and can land below the recent window undetected. Found
  by reading the merge against the write path, not by a failing test — the tests
  were green. Promoted to `learnings/pending/`.
- **A response that carries a state flag is a state source, not a display list.**
  This is the lesson of the round-3 reversal, and the one to carry into Batch 2/3.

  The first revision dropped archived threads from **both** sidebar tabs. The
  audit behind it asked only "does the list still render correctly?", and on that
  question it was right: the web follow tab is `IM cache ∩ sidebar items` —
  `followedKeys` is built from sidebar items (`useFollowSidebar.ts`) and
  intersected against IM-cached threads before `filterArchivedThreads` runs, so
  removing archived server-side just moves the filtering earlier. An
  archived-threads browser already exists (`ThreadPanel`, collapsed by default).
  Every one of those facts is true and none of them is the point.

  The follow tab's response also carries `is_followed`, and it is the **only**
  place any client can learn it. `ThreadPanel` lists threads with
  `threadList(status:"all")` — archived included — then marks each one followed
  from `sidebar/sync?tab=follow`; `handleFollow` branches on that flag. Drop
  archived from the follow tab and an already-followed archived thread returns
  `is_followed=false`: its "unfollow" button becomes "follow", and unfollowing
  becomes impossible — on the archived-browsing screen this task had designated
  as its safety net. Same shape on iOS and Android.

  So: **before filtering anything out of a response, ask what state the removed
  entries were carrying, not just what they were displaying.** A display list can
  be re-filtered by the client; a state flag that is only ever transmitted for
  present entries cannot be reconstructed from their absence. The visibility
  filter now applies to the recent tab only; the follow tab's own filtering stays
  with the clients, and `swagger/sidebar.yaml` says so explicitly so nobody
  removes it on the assumption the server already did.
- **Two unrelated "pin" concepts.** `SyncUserConversationResp.Stick` comes from
  `userDetail.Top` / `group.Top`; `user_pinned_channel` is the sidebar pin. The
  exemption uses the latter (same source as the sidebar) so both endpoints agree.
  Legacy `Top` is not honoured — no regression, since neither endpoint honoured
  it before, but it is a real gap if the two ever need to converge.
- **Unread exemption reads cross-Space `Unread`.** `fillPersonSpaceUnread` runs
  *after* the filter, so a DM with unread in another Space is exempted in the
  current one. Fail-open, and unreachable today (person window defaults to 0),
  but Batch 2 must move the check after per-Space unread if it wants to tighten.

## Pre-existing issues found, not fixed

Both reproduce identically on `origin/main`; neither is caused by this change.

- **`go test -tags integration ./modules/message/` does not compile.** Import
  cycle `api_card_action_test.go → app_bot → bot_api → messages_search →
  message` kills **all 16** integration-tagged files in the module, including
  the two recent-filter e2e files this task's acceptance references. Fixing it
  means moving that test to `package message_test`, which cascades
  (`api_card_revisions_test.go` breaks immediately on shared helpers).
- **`TestE2E_ConvSync_*` fail when run after `TestE2E_RecentFilter_*`.**
  `testutil.NewTestServer` binds the registered handler's ctx to the **first**
  server's config, so the fake WuKongIM URL must be registered before the first
  `NewTestServer` in the process; otherwise the ConvSync tests dial the default
  IM and get nothing back (they fail even with the filter off). The file's own
  header comment already calls itself order-fragile.

To verify this change against those e2e tests anyway, the two cycle-causing
files were moved aside temporarily, the e2e run (all recent-filter e2e pass, in
isolation and as a group), and the files restored.

## Verification

Ran with MySQL 8.0 + Redis + WuKongIM v2.2.4 up locally (`WK_TOKENAUTHON=false`
is the load-bearing env var — the tests use octo-lib's empty manager token).
`go build ./...`, gofmt, `golangci-lint` (0 issues), `make i18n-extract-check`,
`make i18n-lint`, and full default-tag suites for `common` / `thread` /
`message` all pass. **Recreate the `test` DB between packages** — each package
registers a different module subset, so sql-migrate sees the previous package's
rows as "unknown migration in database" and panics.
