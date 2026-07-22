# L2b Business-Extension Cards (占位)

本目录为 L2b(业务自定义卡片)代码规划占位。**L2b 通道本 PR 不开放**,本目录不包含任何生产代码。

## 什么是 L2b

见 `docs/platform-card-base.md` §2 分层模型:

- **L0** 平台基座(`pkg/cardtmpl` 主包)
- **L1** 单卡契约(`pkg/cardtmpl/testdata/handoff/<id>@<ver>/`)
- **L2a** 平台内置卡片(octo 团队维护,`pkg/cardtmpl/<card>/`)
- **L2b** 业务自定义卡片(业务方维护,`pkg/cardtmpl/ext/<owner>/<card>/`) ← 本目录

## 目录规范(L2b 开放后)

```
pkg/cardtmpl/ext/
├── README.md                        (本文件)
├── acme_pipeline/                   ← owner=ext.acme.pipeline
│   ├── template.go                  ← Template 接口实现
│   ├── template_test.go
│   └── testdata/handoff/acme.pipeline.release-approval@1.0.0/
│       ├── manifest.json
│       ├── contract/data.schema.json
│       ├── reports/*.interaction.json
│       └── samples/*.json
```

## 开放前置条件

见 `docs/platform-card-base.md` §2.2 约束 5:

1. L0 (`octo-card@1.x`) 至少一个 release 无 breaking change;
2. L2a 至少 3 张卡片走完 Registry 全链路,覆盖 v1/v2 两种视图模式;
3. `docs/l2b-owners.md` 清单流程演练完成;
4. callback URL 白名单、独立 token 通道、per-owner 观测就绪。

## 当前状态(2026-07)

- L0 骨架:`cardtmpl-registry-pilot` PR 落地中
- L2a 卡片:pilot `docs.access-request@0.2.0` 一张
- L2b 通道:**未开放**,`pkg/cardtmpl/registry.go` 内 `ext.*` owner 前缀会在注册期 panic
- L2b 清单:`docs/l2b-owners.md` 空
