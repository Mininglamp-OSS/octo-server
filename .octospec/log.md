# Change log

Change history for this repo's `.octospec/`, following the
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
change-log convention (§7). Newest first.

## 2026-07-20 (route-missing-retry)

- **Fix** — Card-action dispatch (`internal/cardactiondispatch`) now **defers** a
  `route_missing` at dispatch time (no attempt consumed) instead of dead-lettering on
  the first attempt. An event only enters the queue when its route existed at enqueue
  time, so a miss at dispatch means the process restarted into a run whose
  `OCTO_CARD_ACTION_ROUTES` lacked the route while the durable queue carried the event
  across — previously a permanent, non-self-healing DLQ that read at the UI as docs
  approve/deny cards never updating. Deferring (rather than nacking) matters: a nack
  spends `route.MaxAttempts`, so the event would trip `attempts_exhausted` the moment
  its route returned. Within `routeMissingMaxWindow` (15m) the event waits and then
  dispatches on its original attempt budget; past the window it dead-letters
  (`reason=route_missing`) so a genuine misconfiguration stays visible. The attempt-budget
  interaction was caught by an `xhigh` code review of the first (nack-based) cut. See
  [brief](tasks/route-missing-retry/brief.md) · [journal](journal/shared/route-missing-retry.md).
- **Learning (pending)** — `durable-queue-registry-divergence`: a durable/shared work
  queue consumed against per-process, startup-loaded config can dead-letter valid work
  across a config-divergent restart; treat "config absent at consume time" as a bounded
  retry, not a first-attempt DLQ.
- **Change (config)** — Card-action DLQ retention is now configurable via
  `OCTO_CARD_ACTION_DLQ_RETENTION_DAYS` (whole days, 1–365) through a shared
  `cardactiondispatch.DLQRetentionFromEnv` resolver used by both `main.go` and
  `tools/card-action-dlq` (so they can't drift). **Default stays 30 days** (the pre-change
  value), so an upgrade that doesn't set the override keeps the existing recovery window and
  never prunes older DLQ entries on first deploy; set the env to a smaller value (e.g. `7`) to
  opt into a shorter window. Doc updated.
- **Fix (review round, PR #621, 4 reviewers)** — three blocking corrections folded in:
  (1) a `route_missing` event with a non-positive `ActedAt` now **dead-letters immediately**
  instead of deferring forever (the wait is bounded by elapsed-since-`ActedAt`, so an unset
  timestamp had nothing to measure against and re-deferred every 5s indefinitely);
  (2) the DLQ-retention default was kept at **30 days** rather than lowered to 7 (the running
  server's lazy prune would otherwise silently delete 8–30-day-old DLQ entries on first deploy);
  (3) the `card-action-dlq` CLI's read-only `depth` no longer prunes (new `DepthsNoPrune`), so
  inspecting the DLQ can't delete recoverable entries. The metric-noise nit (per-re-check
  `observeError`) was left as documented-intentional.
- **Fix (review round 2, PR #621 re-reviews)** — two further blocking corrections folded in:
  (4) the bounded route-missing window is now anchored on the **first observed miss** (a durable
  per-event `route_missing_since` marker via `RouteMissingSeenAt`), not on `Event.ActedAt` — an
  event that dwelt in the durable queue past the window before its first dispatch (long
  restart/outage/backlog) now still defers on its first transient miss instead of dead-lettering
  immediately; this supersedes round 1's `ActedAt<=0` special-case (the marker is always a real
  stamp, so that edge is gone by construction), and `ReplayDLQ` clears the marker so a replayed
  event starts fresh; (5) the `card-action-dlq replay` path is now **non-destructive** — an entry
  past the CLI's resolved retention is refused without being deleted, so the running server stays
  the single pruning authority (a shorter CLI window can no longer silently destroy a
  server-retained entry).
- **Fix (review round 3, PR #621 re-review)** — the round-2 first-miss marker
  (`route_missing_since`) leaked: it is one shared Redis hash with a whole-hash TTL (no per-field
  expiry), refreshed on every miss, so under sustained route-missing traffic a field per COMPLETED
  event accumulated unbounded (it was cleared only on replay, not on delivery or dead-letter).
  Fixed by `HDEL`-ing the marker on every exit transition (`ackScript`, `nackScript`
  requeue+dead-letter, and the existing `replayDLQScript`); a new Redis-backed lifecycle test proves
  the field is gone after Ack and after terminal dead-letter. Also folded in two doc-drift fixes (a
  stale CLI "refuses (and prunes)" comment; the pending learning's `ActedAt`-based deadline →
  first-observed-miss, plus a new marker-lifecycle-vs-whole-key-TTL point).

## 2026-07-20 (github-webhook-parity)

- **Feature** — GitHub `pull_request`/`issues` InteractiveCards gained
  Source/Target branch (PR) + Labels(N) FactSet rows, mirroring the GitLab
  MR/Issue cards from `gitlab-mr-issue-cards` earlier the same day.
- **Behavior change** — GitHub adapter no longer filters
  `pull_request`/`issues`/`issue_comment`/`release` events by action
  (explicit product decision, mirroring the GitLab one); every action now
  renders on both text and card paths.
- **Fix** — Applied the `gitlab-mr-issue-cards` task's pending learning
  (whitelist-gate-as-implicit-sanitizer) proactively: every field the filter
  removal exposed was escaped in the same commit (verified by enumerating
  and grepping every call site before committing, not discovered via a
  later review round). Also folded in a pre-existing, previously-unfixed
  escaping gap in `ghLogin`/`ghWithRepo` (GitHub's twin of GitLab's already-
  fixed `glActor`/`glWithRepo`), for adapter parity. Renamed the shared
  `glCappedFactValue` helper to `cappedFactValue` since GitHub's new Labels
  fact now calls it too. See
  [journal](journal/shared/github-webhook-parity.md).

## 2026-07-20 (gitlab-mr-issue-cards)

- **Feature** — GitLab merge_request/issue InteractiveCards gained a
  Source/Target branch (MR) + Labels(N) FactSet, mirroring the existing
  pipeline card. Card-only; text degrade path unchanged.
- **Behavior change** — GitLab adapter no longer filters MR/Issue events by
  action or pipeline events by status (explicit product decision); every
  action/status now renders on both text and card paths.
- **Fix** — A follow-up code review found the filter-removal had silently
  reopened a markdown/link injection: `glActionVerb`'s raw-passthrough
  fallback for unmapped actions was interpolated unescaped. Fixed by escaping
  at every call site; also deduped the pipeline Jobs / new Labels fact
  cap-and-join logic.
- **Fix** — A PR review (lml2468, PR #610) then found the exact same bug
  class on the sibling field the first fix missed: GitLab pipeline `status`
  also lost its whitelist gate in the same commit, and was still interpolated
  raw on the text path. Fixed identically. See
  [journal](journal/shared/gitlab-mr-issue-cards.md) and the pending learning
  on whitelist-gates-as-implicit-sanitizers (updated with this recurrence).
- **Fix** — Re-review (yujiawei, PR #610) found the same class of bug a third
  time, pre-existing in `glActor`'s `username` branch (byte-identical to
  `main`, not introduced by this task, but folded into the same fix pass):
  it assumed GitLab's restricted username charset made escaping unnecessary,
  which does not hold at this trust boundary (the endpoint only checks a
  shared secret, not that the payload is genuinely from GitLab). Also
  addressed two non-blocking review nits (mochashanyao, PR #610): a
  distinguishing `>` prefix when `formatPipelineDuration` clamps a hostile
  value, and a dedicated `cardFactItemMax` constant instead of reusing the
  actor-name clamp for Jobs/Labels fact items (yujiawei, PR #610).

## 2026-07-17 (docs-approval-card-enrich)

- **Feature** — Enriched the docs access-request approval card (header + colored
  status, big title, requester row with optional avatar, boxed reason) across
  pending + terminal states, and added a reviewer deny-reason dialog whose value
  rides a declared hidden `deny_reason` input through
  `DecisionRequest.Inputs` to the docs backend. Additive optional
  `DocsCardFields.actor_avatar_url` (https-validated). Cross-repo (octo-web deny
  dialog). See [journal](journal/shared/docs-approval-card-enrich.md).

## 2026-07-16 (space-new-user-welcome-message)

- **Feature** — At-most-once Space welcome DM from the `notification` bot on a
  human user's first join to a designated Space. New `octo_space_welcome_delivery`
  ledger (migration in `modules/notify/sql/`; `notify/1module.go` gains
  `//go:embed sql` + `SQLDir`), a 60s reconciler and a single-row send worker
  (claim via `FOR UPDATE SKIP LOCKED`, CAS guarded by `status + claim_owner`,
  `attempts` grows only on pre-IM failure with backoff {5s,30s,120s}→failed,
  any post-dispatch failure → `unknown` never retried). Config is five
  `system_setting` keys under `onboarding`; `modules/common` gains an atomic
  `SpaceWelcomeConfig()` snapshot accessor + prospective composite validation on
  the manager write path + i18n code `err.server.common.space_welcome_config_invalid`.
  A notify-local 15s context-aware HTTP sender replaces octo-lib's timeout-less
  helper (octo-lib unmodified). `active_from` vs `space_member.created_at`
  compared via `UNIX_TIMESTAMP` (mirrors `modules/opanalytics`). Observability
  kept minimal (in-process counters + logs). Ships `enabled=false`; three
  product/ops sign-off items gate turning it on. Brief under
  `.octospec/tasks/space-new-user-welcome-message/`; shared journal
  `.octospec/journal/shared/space-new-user-welcome-message.md`.

## 2026-07-16 (card-action-internal-http-actions)

- **Follow-up** — Two small extensions to #588 plus one bundled config
  collapse. `OCTO_CARD_ACTION_ROUTES[].url` now accepts `http://` in addition
  to `https://`; hostname form is intentionally not inspected (route
  registration = operator authorization). URL validator tightened at the same
  time: `Hostname() != ""` (blocks `http://:8080/x`), `ForceQuery` (blocks
  trailing `?`), raw-`#` prefilter (blocks trailing/embedded `#`).
  `OCTO_CARD_ACTION_ALLOWED_URLS` is deleted from code paths and emits a
  structured deprecation WARN if still set, so rolling upgrades do not fail.
  `approval_card.actions` grew an optional 1..5 bounded slice: server-derived
  action IDs, reserved metadata enforced, control-character-in-title checked
  on the raw string, `nil` preserves byte-for-byte legacy approve/deny while a
  non-nil empty slice is rejected as a caller bug. Callback wire contract
  (states, requester notification, HMAC canonical) is deliberately unchanged.
  Coverage — cardactiondispatch 81.5%, cardtmpl 89.9%, notify 71.2%. Brief/context
  under `.octospec/tasks/card-action-internal-http-actions/`; shared journal
  `.octospec/journal/shared/card-action-internal-http-actions.md`.

## 2026-07-16 (webhook-cardmsg-adapter)

- **Feature** — The GitHub/GitLab incoming-webhook adapters render their event
  subset as `InteractiveCard` (=17) octo/v1 cards (structured header + body + a
  "View on {GitHub|GitLab}" `Action.OpenUrl`) when `OCTO_CARD_MESSAGE_ENABLED`
  is on, and degrade to the untouched markdown text path when off (flag-off wire
  byte-identical). New `adapter_card.go` holds the shared card anatomy + one leaf
  escaper + http(s) allowlist + self-validate/degrade selector, used by both
  adapters (trust-boundary parity). GitLab pipeline cards render a
  Branch/Status/Duration/Jobs FactSet (parses `duration` + `builds[]`, card-only).
  Server-only: octo-web already ships the octo/v1 renderer + `iwh_` sender trust.
  Brief/context under `.octospec/tasks/webhook-cardmsg-adapter/`; shared journal
  `.octospec/journal/shared/webhook-cardmsg-adapter.md`.

## 2026-07-13 (card-message-appbot-trust)

- **Fix** — Closed the P0 App Bot card trust split without changing the send
  pipeline: added a cache-free `modules/botidentity` authority over active
  `robot` and published `app_bot` rows (same-statement ambiguity detection,
  `user.robot` never authorizes), moved `cardtrust` display masking onto it while
  retaining the 60-second bounded cache, and made `card/action` resolve sender
  identity live before enqueueing through the unchanged robot event queue. Added
  push/search projection coverage plus App Bot unpublish/republish and full
  action -> poll -> ACK lifecycle tests. `internal/carddispatch` remains a
  separate task. Brief/context under
  `.octospec/tasks/card-message-appbot-trust/`; shared journal
  `.octospec/journal/shared/card-message-appbot-trust.md`.

## 2026-07-09 (sticker-oversized-store-guard)

- **Fix** — Task `sticker-oversized-store-guard` (code-review fix on
  `sticker-oversized-default`): close the regression where the compress-aware
  gate admitted >512 jpg/png trusting compression to downscale, but every
  fail-open path (nil compressor, skipped:concurrency_saturated/timeout, failed,
  or compress_max_dimension > upload_max_dimension) stored the original oversized
  image up to 1024² and served it to peers — reachable under load / attackable by
  saturating the compress slots. Added `stickerCompressResult.OutMaxDim` (actual
  post-compression dimension) + an `api.go` post-block guard that rejects
  (`compress_oversized_rejected`, new pre-warmed terminal metric) when the final
  stored dimension exceeds `upload_max_dimension` — dimension fail-CLOSED while
  compression quality stays fail-OPEN. Deduped the cross-package 1024 literal
  (exported `common.StickerUploadMaxDimensionHardCap`, referenced by modules/file).
  Schema note recommends `compress_max_dimension ≤ upload_max_dimension`; test
  helper reuse cleanup. Four guard regressions (nil/failed/timeout/mis-config) +
  unbroken happy path. No new errcode / i18n / DB / appconfig change. Briefs
  `.octospec/tasks/sticker-oversized-store-guard/`.

## 2026-07-09 (sticker-oversized-default)

- **Change** — Task `sticker-oversized-default` (follow-up to
  `sticker-downscale-store`): make ">512px static jpg/png auto-shrinks to 512" the
  built-in default once compression is enabled, without turning compression on for
  every deployment. `compress_max_dimension` default flips 0(=ceiling)→**512**,
  decoupled from `upload_max_dimension`, clamp `[1,1024]` (getter collapsed to the
  shared `stickerClampIntUpper`). New compress-aware dimension gate
  (`stickerLimitsSnapshot.effectiveGateDim`): jpg/png accept up to the **1024**
  hard cap when `compress_enabled=true` (then shrink to `compress_max_dimension`),
  gif/webp and compress-off stay gated at `upload_max_dimension` (512).
  `compress_enabled` default stays **false** (gray-scale rollout preserved);
  `upload_max_dimension` default and the appconfig `StickerUploadLimits`
  client contract stay **512/unchanged** (compress-aware gate avoids the
  appconfig ripple a 1024 default would cause). Zero-impact when compression off
  (gate = 512 for all formats, compressor never runs). Known edge: APNG (ext
  `.png`) passes the widened gate but can't be shrunk (`skipped:animated`) — later
  fail-closed **rejected** by `sticker-oversized-store-guard` if >
  `upload_max_dimension` (this entry's pre-guard "stored un-shrunk" no longer
  holds). Getter tests rewritten; gate integration tests added; fake made
  faithful to the 512 default. No new errcode / i18n / DB / migration / appconfig
  field. Brief `.octospec/tasks/sticker-oversized-default/brief.md`.

## 2026-07-09 (sticker-downscale-store)

- **Change** — Task `sticker-downscale-store` (phase two of
  `sticker-upload-compression`): decouple the compressor's `imaging.Fit` downscale
  target from the upload dimension gate. New server-side key
  `sticker.compress_max_dimension` (int, `Positive:true`, read-side clamped to
  `≤ upload_max_dimension`, unset ⇒ `= upload_max_dimension` ⇒ no downscale). Swap
  `stickerLimitsSnapshot.compressParams().MaxDim` from `maxDim` (accept gate) to a
  new `compressMaxDim` field so static jpg/png larger than the target but within
  the unchanged accept ceiling are downscaled before re-encode+store, instead of
  the Fit branch being unreachable (gate/target were same-source, so it never
  fired). Accept hard cap stays 1024 (decompression-bomb envelope unchanged);
  webp/gif still validate-only; not exposed via appconfig. Zero-impact default,
  byte-for-byte identical to `main` when unset. New getter clamp tests (no-infra)
  + api-level downscale/regression tests. No new errcode / i18n / DB / migration.
  Brief `.octospec/tasks/sticker-downscale-store/brief.md`.

## 2026-07-09 (P3-3)

- **Change** — Task `card-message-p3-rich-inputs` (card message P3-3): extend the
  octo/v2 input whitelist with `Input.Number/Date/Time` (all AC 1.0, within the
  pinned `card_version:"1.5"` — additive, no version bump). Submit-time value
  validation added (format/type only: Number = finite JSON number; Date =
  `YYYY-MM-DD`; Time = `HH:MM`; `""` = unfilled; declared min/max range NOT
  server-enforced — delegated to bot, same class as `isRequired`/`regex`, which
  likewise stay unenforced). Refactored the element
  whitelist into a single `pkg/cardmsg` authority (`whitelist.go`:
  `displayElements`/`inputElements` + `DisplayElements()`/`InputElements()` +
  `isInputElement`) that send-time validation, submit-time collection, action
  dispatch, and the D12 manifest all derive from — no drifting literals. D12
  `GET /v1/bot/card/profile` additively advertises `elements`/`inputs` for
  element-granularity feature detection. Review-caught fixes folded in: reject
  non-finite `Input.Number` (NaN/±Inf bypass `ParseFloat`); strict JSON-number
  grammar so the server's "valid number" matches the bot's JSON parser (reject
  `ParseFloat`'s Go-only superset — `1_000`/`0x1p4`/leading-`+`/leading-zero —
  which would silently corrupt the value the bot re-parses); `default`
  fail-closed arm in the submit-time type switch; `Column` dropped from the
  manifest `elements` (it is a `ColumnSet` child, not a top-level element the
  validator accepts — advertising it lied about capability); and symmetric
  `inlineAction` dispatch for the new types (no dead buttons). No new errcode /
  i18n / DB / migration / endpoint; additive-only wire contract. Brief
  + journal under `.octospec/tasks/card-message-p3-rich-inputs/` and
  `.octospec/journal/shared/card-message-p3-rich-inputs.md`; learning candidate
  in `.octospec/learnings/pending/`.
- **Change** — Same task/PR, follow-on: **AC 1.5 display-element completion (Tier 1)** —
  added `ImageSet`(1.0) / `RichTextBlock`(1.2) / `Table`(1.5) / `ActionSet`(1.2) to the
  octo/v2 display whitelist (versions verified against adaptivecards.io). Each covers
  send-time validation (structure + URL allowlist + recursion budget), dispatch symmetry
  (`findSubmitInElements` walks ActionSet.actions / Table cells / ImageSet images /
  RichTextBlock inlines for Submit — no dead buttons), plain derivation, and D12 manifest
  `elements` (auto via the displayElements single authority). Corrected the pre-existing
  `TestValidateWhitelistRejections` which mislabeled Table as "AC 1.6, reject" (Table is
  1.5, now supported) → replaced with still-unsupported Media(1.1)/ToggleVisibility(1.2).
  Still out (later, on demand): Media, Action.ShowCard/ToggleVisibility/Execute, templating,
  AC 1.6.
- **Change** — Same task/PR, review hardening (PR#556 review of head `7559c526`): fixed a
  **send-time URL-allowlist bypass (P1)** in the two Tier-1 flat-leaf handlers — `imageChild`
  (`ImageSet.images[]`) and the `RichTextBlock.inlines[]` object branch accepted a child
  without enforcing its declared `type` and never recursed its `items`, so a mislabeled child
  (`{"type":"Container","url":"http://ok","items":[TextBlock with javascript:]}`) passed
  `Validate` with the nested `javascript:` link unchecked. Now both enforce a *leaf* contract —
  reject a present `type` ≠ `Image`/`TextRun` (same discipline as `column()`) AND reject any
  child-collection field (`items`/`columns`/… via `rejectLeafSubtree`), which also closes the
  **typeless-child residual** a conditional `if type present` check leaves open (a no-`type`
  child with a nested subtree) — restoring "校验面 ≥ 渲染面" (`TestTier1MislabeledChildRejected`
  covers typed + typeless). Also completed `TableRow.selectAction` (P2): added it to
  validation (`w.selectAction(row)`) and dispatch (`findSubmitInElements` reads
  `row.selectAction`) symmetrically — row was the only node whose `selectAction` was neither
  validated nor dispatched. Brief updated; `inputs` manifest field confirmed in-scope.
- **Change** — Same task/PR, review hardening cont'd (heads `2c8f1003`→`85baabdf`, three
  reviewers): the foreign-typed-child bypass turned out to recur one child collection at a time
  (ImageSet → its typeless variant → `Table` rows/cells), so generalized the fix into one shared
  discipline instead of patching each instance. New `checkConstrainedChild` (type-pin via a shared
  `childTypeMatches` predicate + closed-set `rejectForeignSubtree`) is now applied to **every**
  flat-validated child position — `ColumnSet.columns[]`, `ImageSet.images[]`,
  `RichTextBlock.inlines[]`, `Table.rows[]`/`cells[]`, `FactSet.facts[]` — closing the `Table`
  send-time bypass (mislabeled cell as `Image` with a `javascript:` url; mislabeled/typeless row
  hiding an un-recursed `items` subtree) plus the Column/Fact instances of the same class. The
  dispatch walker (`findSubmitInElements`) reuses the same `childTypeMatches` predicate to skip
  foreign-typed children, so validate-surface == dispatch-surface can't drift (P2). Tests:
  `TestTier1MislabeledChildRejected` (Table/Column/Fact, typed + typeless) +
  `TestTier1DispatchSkipsMislabeledChild`. Lesson: patch the class, not the flagged instance.

## 2026-07-08 (PR-D)

- **Change** — Task `card-message-p2-capability-manifest` (PR-D, card message P2
  D12): producer capability discovery. New read-only `GET /v1/bot/card/profile`
  (bot-token, existing `authBot()` chain — no new rate limiter, no Space
  middleware) returning the deployment's card capability manifest
  (`enabled` / `card_version` / `profiles` / `limits`) so producers feature-detect
  instead of send-probing. All values sourced from `pkg/cardmsg` constants; the
  `profiles` set comes from a new single-authority `cardmsg.AcceptedProfiles()`
  that `interactiveByProfile` now derives from too (a drift-guard test asserts
  the manifest can't advertise a profile the validator rejects). `enabled:false`
  still returns 200 with the full manifest (a both-halves test pins manifest-200
  + send-still-rejects together). Additive-only wire contract (contract test pins
  the field set). No new errcode / i18n / DB / migration. Independent of PR-B/PR-C
  (both merged). Journal:
  `.octospec/journal/shared/card-message-p2-capability-manifest.md`;
  learning: `.octospec/learnings/pending/card-message-p2-capability-manifest.md`.

## 2026-07-08 (PR-C)

- **Change** — Task `card-message-p2-revision-history` (PR-C, card message P2
  D10): card revision history. New `octo_message_card_revision` side table +
  `pkg/cardrevision` shared store (written by bot_api on edits/clear, read by
  message), `GET /v1/message/card/revisions` (summary / full=1) reusing the
  extracted `authorizeCardChannelMember` gate, bot revision clear + auditable
  tombstone, `transient` frame flag (progress frames skip history), and revoke
  cleanup. Verify caught two P1s (fixed): the query path lacked the
  revoke/deleted/user-local-delete visibility gate, and the revoke cleanup was
  mis-ordered after the notify step. Code-review (B1) then caught that the query
  still enforced a *subset* of the canonical read — missing the `visibles`
  allowlist / read-offset / channel-offset / expiry layers `card/action` carries;
  fixed by extracting `cardCanonicalVisibleToViewer` and sharing it across both
  endpoints (+ `TestCardRevisionsCanonicalVisibility`). Stacked on PR-B; zero
  octo-im changes. Journal:
  `.octospec/journal/shared/card-message-p2-revision-history.md`;
  learning: `.octospec/learnings/pending/card-message-p2-revision-history.md`.

## 2026-07-08

- **Change** — Task `card-message-p2-action-loop` (PR-B, card message P2
  interaction): shipped the interaction closed loop (contract
  `card-message-interaction` D3–D9/D11 + octo/v2 whitelist). New
  `POST /v1/message/card/action` (authz + anti-IDOR + D11 input validation + D4
  Redis idempotency), typed `card_action` bot event on the existing robot queue,
  type-17 `botMessageEdit` unlock (cardmsg validation + D9 `card_seq` CAS in
  `message_extra`), and the `pkg/cardmsg` octo/v2 whitelist filled into the
  merged-P1 seams. Verify caught a real InnoDB deadlock in the D9 CAS under
  concurrent frames (fixed via bounded 1213/1205 retry). Zero octo-im changes.
  D10 revision history / D12 capability manifest split to sibling PRs C/D.
  Journal: `.octospec/journal/shared/card-message-p2-action-loop.md`;
  learning: `.octospec/learnings/pending/card-message-p2-action-loop.md`.

## 2026-07-02

- **Change** — Task `conv-space-catchall-484` (issue #484 follow-up): closed the
  two deterministically reproducible cross-Space paths in the recent-conversation
  list. (1) The default-Space DM catch-all no longer lists a bare DM whose
  `dm_space_presence` rows point exclusively at other Spaces (positive-evidence
  post-pass; legacy no-presence DMs keep the catch-all; system bots exempt; any
  query failure disables the pass). (2) Groups with empty `group.space_id` — and
  their topics, in the conv filter AND sidebar thread-ext filter — now show only
  in the user's default Space instead of every Space (same policy as #337 bare
  DMs / #484 untagged history). This branch also carries the base
  `dm-space-isolation-484` fix (merged in — see the 2026-06-27 entry below), so
  the presence infra is authored once here. Journal:
  `journal/shared/conv-space-catchall-484.md`.
- **Remove** — Task `incoming-webhook-remove-name-prefix`: dropped the
  server-enforced `Webhook-` name prefix that was force-prepended to
  non-admin (member/bot) submitted incoming-webhook display names
  (originally added anti-impersonation, PR #340 review). Members can now
  set any name, same as admins. Kept: avatar lock for non-admins, default
  auto-naming (`Webhook-xxxxxx`) when no name is submitted, and the
  push-time `Username`/`AvatarURL` override block for non-admin webhooks
  (separate control, unaffected). Paired frontend change in octo-web
  removed the now-stale hint text. Brief under
  `.octospec/tasks/incoming-webhook-remove-name-prefix/`.

## 2026-06-29

- **Change** — Task `group-avatar-name-no-text` (client-coordination; repurposes
  `group-avatar-icon-default` S2): newly created groups now default to the
  two-person icon — the group **name is never rendered as avatar text**; text
  appears only when the user sets a custom `avatar_text`. Implemented by changing
  **who gets `is_named=1`**, not the render rule (`writeGroupDefaultAvatar`
  unchanged: `avatar_text > is_named==1 name-text > icon`). `is_named` is
  repurposed from "user named it" to "**pre-cutover legacy group**": all new
  inserts (`CreateGroup`/`AddGroup`/`event.go` system+org+dept) persist
  `is_named=0`, and rename no longer flips it; existing groups keep `is_named=1`
  (already backfilled by migration `20260629000001`) so they are **grandfathered**
  onto their current name-text avatar (no historical group flips to an icon).
  `is_named` stays load-bearing (not deprecated) as the legacy/new discriminator;
  `GroupResp.is_named` re-documented as 1=legacy/0=new predictor. No render-version
  bump, no new migration. Brief under `.octospec/tasks/group-avatar-name-no-text/`.
- **Add** — Task `common-builtin-emoji-manifest`: public, cacheable
  `GET /v1/common/emojis` returning the built-in custom emoji manifest
  (`{version, list:[{key,name,url}]}`) from an embedded JSON single source of
  truth, mirroring the `avatar_palette` (#500) pattern (content ETag +
  `must-revalidate` + 304). Clients fetch + cache instead of hardcoding the
  `[xxx]` emoji list. `url` optional per item (built-ins reuse client bundle);
  no DB / errcode / i18n added. New `modules/common/emoji.go`,
  `modules/common/emojis/manifest.json`, `emoji_test.go`, swagger entry.

## 2026-06-27

- **Add** — Task `default-avatar-text-rule`: script-aware 2-glyph text rule for
  group + personal default avatars. Mixed script → Han only; pure English →
  initials (camelCase/sep split, ≤2, upper); pure digits → 2; empty/symbol/emoji
  → icon (group two-person) / ascii (personal) fallback. New
  `avatarrender.GroupNameText` (前2) + rewritten `IndividualText` (后2) over a
  shared core; `GroupText` kept as the custom-`avatar_text` normalizer (≤4) and
  `writeGroupDefaultAvatar` splits custom-text vs auto-name. Cache-version bumped
  `group-name-v3→v4` and `name-v4→v5` (ETag + CacheKey). Brief + context under
  `.octospec/tasks/default-avatar-text-rule/`, journal
  `.octospec/journal/shared/default-avatar-text-rule.md`.
- **Fix** — Task `dm-space-isolation-484` (#484): authoritative per-Space DM
  presence index (`dm_space_presence`, written at the WuKongIM message webhook,
  read by the conversation Space filter) — fixes cross-Space DM history leak
  (symptom 1, via default-Space policy for untagged messages) and DMs mutually
  hiding between Spaces (symptom 2, window-independent visibility OR-ed with the
  legacy Recents scan). Server-only; no client change.

## 2026-06-25

- **Add** — Task `incoming-webhook-mention-config`: moved the incoming-webhook
  `@mention` from a caller-supplied push-body param to webhook create/update
  config (new `mention_uids` column + `AllowMention*` switches). The push
  endpoint no longer reads `mention` from the body; targets are validated at the
  management boundary and re-filtered to current members at push time. Removing
  the body-source also removed the native-only `allowMention` gate, so mention
  now applies across **all** adapter endpoints (native + github/wecom/gitlab/
  feishu/multica). Deleted the now-dead caller-supplied entity machinery. Brief +
  context under `.octospec/tasks/incoming-webhook-mention-config/`, journal
  `.octospec/journal/shared/incoming-webhook-mention-config.md`.
- **Add** — Task `appbot-token-revocation-redis` (#309): replace the per-process
  in-memory App Bot auth registry with a shared Redis write-through cache so
  token revocation (rotate/unpublish/delete) takes effect on every replica
  immediately; DB stays authoritative (auth fails safe to DB on Redis error).
  Safety-net TTL via system_settings (`app_bot.auth_cache_ttl_seconds`, no new
  env var). Regression test asserts a revoked token is rejected on a peer replica.
- **Update** — Task `group-default-avatar` (increment 4, final): removed the
  member-avatar 9-grid composite chain now that avatarGet renders on demand —
  all 5 publish sites + `beginAvatarUpdateEvent`, the `GroupAvatarUpdate` event
  handler/const/db-helpers, `queryGroupAvatarIsUpload`, dead `memberCount`
  guards, and two obsolete tests. Kept DownloadAndMakeCompose (other use) and
  the CMDGroupAvatarUpdate client-refresh CMD. Historical composite groups fall
  through to the rendered default with no backfill. Feature backend complete;
  only the placeholder group-icon SVG remains to be swapped.
- **Update** — Task `group-default-avatar` (increment 3): group-info update
  (`PUT /v1/groups/:group_no`) now accepts `avatar_text`/`avatar_color`
  (set/clear, validated), persisted via a dedicated `UpdateGroupAvatarCustom`
  service + `db.updateAvatarCustom`; clients refreshed via
  `SendChannelUpdateToGroup`. Composite teardown still pending.
- **Update** — Task `group-default-avatar` (increment 2): `avatarGet` now
  server-renders the default group avatar (colored circle + group-name initials,
  2×2 for CJK / single-line for Latin, group-icon fallback) with weak-ETag/304,
  keyed on `is_upload_avatar`; uploaded avatars still redirect. `pkg/avatarrender`
  gains `RenderGroup`/`GroupAvatarLines`, `RenderIcon` (+ placeholder glyph), and
  shared `ETag`/`IfNoneMatch`. Member-avatar composite teardown still pending.
- **Creation** — Task `group-default-avatar` (increment 1): create-group API gains
  optional `avatar_text`/`avatar_color` params persisted via new `group` columns;
  `pkg/avatarrender` gains `GroupText`/`VisibleRuneCount`/`ColorByIndex`. Brief +
  journal under `.octospec/tasks/group-default-avatar/`. Follow-ups: avatarGet
  server-render branch, group-update keys, composite-avatar teardown.

## 2026-06-24

- **Add** — Task `incomingwebhook-webhooks-alias` (#455): `/v1/webhooks/{id}/{token}`
  push-route alias for the canonical `/v1/incoming-webhooks/...` (native + 5
  adapters), reusing the identical middleware chain. Generalized `pkg/accesslog`
  token scrubbing (`ScrubPath` + panic-dump regex) to mask BOTH prefixes (#246
  parity). Brief + context under `.octospec/tasks/incomingwebhook-webhooks-alias/`,
  journal `.octospec/journal/shared/incomingwebhook-webhooks-alias.md`.
- **Add** — Task `incoming-webhook-mention-broadcast` (#448 item ②): broadcast-pill
  auto-compose on the native incoming-webhook push endpoint. When a permitted
  `mention.all`/`mention.bots` is set, the server prepends the canonical broadcast
  literal (`@所有人`/`@所有AI`) + a space to the text content so all three clients
  render a pill; directed-entity (#449) offsets shift by the prefix's UTF-16
  length. Text-path only; routing / red-dot / bot-summon unchanged. Brief +
  context + journal under `.octospec/tasks/incoming-webhook-mention-broadcast/`
  and `.octospec/journal/shared/incoming-webhook-mention-broadcast.md`.
- **Add** — Task `incoming-webhook-mention-directed-render` (#448 item ① b):
  opt-in server-side directed @mention name-resolution. `mention.render:true`
  resolves each member uid → `user.name`, prepends `@<name> ` to text content, and
  generates the UTF-16 `mention.entities`. Refactored the broadcast compose into one
  `composeMentionContent`. Adversarial review added a forged-broadcast guard (skip
  names that are broadcast labels or contain `@`), incremental budget tracking, and
  cap/iOS/byte-size docs. Ships in the same PR as the broadcast half (#450) → the
  two close #448. Brief + context + journal under
  `.octospec/tasks/incoming-webhook-mention-directed-render/` and
  `.octospec/journal/shared/incoming-webhook-mention-directed-render.md`.

## 2026-06-23

- **Add** — Task `upstream-dep-metrics` (#440 P0-a): upstream-dependency
  observability. Added `dmwork_dependency_duration_seconds` (object-storage
  `DownloadURL` latency) and connection-pool metrics (`go_sql_*` via
  DBStatsCollector + `dmwork_redis_pool_*` via a scrape-time collector). No
  background goroutine, no `octo-lib` change, no business-logic change. Brief +
  context + journal under `.octospec/tasks/upstream-dep-metrics/` and
  `.octospec/journal/shared/upstream-dep-metrics.md`.

## 2026-06-19

- **Update** — Adopted OKF v0.1 compatible frontmatter across all repo rules
  (`commit-style`, `error-handling`, `rate-limit`, `space-isolation`,
  `testing`): added `type`, `title`, `description`, `tags`, `timestamp`. The
  octospec orchestration fields are retained as OKF extension fields.
- **Update** — Bumped global inheritance pin to `octo-spec@1.1.0`.
- **Creation** — Added `.octospec/index.md` (human-readable rule catalog) and
  this `.octospec/log.md` change log.

## 2026-06-18

- **Creation** — octospec pilot scaffolding: rules `error-handling`,
  `rate-limit`, `space-isolation`, `testing`, `commit-style`; manifest, task
  templates, slash commands (PR #418).
- **Creation** — Dogfood task `member-list-name-fallback` (#344 → PR #420).

## 2026-07-13 (card-message-internal-dispatch P2)

- **Pilot** — Enabled the first `internal/carddispatch` producer
  (`summary-notify`): dedicated `summary` bot + producer spec + `NotifyReq.Card`
  structured branch building `octo/v1` DM cards via `cardtmpl` and dispatching
  through the bound `Sender` (per-recipient fan-out, `NotifyResp` preserved).
  Stacked on the P1 foundation branch, not main. Cross-repo (octo-web route,
  octo-smart-summary switch) tracked in the summary-notify contract. See
  [journal](journal/shared/summary-notify-pilot.md).

## 2026-06-19 (tooling)

- **Update** — Synced OKF-aware slash commands, workflow skill, and task brief
  template from octo-spec 1.1.0 so generated briefs/journals stay conformant.
