---
type: Task
title: "Task: file-upload-disposition-hardening"
description: Store browser-renderable uploads with Content-Disposition attachment so a direct CDN URL downloads instead of executing, while in-app preview keeps rendering via the presigned inline override.
tags: ["trust-boundary", "external-content", "wire-contract", "test"]
timestamp: 2026-08-08T06:55:19Z
# --- octospec extension fields ---
slug: file-upload-disposition-hardening
upstream: 渗透测试复测报告 2026-07-30 §4.2 / §4.3（初测 §4.2 / §4.3）
source: self
---

# Task: file-upload-disposition-hardening

## Goal

上传签发侧对**浏览器可执行渲染**的内容类型（`text/html`、`image/svg+xml`、
`application/xhtml+xml`、`text/xml` 等）把存储的 `Content-Disposition` 从
`inline` 改为 `attachment`。

效果：直接在地址栏打开 CDN 对象 URL 时浏览器下载而非执行脚本，存储型 XSS 链
断开；应用内预览不受影响，因为签名 GET 用 `response-content-disposition` 覆盖
存储值（`modules/file/service_cos.go:509`）。

**不封杀 HTML 上传**。见下方 Background 的三处生产者证据。

## Background

复测报告 §4.2（html 文件上传）/ §4.3（存储 XSS）的复现是：拿 token 调
`/api/v1/file/upload/credentials?contentType=text/html&filename=evil.html&path=evil.html&type=chat`
取预签名 PUT，直传后在浏览器地址栏打开
`https://cdn.deepminer.com.cn/im-test/chat/srctest/xss_alert.html`，JS 执行。

复测把 §4.2 标为「已修复」，但其「修复情况」原文是**「存储桶已配置，但接口仍未
授权」**；§4.3 标「部分修复」，原文是**「存储桶已配置，该目录依然可访问触发
xss」**。即桶策略动过，**接口侧一字未改**，原始利用链仍成立。

### 为什么不能封杀 `.html`

HTML 附件是产品一等功能，三处独立证据：

| 位置 | 证据 |
|---|---|
| octo-web | `packages/dmworkbase/src/Components/FilePreviewPanel/renderers/HtmlIframeRenderer.tsx` —— 专门的 HTML 预览渲染器 |
| octo-ios | `WKSafeFilePreviewVC.m:437` 注释：「HTML 文件需要渲染成真实网页(报表/图表/导出页多依赖 JS)，故对 .html/.htm」单独打开 `allowsContentJavaScript` |
| openclaw-channel-octo（agent 通道） | `src/api-fetch.ts:315`、`src/inbound.ts:498` 均映射 `.html → text/html` |

把 `.html` 移出 `allowedExtensions` 会打断 agent 产出的 HTML 报表/图表，是功能
回归而非安全收益 —— 与 reminder 任务里「对无生产者的频道类型硬套 group_member」
同型的错误。

### 为什么改 disposition 就够

1. **成因不是「HTML 存在」，是「HTML 以 `inline` 躺在 CDN 上」**。报告的复现是
   在地址栏直开 CDN URL，脚本跑在 CDN 源（`cdn.deepminer.com.cn`），不是应用源。
2. **应用内预览不依赖存储值**。`PresignedGetURL` 把 disposition 作为**签名 query
   参数** `response-content-disposition` 下发（`service_cos.go:504-509`，S3 同形），
   它覆盖对象存储时的 `Content-Disposition`。`/v1/file/download/url?disposition=inline`
   因此仍能拿到内联渲染的 URL。
3. **预览侧本就沙箱**。`HtmlIframeRenderer` 两条分支都是
   `sandbox="allow-scripts"` 且**无** `allow-same-origin` —— 内容跑在不透明源，
   碰不到主站 DOM / cookie / localStorage。所以保留内联预览不引入应用源 XSS。
4. **移动端不受影响**。iOS `WKSafeFilePreviewVC` 先把字节下载到本地再从 `file://`
   渲染；`Content-Disposition: attachment` 拦不住程序化下载。

### 两条必须一起覆盖的边

**(a) 六个签发点，不止 `modules/file`。** `BuildContentDisposition` 有 6 个调用点：

```
modules/file/api.go:626      multipart uploadFile
modules/file/api.go:934      presigned getUploadCredentials
modules/robot/api.go:2065    robot multipart
modules/robot/api.go:2213    robot presigned
modules/bot_api/file.go:90   bot API multipart
modules/bot_api/file.go:285  bot API presigned
```

只改 `modules/file` 而放着 robot / bot_api 继续吐 `inline`，等于给一侧加防护而
留着同源的另一侧 —— 依 `trust-boundary` 规则的 adapter parity 条款这本身就是漏洞。
而且 **bot 侧正是 agent 生成 HTML 的来源**，是最该覆盖的一条。

**(b) 谓词必须按解析后的 contentType 判，不能只看扩展名。**
`getUploadCredentials` 的 contentType 推导是：

```go
if ext != "" {
    if inferred := mime.TypeByExtension(ext); inferred != "" {
        contentType = inferred      // 服务端接管
    }
}                                    // mime 不认识时，客户端传的 contentType 存活
```

实测 `mime.TypeByExtension(".ndjson") == ""`，而 `.ndjson` 在
`allowedExtensions`（`const.go:189`）里。因此
`?filename=x.ndjson&contentType=text/html` 今天就能把对象存成
`text/html` + `inline` —— **一个只看扩展名的谓词会漏掉这条**。同理
`.appimage`、`.tsv` 等 mime 未覆盖的允许扩展名。

实测 mime 解析（用于确定可渲染集合）：

| 扩展名 | `mime.TypeByExtension` |
|---|---|
| `.html` / `.htm` | `text/html; charset=utf-8` |
| `.xml` | `text/xml; charset=utf-8` |
| `.svg` | `image/svg+xml` |
| `.xhtml` | `application/xhtml+xml` |
| `.ndjson` / `.appimage` | `""`（客户端值存活） |

`.svg` 不在默认白名单内，但 `DM_FILE_EXTRA_ALLOWED` 的注释example 就是 `".svg,.heic"`
（`const.go:226`），部署可开启，故纳入谓词。

## Load-bearing list

- `trust-boundary` / `external-content` — 用户与 agent 上传的内容在浏览器中的
  渲染边界；本改动即该边界本身。adapter parity：六个签发点必须同时覆盖
- `wire-contract` — `/v1/file/upload/credentials`、`/v1/file/upload/presigned`
  响应里的 `contentDisposition` 字段值对可渲染类型从 `inline` 变 `attachment`。
  **字段结构不变**，且客户端必须原样回显该值到 PUT（已签名，改了就 403），
  所以三端无需改动即可生效
- **签名头契约** — `PresignedPutURL` 把 `Content-Type` 与 `Content-Disposition`
  作为**签名头**下发（`service_cos.go:354-357`）。服务端返回什么，对象就存什么；
  客户端无法篡改。这是本方案成立的前提
- **`filename == ""` 分支** — `BuildContentDisposition("")` 现在返回 `""`，
  PresignedPutURL 于是不签该头，对象**没有** `Content-Disposition`，CDN 默认行为
  就是内联渲染。可渲染类型在 filename 为空时也必须发 `attachment`
- **签名 GET 覆盖语义** — `PresignedGetURL` 的 `response-content-disposition`
  必须继续生效，否则应用内预览会随本改动一起断
- `modules/file` 的 legacy 错误响应形状 — 本模块尚未迁移 i18n envelope，
  新增拒绝路径（若有）需与既有 `c.ResponseError` 同形，理由见
  `authtree_guard.go:55-59`
- 既有存量对象 — 已经以 `inline` 存储的对象**不受本改动影响**，仍可被直开渲染

## Out of scope

- **`.html` / `.htm` / `.xml` 的上传白名单不动**。理由见 Background；封杀会打断
  agent HTML 报表与 iOS 的 HTML 渲染路径
- **复测 §4.4 跨用户文件覆盖**。同属文件模块，但它要动「上传 path 里的频道成员
  校验」，风险剖面与本条完全不同（可能打断三端上传），单独立项，不与本条捆绑
- **复测 §4.6 下载接口越权**。`modules/file` 没有任何数据库表（无 `sql/` 目录），
  服务端不存在「这个 object 属于谁」的事实源。仓内已有既定决策记录该问题属于
  **另一层**（`modules/file/authtree_guard.go:30-46`：需归属表 / capability token /
  bucket 策略，且要同时覆盖 human 路由）。本任务不触碰，也不假装关闭
- **AK ID 出现在预签名 URL 中**（复测 §4.6 的后半）。这是 SigV4 的固有属性
  （`X-Amz-Credential` 必含 AKID）。报告建议的「改用 STS」是腾讯云 COS 专属能力，
  本仓有六个存储后端，自建 MinIO / SeaweedFS 部署会全线断掉，不能照建议改
- **存量对象的 disposition 回填**。需要遍历 bucket 改对象元数据，属运维动作，
  单独立项
- 不改存储后端实现（`service_cos.go` / `service_s3.go` / ... 的签名逻辑）
- 不改 CDN / bucket 侧配置

## Acceptance

- [ ] **主判据（必须先红后绿）**：请求
      `/v1/file/upload/credentials?type=chat&filename=evil.html&contentType=text/html&fileSize=10`，
      响应的 `contentDisposition` 以 `attachment` 开头，且**不含** `inline`。
- [ ] **contentType 绕过被堵**：`?filename=x.ndjson&contentType=text/html&...`
      —— 扩展名在白名单、`mime.TypeByExtension` 返回空、客户端 contentType 存活
      的这条路径，同样得到 `attachment`。此判据刻意不依赖扩展名，用于证明谓词
      按解析后的 contentType 判定。
- [ ] **filename 为空**时可渲染类型仍发 `attachment`，而不是回退到
      `BuildContentDisposition("") == ""`（不签头 → CDN 默认内联）。
- [ ] **非可渲染类型逐字节不变**：`.jpg` / `.png` / `.pdf` / `.mp4` / `.txt` /
      `.md` 的 `contentDisposition` 与改动前完全一致（仍为 `inline; filename=...`）。
- [ ] **六个签发点全部覆盖**：`modules/file`、`modules/robot`、`modules/bot_api`
      的 multipart 与 presigned 六条路径对同一个 `evil.html` 都返回 `attachment`。
      守卫测试锁定「`BuildContentDisposition` 及其替代者的调用点集合」，新增调用点
      不覆盖时转红。
- [ ] **签名 GET 覆盖仍生效**：`/v1/file/download/url?path=...&disposition=inline`
      返回的 URL 仍带 `response-content-disposition=inline...`（保证应用内预览不断）。
- [ ] **默认仍是 attachment**：`/v1/file/download/url` 不传 `disposition` 时仍为
      `attachment`（`api.go:1061-1064` 行为不回归）。
- [ ] 可渲染类型集合有独立的表驱动测试，覆盖
      `text/html` / `image/svg+xml` / `application/xhtml+xml` / `text/xml`
      及其带 `; charset=` 参数的形式（`mime.TypeByExtension` 返回的就带 charset，
      谓词不得因参数串而漏判）。
- [ ] `go test ./modules/file/... ./modules/robot/... ./modules/bot_api/...` 通过。
- [ ] `golangci-lint run ./...`、`make i18n-extract-check`、`make i18n-lint` 通过。

### 跨仓依赖（octo-web，同一分支名）

- [ ] octo-web 的 HTML 预览改走 `getPresignedPreviewUrl`。该 helper 已存在且有
      测试（`packages/dmworkbase/src/Utils/download.ts:24`），但**生产代码零调用点**
      —— 目前只有 `download.test.ts` 引用它。不接上的话，本改动会让 HTML 预览
      从「内联渲染」变成「触发下载」。
- [ ] 预览侧的 `sandbox="allow-scripts"`（无 `allow-same-origin`）不得放宽 ——
      它是保留内联预览而不引入应用源 XSS 的前提。加守卫测试锁定该属性。
- [ ] `cd apps/web && pnpm test`、`pnpm lint` 通过。

### 需要人工确认（不阻塞本 PR）

- [ ] 与渗透方确认：§4.2 / §4.3 的判据是「直开 CDN URL 不执行脚本」，还是
      「不允许上传 HTML」。若是后者，本方案不满足，需要产品决策是否砍掉 agent
      HTML 报表能力。**报告正文的复现是前者。**
- [ ] 确认 CDN 是否尊重对象的 `Content-Disposition`（腾讯云 COS 默认尊重）。
      若 CDN 侧有覆盖规则，本改动需配合 CDN 配置才生效 —— 这一点无法在代码里验证，
      需部署侧实测一次。
