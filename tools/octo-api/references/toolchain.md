# 工具链命令 / 配置参考

本工具集所有 `make openapi-*` target、可覆盖变量、spec 输出路径、版本约束的速查。日常开发只需要 `make openapi-check`，其它命令是细分场景。

## 命令清单

| 命令 | 用途 | 何时跑 |
|---|---|---|
| `make openapi-help` | 列所有 `openapi-*` 命令 + 一句话说明 | 忘记某个命令时 |
| `make openapi-check` | 一键校验：coverage → verify (gen + drift) → lint。**跑完全部闸再汇总**，不在第一道失败就停（存量仓库 coverage 长期红时不遮蔽 lint/verify 结果） | 提交代码前 |
| `make openapi-gen` | 重生 `docs/openapi/swagger.yaml`（默认只产 yaml，单一事实来源）| 改了 API 注释后 |
| `make openapi-lint` | 单独跑 spectral 校验 | debug lint 错误时迭代用 |
| `make openapi-verify` | gen + drift 检测 | 单独验证 spec 跟 git 同步 |
| `make openapi-coverage` | 检查 handler 是否都有 `@Router` | 排查 coverage 失败 |
| `make openapi-diff` | 跟 base ref（默认 origin/main）的 spec diff，识别 breaking | 修改现有 endpoint 后 |
| `make openapi-preview` | gen + 用 Redoc 生成 `docs/openapi/index.html` 静态预览 | 本地想看渲染后的 API 文档 |
| `make openapi-doctor` | 环境自检：全局注解 / @Router / go / npx / baseline 是否就绪 | 接入排障、首次使用 |

> swag v2 CLI 和 oasdiff CLI 由 `openapi-gen` / `openapi-diff` **首次跑时自动 `go install`**，用户不需要单独装。

> `openapi-gen`（含 verify / check 链路）每次自动先跑 doctor 的 gen 范围自检（不含 baseline 项），环境缺失会带 adoption 步骤指引直接失败 —— 不会跑出难懂的 swag/spectral 报错。

> 工具自身的回归测试（spectral 规则集 / JS function 单测 / coverage 脚本单测）由上游仓库 [octo-openapi-dev-skill](https://github.com/liuooo/octo-openapi-dev-skill) 维护和 CI 跑，**用户项目不需要本地执行**。

## 可覆盖变量

`assets/openapi.mk` 顶部声明，环境变量或 `make VAR=...` 覆盖：

| 变量 | 默认值 | 何时改 |
|---|---|---|
| `OPENAPI_OUT_DIR` | `docs/openapi` | spec 想放别的目录（如 `api/`）|
| `OPENAPI_OUT_TYPES` | `yaml` | 默认只产 swagger.yaml；需要运行时 /swagger 注册器加 `go`（如 `yaml,go`），需要 json 给下游消费加 `json` |
| `SWAG_VERSION` | `v2.0.0-rc5` | 升级 swag |
| `BASE_REF` | `origin/main` | `make openapi-diff` 对比其它分支 / tag |
| `OCTO_API_DIR` | `tools/octo-api` | skill 包根目录位置（罕见改动）|

用法：

```bash
# 临时
OPENAPI_OUT_DIR=api/ make openapi-gen
BASE_REF=origin/release/v1.0 make openapi-diff

# 永久（主 Makefile include 之前设）
OPENAPI_OUT_DIR := api
include tools/octo-api/assets/openapi.mk
```

## 工具版本

| 工具 | 版本 | 升级方式 |
|---|---|---|
| `swag` | v2.0.0-rc5（pin）| 改 `assets/openapi.mk` 的 `SWAG_VERSION`，跑 `make openapi-gen` 看 schema diff |
| `spectral-cli` | `@latest`（npx）| 不主动 pin，每次 CI 拉最新 |

升级注意：swag 版本变化可能影响生成的 yaml 内容（schema 结构 / 命名）。升级 PR 必须跑 `make openapi-check` 跟 base 对比，确认 spec 兼容。

## Spec 路径

| 项 | 路径 / 说明 |
|---|---|
| 输出目录 | `docs/openapi/`（机器生成产物，跟人工写的 `docs/*.md` 不混）|
| spec 文件 | `swagger.yaml` —— swag 工具的硬编码命名，**内容是 OpenAPI 3.1**（非 Swagger 2.0）。`swagger.json` 默认不再生成（`OPENAPI_OUT_TYPES` 加 `json` 启用）|
| `docs.go` | swag 的 Go 注册器代码，默认不生成 —— 需要时 `OPENAPI_OUT_TYPES=yaml,go`，见下节"运行时暴露 /swagger endpoint" |
| `index.html` | `make openapi-preview` 生成的本地预览，不 commit（加到 `.gitignore`）|

## CI 集成（可选）

是否上 CI 由项目决定。**本地 `make openapi-check` 已足够**保证质量；CI 是把闸搬到 GitHub 让 PR 强制跑。

如要启用：`cp tools/octo-api/assets/templates/openapi-workflow.yml .github/workflows/openapi.yml`。包含 6 个 job：

| Gate | 角色 | 阻塞 PR？ |
|---|---|---|
| Detect changed paths | docs-only PR 跳过整套 | — |
| Swag Annotation Coverage | handler @Router 覆盖 | ✅ |
| Generate & Verify OpenAPI 3.1 | swag 生成 + drift 检测 | ✅ |
| Spectral Lint | 32 条 OCTO 规则 + spectral:oas | ✅ |
| Breaking Change Check | oasdiff 检测 breaking | ✅ on error |
| Toolchain Self-Test | 工具链自身回归 | ✅ |

repo admin 把 5 个 ✅ 的 job 加入 branch ruleset 的 required check 即生效（详见 `adoption.md` 可选增强）。

## 运行时暴露 /swagger endpoint

默认未启用（且 `docs.go` 默认不再生成 —— 先设 `OPENAPI_OUT_TYPES := yaml,go` 重新 gen）。若需运行时让客户端拉 spec（SDK 生成器 / 在线 viewer），按 [swag 官方文档](https://github.com/swaggo/swag#general-api-info) 在 `main.go` 加 import + 在 router 加 `r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))`。
