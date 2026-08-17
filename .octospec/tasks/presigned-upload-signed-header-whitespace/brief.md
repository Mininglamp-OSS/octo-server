---
type: Task
title: "Task: presigned-upload-signed-header-whitespace"
description: 文件名含连续半角空格时预签名 PUT 在腾讯 COS 稳定 403 SignatureDoesNotMatch。根因是 minio-go 的 signV4TrimAll 会折叠引号内空白、而 COS 按 AWS 规范保留，两侧 canonical 不同。修法是让签名头本身成为 Trimall 不动点；用户文件名不变，仍由 filename* 无损承载。同一机制在 Content-Type 上有第二个可达向量。
tags: ["wire-contract", "bot-api", "testing", "commit"]
timestamp: 2026-08-17T13:10:00+08:00
# --- octospec extension fields ---
slug: presigned-upload-signed-header-whitespace
upstream: Mininglamp-OSS/octo-server#760
source: self
---

# Task: presigned-upload-signed-header-whitespace

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

让预签名 PUT 的**签名头在两套 SigV4 canonical 规则下取值相同**，从而消除
GH#760：文件名中出现 **≥2 个连续 `U+0020`** 时，浏览器直传腾讯 COS 稳定返回
`403 SignatureDoesNotMatch`，与文件字节、大小、格式、扩展名均无关。

用户的文件名**不做任何改动**——原始名（含连续空格）仍由 RFC 5987 `filename*`
无损承载（空格编码为 `%20`），下载文件名不变。被归一化的只有带引号的 ASCII
兼容回退 `filename="…"`，因为只有它会把字面空白送进签名。

## Background

`getUploadCredentials` → `BuildContentDisposition` → `ServiceCOS.PresignedPutURL`
→ minio-go `PresignHeader`。SigV4 对每个签名头值取 **canonical 形式**再哈希，而
两侧的 canonical 规则不同：

- minio-go v7.0.61 `pkg/signer/utils.go` 的 `signV4TrimAll` 是
  `strings.Join(strings.Fields(v), " ")` —— 折叠**任意位置**的空白串，**包括引号内**；
- AWS SigV4 规范明确 *"Do not trim excess white space in a header value if it
  appears within a quoted string"*，腾讯 COS 遵守该例外，保留引号内空白。

于是 `inline; filename="a  b.pdf"` 被**签成** `filename="a b.pdf"`、被**验成**
`filename="a  b.pdf"`，canonical request 不同 → 签名不同 → 403。

MinIO 后端从不暴露该缺陷：MinIO server 自带同一份 `signV4TrimAll`，两侧同样折叠。
这解释了为什么本地/自建环境复现不出来，只有 COS（及遵守同一例外的 AWS S3）中招。

历史：octo-web#348 自 2026-06-01 开放至今，4 位用户独立反馈。octo-server PR#219
（"stop signing Content-Disposition into presigned upload PUT"）方向正确但根因描述
错误（归因于「浏览器/代理/网关规范化不一致」），经 3 人 approve 后于 2026-06-03 被
作者关闭且未留说明；实测证明浏览器逐字节透传，改写发生在**服务端签名侧**。

## Load-bearing list

- `BuildContentDisposition` 的输出形状——被 **3 个模块 6 处**调用：
  `modules/file/api.go:626`（multipart `uploadFile`）与 `:945`（`getUploadCredentials`）、
  `modules/robot/api.go:2085` 与 `:2233`、`modules/bot_api/file.go:90` 与 `:285`。
  同时用于预签名 PUT 与服务端 multipart `UploadFile`。
- `/v1/file/upload/credentials` 的 wire contract：`contentDisposition` 与
  `contentType` 均由客户端**逐字节回传**，任何一侧变动都会改变 PUT 成败。
  `contentType` 的默认值改为在**折叠之后**兜底（原先在折叠之前），且带非法
  header 字节时回退默认值而非替换为 `_`——mangle 出一个调用方没要过的 MIME
  类型，比给个诚实的 `application/octet-stream` 更糟。副作用：纯空白
  `contentType` 这条边路上，`Content-Type` 从「不进签名头集合」变成「进」，
  客户端必须按契约回传（octo-web 已经如此）。
- 三个 SigV4 后端（COS / MinIO / S3）的签名头集合。
- 存储对象上的 `Content-Disposition` 元数据（影响裸 `downloadUrl` 的下载名）。
- `modules/bot_api`、`modules/robot` 的调用点**不做 `sanitizeFilename`**，原始
  multipart 文件名 / query 参数直达 `BuildContentDisposition`。

## Out of scope

- **真实 COS 验证**。全部证据来自本地按 AWS 规范实现的替身网关，未打过线上 COS。
- **不采用 PR#219 的方案**（预签名不签 `Content-Disposition`）。该方案会让预签名
  路径上传的对象失去存储态 disposition，公开 bucket 直链下载会退化为 UUID 名；
  且不覆盖服务端 multipart 路径。两方案正交，本任务选择在 header 源头修。
- **`bot_api` / `robot` 缺失的 `sanitizeFilename`**。本修复让 header 无论是否清洗
  都安全，但清洗缺失本身是独立隐患，未处理。
- **扩展名尾随空格**（`report.pdf ` → 凭证接口 400「不支持的文件类型」）。既有行为，
  与 403 无关，未改。
- **Aliyun OSS**。其 V1 string-to-sign 逐字包含 Content-Type、不含
  Content-Disposition，不经 `presignPutHeaders`，未纳入本次不变量。
- **octo-web**：通用「上传失败」文案（octo-web#1396）、失败态呈现（#1393）、
  预签名接口被调两次（`precheckUploadCredentials` 的有意设计）。

## Acceptance

- `TestIssue760_SignedHeaderInvariant`：`BuildContentDisposition` 的输出必须是
  `signV4TrimAll` 的**不动点**（比枚举文件名严格）。
- `TestGetUploadCredentials_ContentTypeContract`：**驱动 handler 本身**，断言
  `resp["contentType"]` 是 Trimall 不动点、是合法 header 值、且与交给 service 的
  值相等。用 `.ini`（白名单内且无 MIME 映射）承载调用方原值，否则
  `mime.TypeByExtension` 会覆盖掉它、测试沦为空转。经变异验证：摘掉
  `normalizeUploadContentType` 该测试变红。
- `TestIssue760_EchoedContentTypeMatchesSigned`：回显给客户端的 `contentType`
  必须与最终被签名的值**相等**，且对两套 Trimall 规则稳定。
- `TestIssue760_Sweep*`：全部 128 个 ASCII 码位 × 4 个位置 × 长度 1–3 的连续串、
  25 个 Unicode 空白码位、混合空白串、引号/反斜杠注入、20000 个定种子随机名，
  逐一满足「两套 Trimall 不动点 + 合法 header 值 + 引号平衡」，且**清洗与未清洗
  两条入口都跑**。
- `TestIssue760_SweepIsNotVacuous`：同一判据对**修复前**的构造必须失败。
- `TestIssue760_CharacterMatrix` / `ConsecutiveSpacesBreakPresignedPUT`：对
  COS 规则网关，此前 403 的每一行现在 200，且 `filename*` 仍带 `%20%20`。
- 回归对照：`modules/{file,robot,bot_api,botfather,group}` 的失败集合与
  `origin/main` **完全一致**（差异仅为本地无 MySQL 导致的既有失败）。
- `go build ./...`、`go vet ./modules/...`、`golangci-lint run ./modules/file/...`、
  `make i18n-extract-check`、`make i18n-lint` 全部通过。
