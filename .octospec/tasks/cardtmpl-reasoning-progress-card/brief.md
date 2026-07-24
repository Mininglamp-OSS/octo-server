---
type: Task
title: "Task: cardtmpl-reasoning-progress-card"
description: 把 ai.reasoning-process handoff 翻译成一张 live 平台卡 —— 应用"无操作行为"产品决策(去 reasoning_stop/reasoning_retry 两个 Submit,降 octo/v1,只留 ToggleVisibility 折叠),重出 v1 handoff + 重编 goldens,建子包并经 RegisterJSON 注册,证明经 Registry.Render 正确渲染。依赖 E1 引擎(PR #654)。
tags: [cardtmpl, ai-reasoning-process, json-template, wire-contract, trust-boundary, testing]
timestamp: 2026-07-23T14:30:27Z
# --- octospec extension fields ---
slug: cardtmpl-reasoning-progress-card
upstream: (self · roadmap E1 下游 · 依赖 PR #654)
source: self
---

# Task: cardtmpl-reasoning-progress-card

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

把业务方交付的 `ai.reasoning-process` handoff **原样翻译成一张已注册、可渲染的 live 平台卡**
(决策 A,2026-07-23):**保留 handoff 自带的操作按钮**(`reasoning_stop` / `reasoning_retry`
`Action.Submit` + `Action.ToggleVisibility`),视图维持 **octo/v2**。在 E1 JSON 模板引擎
(PR #654)之上注册,证明经 `Registry.Render` + `cardmsg.Validate` 三态正确渲染,且按钮
契约合法可路由。

**按钮的 handler(真正停止/重推理逻辑)+ RouteSpec + bot 下发不做**(下游任务)——本任务
交付"按钮存在、ActionContract 合法可路由、能渲染"的卡;点击的服务端 handler 后续补。

产物:
1. **子包 `pkg/cardtmpl/ai_reasoning_process/`** 内嵌 handoff(manifest + contract +
   templates + view 命名 reports + samples + goldens),导出 `Assets`/`HandoffRoot`/
   `TemplateID`/`TemplateVersion`;
2. **owner `ai`**:加入 `l2aOwnerAllowlist`(L0 小改),manifest `owner: ai` +
   `actionType: reasoning.control`;Submit.data 补 `owner`/`action_type`(基座 ActionContract
   self-check 要求,与 docs 卡一致);
3. **注册**:`main.go` 经 `Registry.RegisterJSON` + `SetDefault`;
4. **验证**:conformance(sample → golden 字节等价)+ 三态经 `Registry.Render` 渲染 +
   ActionContract self-check 通过。

## Background

- E1(PR #654)已交付通用 JSON 引擎(`pkg/cardtmpl/jsontmpl` + `jsonTemplate` +
  `Registry.RegisterJSON`)。**本任务是下游**:把这张卡接进生产 Registry。
- as-delivered handoff 现**不能原样注册**,本任务负责补齐(**不改卡片视觉/按钮**):
  1. manifest **无 owner/actionType** → 加 `owner: ai` + `actionType: reasoning.control`,
     `ai` 入 L2a 白名单;Submit.data 补 `owner`/`action_type` 让 self-check 通过(唯一对
     handoff 字节的偏离,按钮可路由的必要前提,goldens 同步);
  2. reports 按 **state 命名** → 合并/改成 **view 命名**(`active` = reasoning_stop+toggle,
     `result` = toggle,`error` = reasoning_retry+toggle),内容不变。
- 客户端已渲染 v2 交互 AC(memory `card_message_v2_web_gate`,2026-07-23 更新:docs 允许/
  拒绝 Submit 实测活),推理卡按钮渲染无门。

## Load-bearing list

- **wire-contract** — `Registry.RegisterJSON` / `manifest.views` / v2 view 需 view 命名
  interaction report;`views[].states` 齐全供 state→view;缺项 → 注册期 fail-close。
- **wire-contract** — `l2aOwnerAllowlist` 增 `ai`(L0):新平台卡 owner 家族;不放松 L2b
  `ext.*` 拒绝逻辑。
- **wire-contract** — ActionContract:manifest `owner`+`actionType` 派生 contract,注册期
  self-check 断言每个 Submit.data.{owner,action_type} 一致 → Submit.data 必须携带这两键。
- **wire-contract / trust-boundary** — 经 `Registry.Render` + `cardmsg.Validate` 同一管线
  (组件白名单 / URL allowlist / 上限);data 字面绑定 + Validate 兜底(E1 D6)。
- **wire-contract** — `main.go` composition root:新增一处 `RegisterJSON` + `SetDefault`,
  Freeze 前;不改既有卡。

## Out of scope

- **按钮 handler + RouteSpec + bot 下发** —— 停止/重推理的服务端 handler、
  `cardactiondispatch` RouteSpec、bot 发卡 + 流式 `ReplaceView` 全部**下游任务**。本任务
  按钮点击暂无 route(handler 落地前)。
- **Model A 能力发现广播**(`/v1/bot/card/profile` templating)。
- **多语言**(单 `defaultLocale: zh-CN`);**改 E1 引擎**(已冻结,只消费)。

## Acceptance

- **形态忠实 handoff**:3 个模板保留 `reasoning_stop`/`reasoning_retry`/`reasoning_toggle`
  三种 action;3 view `wireProfile` 均 `octo/v2`;`views[].states` 齐全
  (reasoning/answering→active,completed/stopped→result,error→error);view 命名 reports 齐。
- **ActionContract**:manifest `owner: ai` + `actionType: reasoning.control`;`ai` 已入
  L2a 白名单;每个 Submit.data 携带 `owner: ai` + `action_type: reasoning.control`;注册期
  self-check 通过。
- **conformance**:子包 conformance 测试 —— 每 sample 按 state 选 view 展开 → 与 goldens
  canonical 字节等价(goldens = as-delivered + Submit.data 补 owner/action_type,机械同步)。
- **端到端渲染**:`Registry.Render` 渲染 active(reasoning/answering)、result
  (completed/stopped)、error 三态成功,payload profile=`octo/v2`,过 `cardmsg.Validate`。
- **注册生效**:`main.go` 注册 + `SetDefault`;boot 不 panic;`Registry.List()` 含
  `ai.reasoning-process`。
- **零回归**:`go test ./pkg/cardtmpl/... -race` + `go build ./...` 全绿;既有卡不受影响。
- **门禁**:gofmt / go vet 干净。

## Risks / decisions(已定)

- **D1 · 决策 A** —— 保留按钮、v2 原样翻(用户 2026-07-23 定)。
- **D2 · handler 不做** —— 按钮契约合法可路由,但停止/重推理 handler + RouteSpec + bot
  下发是下游;本任务按钮点击暂无 route(用户 2026-07-23 定)。
- **D3 · owner `ai`** —— 加 L2a 白名单,actionType `reasoning.control`;两个按钮
  (stop/retry)共用该 route,靠 Submit.data `effect`(stop_reasoning/retry_reasoning)区分。
- **D4 · 版本 `0.1.0`** —— as-delivered 从未 publish,直接以其版本号落生产(视觉/按钮不变,
  仅补注册必需的 owner/action_type + reports 改名)。
- **D5 · goldens 机械同步** —— templates 与 goldens 同步加 `owner`/`action_type` 两个静态
  键(非 `${}`,引擎原样透传),engine(template)==golden 自洽;PR 显式标注该 diff。
- **依赖 PR #654** —— 基于 E1 分支尖;开 PR 前 rebase 到合并后 main,丢引擎 commit(同 #650)。

