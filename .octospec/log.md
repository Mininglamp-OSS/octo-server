# Change log

Change history for this repo's `.octospec/`, following the
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
change-log convention (§7). Newest first.

## 2026-08-24 (group-exit-notice-visibility)

- **Fixed** — 「某成员退出群聊」系统提示（`type=1021`）改为**全员可见 + RedDot:0**，
  与 bot 级联移除 Tip 同一套语义。此前它带 `visibles` 白名单只给一位管理员可见，
  非管理员看不到气泡却被计一格未读、且永远消不掉。实现走 octo-server 侧新增的
  `modules/group/group_exit_notice.go`，**不动 octo-lib 的 `SendGroupExit`**。
  See [journal](journal/shared/group-exit-notice-visibility.md).
- **Learned** — `visibles` 挡的是**内容**，挡不住 **seq**。IM 的未读是纯游标减法
  （`unread = latest_msg_seq − read_seq`），与 `red_dot`、`visibles` **都无关**
  （octo-im `build.go` / `sync.go`，octo-server 只原样透传）。所以「改红点字段」
  这个直觉修复**完全无效** —— 真正修好未读的是让消息可读。推论：单条共享 channel log
  里，「只给部分人看的持久气泡」与「不给其他人产生未读」不可兼得。
- **Guarded** — 可见性白名单里藏着一条**静默早退**：白名单为空（群里没有其他
  管理员）时整条提示不发。那不是产品规则，只是可见性实现的副作用。现已由
  `TestGroupExitTipSentWhenNoOtherAdmin` 钉死。`groupExit` 侧还连带去掉了一个
  查询失败即 500 中断整个退群的错误分支。
- **Learned** — 差分验证若只回退**改动面的一部分**，同样失真。第一轮只回退了 helper
  的 payload，两条测试确实红了，但那条早退门槛的断言从未被验证过；补做整段还原才
  跑出 `"[]" should have 1 item(s), but has 0`。移除了 N 个行为门槛，就得逐个还原。
  See [learning](learnings/pending/group-exit-notice-visibility.md).
- **Known gap** — `groupExit` handler 那处发送门槛的改动**无运行中的测试**：
  `api_test.go` 整片 HTTP handler 测试（19 处 skip）卡在 issue #17 的路由重复注册
  （解除 skip 会 `panic: handlers are already registered`）。补齐前置是先修 #17。
- **Out of scope** — 排查中确认的第二个问题（跨端已读不同步：`unreadClear` CMD
  `NoPersist:true` 离线丢失、`readed_to_msg_seq` 被 octo-lib 丢弃、iOS
  `reconcileServerSnapshot` 走 `MAX` 本地优先）未动，独立任务。

## 2026-08-23 (bot-owner-self-removal)

- **Implemented** — 普通群成员现在可以把自己名下（`robot.creator_uid`）的 bot 移出
  群聊。`memberRemove` 在调用方非 Creator/Manager 时落到一条窄口径自助分支：目标必须
  **全部**是本群内属于调用方的活跃 bot，否则整批拒绝。成员列表新增 per-viewer 的
  `bot_owned_by_me` 供前端逐行判权；「你被 X 移除群聊」换成 owner 视角的 Tip；两条
  移除路由挂上 `SharedUIDRateLimiter`（它们现在对普通成员开放）。
  上游 Mininglamp-OSS/octo-web#1511。
  See [journal](journal/shared/bot-owner-self-removal.md).
- **Guarded** — 移除侧的判据必须是默认拒绝的白名单（`QueryBotUIDsOwnedByUIDs`）。
  复用入群侧的 `checkBotOwnership` 会是提权漏洞：它对非 bot UID 返回 nil，搬到移除侧
  等于放开踢人权限。由 `TestBotOwnerSelfRemoval_RejectsHumanTarget` 钉死。
- **Fixed in review** — 三处只有 code review 才抓到的问题：授权谓词漏用活跃口径，
  让被拉黑成员拿到一个能改群成员表并写持久化 Tip 的写操作；`queryMemberWithGroupNoAndUID`
  漏选 `group_member.robot`，使 `bot_owned_by_me` 在 memberGet 上静默恒 false；
  前端只把 `removeAction` 透传给「查看全部」，而该入口在 19 人以下的群根本不渲染，
  功能等于没上。
- **Learned** — `register.GetModules` 用进程级 `sync.Once` 构造模块实例，一个测试
  二进制里 handler 永远持有第一个 `NewTestServer` 的 ctx。因此对系统消息的断言必须
  走 service 层，走 HTTP 路由时 IM 桩只对进程内第一个测试生效（表现为「单独跑绿、
  一起跑红」）。候选规则见 `learnings/pending/bot-owner-self-removal.md`。

## 2026-08-21 (space-member-removal-cleanup)

- **Implemented** — Space member removal now takes the member out of the Space's
  groups and sub-threads instead of only soft-deleting `space_member`. A
  transactional outbox (`space_member_removal_cleanup`) drives a leased, retried
  cascade that exits every group in the Space (full group-exit semantics, with
  creator handover first); the `SpaceMiddleware` and notify membership caches are
  invalidated inside the request. Covers all five removal paths — the
  owner-initiated `DELETE /v1/space/:space_id` was not in the original survey
  because it only flipped `space.status`.
  See [journal](journal/shared/space-member-removal-cleanup.md).
- **Split** — Person-channel (DM) isolation was originally part of this task and
  was moved to `space-member-dm-isolation` after eleven review rounds. Two
  measured reasons: WuKongIM's `whitelistOffOfPerson` defaults to `true`, so the
  DM half changes no delivery behaviour in any current deployment; and it did not
  converge by point fixes (a new escape in six consecutive rounds, with
  structural root causes). Keeping it would have blocked a group cascade that is
  live behaviour today behind a half that is inert.
- **Learning (pending)** — Deferred cleanup must be scoped with was-ever
  predicates: an is-currently predicate observes the state the trigger already
  destroyed and turns the job into a silent no-op. See
  [learning](learnings/pending/cleanup-predicate-tense.md).

## 2026-08-13 (cutover-framework)

- **Refactor** — Task `cutover-framework`: extracted the control plane the three
  one-way cutover mechanisms (#627 msgextra, #697 botevent, #733 token session)
  had hand-written separately into the new leaf package `pkg/cutover` —
  singleton state read, FOR UPDATE CAS flip with under-lock evidence and floor
  bounds, and the malformed-fails-closed expected-mode guard — and folded the
  two standalone operator tools into the server binary as
  `app cutover <domain> {preflight,activate,status}` so they finally ship in
  the image (the #733 precedent, generalized). Refusal conditions, floor
  semantics (#627 inclusive vs #697 strict), sentinel errors, and runtime hot
  paths are unchanged; the characterization tests moved with the code. The
  session rollout stays on its own five-phase surface and shares only the
  documented conventions (`docs/cutover-framework.md`: state-table template,
  `OCTO_<DOMAIN>_EXPECTED_MODE` naming, the flip-then-arm ordering invariant,
  evidence discipline, Down 3819 pattern). Runbooks now live in `docs/`
  (msgextra moved, botevent written for the first time). A review round fixed
  twelve findings on top, four of them operationally material: the endpoint
  print echoed the full MySQL DSN (password included) and is now redacted; a
  committed flip could be reported as a failure when releasing the pinned
  connection failed; msgextra never named the Redis instance whose scan sets its
  cutover floor; and `msgextra status` hard-failed on a missing state row,
  hiding the guard readout in exactly the state that fails every write closed.
  A second review round fixed twelve more, led by a regression from the first
  round's own fix: the signal handler added to make a wedged activation
  abortable had disabled default termination while no evidence phase could
  observe the context, so the command ignored every Ctrl-C. Interrupts are now
  two-stage (cancel, then restore default handling so a second signal
  terminates), the botevent score ceiling moved into the domain, and the
  msgextra mode constants alias the shared ones rather than restating them.
  A third round added a schema conformance test for every registered domain's
  state table (asserting the DDL from the live migrated schema and the inert
  seed from the migration source), and a fourth closed thirteen more: an
  operator interrupt was being reported as unreadable evidence, the interrupt
  notice raced process exit, two botevent MySQL reads still ignored the
  deadline, SIGTERM now exits 143 rather than sharing SIGINT's 130, and the
  operator commands say which config file they resolved again (on stderr, so
  `session-rollout status` stays parseable).
  See [journal](journal/shared/cutover-framework.md).

## 2026-08-12 (profile-visibility-system-bot-whitelist)

- **Task** — `profile-visibility-system-bot-whitelist`: took the public-bot
  exemption in the shared person-profile visibility decision off the writable
  `user.category` column and put it on the `pkg/space.SystemBots` whitelist, so a
  `category=system` row that is not a system bot — the superuser account, which
  has a fixed guessable UID — no longer skips the Space, friend, and common-group
  legs. The input field was renamed `SystemAccount` to `SystemBot` to close the
  wiring that caused the defect, and both endpoints moved together. Recorded as a
  narrowing of the authorization input, not an incident fix: no material
  disclosure was reproduced. The two endpoints differ, and the record states them
  separately — `/v1/users/:uid` withheld the short number via the `Follow` gate at
  `modules/user/api.go:1431-1436`, while `/v1/channels/:id/:type` has no such gate
  and did hand a stranger the superuser's `extra.short_no` plus online state. That
  value is a public seed constant in this repository and carries no PII and no
  capability; `username` and `vercode` were checked and never reached a stranger.
  Two inherited tests had asserted the defective behavior and were rewritten, with
  reverse regressions added on both endpoints for `system` and `customerService`.
  **Operators upgrading**: any human support account sitting on
  `category=customerService` with `robot=0` degrades to the minimal profile set on
  deploy; `SystemBots` is a compile-time literal, so the supported remedies are
  adding the UID to that whitelist, marking the account `robot=1`, or relying on a
  normal relationship path. Focused MySQL-backed runs and the prior authorization
  matrix pass, before and after rebase. See
  [journal](journal/shared/profile-visibility-system-bot-whitelist.md).

## 2026-08-12 (botfather-space-binding-hardening)

- **Task** — `botfather-space-binding-hardening`: made the server authoritative
  for conversational Bot-to-Space binding. Payload/channel Space IDs are now
  selectors only; active creator, Space, and membership are checked fail-closed
  before creation and again under row locks in the membership transaction.
  Missing selectors require exactly one active creator Space. Binding or core
  persistence failures compensate created artifacts and revoke credentials,
  while external failures remain generic and logs remain identifier-free.
  Review hardening scopes friendship compensation to the newly-created
  creator/Bot pair (indexed range access, not a full-table scan), documents the
  binding lock order, and covers zero-row insert and begin-failure paths.
  MySQL-backed regression, end-to-end Space-isolation, race, coverage, vet,
  lint, i18n, cleanup, and redaction checks passed. See
  [journal](journal/shared/botfather-space-binding-hardening.md).

## 2026-08-12 (login-audit-ip-spoofing)

- **Fix** — Task `login-audit-ip-spoofing`: login, account-creation, logout, and
  OIDC audit/guard paths now use the same validated proxy-aware client-IP source
  as shared rate limiting. The stacked server change covers all 15 user and four
  OIDC sources, preserves the audit/wire/quota contracts, and now pins merged
  octo-lib #119 commit `233dd6f`. Review follow-up closes empty-IP callback
  guard bypasses with a stable unknown bucket, directory-wide source guards,
  and a routed user audit assertion. Production CLB/direct-access checks remain
  rollout gates; independent incoming-webhook parsing is a follow-up. See
  [journal](journal/shared/login-audit-ip-spoofing.md).

## 2026-08-11 (token-session-rollout-simplify)

- **Task** — `token-session-rollout-simplify`: collapsed the #725 five-phase
  session rollout into a MySQL-authoritative control plane. A singleton owns
  floor/cap/version/pause, and append-only evidence commits in the same
  transaction as each floor or cap CAS; #725 Redis floor and legacy MODE/MAX are
  one-time takeover inputs only. Runtime mode publication fences issuance,
  applies local state, then atomically publishes writer state + lease; failure
  stays fenced without breaking existing-session reads. Observe, migrate and
  reconciler share a scan-owner lease and `run_id`-bound scanner that discards
  counters on failover. The writer registry still proves fleet convergence;
  empty token keyspace is valid absence evidence, while an empty writer set is
  a blocker. The reconciler ships disabled, and rollback to a Redis-floor-only
  artifact is forbidden after the MySQL floor or cap changes. Tooling remains in
  `app session-rollout`, reads MySQL state directly, and exposes an audited
  `set-cap` path rather than restoring env/Redis authority. See
  [journal](journal/shared/token-session-rollout-simplify.md) and
  [verification](tasks/token-session-rollout-simplify/verification.md). PR #733.
- **Learning (pending)** —
  [characterize-before-you-design](learnings/pending/characterize-before-you-design.md):
  the brief was written from code reading and verified afterwards; the
  verification found a defect that changed a design decision, so it landed as a
  patch. A runnable characterization of current behaviour belongs in Plan as an
  input, split into invariants that must stay green and tripwires that must go
  red.

## 2026-08-10 (token-lifecycle-hardening PR 2)

- **Task** — `token-lifecycle-hardening-pr2`: added an inert-by-default,
  monotonic v3 session rollout; absolute deadlines, generation/fence validation,
  generation-scoped bounded indexes, durable high-risk revocation intents, and
  a controlled legacy migration/admin tool. Review follow-up closed owned v3
  compensation/index leaks, disabled scan-code redemption, campaign resume,
  floor-evidence, lease and post-commit retry gaps; finite migration policy is
  now explicit rather than hardcoded. The production runbook pins Redis,
  connection, old-replica, required-floor and irreversible rollback gates.
  Production activation, device-scope completion, migration, enforce, and
  security retest remain explicit gates. See
  [journal](journal/shared/token-lifecycle-hardening-pr2.md).
- **Learning (pending)** —
  [scope-revocation-cleanup-to-generation](learnings/pending/token-lifecycle-hardening-pr2.md):
  a monotonic authority update does not make later shared-index cleanup safe;
  partition cleanup by the exact revoked generation and test secondary
  invariants such as caps after event replay.

## 2026-08-09 (token-lifecycle-hardening PR 1)

- **Task** — `token-lifecycle-hardening`: bounded all new and touched user HTTP
  bearers with the existing Token TTL, centralized readers/writers in a shared
  Redis Session Store, preserved deadlines atomically across replicas, added
  strict v3 forward validation, startup Lua compatibility probing, bounded pool
  metrics, and a rate-limited aggregate-only migration observer. This PR does
  not bulk-expire historical persistent v1/v2 sessions; v3 activation,
  generation/indexed revocation, migration apply/enforce, and final security
  closure remain PR 2 plus controlled operations. See
  [journal](journal/shared/token-lifecycle-hardening.md).
- **Learning (pending)** —
  [scope-explicit-empty-env-validation](learnings/pending/token-lifecycle-hardening.md):
  distinguish absent from explicitly empty security env values by reading the
  one documented key; never mutate shared configuration semantics as a local
  validation shortcut.

## 2026-08-09 (scan-login-authorization)

- **Task** — `scan-login-authorization`: added a deployment-wide
  `login.scan_enabled` policy gate, split scan from explicit confirmation, bound
  redemption to the browser-issued `poll_secret`, and made confirmation and
  redemption atomic across replicas. Review follow-up added a per-UUID claim so
  different auth codes for one displayed QR cannot both become redeemable,
  made the rollout gate default disabled and fail closed before the first
  successful settings load, and scrubbed confirmation credentials from logs.
  Anonymous polling exposes credential fields
  only to the matching browser and keeps strict per-IP limits; non-finite scan
  limiter values fall back to finite defaults. Incomplete post-consume login work
  emits a bounded warning, while failed QR-state publication restores the pending
  confirmation without overwriting a newer attempt. The OIDC-only production
  rollout keeps scan login disabled before new binaries start and blocks the
  routes during any old/new mixed-version or rollback window. See
  [journal](journal/shared/scan-login-authorization.md).
- **Learning (pending)** —
  [redis-write-errors-have-ambiguous-outcomes](learnings/pending/scan-login-authorization.md):
  a Redis write error does not prove the write was absent; compensation and
  clients must tolerate the committed-write/error-response branch without
  reopening an authentication credential.

## 2026-08-09 (channel-get-object-authz)

- **Task** — `channel-get-object-authz`: `GET /v1/channels/:channel_id/:channel_type`
  (`channelGet`) had no object-level authorization — `loginUID` was a render
  param, never an authz subject, so any authenticated caller could swap
  `channel_id` and read group detail (name/notice/member count/`space_id`) of
  groups they'd left or never joined, sub-channel metadata with no parent
  membership check, and any user's `short_no`/device flags/realname. Added
  per-type gating: GROUP/COMMUNITY_TOPIC require (parent) membership
  (non-member + missing → one `ErrGroupViewForbidden`, no existence oracle);
  PERSON is relationship-graded (self/friend/same-Space/common-group/bot/system/
  webhook → full; unrelated → a minimal whitelist DTO, *not* a 403, since this
  is the sole datasource for rendering arbitrary message senders). Mounted
  `SharedUIDRateLimiter`; fixed a missing-group nil-panic (500 empty body) that
  was itself an existence oracle. Traps: display-layer datasources are not an
  authz layer; a `group_member` row outlives a disbanded group (async cleanup)
  so `ExistCommonGroup` must `JOIN group` and exclude dissolved ones; a datasource
  error had stranded the handler's not-found branch (fixed via the
  `ErrorUserNotExist` sentinel → `ErrDatasourceNotProcess`); a minimal response
  needs its own DTO because `model.ChannelResp` has no `omitempty`. Origin: an
  internal security review (object-level read). `GET /v1/users/:uid` is the same
  root cause through a second door and is fixed in the same change: the decision
  now lives in `modules/channel/service` (dependency-free leaf that
  `modules/user` already imports) so the two endpoints cannot drift, with the
  common-group lookup injected via a registration hook because `modules/user`
  cannot import `modules/group`. The profile endpoint's minimal set keeps
  `follow` (the add-friend entry needs it) where the channel one omits it; both
  emit `status`, because clients read an absent `status` as their banned
  sentinel and persist it, so "absent" had to be decided per field rather than
  applied as a blanket rule.
  Review follow-ups: a public brief must not point at an unpatched sibling
  (fixed by landing both); `status = Normal` was too strict — admin-disabled
  groups are live everywhere else in the module, so the check is
  `<> GroupStatusDisband`; existence checks use `SELECT 1 ... LIMIT 1`. See
  [journal](journal/shared/channel-get-object-authz.md);
  learning [membership-row-outlives-its-parent](learnings/pending/membership-row-outlives-its-parent.md).

## 2026-08-07 (reminder-sync-membership-scope)

- **Task** — `reminder-sync-membership-scope`: `POST /v1/message/reminder/sync`
  returned every channel-level (`@所有人`) reminder in the system to any
  authenticated caller — `channel_id` / `publisher` / `message_id` /
  `message_seq` for channels they had never joined. `remindersDB.sync`'s
  `uid=''` branch had no membership predicate, and an empty client-supplied
  `channel_ids` meant "no filter" rather than "no channels". Channel-level rows
  are now scoped to the caller's active groups
  (`group.IService.ActiveMemberGroupNos`, mirroring `ExistMemberActive`'s
  `is_deleted=0 AND status=Normal`); the client's list can only narrow.
  The 2026-07-30 retest filed this as §4.11 "垂直越权 via X-Space-Id removal" —
  **that attribution would have produced a false fix**: the handler never reads
  the validated `space_id`, and a caller holding a header for a space they do
  belong to got the same dump. The primary regression test therefore keeps a
  valid `X-Space-Id` and still requires non-member rows to be absent. Scope
  covers Group and CommunityTopic (group membership) plus Person (party-hood —
  the recipient's uid *is* the `channel_id`, so no table is needed). Only
  CustomerService / Community / Info remain a knowingly-retained residual, and
  for a different reason: they have no producer anywhere in the server. The gate
  is an allowlist, so an unknown or future channel type fails closed. The
  residual set is pinned by `TestChannelLevelReminderChannelTypes`.
  Membership is matched with bind-parameter `IN`, not a join to `group_member`:
  the two tables can land on different collations per deployment (Error 1267),
  and pinning `COLLATE` costs the index — the trap documented in
  `20260711000001`. `SpaceMiddleware`'s fail-open and the same shape in DM Space
  filtering are deliberately out of scope. `EXPLAIN` is plan-neutral; the query
  was already a full scan on `main` (no `version` index), recorded as a
  follow-up rather than fixed here.
  See [journal](journal/shared/reminder-sync-membership-scope.md).
- **Learning (pending)** —
  [a-repro-is-not-a-root-cause](learnings/pending/a-repro-is-not-a-root-cause.md):
  a reproduction proves reachability, not which missing check allowed it; write
  the primary regression test against the variant that keeps the report's toggle
  intact.

## 2026-08-07 (scanlogin-poll-binding)

- **Task** — `scanlogin-poll-binding`: `loginstatus` no longer hands `auth_code`
  to any anonymous caller who knows the uuid. `loginuuid` mints a `poll_secret`
  (response body only, never in the QR payload) and `loginstatus` releases the
  credential fields only to a caller presenting it;
  everyone else gets the real status filtered through an allow-list. Both
  endpoints — unauthenticated by design, since the QR renders before any token
  exists — gained `StrictIPRateLimitMiddleware`; `auth_code` TTL 10min → 5min;
  the secret is revoked on redemption; the 10s long poll releases on disconnect
  and only reclaims its own channel. Cross-repo: octo-web replays the header.
  **This closes QR-observer hijack, not QRLJacking** — the attacker mints the
  uuid and so receives the secret too. The confirm-screen device context that
  *would* close it was pulled after review: every field of it is
  attacker-controlled today (gin trusts all proxies, so even the IP is
  forgeable), which would have turned weak evidence into false assurance.
  Tracked in octo-ios#71 / octo-android#116. Review also surfaced an auth-code
  expiry inversion that the TTL change had made reachable, a channel-displacement
  vector on the long poll, and post-login state (incl. Signal key material)
  outliving the session — all fixed here. The secret travels as a query
  parameter; a custom header was tried and reverted — it breaks cross-origin
  scan-login for a benefit that only holds against readers who already have
  Redis access. See [journal](journal/shared/scanlogin-poll-binding.md).

## 2026-08-07 (cardtmpl-reasoning-phase-tools-successor)

- **Task** — `cardtmpl-reasoning-phase-tools-successor`: published
  `ai.reasoning-process@0.4.0` carrying the front-end's per-phase collapsible
  tool panels and simplified header, adapted onto the bounded #667/#681 data
  contract instead of the handoff's unbounded schema. Registry default and Bot
  new-send cut to `0.4.0` by image release (not a runtime activation — Bot
  advertisement is a compile-time exact-version constant); `0.1.0`–`0.3.0` stay
  byte-frozen and exact-version editable. The handoff declared itself `0.3.0`,
  colliding with the live `0.3.0`, so the delta had to become a new exact
  version rather than an in-place edit. Three handoff defects were corrected
  rather than adopted: manifest-declared `submit_actions` (it is derived from
  the interaction report here), missing `owner`/`protocol`, and a schema with
  every bound stripped. See
  [journal](journal/shared/cardtmpl-reasoning-phase-tools-successor.md).
  Verify found two things no gate would have caught: the shipped interaction
  reports enumerated `${$index}`-generated toggle ids (true only for a 2-phase
  card — `assertInteractionReport` compares Submit/Input ids only, so an
  indexed id is an unverified published claim), and the "a matching golden
  cannot launder an injected `Action.Submit` in the `octo/v1` result frame"
  invariant had a generic-compiler test but none for this artifact.
- **Decision** — `phases[].thought` gets **two ceilings**: accept `4001`, display `400` with
  server-side truncation (`x-octo-constraints.truncateStrings`, new engine capability). It
  stops mirroring the producer's truncation length either way, and an over-long summary now
  renders clamped instead of failing the card. The display number is a **product decision**
  ("≤ 400 is fine, truncate above it, don't error"); the engine's real contribution is that
  frame size stops depending on caller input — 400 and 4001 produce byte-identical frames.
  Four review rounds to get here, and the fourth invalidated the previous three's arithmetic:
  they measured the render output, but `cardmsg.Finalize` afterwards adds a top-level `plain`
  worth **+47%**, and that is what gets stored. Re-measured on persisted bytes, `thought` turns
  out never to have been the dominant term (at `thought = 1` the worst frame is still **107%**
  of the 64 KiB column; the frame is dominated by 13 actions × (`tool` 81 + `detail` 192) plus
  `plain`), and the live `0.3.0` is **already at 121.6%** at its own bound — pre-existing debt,
  unfixable in place because its bytes are frozen. `tool` / `detail` / `errorMessage` keep their
  zero-headroom design and were knowingly left alone.
- **Decision** — The storage budget moved to the **write boundary**, since no single schema
  version can enforce it. `carddispatch.NormalizeFrameForPersistence` is now the one judge of
  "can this frame be stored", shared by `CardMutator.Mutate` and both `CardUpdater` paths,
  returning `ErrCardMutationTooLarge` (wrapping `ErrCardMutationInvalid`, so existing error
  mapping is unchanged) with the byte count. Covers `0.1.0`–`0.4.0` and every future template;
  turns MySQL `Data too long` / silent truncation into invalid JSON into a typed, logged refusal.
  Still open: an all-fields-at-maximum CJK frame persists at **92.6%** — under 5 KB of margin —
  so `MEDIUMTEXT` widening (own brief, hot table, `ALGORITHM=COPY, LOCK=SHARED`) or truncating
  `tool`/`detail` (a visual change, hence a product decision) remain the durable fixes.
- **Decision (reversed in review)** — D4a: the handoff's chevrons fetched from
  `api.iconify.design` — the repo's first outbound template dependency, permanent once the
  version's bytes freeze, and unreachable from mainland China networks. Now **inlined as vetted
  `data:image/svg+xml` bytes**. The first relaxation of `cardmsg`'s URL allowlist used a
  trusted-caller `ValidateOption` plus a substring denylist; review broke both — the option was
  applied at render but not at the three paths that re-validate before persisting (so every
  `0.4.0` card would send once and freeze on first edit), and the denylist had five reproduced
  bypasses (namespace prefixes, CSS identifier escapes, SVG 1.2 Tiny `<handler>`). Replaced by
  an **exact-byte allowlist**, which is smaller and makes all four problems unrepresentable
  rather than fixed. `data:` stays refused on non-image URL fields and for any unvetted bytes.
- **Learning (pending)** —
  [`schema-bound-must-not-mirror-producer-truncation`](learnings/pending/schema-bound-must-not-mirror-producer-truncation.md):
  a `maxLength` equal to `producer_cap + 1` is a coupling, not a bound — it is
  exactly saturated, and the two sides count in different units (JSON Schema
  code points vs JS UTF-16 code units vs graphemes, measured and confirmed).
  Over-limit means the whole card fails to send, not a display regression.
  Candidate rule: `trust-boundary`.
- **Review corrections (four rounds)** — the first draft of that learning ended "the bound was
  never protecting a real resource", and D3a claimed "~4× headroom" at `thought: 4001`. Round
  one showed both were measured against the wrong ceiling (512 KiB render gate vs 64 KiB storage
  column). Round two rejected keeping `4001` with a boundary refusal for *declared field length*
  — a contract published to *every* bot via `/v1/bot/card/profile` (which also advertises a
  512 KiB payload allowance) must not admit what the store cannot hold, however politely the
  write fails. Round three answered that with the accept/display split. Round four found the
  error under all three: **every byte figure had been measured on the render output, not on the
  bytes that get stored** — `cardmsg.Finalize` adds a top-level `plain` afterwards worth +47%.
  Correcting it reversed two conclusions (`thought` was never the dominant term; `0.3.0` is
  already over) and relocated the enforcement point from the schema to the write boundary. The
  same round found the inline-icon trust boundary applied at one call site out of four, with the
  gap invisible because the edit tests use a fake mutator that never re-validates.
- **Learning (pending)** —
  [`relax-validation-by-artifact-not-by-caller`](learnings/pending/relax-validation-by-artifact-not-by-caller.md):
  a validator relaxed via a trusted-caller flag has to be kept in sync across every path that
  re-validates the same artifact, and nothing types that coupling. Key the relaxation on the
  artifact (these exact bytes are vetted) instead — it cannot drift, it makes sanitizer bypasses
  unreachable, and it forces interpolated values out of the relaxed position by construction.

## 2026-08-06 (bot-setting-store)

- **Task** — `bot-setting-store`: added `bot_setting`, a generic per-bot config
  store, replacing "add another column to `robot`" as the way a bot-level switch
  gets stored. One registry backs the write whitelist, the owner-facing catalog,
  and the bot → `system_setting` → code-default resolution chain, so a new key
  is an entry rather than a migration. First consumer is the four card switches
  (`card_enabled` derived read-only from the deployment env; display /
  interaction / reasoning owner-editable, default true because the master switch
  is already fail-closed). `GET /v1/bot/card/profile` gained one additive
  `config` object; `sendMessage` enforces the same values independently.
  The precedent that looked right was the wrong one — `bot_mention_pref` is a
  table because it is two-dimensional, not because tables are preferred. See
  [journal](journal/shared/bot-setting-store.md).
  Three review rounds across four heads found three P1s, every one a sibling
  path or sibling consumer left behind when a new gate went in: the raw branch
  of `bot/message/edit`, the legacy robot send ingress, and the manifest that
  advertises what the gate accepts. The journal's "What review found that the
  author did not" section records the pattern rather than only the fixes.
- **Learning (pending)** —
  [`cleanalltables-does-not-reset-in-process-caches`](learnings/pending/cleanalltables-does-not-reset-in-process-caches.md):
  generalizes the existing "CleanAllTables does not clear Redis rate-limit
  buckets" note to every non-DB layer, after a process-wide `SystemSettings`
  snapshot leaked between cases and failed only under `-shuffle`.
  Candidate rule: `testing`.

## 2026-08-05 (bot-api-per-bot-ratelimit)

- **Task** — `bot-api-per-bot-ratelimit` (#696): moved bot rate limiting off the
  client-IP axis onto bot identity, and gave the two self-heal channels
  (`heartbeat`, `register`) quotas of their own. The reported cause was wrong in
  a way that changed the fix — the shared axis was IP, not "business vs
  heartbeat", so a bot was starved by a co-located neighbour rather than by
  itself. A follow-on incident showed protecting heartbeat alone is not enough:
  the reconnect path (`register`) was rate limited too, so the bot could notice
  it was down but never get back up. New `pkg/ratelimit` keeps quotas
  hot-tunable (lib's middlewares fix them at construction, which is what made
  the incident's own mitigation cost a 93-second oscillating rollout) and adds
  shadow mode so a candidate quota can be evaluated without touching clients.
  Every **per-bot** layer ships `enabled=false` + `dry_run=true`, so merging
  changes no limiting behaviour. The two pre-auth per-IP floors are the exception
  and are live on deploy — deliberately unswitchable, since a toggleable outer
  layer would leave the excluded heartbeat endpoint unprotected while the inner
  layer is off. Both were first sized at 2x the measured peak and revised upward
  before opening the PR: that is how you size a *quota*, whereas a floor only has
  to turn unbounded into bounded and needs an order of magnitude of headroom. See
  [journal](journal/shared/bot-api-per-bot-ratelimit.md).

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
  (no module shutdown hook), a whole page of undecodable members starves that one
  request (bounded, never spinning), and hold budgets are per process
  (`maxEventHolds × replicas` fleet-wide).
- **Review rounds 3–4 — the hold loop's progress guarantee.** Four review rounds
  each found a branch of `waitForEvents` that made no progress, so the loop was
  restated as one invariant rather than patched per finding: **every iteration
  either burns a chunk of wall clock or advances the queue cursor, never
  neither.** Concretely: a refused hold now pauses (round 3) under a budget of
  its own so back-pressure cannot become the resource sink (round 4); a failing
  BLPOP pays out its chunk and logs once per hold, instead of retrying at
  go-redis's ~8ms backoff (measured: **924 authoritative reads in 8s**); and the
  cursor advances from the read itself — before the App Bot filter, covering
  undecodable members — so progress no longer rests on an auto-ACK `ZREM` that
  only warns on failure, and the block is skipped only when the cursor actually
  moved (measured without that guard: **38,722 reads in 6s**). Both figures come
  from deleting the fix and re-running the regression tests, which count Redis's
  own `INFO commandstats`. Also: `readEventPage` is now the single seam both the
  immediate and held paths read through; the entry page is threaded into the hold
  so a pre-existing backlog drains instead of waiting for a bell that will never
  ring; `Ring` moved to `rd.NewScript` (EVALSHA) and its failures are logged at
  all five producers; `OCTO_BOT_EVENTS_MAX_HOLDS` is validated at boot and
  documented, with the per-replica connection budget (shared + wait 68 + ring 10)
  in the new `docs/bot-events-longpoll.md`.
- **Review round 5 — the invariant's own counterexamples.** All four reviewers
  approved round 4; the remaining findings were non-blocking and fixed anyway,
  because every one of them is the failure mode this task keeps reproducing: a
  written claim stronger than the code. The stated invariant had two
  counterexamples (a doorbell token whose event lands *below* the caller's
  cursor advances nothing; the entry page's skip was ungated while the in-loop
  one was gated), so it now reads *burns a chunk, **or** advances the cursor,
  **or** consumes a token* and both skip sites share one `eventPage.advanced`.
  The claim that the cursor covers undecodable members narrowed to the truth — it
  clears them only incidentally, when a decodable member with a higher id shares
  the page. `docs/bot-events-longpoll.md` said a hold overshoots by "less than one
  second"; go-redis's `timeout + 10s` command deadline makes the real worst case
  **~45s for a 30s hold**, which is the number an operator sizes a proxy idle
  timeout from. The `event_id == ZSET score` equality the cursor rests on is now
  written down. Finally, round 4's commit message claimed every new test was
  verified by deleting its fix, and that was untrue of one: the failed-ACK test
  re-seeded events after a *successful* ZREM, which is not the read-succeeds /
  write-fails state it named. It is now a real one via an `ackFilteredEvent`
  seam, and the anti-spin test's slack tightened from `+3` to `+1` so the entry
  gap fails it (3 reads where 2 suffice).
- **Review round 6 — the producer side.** Four rounds of hardening had all
  concentrated on the consumer loop; the one blocking finding this round was on
  the producer path, which had received a ring call in round 1 and no
  tail-latency analysis since. `botevent.Ring` was a **synchronous** network call
  inline in `saveRobotMessage`, which runs inside a `msgSem` slot the message
  listener acquires with a *blocking* send on its own goroutine (capacity 100) —
  so ring latency became held slots, and 100 held slots stop bot message fan-out
  process-wide, for every bot, including bots that never long-poll. The tail was
  bounded by nothing: a 10-connection pool against a path admitting 100
  concurrent callers, plus go-redis defaults of 5s dial / 3s read / 4s pool with
  one retry. Not patched with tighter timeouts — the shape changed. Producers now
  call `botevent.Notify`, which does **no I/O on their goroutine**: rings are
  coalesced per bot (`LTRIM 0 0` makes N rings for one bot indistinguishable from
  one, so the queue is bounded by distinct bots rather than message rate), handed
  to a bounded worker pool, and dropped-with-a-counter when saturated — losing a
  hint costs a waiter one chunk, blocking a producer costs delivery for everyone.
  `ringPoolSize` is now derived from `ringWorkers` rather than copied from an
  unrelated convention, with sub-second timeouts and no retries. The repo had
  already rejected the synchronous shape for the same reason in
  `modules/bot_api/auth.go`'s fire-and-forget registry warm-up.
  Also this round: `MaxRetries = 0` on the wait client (a retried BLPOP added
  roughly a chunk beyond the documented worst case); the cursor's stated
  invariant corrected from score **equality** to score **uniqueness**, which
  `GenSeq`'s block allocator does not guarantee across replicas and which would
  silently drop an event — recorded as assumed-and-unverified rather than
  claimed; the read-error exit's asymmetry justified with the client's actual
  backoff behaviour instead of left implicit; `ackFilteredEvent` moved off a
  mutable package global onto `BotAPI`; and `brief.md`'s Out-of-scope line, which
  still forbade self-building an `rd.Client` while the finalised decision
  required it.
- Brief/context under `.octospec/tasks/bot-events-longpoll/`; shared journal
  `.octospec/journal/shared/bot-events-longpoll.md`; learning candidates
  `.octospec/learnings/pending/bot-events-longpoll.md` (lower-bound assertions for
  timing promises → `testing`) and
  `.octospec/learnings/pending/loop-progress-invariant.md` (state the invariant
  instead of answering findings one at a time → `testing`). Consumer half is a
  sibling change in openclaw-channel-octo. PR #685.

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

## 2026-07-29 (inactive-hiding-user-control, Batch 1)

- **Consistency** — Archived 子区 now leave the conversation lists, not just
  `/v1/conversation/sync`: `dropArchivedThreadItems` converges
  `/v1/sidebar/sync`'s **recent tab** on the same `status=active` predicate,
  fail-open on an unknown status. The **follow tab deliberately keeps them** —
  its response is the clients' only source of `is_followed`, so filtering it
  server-side would report an already-followed archived thread as unfollowed and
  make unfollow impossible; that tab's display filtering stays with the clients.
  Extends the direction XIN-1135 set for the self-created
  exemption to the paths it left open. See
  [journal](journal/shared/inactive-hiding-user-control.md) and
  [brief](tasks/inactive-hiding-user-control/brief.md).
- **Config** — Thread auto-archive policy (`enabled` / `days`) moved from
  env-only to `system_settings` with DB → env → code-default resolution and no
  migration-written row, so rollout is behaviour-identical. The worker re-reads
  policy per tick (no restart to change it) and `effective_value` makes the
  running window queryable for the first time.
- **Invariant** — `archive_days >= recent_filter_thread_days` enforced on both
  keys' write entry points against the post-merge state: the two windows are a
  two-stage decay, and inverting them makes the recent-tab window silently
  unobservable.
- **Safety** — Unread / pinned / system-bot conversations are now exempt from
  the inactivity window on both endpoints. A window that can swallow unread
  cannot be handed to users, so this is a precondition for the per-user windows
  in Batch 2, not a follow-up.
- **Learning (pending)** — A cross-key `system_setting` merge guard must resolve
  an empty payload as reset-to-default; carrying the current value forward lets
  a clearing write land in the state the guard exists to reject. See
  [learning](learnings/pending/system-setting-empty-payload-is-reset-not-keep.md).

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

## 2026-08-14 (notification-pause-manual-mode)

- **Implemented** — Added explicit manual/timed pause state, server-side fixed
  durations, unified REST/CMD responses, migration, validation, and tests.

## 2026-08-22 (cleanup-membership-predicate)

- **Fixed** — The two removal-cleanup rejoin guards were asking one question
  with two different predicates: a disbanded Space silently voided cleanup
  (orphan `space_member` row), a banned Space wrongly triggered it. Both now use
  `CheckMembershipForCleanup` (`sm.status=1 AND s.status <> 0`).
  `CheckMembership` is deliberately **unchanged** — #797's original proposal to
  relax it would have admitted banned Spaces across all **36** of its non-test
  call sites, `SpaceMiddleware` — the primary auth gate — included. A behavioural truth table now pins both predicates' answers
  across `{disbanded, normal, banned} x {active, removed}`, including the one
  cell where they must disagree. (Corrected 2026-08-23: this entry originally
  claimed a *source guard*; that guard was deleted on the same branch for
  passing the regression it was named for.) Closes two #797 items.
  See [journal](journal/shared/cleanup-membership-predicate.md).

## 2026-08-22 (cleanup-queue-durability)

- **Fixed** — Two silent-failure items from #797. The cleanup queue's retry budget
  was only enforced in `releaseCleanupJob`, which a `SIGKILL` never reaches, so a
  process-killing job was re-claimed forever and head-of-lined the whole queue;
  the budget now gates the claim itself and a 1-minute sweep pushes exhausted rows
  to `abandoned`, with three gauges so the new terminal state is not just as silent
  as the old loop. Separately, a failed membership-cache `DEL` now returns, is
  logged, and is overwritten with a negative entry — a total Redis outage was
  already safe, but a DEL-only failure let a removed member keep passing
  `SpaceMiddleware` for 60s with nothing logged.
  See [journal](journal/shared/cleanup-queue-durability.md).

## 2026-08-23 (space-member-removal follow-ups · wrap-up)

- **Reverted** — The durable IM-unsubscribe outbox (`eb74529`) was implemented and
  then withdrawn (`78e46d3`) after five-lens adversarial review. Three reviewers
  independently found it reintroduced the exact leak it targets (an `abandoned` row
  became a permanent tombstone that silently swallowed every later enqueue while the
  log claimed "queued for retry"), and it added a new one (firing without
  re-validating membership turns blacklist→un-blacklist into a permanent cutoff of an
  active, visible member). The problem statement and the measured broker evidence
  stand; the design does not. Corrected requirements are written into
  `.octospec/tasks/im-pending-outbox/brief.md`.
- **Fixed** — Two guard tests that could not fail for what they existed to check
  (mutation-proven, then re-verified with the reviewers' own mutations), and a sweep
  that took next-key locks across the whole pending range — reproduced as
  `ERROR 1205` on a brand-new non-conflicting insert, which is the removal-cleanup
  enqueue inside the removal transaction. See
  [journal](journal/shared/cleanup-queue-durability.md).
- **Learning** — `learnings/pending/mutation-testing-must-be-adversarial.md`: an
  author-chosen mutation only proves the test catches what the author already thought
  of. The same guard was green on the real security regression and red on whitespace.


## 2026-08-20 (featuregate-user-scoped-flags)

- **Implemented** — Revived the generic feature-gate framework from the
  never-merged PR #280 (framework only; the incoming-webhook gating commit was
  left out), extended it with a `user` dimension and a per-rule `bucket_by`, and
  added `GET /v1/featuregate/flags` for per-user flag delivery. `appconfig` keeps
  its no-auth contract untouched. Ships with an empty client-flag registry — the
  mechanism, not any gated feature. See
  [journal](journal/shared/featuregate-user-scoped-flags.md).
- **Fixed while reviving** — The original `percent` branch never read the
  whitelist, so `whitelist -> percent` silently dropped the dogfood cohort and
  `addScope` during a ramp was a no-op that still returned 200. The whitelist now
  applies under `whitelist` and `percent`, never under `off`.
- **Learning** — `learnings/pending/absence-as-semantics-is-undescribable.md`:
  making "key absent from the response" carry meaning cannot be expressed in
  OpenAPI 3, so codegen clients get it wrong by default — exactly the failure the
  design existed to prevent.
