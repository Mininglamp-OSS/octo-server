---
type: Journal
title: "conv-space-catchall-484: default-Space DM catch-all + spaceless group/topic isolation"
description: Close the two deterministically reproducible cross-Space paths in the recent-conversation list
tags: [space, isolation, message, conversation, sidebar]
timestamp: 2026-07-02T00:00:00Z
---

# conv-space-catchall-484

Branch `fix/conv-space-catchall-484` (based on origin/main 236b78b; stacked-free —
presence infra re-introduced byte-identical to the unmerged #484 branch by user
decision, so the eventual merge dedupes).

## What was done

1. **Default-Space DM catch-all tightened** (production-reproducible leak): the
   catch-all in `decideConvKeepInSpace` kept every bare DM in the default Space.
   Now a post-filter pass (`space_filter_default_catchall.go`) hides a bare DM
   from the default Space iff `dm_space_presence` has rows for the pair and none
   of them is the default Space and the Recents window carries no untagged /
   default-tagged counter-evidence. No presence rows → legacy catch-all kept.
   System bots exempt; bot visibility stays with the catch-all bot sub-check;
   `skipBotFilter` or any query failure disables the pass (never hide on doubt).
   Self-heals: one default-Space message re-adds visibility.
2. **Spaceless groups/topics attributed to the default Space only** (the exact
   path a production client diagnostic showed rendering in every Space): the
   `spaceID == "" → return true` fail-open in the group branch, the
   `parentSpaceID == ""` fail-open in `filterThreadConvCore`, and the legacy-keep
   in sidebar `filterThreadExtsBySpace` all now show the conversation only when
   `filterSpaceID == defaultSpaceID` — same policy as #337 bare DMs and #484
   untagged DM history.
3. Both v1 (`/v1/conversation/sync`) and v2 (`/v1/sidebar/sync`) paths switched
   to `GetUserDefaultSpaceIDE`; on lookup error the conv filters fall open for
   the request (defaultSpaceID=filterSpaceID, catch-all pass disabled) so a DB
   hiccup never hides more than today; `filterThreadExtsBySpace` keeps its
   fail-closed error contract.

## Verification

- Unit: new `space_filter_default_catchall_test.go` (evidence gates, fail-open
  gates, counter-evidence, bot exemptions); two legacy assertions deliberately
  flipped (`...ThreadChannelLegacyParent`, `...Group_LegacyNoSpace_*`).
- Integration (real handlers + webhook-written presence):
  `TestConvSpaceCatchall_DefaultSpaceHidesElsewhereOnlyDM`,
  `TestConvSpaceCatchall_SpacelessGroupAndTopicOnlyDefault` — both PASS; suite
  helpers are cs-prefixed to avoid collisions with the #484 branch harness.
- `go build ./...`, `go vet`, `make i18n-lint`, `make i18n-extract-check` green.

## Consolidation (2026-07-02)

Merged the base `fix/dm-space-isolation-484` work into this branch (user request:
one branch / one PR for all of #484). The presence infra was already byte-identical
so it deduped; the only hand-merge was `space_filter.go` — branch-1's `dmPresentSet`
threading + non-default DM presence-OR now coexists with this branch's group/topic
default-Space attribution and the default-Space catch-all post-pass. The two
integration suites were unified onto one fake-IM + webhook secret (the message
handlers bind modules once per process via `register.GetModules` sync.Once, so two
independent fake-IM servers made the second suite lose its IM binding). Deleted the
obsolete `TestRepro484_ProdTrace_SpacelessGroupShownInEverySpace` (it characterized
the pre-fix bug now fixed here; `TestConvSpaceCatchall_SpacelessGroupAndTopicOnlyDefault`
covers the fixed behavior). All 10 integration tests + unit suites green together.

## Learnings

- Topic conversations must ALSO pass the thread liveness whitelist
  (`QueryActiveShortIDs`, status=1) — integration tests seeding topics need a
  `thread` row, not just `group_member`.
- The shared `test` DB across test binaries still causes cross-binary migration
  clashes; DROP/CREATE between package runs remains mandatory.
- `personConvHasSpaceMessages(conv, "")` does NOT match untagged messages (they
  lack the key) — untagged counter-evidence needs its own scan.

## Review round (2026-07-02, PR #519)

First pass: three reviewers, no blocking findings. Re-review on the squashed head
surfaced a real **P1 cross-Space DM leak** (below), fixed. Addressed items:

3. **P1 — default-Space-lookup error must fail *closed* for the DM catch-all**
   (`space_filter.go`, both v1 `:38-42` and v2 `:711-715`). yujiawei + OctoBoooot
   caught that the fail-open branch set `defaultSpaceID = filterSpaceID`, which
   makes `filterSpaceID == defaultSpaceID` true for a **non-default** request during
   a `space_member` DB error → the bare-DM catch-all (`decideConvKeepInSpace:332`)
   fires and returns **every** bare DM (including other-Space-only DMs) in that
   Space — a cross-Space leak, and a regression vs pre-PR (old `GetUserDefaultSpaceID`
   returned `""` on error). Fix: set `defaultSpaceID = ""` — a sentinel that can
   never equal a real `filterSpaceID` (the filter only runs when `spaceID != ""`),
   so the DM catch-all cannot fire on the error path and bare DMs fall through to
   the presence/Recents scan (isolated); spaceless groups/topics fail-closed
   (hidden — a subset of pre-PR "visible everywhere", not a regression). Note the
   *soft* group-attribution paths that are NOT a catch-all gate keep `= spaceID`
   (`api.go` untagged-history, `api_sidebar.go` thread-ext) — those only ever show
   current-Space-subset content, never leak. Regression guard:
   `TestFilterConversationsBySpace_DefaultLookupError_BareDMFailsClosed` asserts a
   bare cross-Space DM is dropped when `defaultSpaceID == ""` and documents the
   pre-fix leak.

1. **D — `filterThreadExtsBySpace` default-Space lookup now fail-open**
   (`api_sidebar.go`). The `GetUserDefaultSpaceIDE` call there only drives the
   *soft* "spaceless parent group shows in which Space" heuristic, not a real
   auth boundary, yet a transient `space_member` blip used to 500 the whole
   follow tab. It now logs + falls back to `defaultSpaceID = spaceID` (same
   fail-open口径 as `FilterConversationsBySpace`); the genuine boundary queries
   (group table, external-group map) stay fail-closed. Safe against the
   authoritative-membership contract because `/v1/sidebar/sync` does **not**
   emit `space_memberships` (only `/v1/conversation/sync` does — see below).
2. **C — DB-level accessor tests** (`dm_presence_accessor_test.go`, integration
   tag): lock down `UpsertDMSpacePresence` (GREATEST monotonic, empty no-op),
   `DMSpacePresenceSet` (per-Space IN filter), `DMSpacePresenceAnySet`
   (any-Space set + elsewhere-only derivation) directly against MySQL.

### Known boundaries (surfaced in review — by design, not regressions)

- **Encrypted (Signal) DMs skip the presence write** (`webhook/api.go`): payload
  is unparseable, so a DM that exists *only* as encrypted messages in Space B
  gets no presence row and the default-Space catch-all cannot hide it. This is
  the fail-open contract (never hide without positive evidence); the leak is not
  *introduced* here, just not closed for encrypted-only DMs.
- **Spaceless-group → default-Space-only is a user-visible change**: a group that
  genuinely belongs to Space B but has empty `group.space_id` (missing backfill)
  disappears from B until backfilled. Direction is fail-closed (safer for a leak
  fix) and self-heals on `group.space_id` backfill.
- **`/v1/sidebar/sync` returns only `{items, version, follow_version}`** — it does
  NOT emit `space_memberships`. Only `/v1/conversation/sync` (`buildSpaceMemberships`)
  carries the authoritative wipe-replace membership list. Any client treating the
  sidebar response as a `space_memberships` source is reading a field the server
  never sends.

### Deferred

- Per-conversation decision observability for production triage. A global
  DEBUG-log switch was rejected (needs redeploy to flip, floods all users, can't
  target an after-the-fact report). Next round: a `X-Internal-Token`-gated
  read-only "explain" path that replays the filter for a given uid+space and
  returns per-conversation kept/dropped + reason, so ops can diagnose from the
  response without packet capture. Not in this PR.
