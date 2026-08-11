# Verification: brief claims measured against HEAD

本文件把 brief 里对**当前实现**的断言从代码阅读升级为实证。

两组测试，分工相反：

| 文件 | 内容 | 改完应当 |
|---|---|---|
| `pkg/auth/session_rollout_invariants_test.go` | 必须存活的控制面规则 | **仍绿** |
| `pkg/auth/session_rollout_legacy_behavior_test.go` | 被移除的缺陷行为（tripwire） | **翻红** |

tripwire 默认 skip，编译但不运行——这样签名变更会打断构建而不是悄悄腐烂，
同时绿色 CI 不依赖缺陷继续存在。

- 基线：`d68d0ad`（`main` + brief rev2），`pkg/auth` 生产代码未做任何改动
- 环境：`redis-server` 6379（intended）+ 6380（wrong），`--save "" --appendonly no`；
  MySQL / WuKongIM **本次不需要**——`pkg/auth` 的 `newLegacyMigrationTestStore`
  只用 `config.New()` 的默认 Redis
- 复现 tripwire：`OCTO_ROLLOUT_LEGACY_TRIPWIRES=1 go test ./pkg/auth/ -run TestTripwire -v -count=1`
- 复现不变量：`go test ./pkg/auth/ -run 'TestRolloutFloor|TestRolloutControl|TestMigrationApplyStaysGated' -v -count=1`
- 全量回归：`go test ./pkg/auth/ -count=1` → `ok`

> 下文编号 C1–C6 对应 tripwire 的 T1–T6。

---

## C1 — Redis 丢 control key ⇒ 拒绝启动

```
C1a mode=revoke, control missing -> auth: session rollout control is required for mode revoke
C1b mode=expand, floor=revoke    -> auth: configured session mode expand is below persisted floor revoke
```

`runtime.go:90-93` 把这个 error 变成 `panic()`，而 `main.go:188` 在 boot 路径上。
测试环境 Redis `appendonly no`，只有 RDB 快照。

C1b 同时是那个**顺序陷阱**的机器证据：先推 floor 再改 env，窗口内任何一次
pod 重启都撞这条。

→ brief §1（floor 丢失向上收敛）、§4（mode 由 floor 派生）

## C2 — 连错 Redis 无法被识别，且会造成真实破坏

初版只比较了纯函数的两次调用，近乎同义反复（指纹根本不接触 client）。
改成端到端后结论更强：

```
T2 fingerprint identical: 00c12d99322929a9aa341fa978e9c361
T2 instance ids differ  : e894455d6c1f44ca vs 96ded2e003cdc18b
T2 apply on wrong instance: shortened=1, victim TTL now 30m0s
```

一个完整配置的 store 指向**错误实例**，成功执行了一次真实的 `--apply`，
把一条无关 token 的 TTL 改掉了，全程无人反对。所有身份物件都与"指向正确实例"时一致。

第二行是关键：**区分它们的原料早就有了**，`currentRedisInstanceID()`
（`session_migration.go:503`）migration checkpoint 已在用，只是没进指纹。

→ brief §6（指纹纳入实例身份）

## C3 — observe 与 migrate 对坏 payload 口径不一致

```
observe : total=2 v1=0 v2=1 v3=0 decode_invalid=1
migrate : scanned=2 v1=1 v2=1 v3=0 invalid=0
```

同一批 key，同一个坏 payload：observe 走 `Decode()` 计 `decode_invalid`；
migrate 的 Lua 只看前 3 字节、`version` 默认值就是 `1`
（`session_migration.go:22-27`），把它**当 v1 正常处理**且不单独报告。

这就是实操记录里 53 vs 54 的来源。`enforce` 门禁依赖 observe 的 `v1=0`，
收敛动作却由 migrate 执行——两把尺子量同一件事。reconciler 要靠这个谓词
自动推进，口径不一致会直接变成自动化的错误决策。

→ brief §6（统一口径，坏 payload 计 `invalid` 并跳过）

## C4 — greenfield 死路

```
migrate --apply on empty keyspace: scanned=0 complete=true
completion evidence key exists   : true
RecordRolloutObservation(empty)  -> auth: rollout observation evidence must cover a non-empty token scope
```

空 keyspace 能通过 migration completion（`recordMigrationCompletion` 只看
`result.Complete`），但**永远拿不到 observation evidence**（`Total > 0` 硬门槛），
所以 floor 到不了 `bounded`。零存量部署必须先伪造一个 canary 登录。

→ brief §6（谓词改为 `v1=0 ∧ v2=0`，空扫描成为最强证据）

## C5 — 仪式集中在最后两跳

```
expand -> revoke took 1ms with zero evidence
revoke -> bounded blocked: auth: bounded floor migration evidence: required evidence is missing
```

与实操吻合（那两跳 60 秒，绝大部分是 pod 重启）。前两跳零门槛，
全部证据机集中在 `→bounded` / `→enforce`。

→ brief §5（reconciler 自动走前两跳，人只在最后停一次）

## C6 — 一个坏 payload 能把 floor 卡死（**新发现；可达性未验证**）

```
migrate --apply natural: scanned=1 shortened=0 unchanged=1 deleted=0 complete=true
observe                : total=1 decode_invalid=1
RecordRolloutObservation -> auth: rollout observation evidence contains ambiguous token records
```

一条**有限 TTL 且在 maxTTL 内**的坏 payload：

1. migrate 在 `natural` 下认为它是普通 v1、TTL 合规 → `unchanged`，不动它；
2. observe 每次都报 `decode_invalid=1`；
3. `validateRecordableRolloutObservation` 拒绝任何 `decode_invalid != 0` 的观测。

**结果：floor 卡死，直到那条 token 自然过期（最长 `TokenExpire` = 720h），
且没有任何工具能清掉它。**

测试环境侥幸躲过了：那 1 条 `decode_invalid` 恰好是**永久** token，
被 migration 压到 cutoff 后自然消失（迁移后 `decode_invalid: 0`）。
如果它当时是有限 TTL 的，现在就已经卡住了。

这条对 reconciler 尤其要紧：人工流程卡住会有人去查，**自动流程卡住可能几周没人发现**。

**但严重度待定**：harness 里那条记录是人工注入的，生产中"有限 TTL 的坏 payload"
由什么机制产生尚不清楚。若损坏只来自那条永远写永久 token 的旧路径，本条实际不可达。
实现前需要查清来源（brief Decisions #9）。

→ brief 新增：坏 payload 的清理路径 + `blocked_by` 报出该原因 + 可达性调查

---

## 不变量侧（必须存活）

同时补了一组刻画测试，锚定本次要改动的控制面中**不该变**的规则——
没有它们，实现阶段"我没改坏"这句话没有依据：

| 测试 | 锚定的规则 |
|---|---|
| `TestRolloutFloorRejectsDowngradeAndPhaseSkip` | 单调 + 逐阶；被拒的推进不得改动记录 |
| `TestRolloutFloorFirstAdvanceMustBeV3Write` | 首个 floor 只能是 v3-write |
| `TestRolloutControlAdvanceIsSingleWinnerUnderConcurrency` | 8 路并发只有 1 个赢，其余 `ErrRolloutControlChanged` |
| `TestRolloutControlMustNotExpire` | 带 TTL 的 floor 记录必须被拒 |
| `TestRolloutControlCorruptRecordFailsClosed` | 5 种损坏形态均 fail closed，不得当作"尚未初始化" |
| `TestMigrationApplyStaysGatedOnRevokeFloor` | apply 的 revoke 门禁——它是"完整 SCAN 周期足够"的前提 |

全部通过。这一组在实现后**必须仍然通过**。

## 结论

brief 中 5 条对现状的断言全部成立，另发现 1 条（C6，可达性待查）。
`pkg/auth` 全量测试在两组新测试存在时仍为 `ok`，说明断言不依赖任何测试污染。

---

# Verify 阶段（octospec-check）

对照注入的 rules（`context.yaml`）、brief 的 Acceptance 与 Out of scope 复核 diff，
自查出**两处问题，均已修复**。

## V1 — 我自己引入了一个 fail-open（已修）

`runtime.go` 在 `ResolveRolloutBoot` 报错时回落到 `expand`。看起来保守，实际是放松：
**`expand` 不检查 legacy deny marker**（`checksLegacyDeny` 要求 ≥ `v3-write`）。
于是一个 floor=`revoke` 的部署，遇到 Redis 抖动或 control 记录损坏时，
**已撤销的 legacy bearer 会重新可用**——正是这套机制要防的东西。

根因是我只把判定表用在了 `control == nil`，没用在 `control` 读取失败。两者应当同样处理：
**不可读 = 缺失，都由 marker 决定方向**（invariant 6：只能向上收敛）。

修复后：

| control | marker | 结果 |
|---|---|---|
| 不可读 | 存在 | `enforce` + 告警 |
| 不可读 | 缺失 | `expand`（从未初始化，无保护对象） |

回归测试：`TestUnreadableFloorResolvesUpwardNotToExpand`、
`TestUnreadableFloorOnFreshDeploymentStaysAtExpand`。

## V2 — Acceptance 有一条没实现（已补）

「observe 的 `decode_invalid` 与 migrate 的 `invalid` 计数相等」未做。

实现时发现一个连带的事实错误：**既有测试的 v1 fixture 用了 JSON**（`{"uid":"legacy"}`），
而 v1 的真实线格式是 `uid@name[@role]`（`decodeLegacy`）。生产实测也印证了这点——
测试环境 observe 报 v1=53，说明那 53 条都能被 `decodeLegacy` 解析。fixture 一直是错的，
只是旧的 Lua 无前缀就当 v1，从没暴露出来。

migrate 的 Lua 现在镜像 `decodeLegacy` 的判断（1~2 个 `@` 且 uid 非空），
不合格的记为 `invalid_payload` 并**跳过**——它不是可用凭证，也不再阻塞 floor，
让迁移去改一条它解析不了的记录没有任何收益。

回归测试：`TestObserveAndMigrateAgreeOnPayloadVersions`（四种 payload 逐项对齐）、
`TestMigrateSkipsUndecodablePayloads`。

## 规则复核

| rule | 结论 |
|---|---|
| `space-isolation`（load-bearing） | 无新增路由；registry payload 只有 build/pod/floor/时刻，无 UID，由 `TestWriterEntryCarriesNoUserData` 锁定 |
| `error-handling`（load-bearing） | 未新增任何 `c.ResponseError` / `c.JSON` / `AbortWithStatusJSON`；`ErrWriterLeaseLost` 汇入既有内部错误路径，`i18n-lint` 与 `TestUserNoLegacyResponseError` 均过 |
| `testing` | 迁移文件符合 `modules/<name>/sql/<yyyyMMdd>-<seq>_<name>.sql`；registry key 在各测试 `t.Cleanup` 中清理（`CleanAllTables` 不清 Redis） |
| `commit-style` | 英文 Conventional Commits |

Out of scope 逐条核对：`session_v3.go` 的改动只有 mode 读取动态化与两处 lease 门禁，
未触及 v3 payload、generation、issuance fence、有界索引、撤销矩阵；
migration 的 immutable cutoff、单占租约、`run_id` checkpoint、只缩短不延长、
`--confirm-elapsed-cutoff` 全部原样。

## 一个过程事故

实现中执行 `gofmt -w .` 波及了仓库里本就不合格式的 ~120 个无关文件。
已逐个 `git checkout` 还原，最终 diff 只含预期范围。**后续只用 `gofmt -w <具体文件>`。**

## 门禁结果

`go build ./...`、`go vet ./...`、`golangci-lint run ./...`（0 issues）、
`go test -race ./pkg/auth/`、`go test ./pkg/metrics/`、`go test ./modules/user/`（100s ok）、
`make i18n-extract-check`、`make i18n-lint` —— 全部通过。

`modules/oidc` 的 `TestRedisBindStore_Behavior_Integration` 与 `modules/common` 的
`TestManagerSystemSetting_GetReturnsSchemaAndMaskedSecrets` 失败，
已在 `git worktree` 的干净 HEAD 上复现，**与本次改动无关**。
