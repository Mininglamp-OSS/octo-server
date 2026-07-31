# Change log

Change history for this repo's `.octospec/`, following the
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
change-log convention (§7). Newest first.

## 2026-07-31 (bot-events-longpoll)

- **Feature** — Task `bot-events-longpoll` (card-message-interaction D5 / P3-2):
  `POST /v1/bot/events` gained an opt-in long poll. Bot delivery was cursor short
  polling (one `ZRangeByScore`, immediate return), so card interaction latency
  equalled the bot's poll cadence. **Every** producer that ZADDs into
  `robotEvent:{robotID}` now rings a per-bot doorbell — five sites, including
  the highest-volume `saveRobotMessage`; review found the first revision had
  wired only two, so the invariant is now held by a source guard rather than by
  the docstring that caused the miss — and a caller passing `wait` seconds parks
  on it via BLPOP. New leaf package `pkg/botevent` owns the key format, since
  `modules/bot_api` already imports `modules/robot` and either module would have
  meant an import cycle or a drifting copy.
  **The doorbell is a hint, never the event** — every wake-up re-reads the
  authoritative sorted set from the caller's cursor, so a lost, stolen or stale
  bell costs latency only. `wait` defaults to 0, keeping today's behavior
  field-for-field; the default was decided by the consumer, whose hard 10s client
  timeout would have made a default-on hold abort and log on every poll. Waiting
  uses a dedicated Redis client with an explicit `PoolSize` because BLPOP pins its
  connection and the shared pool has none. No new errcode, i18n entry, endpoint or
  migration — an expired hold reuses the existing OK empty-batch shape.
  Known bounds recorded rather than hidden: drain can be extended by one 5s chunk
  (no module shutdown hook), a failing doorbell degrades holds silently until G1
  adds metrics, and hold budgets are per process (`maxEventHolds × replicas`
  fleet-wide). Brief/context under `.octospec/tasks/bot-events-longpoll/`; shared
  journal `.octospec/journal/shared/bot-events-longpoll.md`; learning candidate
  `.octospec/learnings/pending/bot-events-longpoll.md` (lower-bound assertions for
  timing promises → `testing`). Consumer half is a sibling change in
  openclaw-channel-octo. PR #685.

## 2026-07-30 (space-join-apply-resubmit)

- **Recoverable join applications** — A pending Space join application now adopts
  a freshly submitted invite code (refreshing the application time and
  re-notifying admins) instead of staying bound to a spent/disabled/expired one,
  so an applicant can repair their own stuck application without an admin
  rejecting it first. Approval-time invite failures are classified (exhausted vs
  invalid), notify the applicant, and share one implementation across all three
  approval entry points. Invite-slot consumption stays at approval time — a
  tracked follow-up. Upstream #683. See
  [journal](journal/shared/space-join-apply-resubmit.md).

## 2026-07-30 (cardtmpl-reasoning-controls-hidden-successor)

- **Safe reasoning successor** — Added immutable
  `ai.reasoning-process@0.3.0` from the bounded V2 contract, removing unsupported
  stop/retry Submit controls while preserving five states, the local reasoning
  toggle, and active/error=`octo/v2`, result=`octo/v1`.
- **Version cutover** — Registry default and Bot new sends now select V3;
  V1/V2/V3 remain available only for same-exact-version historical edits.
  Runtime static reconciliation claims V3 and dynamic same-key collision stays
  fail-closed.
- **Visual-profile contract** — Raw Bot send/edit callers no longer need to
  provide `render_profile`; the Bot API authors `octo-chat/v1` on effective
  frames. Registry callers still cannot override the server-authored manifest
  value. See
  [journal](journal/shared/cardtmpl-reasoning-controls-hidden-successor.md) and
  [brief](tasks/cardtmpl-reasoning-controls-hidden-successor/brief.md).
- **Boundary** — No OpenClaw release, active-run stop/retry, dynamic grant, or
  production gate enablement is included. V3-capable server rollback support is
  required after the first V3 message is sent.
- **Review closure** — The runtime-catalog runbook now forbids routine
  static-to-static Activate/Rollback and requires active-pointer compatibility
  before binary rollback, preventing an older image from treating a persisted
  static V3 target as sticky global integrity failure. Protocol docs now include
  the Bot templating/empty-submit capability; focused tests reject legacy V3
  stop/retry IDs through the Submit and ActionContext gates and caller-owned
  Registry `render_profile`. The reported V1 self-check gap was mutation-tested
  and is already closed by registration-time `cardmsg.Validate`; a regression
  now locks it, and the V3 RouteSpec test directly covers its intended branch.

## 2026-07-29 (cardtmpl-runtime-catalog-overlay, roadmap E3 PR-B server)

- **Runtime overlay** — Added one composition-root RuntimeCatalog over frozen
  built-ins and the immutable MySQL artifact store, with authoritative
  exact/default resolution, bounded compiled caching, and fail-closed dynamic
  authorization. See [journal](journal/shared/cardtmpl-runtime-catalog-overlay.md),
  [brief](tasks/cardtmpl-runtime-catalog-overlay/brief.md), and Issue #672.
- **Audited state machine** — Added revision-CAS activate, explicit prior-active
  rollback, one-way emergency block with fallback/disable, manager detail/audit,
  and transactional state-plus-audit persistence.
- **Runtime consumers** — Migrated Bot template send/edit, notify, CardUpdater,
  and message action-context to the catalog interface with server-authored
  purpose/principal/Space context. Control and dynamic new-send remain dark and
  no production grants are installed.
- **Recovery hardening** — Startup reconciliation is asynchronous and retryable;
  integrity remains sticky while super-admin detail/audit diagnostics stay
  readable. Static interactive rollback targets no longer inherit the dynamic
  RouteSpec precondition, and notify catalog DB calls are deadline-bounded.
- **Current-head review closure** — Catalog startup state now participates in
  `/v1/ready`; active-target validation has a 128-target hard cap, independent
  per-target deadlines, and a bounded gauge. Notify preserves and fail-closes
  runtime catalog safety errors, Bot construction lookups are deadline-bounded,
  and audit paging/prior-active lookups have supporting indexes. Trusted
  producer provenance is explicitly deferred as a PR-C grant prerequisite.
- **Trust-boundary closure** — Runtime schemas now reject non-empty
  `patternProperties`; `additionalProperties=false` alone does not bound regex
  keyspaces.
- **Verification boundary** — Core cardtmpl/catalog coverage exceeds 80% and all
  focused, race, clean-DB integration, build, vet, lint, i18n, and diff gates are
  green locally. Legacy Bot/notify/message whole-package coverage remains below
  80%, so that literal brief checkbox stays open. PR #674 merge and the PR-B
  rebase are complete; post-rebase CI/current-head approval, E1d/E1e, PR-C
  grants, joint E2E, and production enablement remain pending.
- **Learning (pending)** — A fail-closed runtime must retain an authenticated,
  bounded diagnostic read path. See
  [learning](learnings/pending/cardtmpl-runtime-catalog-overlay.md).

## 2026-07-28 (cardtmpl-runtime-catalog, roadmap E3 PR-A)

- **Dark publishing foundation** — Added the shared strict JSON artifact
  compiler, deterministic canonical identity, immutable static/dynamic version
  claims and artifacts, transactional publish audit, and startup static
  inventory reconciliation. See
  [journal](journal/shared/cardtmpl-runtime-catalog.md),
  [brief](tasks/cardtmpl-runtime-catalog/brief.md), and PR #674 / Issue #669.
- **Control plane** — Added super-admin-only, authenticated and UID-rate-limited
  validate/publish endpoints with a 2 MiB body cap, localized safe errors, and
  bounded operation/compile/DB metrics. Published artifacts remain inactive and
  are not read by production render/send/edit paths in PR-A.
- **Fail-close hardening** — Runtime manifests now align identity lengths with
  persistence columns, reject ambiguous SemVer/Unicode identities, resolve local
  schema refs (including array items) with cycle detection, bind samples within
  their declaring view, and expose canonical manifest metadata.
- **Review hardening** — Added full static/runtime canonical parity coverage,
  contention-safe static-claim upsert plus bounded retry, and a distinct internal
  error/metric classification for persistent catalog-integrity failures. The
  bounded-schema proof is now a context-aware, visit-budgeted single traversal;
  publish DB work has a 10-second deadline, bounded whole-transaction retry for
  MySQL 1205/1213, and separate low-cardinality failure outcomes.
- **Final review closure** — Unified golden/runtime number semantics, made
  canonical integer limits notation-independent and recompile-safe, rejected
  open-keyspace `patternProperties`, preserved `allOf` traversal-abort signals,
  fixed mixed exact/positional sample assignment, made validation-document
  selection deterministic, removed UTF-16 sort allocations and a redundant
  envelope/bundle decode pass, and durably audited immutable/static-source
  publish conflicts without mutating artifacts. A real-MySQL concurrency test
  now verifies same-hash idempotency, different-hash immutability, audit results,
  and migration Up/Down cleanup.
- **Startup recovery** — Kept exact-key reconciliation fail-closed and documented
  the safe PR-A break-glass path in the
  [runbook](../docs/card-template-runtime-catalog-runbook.md): roll back or
  correct the conflicting image and assign a new built-in version; never delete,
  rewrite, or bypass permanent claim/artifact state.
- **Learning (pending)** — Artifact validation must prove both persistence
  compatibility and worst-case schema resource bounds; unknown or union schema
  forms cannot be treated as bounded by omission. See
  [learning](learnings/pending/cardtmpl-runtime-catalog.md).
- **Boundary** — activation/rollback/block/runtime overlay remain PR-B; grants,
  B1/B2, and Bot capability merge remain PR-C. Object keyspace bounds and strict
  untrusted render-field decoding must close before dynamic activation.

## 2026-07-24 (bot-card-template-consumption, roadmap E1b)

- **Protocol** — Added the explicit Bot Registry template catalog to
  `/v1/bot/card/profile` and Registry-backed `template_ref + state + data`
  modes to Bot send/edit. Server owns view/profile/render metadata/Space/plain;
  raw Model B remains supported under a total XOR. See
  [journal](journal/shared/bot-card-template-consumption.md) and
  [brief](tasks/bot-card-template-consumption/brief.md).
- **Mutation safety** — Registry edits retain immutable template id/version and
  reuse `CardMutator` ownership, lifecycle, positive `card_seq` CAS, revision,
  and CMD paths. Transient frames skip revision history. Server-authored
  `template_ref` provenance plus metadata equality prevents raw cards from
  entering the Registry edit path.
- **Fail-close** — Only explicitly catalogued templates are advertised/accepted;
  JSON-template interaction reports are checked against rendered v2 samples at
  registration; invalid or forged requests have zero dispatch/mutation effects.
- **Learning (pending)** — JSON mutual exclusion must use raw key presence, not
  decoded zero values; otherwise empty string/null can bypass the both-present
  guard. See
  [learning](learnings/pending/bot-card-template-consumption.md).
- **Rollout boundary** — frozen `ai.reasoning-process@0.1.0` remains registered,
  but a bounded successor + Bot catalog cutover (or an explicitly disabled Bot
  card sub-gate) is required before Model A production enablement.

## 2026-07-24 (bot-send-permission-error-classification)

- **Error classification** — Bot API sends to a missing group or missing thread
  parent now reuse `err.server.bot_api.group_not_found` (wire 400, semantic
  404); real group-status and membership query failures remain internal
  `query_failed` results. Existing DM, Space, disbanded-group, and non-member
  denials are unchanged.
- **Observability and privacy** — added the bounded
  `dmwork_bot_send_permission_failure_total{stage,reason}` counter and one
  request-correlated terminal log without raw Bot/user/channel/group/thread/
  Space identifiers. OBO friend-gate lookup failures retain `not_friend`
  fail-closed behavior while using the same sanitized observer.
- **Verification** — handler tests cover group and parent-group absence, real
  DB failure, D14 wire semantics, trace correlation, and zero dispatch; focused
  tests cover DM/App Bot/Space/OBO outcomes and metric cardinality. All build,
  test, vet, lint, and i18n gates are green. See
  [journal](journal/shared/bot-send-permission-error-classification.md).
- **Learning (pending)** — when fail-closed authorization intentionally
  collapses an infrastructure error into a business denial, preserve a bounded
  internal diagnostic signal and emit it once at the request boundary. See
  [learning](learnings/pending/fail-closed-diagnostic-signal.md).

## 2026-07-24 (cardtmpl-reasoning-progress-card, roadmap E1a)

- **Feature** — Onboarded `ai.reasoning-process@0.1.0` as the first live JSON-mode
  card on the E1 engine (#654), via `Registry.RegisterJSON` (no Go `Build()`).
  Decision A: the producer's action buttons are kept (`reasoning_stop` /
  `reasoning_retry` `Action.Submit` + toggle) under a fixed owner `ai` /
  action_type `reasoning.control` (added `ai` to the L2a allowlist); `active`/
  `error` are octo/v2, the display-only `result` view is octo/v1 (mirrors
  docs.access-request 0.3.0). `Submit.data` carries owner/action_type so the
  ActionContract self-check passes; goldens synced. The button handler +
  RouteSpec + bot streaming delivery are downstream — the card is registered and
  renderable only. See [journal](journal/shared/cardtmpl-reasoning-progress-card.md).

## 2026-07-23 (cardtmpl-json-template-engine, roadmap E1)

- **Feature** — Added a JSON-template render path to `pkg/cardtmpl` (roadmap E1):
  a bounded Adaptive Card Templating engine (`pkg/cardtmpl/jsontmpl/`) + a generic
  `jsonTemplate` + `Registry.RegisterJSON`, so a card can register and render via
  `Registry.Render` from a JSON handoff without a hand-written Go `Build()`.
  Validated byte-for-byte against the `ai.reasoning-process@0.1.0` goldens. Made
  `BuildResult.DeepLink` optional (D7) for cards with no canonical URL. The 5 Go
  cards are untouched. See
  [journal](journal/shared/cardtmpl-json-template-engine.md); learning candidate
  `learnings/pending/template-engine-literal-bind-validate-backstop.md`.

## 2026-07-23 (cardtmpl-l2a-summary-migration PR-2)

- **Feature** — Roadmap C second slice: `summary.completed@0.1.0` and
  `summary.failed@0.1.0` (v1 display cards) migrated onto `Registry.Render`
  reusing the PR-1 (#649) copy-directory shape and the PR-1
  `BuildSummaryResourceCardBodyWithLang` scaffold. `NotifyReq` /
  `SummaryCardFields` external shape unchanged (plan B);
  `deliverCardNotification` routes both kinds through Registry with F7
  fail-close and C1 preflight, matching the docs display cards. Both
  manifests declare `renderProfile` + `renderProfileCompatibility` (#647).
- **Milestone** — After PR-2 the Registry holds 5 L2a cards
  (docs.access-request v2 + docs.commented v1 + docs.shared v1 +
  summary.completed v1 + summary.failed v1); every legacy display-card
  deliver branch is Registry-backed. Only `generic.approval` remains legacy
  (dynamic-owner conflict, tracked separately).
- **Test** — `card_via_registry_summary_baseline_test.go` runs 4 fixtures
  through legacy `buildSummaryCard` vs `buildSummaryCardViaRegistry` and
  asserts canonical byte-equality after stripping
  `metadata.octo.{protocol,template}`; C1 preflight and F7 unwired assertions
  extend the pattern PR-1 established. `TestMain` grows to register the two
  new cards. See [journal](journal/shared/cardtmpl-l2a-summary-migration.md).

## 2026-07-23 (group-welcome-message)

- **Feature** — Group new-member welcome (群入群欢迎语): a group's
  creator/manager configures **one** welcome via `GET/PUT/DELETE
  /v1/groups/:group_no/welcome` (new `octo_group_welcome_config`, insert-then-lock
  `UpsertMerged`), and on a human member's **first** join it is **posted publicly
  into the group channel** (`channel_type=GROUP`), at most once per `(group_no,
  uid)` via the new `octo_group_welcome_delivery` ledger. The body supports a
  `{member}` placeholder rendered to the joiner's display name. Delivery mirrors
  the per-Space engine (reconciler + worker + rotating cursor + `FOR UPDATE SKIP
  LOCKED` + CAS/at-most-once) as a **parallel, group-scoped** copy — not a
  refactor of the reviewed space code. **No platform-global content fallback**: a
  group with no enabled row gets no welcome.
- **Config** — new master switch `onboarding.group_welcome_enabled` (bool,
  **default off** = dark launch), read via `SystemSettings.GroupWelcomeEnabled()`,
  checked at enqueue/reconcile/worker. Enablement only — no content fallback;
  flip-off is an instant, reversible kill that touches no per-group rows. Ships
  double-inert (master off AND per-group `enabled=false` by default).
- **Behavior** — burst coalescing: a batch invite (one `GroupMemberAdd` event → N
  rows) is delivered as **one** public post naming everyone (`{member}` → joined
  list; overflow → `…、nameK 等 N 人`) instead of N posts. Freshly-enqueued rows are
  held a short coalesce window via `next_retry_at`; the worker `claimBatch`es a
  group's due rows and posts once, preserving per-row at-most-once (shared
  `message_id`). One coalesced post per group per wake also rate-limits a single
  group's welcomes.
- **Test** — full `-race` suites green for `common`/`notify`/`group`/`space`;
  committed **live e2e** (`group_welcome_e2e_test.go`) drives the whole pipeline
  against real WuKongIM, confirming `notification` posts into a group channel it
  is not a member of and that a burst coalesces to one `message_id`. Incidental
  polish carried from #646 (Upsert doc caveat, `r.Enabled` read, request-context
  DB, DELETE→enabled-global-fallback test). See
  [journal](journal/shared/group-welcome-message.md).

## 2026-07-23 (cardtmpl-l2a-migration PR-1)

- **Feature** — Roadmap C first slice: `docs.commented@0.1.0` and
  `docs.shared@0.1.0` (v1 display cards) migrated onto `Registry.Render` via
  the pilot copy-directory shape. `NotifyReq`/`DocsCardFields` external shape
  unchanged (plan B); `deliverDocsCardNotification` routes both kinds through
  Registry with F7 fail-close, matching `access_requested`. Both manifests
  declare `renderProfile` + `renderProfileCompatibility` (#647).
- **Milestone** — §2.2-5 L2b hard gate ② is now met: `docs.access-request`
  (v2) + `docs.commented` (v1) + `docs.shared` (v1) = 3 L2a cards running
  the full Registry path across both wire profiles.
- **Base helpers** — `pkg/cardtmpl.SanitizeLine` is the single source of
  truth (G5, notify + pilot become wrappers).
  `BuildSummaryResourceCardBodyWithLang` added to scaffold PR-2.
- **Test** — `card_via_registry_display_baseline_test.go` runs 4 fixtures
  through legacy `buildDocsCard` vs `buildDocsDisplayCardViaRegistry` and
  asserts canonical byte-equality after stripping the two injected
  `metadata.octo` fields. `modules/notify/testmain_test.go` wires the
  Registry once for the whole test package (mirrors production wiring). See
  [journal](journal/shared/cardtmpl-l2a-migration.md).
- **Review fix (P1, PR #649)** — Over-length display fields were flipping to a
  400 zero-delivery under the C1 preflight where legacy truncated & delivered.
  `mapDocsCardFieldsToDisplayJSON` now server-side truncates title/actorName/
  excerpt/updatedAt to the schema/render caps before preflight (delivery
  preserved); `docId` stays a hard C1 400 (deep-link key). Exported
  `cardtmpl.MaxTitleRunes` + `cardtmpl.TruncateRunes` (single cap/impl, G9);
  added `TestSchemaCapsMatchRenderCaps` (closes the previously-dangling G9
  field-cap reference) + `TestMapDocsDisplayFields_TruncatesDisplayFields` +
  `docs_shared` en test; made the docs build-error log label kind-generic.

## 2026-07-22 (incoming-webhook-quota-per-thread)

- **Behavior change** — Incoming Webhook creation quota re-scoped from
  *per parent group* to *per delivery scope* `(group_no,
  thread_short_id)`: the group itself and each thread (子区) now hold
  independent webhook budgets instead of sharing one per-group cap.
  `insertWithQuota` narrows both the group-level and per-creator
  `COUNT(*)` by `thread_short_id`; the `FOR UPDATE` serialization lock
  stays on the parent `group` row (narrowing it would reintroduce the
  gap-lock deadlock). Motivated by Octo Loop provisioning a webhook per
  thread. Supersedes the `incoming-webhook-thread` task's locked
  "threads share `max_per_group`" decision.
- **Config** — `incomingwebhook.max_per_group` /
  `incomingwebhook.max_per_creator` keep their keys and defaults
  (10 / 5) but are reinterpreted "per delivery scope"; setting docs +
  admin schema descriptions + the two 409 quota messages (en-US markers
  + zh-CN) updated. No schema/data migration; existing rows
  (`thread_short_id=''`) fall into the group-self bucket.
- **Config (follow-on)** — added two precise-control knobs:
  `incomingwebhook.max_per_thread` (per-thread scope cap, decoupled from
  `max_per_group`; falls back to it when unset) and
  `incomingwebhook.max_total_per_group` (group-wide aggregate ceiling
  across the group + all threads; `0`=disabled default). `insertWithQuota`
  evaluates all three quota layers inside the one parent-group `FOR UPDATE`
  critical section (race-exact; verified by a concurrent aggregate-ceiling
  test under `-race`). New 409 `mgmt_total_quota_exceeded`. See
  [journal](journal/shared/incoming-webhook-quota-per-thread.md).

## 2026-07-22 (cardtmpl-interaction-closure)

- **Feature** — Closed the post-#633 interactive-card loop (roadmap group A).
  `CardUpdater` (`ReplaceView` + progress-frame `Append`) composes the existing
  `CardMutator` CAS/revision/CMD path; `docs.access-request@0.3.0` adds an
  `approved`/`rejected` `result` view (`octo/v1`) registered beside the frozen
  `0.2.0`; the docs finalizer now upgrades approved/denied cards in place to
  `0.3.0/result`. In-flight `0.2.0` pending cards upgrade too — missing
  decorative fields are omitted, not fabricated.
- **Contract** — Route-versioned callback envelope: `legacy` flat body remains
  the default (byte-compatible), `octo-card-v1` opt-in nested envelope carries
  `protocol`/`type=card.action`/`card.{…}`/`trigger_id`; `response_url` stays
  reserved (no authenticated response body defined in §7). See
  [journal](journal/shared/cardtmpl-interaction-closure.md).
- **Observability** — Bounded counters `dmwork_cardtmpl_callback_total`
  (`ok|rejected|error`) + `dmwork_cardtmpl_update_total` (`ok|error`); labels
  only from registered metadata + declared interactions.
- **Learning (pending)** — `card_seq` for authoritative updates must come from
  a monotonic source; the docs finalizer reuses `event.EventID`, an implicit
  contract now documented. See
  [learning](learnings/pending/cardtmpl-interaction-closure.md).

## 2026-07-22 (cardtmpl-registry-pilot)

- **Feature** — Introduced the octo-card@1.0 platform base
  (`pkg/cardtmpl.Template` + `Registry` + `Registry.Render`
  8-step pipeline) and migrated `docs.access-request@0.2.0` as the
  first L2a pilot. `metadata.octo.{protocol,template}` are now
  injected by the base on every payload rendered through the registry
  (docs approval-request cards, initially).
- **Contract** — `docs/platform-card-base.md` is added as the L0
  authoritative contract; `docs/l2b-owners.md` reserves the empty L2b
  owner allowlist. Handoff artefacts (manifest / contract /
  samples / reports) live at
  `pkg/cardtmpl/docs_access_request/handoff/docs.access-request@0.2.0/`
  and are the machine-readable cross-repo reference.
- **Behavior change** — For docs `access_requested` cards with the
  approval gate on, schema-level field errors returned by
  `Registry.Render` (typed `cardtmpl.ErrFieldsInvalid`) now become
  **HTTP 400 zero-delivery** rather than degrading to a plain-text DM
  (C1 policy).
- **Fix / hardening** — Rewrote the pilot `pending.interaction.json`
  to match real Go action IDs and dataKeys, so the A15c interaction
  contract lock is code-vs-report equality instead of a
  design-phase-vs-code superset check.
- **Learning** — Deposited
  `cardtmpl-registry-pilot.md` under `.octospec/learnings/pending/`:
  a handoff schema authored for a *full compiled card* is NOT the
  same as a caller-input schema and should not be wired unchanged as
  the Registry input contract.

## 2026-07-21 (space-welcome-per-space-admin-crud)

- **Feature** — The onboarding welcome message became **per-Space and
  self-service**. Space admins (Role>=1) CRUD one config per Space via
  `GET/PUT/DELETE /v1/space/:space_id/welcome` (new `octo_space_welcome_config`
  table + `common.SpaceWelcomeConfigStore`). Follows #604/#606 which shipped a
  single platform-designated Space, superadmin-only; lifts that task's
  out-of-scope "per-Space admin self-service" item into scope.
- **Precedence** — A present per-Space row wins over the platform-global config
  outright, even when disabled (opt a Space out of a global campaign); no row →
  the global config applies iff it names the Space; else off. Global config kept
  as a superadmin fallback. Ships `enabled=false` per Space (no behavior change).
- **Delivery driver** — notify's event/reconciler/worker went single-Space →
  all-enabled-Spaces, resolving the per-Space effective config each cycle. Both
  reconciler and worker rotate a per-replica cursor over the enabled set for
  fairness (a greedy in-order worker starved tail Spaces under sustained load —
  see [learning](learnings/pending/multi-space-worker-rotating-cursor.md)).
  Cross-space sweep added (`idx_sweep`); ledger state machine / at-most-once /
  sender identity unchanged.
- **Verified** — all gates + `-race` suites green; real-wire e2e against live
  MySQL/Redis/WuKongIM confirmed actual receipt and no cross-Space mixing (each
  recipient's channel read back from the IM). See
  [journal](journal/shared/space-welcome-per-space-admin-crud.md).

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
