# Card Message Protocol — Work Handoff

Status snapshot at the end of the design + P1-implementation sessions. Read this
first; it points at the authoritative specs and lists exactly what remains.

## TL;DR

- **Design is frozen-pending-review** across two octospec briefs (PR #525).
- **P1 (display-only) is fully implemented, tested, and reviewed** on branch
  `feat/card-message-p1-display` — but **not yet opened as a PR**.
- **P2 (interactivity) is specced + POC-proven**, implementation not started
  as a formal PR.
- **octo-im: zero changes** (Decision 4 invariant — verified, keep it that way).

## Branches (octo-server) — all pushed, all in sync with origin

| Branch | Purpose | HEAD | State |
|---|---|---|---|
| `docs/card-message-protocol-brief` | The two octospec briefs = **spec of record**. PR **#525** (draft). | `13edd5f5` | Awaiting reviewer sign-off on round-3 trust-boundary items + D12. |
| `feat/card-message-p1-display` | **PR-A**: P1 display-only implementation. Branched off `main`. | `946869bd` | Green + reviewed + review-fixes applied. **No PR opened yet.** |
| `poc/card-message` | POC that proved the whole P1+P2 pipeline (demos, IM integration tests). Evidence only — **never merge**. | `96b691b4` | Frozen. Keep until PR-B lands. |

octo-im branch `claude/octo-message-protocol-fdccgs`: clean, zero card code.

## Authoritative specs (do not duplicate — amend these, code mirrors them)

- `.octospec/tasks/card-message-protocol/brief.md` — **P1** (Decisions 1–10).
- `.octospec/tasks/card-message-interaction/brief.md` — **P2** (Decisions D1–D12).
- `.octospec/tasks/card-message-p1-display/brief.md` — PR-A **execution** brief
  (harvest map E1–E11; defers to the protocol brief on any conflict).
- `docs/card-protocol.md` — client-facing contract (mirrors `pkg/cardmsg`;
  already contains the full P2 action contract so clients architect once).

## What P1 (PR-A) delivered — on `feat/card-message-p1-display`

`InteractiveCard` = ContentType **17**. Standard Adaptive Cards 1.5 JSON,
`octo/v1` whitelist. Shipped behind `OCTO_CARD_MESSAGE_ENABLED` (default off).

- `pkg/cardmsg/` — single whitelist authority: `Validate` (profile pinned to
  {octo/v1}; 512 KiB payload cap; 200-node/16-depth tree caps; positive http(s)
  URL allowlist incl. markdown link targets, whitespace-tolerant), `Finalize`
  (authoritative `plain` recompute after enrich + size recheck), `BuildPlain`
  (Decision-8 derivation), `DisplayTextFor` (the masking API), constants incl.
  `MaxSendBodyBytes = 2 MiB`.
- `modules/cardtrust/` — **the one** "trusted card sender" predicate (bot OR
  `iwh_` webhook, fail-closed) behind an LRU+TTL cache. Used by every masking
  surface; never re-copy this logic.
- Ingress gates: user send reject (Decision 2a); bot send gate (rollout flag →
  OBO reject before grant, Decision 2b → Validate → Finalize); robot symmetric
  gate; 2 MiB pre-decode `MaxBytesReader` on bot/robot send+edit.
- Decision 7 immutability: type-17 `content_edit` rejected on user/bot/robot
  edit paths.
- Display masking (Decision 2 residual-risk, uniform): offline push, search hit
  projection, pin tips — all render `[卡片]` for non-bot/webhook senders via
  `cardtrust` + `cardmsg.DisplayTextFor`.
- incomingwebhook `msg_type:"card"` with Decision-8 `text` fallback-seed.
- 7 P1 errcodes (per-module files) + zh-CN; OBO fan-out contract test extended
  to type-17.

### Test state (all green as of handoff)

Requires MySQL + Redis + WuKongIM. Full sweep passed across `pkg/cardmsg`,
`modules/cardtrust`, `message`, `bot_api`, `robot`, `webhook`,
`incomingwebhook`, `messages_search`, **including the two WuKongIM-backed tests
(confirmed RUN, not skip)**. `make i18n-extract-check` + `i18n-lint` green;
`golangci-lint run` 0 issues.

## Code review outcome (high-effort, 8 angles)

Fixed and committed on the branch:
1. **Body-cap regression** (was 1 MiB = RichText's own 1 MiB payload limit →
   413'd legit RichText). Now `cardmsg.MaxSendBodyBytes = 2 MiB`, invariant
   documented, brief corrected (`13edd5f5`).
2. **Markdown allowlist bypass** — `[x]( javascript:…)` (space after `(`)
   skipped the URL scheme check while a CommonMark renderer renders it live.
   Regex now `\(\s*`; regression tests added.
3/4/6. Trust predicate duplicated + per-recipient/per-hit DB queries → the
   `cardtrust` package (dedup + cache).
8. errcodes moved from a cross-module `card.go` into per-module files.
9/10. robot test `CleanAllTables`; search test asserts exported const.

**Deliberately deferred (not P1 defects — flagged for their phase):**
- **#5** incomingwebhook `text` seed can override a card whose TextBlock text is
  literally `[卡片]` (equality-on-sentinel coupling). Root fix = have
  `Finalize` return a `fellBack bool`; changes the `cardmsg` public API, so
  weigh separately. Rare edge; flag is default-off.
- **#7** `pkg/cardmsg/validate.go` — flipping the walker's `interactive` flag
  for P2 opens `Action.Submit`/`Input.*` on a **zero-validation** accept path.
  **P2 MUST add id-required / frame-unique / choices validation there BEFORE
  flipping the flag.** P1 is safe (octo/v2 is rejected as an unknown profile).

## Pending — to open PR-A

1. **Merge precondition (needs repo access I don't have)**: grep the dmwork
   **client repos** confirming ContentType `17` and envelope field names
   (`card` / `card_version` / `profile`) are unused; attach as a PR comment.
2. **#525 must be frozen first** — it's the spec of record. After it merges,
   `git rebase origin/main` this branch so the merged briefs come along, then
   open PR-A with: Linked Spec (both briefs) + the COMPREHENSION three
   questions (this is a load-bearing change).
3. **Rollout precondition (deployment)**: `OCTO_CARD_MESSAGE_ENABLED` stays
   default-off; enable per-deployment only AFTER the client render-gate ships.
   Merging PR-A does NOT imply enabling.
4. octo-im deployment check only: WS frame `maxPayloadBytes`
   (`pkg/gateway/transport/gnet/ws_frame.go:74`) ≥ 1 MiB. No code change.

## Pending — P2 (PR-B), after PR-A

Harvest again from `poc/card-message` (that branch carries the working P2 code):
`octo/v2` whitelist + `Input.*`/`Action.Submit` (with the #7 validation),
`POST /v1/message/card/action` + D4 claim store, `card_action` typed events,
`botMessageEdit` type-17 unlock, **D9 card_seq → a real `message_extra` column
+ conditional UPDATE (POC used Redis — replace it)**, **D10 revision history
side table (entirely un-built; POC only has a front-end mock)**, D11 input
validation, D12 `GET /v1/bot/card/profile` capability manifest. Also: bot-side
`IsMember` single-row query to replace the O(n) `GetMembers` scan; add WuKongIM
as a CI service so the IM-backed tests run in CI.

## Open decisions awaiting the maintainer

- Freeze/approve #525 (round-3 trust items P1-2/3/4 + D12 asked for human
  verification).
- Whether to open PR-A now (blocked only on the client-repo grep evidence).
- Deferred review items #5 / #7 — accept the deferral or fix now.
- Pre-existing `guobin.a@` emails in
  `modules/base/common/service_email_envelope_test.go` on `main` (unrelated to
  cards; flagged earlier, still awaiting a call).

## How to resume the environment (services die on shell reset)

```bash
# Redis + MySQL
redis-server --daemonize yes
export PATH=$PATH:/usr/sbin
pgrep -x mysqld >/dev/null || mysqld --user=mysql --daemonize   # root/demo
# WuKongIM (IM-backed tests; :5001 health)
WK_MODE=debug WK_TOKENAUTHON=false WK_EXTERNAL_IP=127.0.0.1 \
  WK_EXTERNAL_WSADDR=ws://127.0.0.1:5200 wukongim -i &

# Tests (recreate the DB per package, as CI does — utf8mb4_general_ci matters):
mysql -uroot -pdemo -e "drop database if exists test; \
  create database test charset utf8mb4 collate utf8mb4_general_ci;"
OCTO_MASTER_KEY=0123456789abcdef0123456789abcdef go test ./modules/bot_api/...
# ^ MASTER_KEY env does NOT persist between Bash calls — prefix every run.
```

Gotchas: `register.GetModules` caches module closures with the FIRST test's ctx
(`sync.Once`) — tests needing a controllable ctx use the package-local
`newTestServer()` + `New(ctx)` + `Route`, not `testutil.NewTestServer`. The UID
rate-limit bucket survives `CleanAllTables` — reset `ratelimit:uid:*` in setup.
