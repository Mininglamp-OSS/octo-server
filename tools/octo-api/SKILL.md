---
name: octo-api
description: |
  OCTO 项目 OpenAPI / swag endpoint 设计、实现、审查、修改、接入工具链。触发场景：

  - **加 / 改 / 审查 endpoint**："加一个 X 接口" / "设计 API" / "审查这个 endpoint" → 读 `references/api-spec.md`，按工作流走，跑 `make openapi-check`；改 endpoint 时跑 `make openapi-diff` 看 breaking
  - **接入新仓库**："给 X 仓库加入这套工具链" → 读 `references/adoption.md`
  - **工具命令咨询**："openapi-check 是什么" / "怎么跑 lint" → 读 `references/toolchain.md`

  范围：API 形态（URL / schema / 错误码 / swag）+ 本地校验。**不在范围**：handler 业务实现、部署、contract test。
---

# Octo API skill

设计 endpoint 时按工作流写 Go struct + swag 注释，跑 `make openapi-check` 校验，错误按提示修。详细规则按需读 `references/api-spec.md`。

---

## 1. 实现 / 修改 / 审查 endpoint 工作流

接到 "加 / 改 / 审查 X 接口" 都按这套顺序 8 步走。详细规则在 `references/api-spec.md` 各章节，按步骤遇到决策点时读（R 编号速查表在该文件开头）。修改场景 = 同步跑 §2 的 breaking 识别。

审查场景按下面格式输出（逐维度对照 + R 编号 + 实测）：

| 维度 | 结论 | 说明 |
|---|---|---|
| URL / operationId（R6 / R10 / R12） | ✅ / ❌ | 违规点 + 修复建议 |
| 响应 envelope（R1） | ✅ / ❌ | … |
| 字段命名（R3 / R7 / R8） | ✅ / ❌ | … |
| 错误码（R2 / R4） | ✅ / ❌ | … |
| swag 标签（R13） | ✅ / ❌ | … |
| 分页 / 批量（R5 / R11，如适用） | ✅ / ❌ | … |

结尾附 `make openapi-check`（改动场景另加 `openapi-diff`）的实测结果，不要只凭目测下结论。

| 步骤 | 决策内容 | 详见 |
|---|---|---|
| 1.1 | 解析需求（资源/动作/调用方/鉴权） | 见下 |
| 1.2 | URL & operationId | `references/api-spec.md` A |
| 1.3 | envelope 选型 | `references/api-spec.md` B |
| 1.4 | 字段 / 参数命名 | `references/api-spec.md` C |
| 1.5 | 错误码选择 | `references/api-spec.md` D |
| 1.6 | swag 注释 | `references/api-spec.md` E |
| 1.7 | 分页（list）/ 批量（_batch），按需 | `references/api-spec.md` F / G |
| 1.8 | 本地校验：`make openapi-check` | — |

### 1.1 解析需求

| 维度 | 取值 |
|---|---|
| **资源** | 见 `references/api-spec.md` A.1 各服务资源表（参考用，非穷尽） |
| **动作** | create / list / get / update / delete / 状态机动词（close / reopen / archive / extract） |
| **调用方** | Bot / User / Admin（影响 A.2 受众段判断） |
| **鉴权** | Bearer / 无（公开接口，需注明豁免原因；具体鉴权机制由仓库自定） |
| **状态机** | 是 → RPC verb；否 → REST 标准 |

> **示例（贯穿 1.2-1.7）**：用户说"删除 matter 接口" → 资源 = matter，动作 = delete，调用方 = Bot/User，鉴权 = Bearer，非状态机 → REST DELETE

### 1.2-1.7 决策示例（接续示例）

| 要点 | 值 |
|---|---|
| 客户端实际 URL | `DELETE /v1/matters/{matter_id}` |
| swag `@Router` 写法 | `/matters/{matter_id} [delete]`（**不含 `/v1`** —— 由 `@BasePath /v1` 提供前缀，否则 servers + path 双 `/v1` 会重复成 `/v1/v1/...`）|
| operationId | `matter.delete` |
| 成功 envelope | `envelope.Data[EmptyResp]` |
| 失败 envelope | `envelope.Error` |
| 字段 | 仅 path 参数 `matter_id`（无 body / 无响应字段）|
| 错误码 | 401 `AUTH_REQUIRED` / 403 `FORBIDDEN` / 404 `NOT_FOUND` / 429 `RATE_LIMITED` / 500 `INTERNAL_ERROR` |
| swag | 9 个必带标签按 E 模板填 |
| 分页 / 批量 | 不涉及 |

### 1.8 本地校验

```bash
make openapi-check
# coverage → verify (gen + drift) → lint
```

跑通后业务代码跟 swag 自动生成的 spec 文件一起进 git。若启用 CI（可选增强，见 `references/adoption.md`），PR 自动跑全套 6 个 job。

失败处理：
- **环境 / 前置问题**：`openapi-gen` 每次自动跑 doctor 自检（也可单独 `make openapi-doctor`），按报错提示走 `references/adoption.md` 对应步骤 —— 不需要 AI 手动探测环境
- **coverage 失败**：handler 缺 `@Router` 注释（按 api-spec.md E 章节模板补）
- **verify 失败**：spec 没重生，跑 `make openapi-gen`
- **lint 失败**：spectral 报具体规则 ID + 位置，按错误信息修
- **改了现有 endpoint**：另跑 `make openapi-diff` 看 breaking change（详见下面 2 章）

---

## 2. 修改 endpoint + breaking 识别

根据改动范围对应不同路径：

| 改了什么 | 必走 |
|---|---|
| 字段名 / 类型 | 重读 api-spec.md C → 更新 swag @Param/@Success → `make openapi-check` |
| 错误码 | 重读 api-spec.md D → 更新 swag @Failure → `make openapi-check` |
| 鉴权 | 更新 swag @Security + @Failure 401/403 → `make openapi-check` |
| 请求体 schema | 重读 api-spec.md C → 更新 swag @Param → `make openapi-check` |
| URL 路径 | ⚠️ 必 breaking → 走 deprecate 流程（见 api-spec.md H） |

### 识别 breaking change

修改 endpoint 后**必跑** `make openapi-diff`（默认对比 `origin/main`），oasdiff 按 OpenAPI 语义自动分类每个改动：

- 🔴 `error` 严重度 = breaking → 命令 exit 1，CI 阻断
- 🟡 `warning` / 🟢 `info` = non-breaking → 通过

AI 看到 🔴 输出时**主动提示用户**：

> 本次修改 oasdiff 报告 N 个 breaking change：[转述 oasdiff error 列表]
> 建议：(a) 走 Deprecate 流程（见 api-spec.md H）/ (b) 暴露新版本 endpoint / (c) 跟 octo-cli 等客户端对齐后再合并。

---

## 3. AI 工作元规约

接 endpoint 任务时除工作流外的元行为约束：

- **引用规则编号说明设计依据**（"按 R6 资源段要复数所以用 `/matters`"）—— 让用户能追溯
- **遇决策点先读 `references/api-spec.md` 对应章节再下结论** —— 不要凭印象
- **不发明"规范没说的"** —— 不确定的让 `make openapi-check` 报错指出，而非编规则
- **handler 业务实现不规定** —— 本 skill 只管 API 形态（URL / schema / 错误码 / swag），实现交给开发者
