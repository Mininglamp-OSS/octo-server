---
type: Journal
title: "Journal: card-message-p3-rich-inputs (card message P3-3)"
description: Record of P3-3 — extend the octo/v2 input whitelist with Input.Number/Date/Time (all AC 1.0, within the pinned card_version "1.5"; additive, no version bump), add submit-time format validation (finite Number, Date YYYY-MM-DD, Time HH:MM; min/max range not server-enforced — delegated to bot per PR#556 review), refactor the whitelist into a single pkg/cardmsg authority that validator + collector + dispatcher + D12 manifest all derive from, and additively advertise elements/inputs on GET /v1/bot/card/profile. No new errcode/DB/endpoint.
tags: ["card", "wire-contract", "trust-boundary", "bot-api", "testing"]
timestamp: 2026-07-09T01:00:00Z
# --- octospec extension fields ---
task: card-message-p3-rich-inputs
upstream: card-message-interaction P3-3
source: self
---

# Journal: card-message-p3-rich-inputs (card message P3-3)

## What was done

Delivered the server half of P3-3 (richer card inputs). Octo's octo/v2 whitelist
accepted only 3 of Adaptive Cards' 6 input elements; added the missing three.

- **`pkg/cardmsg/whitelist.go` (new)** — the single authority for the element
  whitelists: `displayElements` (octo/v1 display set, shared) and `inputElements`
  (octo/v2 interactive inputs), plus `DisplayElements()` / `InputElements()`
  accessors. `isInputElement` moved here. The validator, the submit-time inputs
  collector, the dispatch walker, and the D12 manifest now all derive from these
  two slices — no re-typed literals anywhere.
- **`validate.go`** — send-time whitelist extended to `Input.Number/Date/Time`.
  The `element()` type switch no longer enumerates input literals; unknown types
  fall through to `default` and are accepted iff `isInputElement(t)`, so adding
  an input element is a one-line change in `whitelist.go`.
- **`inputs.go`** — submit-time **format** validation for the new types (trust
  boundary D11): Number = finite numeric (reject `NaN`/`±Inf`); Date =
  `YYYY-MM-DD`; Time = `HH:MM` (24h); `""` passes as "unfilled". `min`/`max`
  range is **not** server-enforced (delegated to bot — see Open item, resolved
  by maintainer in PR#556 review); `isRequired`/`regex` likewise remain
  client-UX + bot concerns.
- **`interactive.go`** — dispatch (`findSubmitInElements`) walks `inlineAction`
  via the same `isInputElement` predicate as validation.
- **`modules/bot_api/card_profile.go`** — D12 manifest additively advertises
  `elements` and `inputs` (from the cardmsg authority) so producers can
  feature-detect at element granularity even while `card_version` stays "1.5".
- **`docs/card-protocol.md`** kept a faithful mirror (§2 whitelist, §3 manifest,
  §7.1 inputs trust boundary).

No new errcode / i18n / DB / migration / endpoint; wire contract additive-only.
Verified live against adaptivecards.io that all three new inputs are AC 1.0 and
`Data.Query` (dynamic typeahead) is AC 1.6 — so this stays inside the pinned 1.5.

## Structural learnings

- **Widening a whitelist means widening every surface that mirrors it — and the
  cleanest way to guarantee that is one predicate/authority all surfaces derive
  from.** This change touches four surfaces of the same whitelist: send-time
  validation, submit-time input collection, action dispatch, and the capability
  manifest. Two review bugs came precisely from surfaces that were *not* driven
  by a shared authority: (1) dispatch still hardcoded the old 3 input types, so
  `Input.Number/Date/Time.inlineAction` validated at send but was undispatchable
  → a "send-ok, click-invalid" dead button; (2) the manifest would have re-typed
  the input list. Routing validation + collection + dispatch + manifest all
  through `isInputElement` / `InputElements()` makes the four physically
  incapable of drifting. Guard tests (`TestInputElementsAuthority`,
  `TestSubmitActionDispatchRichInputInlineAction`) pin the symmetry.
- **`strconv.ParseFloat` accepts non-finite tokens without error.** `"NaN"`,
  `"Inf"`, `"+Inf"`, `"Infinity"` (case-insensitive) all parse to a valid
  `float64` with `err == nil` — so a `ParseFloat`-only "is it a number" check
  silently accepts non-finite values, and `NaN` further compares `false` against
  any bound (i.e. it would also slip past a naive `min/max` gate). Non-finite is
  not a valid numeric input regardless of whether a range is enforced: any
  numeric wire-input validator must add an explicit `math.IsNaN || math.IsInf`
  reject. This is the numeric analogue of the "validation surface must cover the
  whole value space" discipline.

## Gotchas

- Additive-within-1.5 is invisible to version-based negotiation. Because the
  three new inputs are additive to octo/v2 and `card_version` stays "1.5", a
  version-only capability gate cannot distinguish a deployment that accepts them
  from an older 1.5 deployment that 400s them. The manifest's new `elements` /
  `inputs` arrays are exactly the element-granularity probe that closes this gap.
- Host has no `go` binary; unit tests were run via a workspace-local
  `.context/go` (go1.25.12) reusing the host module cache. `pkg/cardmsg` is pure;
  `modules/bot_api` / `modules/message` card tests need MySQL (`octo-test-mysql`)
  + `OCTO_MASTER_KEY` (exactly 32 bytes).

## Resolved / Open

- **RESOLVED (PR#556 review, maintainer)** — `min/max` range is **not**
  server-enforced; it is delegated to bot business validation. The server only
  format/type-checks (finite number / `YYYY-MM-DD` / `HH:MM`). Rationale: AC's
  own schema defines `min/max` as an ignorable *hint* (so a spec-conforming
  client can submit out-of-range), and card/action collapses all validation
  errors to one anti-enumeration `invalid` (so a range rejection gives the user
  no actionable feedback). Unlike `ChoiceSet` choices (a *constitutive* bound —
  a value outside them is forged), `min/max` is *advisory*, in the same class as
  `isRequired`/`regex` which were already not enforced.
- **Open** — AC 1.6 `Data.Query` (dynamic typeahead) is a separate XL item: it needs a new
  synchronous client→bot query channel that Octo's async event-queue model does
  not have. Sequenced after modal forms (Goal 4), gated on a real
  remote/huge-choice-set need.

## Tier 1 追加 — AC 1.5 展示元素补全（同 PR，PR#556 讨论后加入）

同一 PR 内把 octo/v2 展示白名单补齐到 AC ≤1.5：新增 **ImageSet(1.0) / RichTextBlock(1.2) /
Table(1.5) / ActionSet(1.2)**（版本实测 adaptivecards.io）。每个元素覆盖四个面：

- **校验**（validate.go）：ImageSet 逐图复用 Image 纪律（新 `imageChild`）；RichTextBlock 遍历
  inlines、TextRun.selectAction 过 URL allowlist（TextRun 非 markdown，text 无链接面）；Table
  递归 rows→cells→items（计入节点/深度预算）+ cell selectAction/backgroundImage；ActionSet.actions
  走动作 allowlist（Submit 仍受 octo/v2 门控）。
- **派发对称**（interactive.go）：`findSubmitInElements` 同步遍历 ActionSet.actions、Table
  cells、ImageSet images、RichTextBlock inlines 的 selectAction —— 与本 PR 的 inlineAction 教训
  同源（校验面 == 派发面，防「发送通过、点击死按钮」）。
- **plain 派生**（plain.go）：RichTextBlock 拼接文本、ImageSet 每图 `[图片]`、Table 递归 cells；
  ActionSet 不参与。
- **单一权威**（whitelist.go `displayElements`）：D12 清单 `elements` 自动带上四者。

Gotcha：修正了既有 `TestValidateWhitelistRejections` 把 Table 误标「AC 1.6 应拒」——Table 实为
**1.5**、现已支持，替换为仍未支持的 Media(1.1)/ToggleVisibility(1.2) 保持负向覆盖。仍未支持
（后续按需）：Media、Action.ShowCard/ToggleVisibility/Execute、模板/数据绑定、AC 1.6 元素。

Gotcha 2（review 追加）：Table 是**第三个**需要同步的镜像面证据。加 Table 时补了校验
（`w.elements` 递归 cell.items）和派发（`findSubmitInElements` 递归 Table），却漏了**提交期 input
采集**（`collectInputSpecsFromElements` 只递归 Container/ColumnSet）。后果：Table 单元格里的
`Input.*` 发送/派发都通过，提交却被当「未声明」拒。修复=采集侧也递归 Table cell items。教训重申：
「校验 / 派发 / **采集**」是同一白名单的三个消费面，任何递归容器都必须三处同步——理想是共享一个
遍历器，退而求其次是每个面都有守卫测试（本次补 `TestTier1TableCellInputCollected`）。
