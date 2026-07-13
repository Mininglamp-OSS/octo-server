# Handoff — card-message-internal-dispatch

> Forward-looking guide for the next stage. The spec is `brief.md` in this
> directory; this file says **where we are** and **what to do next**.
> Last updated 2026-07-13.

## TL;DR

This task ships as **two PRs**:

- **PR 1 — P1 (done):** the dormant `internal/carddispatch` foundation. Reviewed,
  `go build/vet/test` green, **zero production producers registered**, so the only
  runtime behaviour change is the notify card-payload gate (Decision 14). This is
  the current `feat/card-message-internal-dispatch` branch.
- **PR 2 — P2 (next stage):** enable the `summary-notify` pilot — a code/config
  change across three repos that registers the first producer and turns summary
  DMs into cards. Gated only by landing the template/deep-link guard tests.

Every pilot decision is maintainer-confirmed (2026-07-13). Keeping P1 and P2 as
separate PRs is deliberate (brief › rollout): the foundation is behaviourally
inert, so it can merge and sit; enabling the pilot is the reviewed change with a
one-line rollback (remove the producer spec).

### PR hygiene note

`feat/card-message-internal-dispatch` is based on a main that already carries
#570 (App Bot card trust) and other merged work. When opening **PR 1**, base it
on a main with #570 merged (or stack it on #570) so the foundation PR's diff is
*only* `internal/carddispatch`, `pkg/cardtmpl`, `tools/lint-card-dispatch`, the
notify gate, the botidentity resolver extension, and this task's docs — not a
re-diff of #570.

## Current state

| Item | State |
| --- | --- |
| Brief (`brief.md`) | Complete; all decisions confirmed 2026-07-13 |
| Foundation (`internal/carddispatch/`, `pkg/cardtmpl/`, `tools/lint-card-dispatch/`) | Implemented, `go build/vet/test` green |
| Registry wiring (`main.go:346` `installCardDispatch`) | `NewRegistry(deps, nil)` — **zero producers**, behaviourally inert |
| Decision 14 notify gate (`modules/notify/api.go`) | **Live** — rejects card-shaped payloads on `/v1/internal/notify[/batch]` |
| Code review | Done; F1/F2/F3/F5 fixed (commit `d815947`), F4/F6 deferred (see below) |
| Branches | `feat/card-message-internal-dispatch` = impl + review fixes. `claude/card-message-dispatch-f7ubz3` = brief only (now redundant; brief is byte-identical on feat) |

## The P1 → P2 (dormant → enabled) boundary

Enabling a producer is **one reviewed config/code change**: build a
`carddispatch.ProducerSpec`, pass it to `NewRegistry`, obtain the bound
`Sender`, and inject only that `Sender` into the owning module. Rollback =
remove that one spec. Until then (all of P1) the package is a no-op except the
notify gate.

## P2 (PR 2) — enable the `summary-notify` pilot (DM summary cards)

Cross-repo; land together. Owning module is `modules/notify`.

### 2a. octo-server — provision the summary bot

- Add a dedicated `summary` bot (static UID) provisioned as a full User Bot
  (`user` + `app` + `robot.status=1`), mirroring `modules/notify/bot_manager.go`
  `ensureNotifyBot`. Keep the `notification` bot for the generic text path.
- Confirm `modules/botidentity` resolves the new UID as `KindUserBot`.

### 2b. octo-server — register the pilot producer + inject the Sender

In `installCardDispatch` (`main.go:346`) register the spec and thread the bound
sender into `notify.New`:

```go
spec := carddispatch.ProducerSpec{
    ID:                  "summary-notify",
    Enabled:             true,
    SenderUID:           notify.SummaryBotUID,            // the new summary bot
    AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()}, // DM only at pilot
    AllowedProfiles:     []string{cardmsg.ProfileV1},     // display-only
    SpacePolicy:         carddispatch.SpacePolicySystemNotification, // notify semantics
    GroupPolicy:         carddispatch.GroupPolicyMemberRequired,     // unused at DM pilot; group回发 flips to MemberExempt
    ActionEventOwner:    "",                              // no octo/v2, no card_action owner
    MaxInFlight:         20,                              // confirmed 2026-07-13
}
registry := carddispatch.NewRegistry(deps, []carddispatch.ProducerSpec{spec})
// carddispatch.Install(ctx, registry) as today, then:
sender, _ := registry.Sender("summary-notify")           // inject into notify.New(ctx, sender)
```

- `notify` builds the card via `cardtmpl.BuildSummaryResourceCard(...)` and calls
  `sender.Send(ctx, target, card)` for the DM path. **Do not** send the card
  through the existing `/v1/internal/notify` text path — that path stays
  text-only and now rejects cards by design.
- Web base URL for the deep link comes from `External` config
  (`cardtmpl.summaryDeepLink` takes it as a param; reuse `External.WebLoginURL`
  origin, decided at this review).

### 2c. octo-smart-summary — send structured fields, not a hand-built card

- Today the worker posts `{"type":1,"content":text}` to `/v1/internal/notify`
  (`internal/notify/notify.go:283`, `client.go`). For cards, send **structured
  fields** (title, time range, participant/message counts, `summary_id`) to the
  card path; octo-server owns the template. Keep the existing per-recipient
  `summary_notification` dedup/retry state machine and `X-Internal-Token`.
- Duplicates on transport-ambiguous failure are accepted (no outbox) — confirmed.

### 2d. octo-web — add the `/s/:taskId` deep-link route

- Register a standalone browser route `/s/:taskId?sp={spaceId}` mirroring the
  `/d/:docId` machinery (cold-load → login bounce → multi-session sid recovery,
  the XIN-398 `recoverSession` suite). Logged-in navigation calls the existing
  `WKApp.openSummaryDetail(taskId)`. No new renderer needed; type-17 already
  renders. Add a route-contract test asserting `/s/:taskId` is registered.

### P2 gating tests (must land with enablement)

- octo-server: `pkg/cardtmpl` snapshot + `cardmsg.Validate` guards (exist);
  the `summaryDeepLink` shape test.
- octo-web: `/s/:taskId` route-contract test.
- The DB-backed authorizer tests (`TestDBAuthorizerPolicyMatrix` etc.) run on
  sqlite locally but should run against MySQL in CI per the brief.

### P2 preconditions raised in the P1 review (#577)

- **WuKongIM transport timeout.** `SendMessageWithResult` → `network.Post` uses
  a zero-value `http.Client{}` with no timeout, and the dispatcher holds a
  producer in-flight slot across that call. A hung upstream would pin slots
  until `busy`. Pre-existing platform-wide (every `SendMessage*` caller,
  including today's notify pool) and cancellable transport is out of P1 scope —
  but ensure the WuKongIM client carries a request timeout before enabling the
  pilot, so the per-producer budget can't be pinned by a slow upstream.
- **Large-integer JSON precision (octo/v2 only).** The card document is decoded
  with default `json.Unmarshal`, so bare JSON numbers become `float64` (ints
  above 2^53 lose precision on re-serialize). Harmless for `octo/v1` display
  cards (strings/enums/URLs) and consistent with the `bot_api` ingress. If a
  future `octo/v2` producer places large integer IDs in card data, switch that
  path to `json.Decoder` + `UseNumber()`.

## After P2 (later PRs — brief has the detail)

Each of these is its own follow-up PR after the pilot is live.

- **group回发 (Scenario A1 group):** flip the pilot's `GroupPolicy` to
  `GroupPolicyMemberExempt` and widen `AllowedChannelTypes`. This review MUST
  add a **per-channel rate rule** and settle the **cluster-wide cap** (see
  brief › Industry practice alignment). Member-exempt already honors explicit
  group bans (F3). Delivery = post if group eligible, else creator-DM fallback.
- **A2 user forward (`summary-forward-card`):** a new user-facing forward
  endpoint on **summary-api** relaying through the existing `X-Internal-Token`
  ingress with `actor_uid`; octo-server verifies the actor is an active member
  of the target channel. Bot-sent card with a forwarder-attribution header;
  **no OBO** (card-protocol Decision 2b). Person targets keep today's
  plain-text forward — a bot cannot post into a two-party human DM.
- **`docs-share-card`:** blocked on a docs S2S ingress + docs bot identity;
  copy the A2 forward shape once those exist.

## Locked decisions (2026-07-13)

| Decision | Value |
| --- | --- |
| Sender identity | dedicated `summary` bot (not `notification`) |
| Message ownership | bot-sent + forwarder-attribution header; OBO rejected |
| Pilot channels / profile | DM only / `octo/v1` display-only |
| Group posting (post-P2) | member-exempt: no membership required, member list/count untouched, **explicit ban honored** |
| Concurrency | MaxInFlight 20 / process |
| Duplicates | acceptable on transport-ambiguous failure; no outbox |
| Cluster cap | none; per-process semaphore (replica count recorded at deploy review) |
| Template + deep-link | `ResourceCard` in `pkg/cardtmpl`; `/s/:taskId?sp=` route |

## Code-review status

Fixed on `d815947` (this branch): **F1** wrapper-evasion in the guard, **F2**
allowlist path-anchoring, **F3** member-exempt honors group bans, **F5** optional
card icon. Deferred (non-blocking):

- **F4 (low):** `tools/lint-card-dispatch` cross-file *constant-of-constant*
  chains are file-order dependent (a package-level fixed point would close it).
  Exotic shape; guard is a backstop.
- **F6 (nit):** `cardtmpl.labelsForLanguage` hardcodes the two action labels
  instead of using the i18n message catalog. Fine for two fixed strings.

## Verify (focused command set from brief acceptance)

```bash
go test -race -cover ./internal/carddispatch/...
go test ./pkg/cardmsg/... ./pkg/cardtmpl/... ./modules/botidentity/... ./modules/notify/...
go run ./tools/lint-card-dispatch ./modules ./internal
go vet ./internal/carddispatch/...
make i18n-extract-check && make i18n-lint
golangci-lint run ./...
git diff --check
```

## Gotchas for the next implementer

- **Don't route cards through `/v1/internal/notify`** — that path is text-only
  and rejects cards (Decision 14). Cards go through the injected `Sender`.
- **The `Sender` is producer-bound** — `SendRequest` has no `from_uid` and no
  arbitrary payload; you pass a `Target` + a `Card{Profile, Document}` only.
- **Registry is immutable** — one production instance owns all budgets; register
  specs at bootstrap, never from `init()`.
- **The guard must stay green** — adding any new type-17 transport owner fails
  `tools/lint-card-dispatch`; onboard through the dispatcher or add a reviewed
  path-anchored allowlist entry with a reason.
- **DB authorizer tests use sqlite locally** but exercise real SQL — run them in
  MySQL CI before enabling the pilot.
