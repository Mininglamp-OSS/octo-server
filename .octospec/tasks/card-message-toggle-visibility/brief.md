---
type: Task
title: "Task: card-message-toggle-visibility"
description: Add the octo/v1 local-action batch to the pkg/cardmsg card validator — Action.ToggleVisibility (+ element id / isVisible / targetElements) and the octo-custom Action.CopyToClipboard — and advertise the accepted local action set in the GET /v1/bot/card/profile capability manifest. Enables server-authored collapsible sections ("collapse/expand reasoning") and copy-to-clipboard affordances as purely local, no-server-callback client interactions.
tags: ["card", "wire-contract", "trust-boundary", "bot-api", "validator", "testing", "commit"]
timestamp: 2026-07-10T00:00:00Z
# --- octospec extension fields ---
slug: card-message-toggle-visibility
upstream: self
source: self
---

# Task: card-message-toggle-visibility

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

Add the **octo/v1 local-action batch** to the octo card validator so producers
can author **collapsible sections** (fold/expand a subtree — e.g. the "收起推理 /
展开推理" control in the deep-thinking card) and **copy-to-clipboard** affordances:

- `Action.ToggleVisibility` (standard AC) plus its supporting element attributes
  `id` / `isVisible` / `targetElements`.
- `Action.CopyToClipboard` (octo-custom action; standard AC has none) carrying a
  `text` payload the client copies locally.

**Both go in `octo/v1` (the display profile), NOT a new `octo/v2`/`octo/v3` tier.**
Each is a purely local interaction that makes **no server callback** (neither hits
`POST /v1/message/card/action`, enqueues a `card_action` event, nor needs
idempotency / membership / anti-forgery). That is exactly the class of
`Action.OpenUrl`, which already lives in `octo/v1`. The capability line therefore
becomes:

- **octo/v1** = display + local, no-server-callback interactions
  (`Action.OpenUrl` navigation, `Action.ToggleVisibility` fold/expand,
  `Action.CopyToClipboard` local copy).
- **octo/v2** = interactions that call back to the server
  (`Action.Submit`, `Input.*`).

`card_version` stays pinned at `"1.5"` (ToggleVisibility is AC 1.2 core, well
within 1.5 — this is a white-list *additive* change, not a version bump, per the
house rule already applied to Input.Number/Date/Time).

Also: **advertise the accepted local action set** in the `GET
/v1/bot/card/profile` manifest so producers (bot SDKs, the OpenClaw adapter) can
feature-detect ToggleVisibility instead of probing with a 400. The manifest today
advertises `elements`/`inputs` but has **no** actions field.

## Background

- **Source request**: client/product ask to render collapsible AI-reasoning cards
  (fold/expand reasoning steps) plus a local "copy" affordance. This brief covers
  only the fold/expand half; copy (`Action.CopyToClipboard`) is a separate
  follow-up PR.
- **Authoritative validator**: `pkg/cardmsg` is the single write-strict authority
  for InteractiveCard (ContentType=17). `docs/card-protocol.md` is a mirror; the
  master interaction contract is
  `.octospec/tasks/card-message-interaction/brief.md`. `ToggleVisibility` +
  `isVisible`/`targetElements` are currently listed there and in
  `docs/card-protocol.md:44` as a **future** item — this task promotes it to
  supported. Amend doc + master brief together with the code.
- **Current behavior (verified)**:
  - `pkg/cardmsg/validate.go` `action()` (`:596-643`) whitelists only
    `Action.OpenUrl` (both profiles) and `Action.Submit` (octo/v2); everything
    else → `ErrCardUnknownAction`. `Action.ToggleVisibility` is therefore rejected
    today — asserted by `pkg/cardmsg/cardmsg_test.go:127-130` (via `selectAction`).
  - The walker (`walker.element`, `:163-458`) recurses into all subtrees
    regardless of any `isVisible` flag and counts every node toward
    `MaxNodes`/`MaxDepth` — so "hidden content is still fully validated + budgeted"
    already holds for free.
  - `id` is only registered/uniqueness-checked for `Action.Submit` and `Input.*`
    (`walker.registerID`/`seenIDs`, `:127-137`); display-element `id` is currently
    tolerated as an unknown scalar (never registered, never referenced).
  - Manifest handler `modules/bot_api/card_profile.go:36-53` returns
    `enabled/card_version/profiles/elements/inputs/limits`, every value sourced
    from `pkg/cardmsg` authorities (`DisplayElements()`/`InputElements()`/
    `AcceptedProfiles()`). No `actions` field exists yet.
  - Whitelist authority pattern to mirror: `pkg/cardmsg/whitelist.go`
    (`displayElements`/`inputElements` + `DisplayElements()`/`InputElements()`),
    locked by `TestDisplayElementsAuthority`/`TestInputElementsAuthority`
    (`whitelist_test.go`).
- **Rollout ordering note (load-bearing for release, not for this diff)**:
  server discovery ≠ client rendering. The web renderer does **not** call
  `/v1/bot/card/profile`; it hard-codes its own support set and whole-card-
  downgrades to `plain` on any unknown action. So once the server accepts +
  advertises ToggleVisibility and a producer emits it, **un-updated web clients
  downgrade the entire card to plain** (losing even renderable parts). Safe
  rollout is web-render-support-first, or keep `OCTO_CARD_MESSAGE_ENABLED` gated
  until the client ships. This task does not flip the gate.
- **Rollout checklist — backward-incompatible id tightening (PR#561 review)**:
  display-element `id`s were previously tolerated as unknown scalars; they are now
  registered into the shared frame-unique namespace, so a card that **reuses** a
  display `id` now fails closed (whole-card 400). Blast radius is low (feature
  gated off by default; no known producer assigns display ids today), but before
  flipping `OCTO_CARD_MESSAGE_ENABLED` confirm no already-deployed/authored cards
  rely on duplicate display ids so early adopters aren't surprised.

## Design decisions to pin (for confirmation)

1. **Profile**: allow `Action.ToggleVisibility` in **both** profiles (no
   `w.interactive` gate) — it is octo/v1-tier. `card_version` unchanged (`1.5`);
   `AcceptedProfiles()` unchanged (still `{octo/v1, octo/v2}`); **no octo/v3**.
2. **`Action.ToggleVisibility` shape**:
   - `targetElements` (optional per AC, but a toggle with none is a no-op —
     **require present + non-empty array**). Each entry is either a **string**
     (element id) or an **object** `{elementId: string, isVisible?: bool}` (AC
     `TargetElement`). Reject any entry that is neither, whose `elementId` is
     missing/empty, or whose `isVisible` (when present) is non-boolean.
   - Counts toward the node budget like any other action (`w.bump`).
   - No `url`/`data` semantics; no `SubmitAction` dispatch involvement (it is not
     a Submit — `interactive.go` findSubmit* naturally ignores it, unchanged).
3. **Element `id` + reference integrity (forward-safe)**: collect every element
   `id` (display + input) into a frame set during the walk; collect every
   `targetElements` reference into a pending list; **after** the full walk verify
   each reference resolves to a declared **element** id. This handles forward
   references (toggle may appear before or after its target). A dangling reference
   → reject.
4. **`id` uniqueness / namespace**: any element that declares an `id` registers it
   frame-uniquely, coordinated with the existing `seenIDs` used for
   `Action.Submit`/`Input.*` — one unified frame-unique id space (matches AC's
   card-global id model). `targetElements` resolve against ids declared by
   **elements** (not action ids). Duplicate id anywhere in the frame → reject.
   (This is a *new* constraint on display-element ids, which were previously
   unconstrained; risk is low because no prior feature gave display ids meaning,
   so producers do not set — let alone duplicate — them. Called out for review.)
5. **`isVisible`**: universal AC element attribute — when present on any element,
   must be boolean, else reject. Hidden subtree still fully walked + budgeted
   (already true).
6. **Manifest `actions` field (additive wire contract)**: refactor the action
   whitelist into a `pkg/cardmsg` data authority mirroring
   `displayElements`/`inputElements` — `displayActions = ["Action.OpenUrl",
   "Action.ToggleVisibility", "Action.CopyToClipboard"]` (octo/v1 local actions)
   with a `DisplayActions()` accessor, locked against the validator accept-set
   with a new `TestDisplayActionsAuthority` guard. Add `"actions":
   cardmsg.DisplayActions()` to the manifest response. `Action.Submit` remains
   discoverable via the `octo/v2` profile tier (status quo — Submit is not
   advertised by name today); optionally add a symmetric `interactive_actions`
   list, but default is to keep this PR minimal and add only `actions`.
7. **Error mapping**: reuse existing `pkg/cardmsg` sentinels — structural
   problems (`targetElements` wrong shape, non-boolean `isVisible`, duplicate id,
   missing/oversized/non-string `CopyToClipboard.text`) map to `ErrCardBadShape`;
   a dangling `targetElements` reference may use a dedicated internal sentinel
   (e.g. `ErrCardTargetMissing`) that still collapses to the send path's existing
   single generic card-invalid 400 (anti-enumeration) — **no new `pkg/errcode`
   code, no new i18n toml entry, no new migration**.
8. **`Action.CopyToClipboard` (octo-custom, octo/v1)**: standard AC has no copy
   action — this is an octo extension; the type name follows the client doc's
   proposed protocol `{type:"Action.CopyToClipboard", title?, text}` (unprefixed
   — the only collision risk is a hypothetical future Microsoft action of the
   same name, judged acceptable, no vendor prefix per the doc). Allowed in **both**
   profiles (octo/v1-tier, no `w.interactive` gate). Validation: `text`
   **required, string, ≤ `MaxCopyTextBytes` (4 KiB)**; `title` optional string;
   the action counts toward the node budget (`w.bump`). **No server callback** —
   the client copies `text` locally; it never hits `/v1/message/card/action` and
   enqueues no event, so `interactive.go` dispatch is untouched. `text` is copied
   **verbatim, not rendered**, so **no URL/markdown allowlist applies**; the
   producer-side rule "don't copy hidden/sensitive fields" is a client/producer
   concern, not a server structural check. Add a `MaxCopyTextBytes = 4 << 10`
   constant to `pkg/cardmsg/cardmsg.go` (source-of-truth) and advertise it in the
   manifest `limits` as `max_copy_text_bytes` (PR#561 review #1 — symmetric with
   `max_nodes`/`max_depth`/`max_payload_bytes`, so producers feature-detect the
   threshold instead of learning it via a rejected send).

## Load-bearing list
<!-- touches: tags drive rule injection. -->
- **Card validator trust boundary** (`pkg/cardmsg/validate.go`) — the "校验面 ≥
  渲染面 / 派发面" invariant: any newly accepted action/attribute must be fully
  validated so nothing renderable escapes the whitelist. `touches: trust-boundary,
  wire-contract, validator`.
- **Whitelist single-authority + anti-drift** (`pkg/cardmsg/whitelist.go`,
  `profiles.go`) — action accept-set becomes a data authority feeding both the
  validator and the manifest; must stay drift-locked by a guard test.
  `touches: wire-contract`.
- **Capability manifest wire contract** (`modules/bot_api/card_profile.go`, D12)
  — additive-only (`actions` added; nothing renamed/removed/retyped); values
  sourced from `pkg/cardmsg` constants, never re-typed literals.
  `touches: wire-contract, bot-api`.
- **Frame-unique id semantics** (`walker.seenIDs`) — extending the id namespace
  from Submit/Input to all elements must not break existing Submit/Input id
  uniqueness or the `card_action` addressing / idempotency keys that rely on it.
  `touches: wire-contract`.
- **Node/depth budget accounting** (`MaxNodes`/`MaxDepth`) — ToggleVisibility
  actions and toggled/hidden subtrees continue to count; no budget bypass via
  hidden nodes. `touches: validator`.
- **bot_api endpoint** (`GET /v1/bot/card/profile`) — deployment-level, bot-token
  auth, no SpaceMiddleware, no new rate limiter (unchanged); still returns 200 +
  full manifest when disabled. `touches: bot-api`.
- **Tests** — validator + manifest guard/behavior tests. `touches: testing`.
- **Commit style** — Conventional Commits, English. `touches: commit`.

## Out of scope
- Any change to `Action.Submit` / `Input.*` / the `card_action` server-callback
  loop, idempotency, membership, or `interactive.go` dispatch.
- Introducing `octo/v3` or bumping `card_version` off `"1.5"`.
- Narrowing `selectAction + Submit` (a separate product-policy question).
- Per-element `fallback` (named P3 mechanism) and any client/web renderer work
  (web is owned separately; it hard-codes its own whitelist and downgrades to
  `plain`).
- Flipping `OCTO_CARD_MESSAGE_ENABLED`.
- New DB table / migration / `pkg/errcode` code / i18n toml entry.

## Acceptance
<!-- Machine-checkable where possible. -->
- `go test ./pkg/cardmsg/...` passes, with new cases:
  - `Action.ToggleVisibility` in `card.actions[]` whose `targetElements`
    reference an existing element `id` → `Validate` returns nil in **both**
    `octo/v1` and `octo/v2`.
  - `Action.ToggleVisibility` carried via a `Container.selectAction` → accepted
    (this **flips** `cardmsg_test.go:127-130`, which currently expects
    `ErrCardUnknownAction`; update that case).
  - `targetElements` string form and `{elementId, isVisible}` object form both
    accepted; entry that is neither / missing `elementId` / non-boolean
    `isVisible` → `ErrCardBadShape`.
  - `targetElements` referencing a non-existent id → rejected
    (`ErrCardBadShape` or `ErrCardTargetMissing`).
  - Forward reference (toggle element ordered before its target in `body`) →
    accepted.
  - Duplicate element `id` in a frame → `ErrCardBadShape`; and an element `id`
    colliding with an `Action.Submit`/`Input.*` id in the same frame → rejected.
  - Non-boolean `isVisible` on any element → `ErrCardBadShape`.
  - `isVisible:false` subtree containing an over-budget node or a `javascript:`
    URL is **still** rejected (visibility does not exempt budget/URL checks).
  - `Action.CopyToClipboard` with a valid `text` → accepted in **both** profiles;
    missing `text` / non-string `text` / `text` > 4 KiB → `ErrCardBadShape`;
    optional `title` accepted; `text` carrying a `javascript:`/`data:` string is
    **accepted** (verbatim clipboard content, not a URL surface).
  - `Action.CopyToClipboard` does not appear in `SubmitAction` dispatch results
    (it is not a Submit) — `interactive.go`/`interactive_test.go` untouched.
- Manifest: `modules/bot_api/card_profile_test.go` updated so `GET
  /v1/bot/card/profile` response includes `actions` containing
  `"Action.OpenUrl"`, `"Action.ToggleVisibility"`, and
  `"Action.CopyToClipboard"`, sourced from `cardmsg.DisplayActions()`.
- New `TestDisplayActionsAuthority` in `pkg/cardmsg` locks `DisplayActions()`
  equal to the validator's octo/v1 action accept-set (drift guard, mirroring
  `TestDisplayElementsAuthority`).
- `interactive.go` / `interactive_test.go` unchanged and green (no Submit-dispatch
  impact); `AcceptedProfiles()` still `{octo/v1, octo/v2}`; `cardmsg.CardVersion`
  still `"1.5"`.
- `make i18n-extract-check` and `make i18n-lint` pass unchanged (no new codes /
  raw responses).
- `golangci-lint run ./pkg/cardmsg/... ./modules/bot_api/...` clean.
- `docs/card-protocol.md` and
  `.octospec/tasks/card-message-interaction/brief.md` updated together to move
  ToggleVisibility/`isVisible`/`targetElements` from "future" to "supported
  (octo/v1)", document the octo-custom `Action.CopyToClipboard` (octo/v1,
  `text` ≤ 4 KiB, local copy, no callback), and document the new manifest
  `actions` field.
- Work committed on branch `claude/client-ac-requirements-fvoizm`.
