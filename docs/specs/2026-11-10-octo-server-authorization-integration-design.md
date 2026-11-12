# OCTO-266：octo-server 统一权限体系接入设计

状态：`/v2/cli` 已实施，审查修复的定向回归通过；11 项 `im.*` 权限已写入测试库，目标 Token 权限快照及各环境部署、跨服务链路仍需验证。

## 用户原始需求

1. “开始 OCTO-266，完成 octo-server 接入统一新权限体系的方案设计。”
2. “在隔离的 octo-server 仓库调查现有认证、授权与调用链；先读取 Loop 中 OCTO-266（任务 UUID bb680bd6-d8f7-4e02-bb18-d1b3f18a6853，工作空间 UUID ac3cc2e3-6ff9-44e1-bee8-85f04e56e6bb）的内容、评论及附件接口清单，并读取父任务 OCTO-261 的一期实施计划与技术方案，核实 Auth Server、Auth SDK 与开发者平台的现状和接入契约。结合实际代码厘清 Server 与其他项目的职责及依赖。”
3. “使用 spec-design 等适用技能，先形成方案并在子区内以最少问答补齐必须由 owner 决定的问题。此任务为方案设计，不预设直接实现或上线；不要假设权限规则、兼容策略、服务可用性或调用方接入现状。参考现有任务及附件证据，明确尚待确认的内容。若资料或仓库访问受限，说明具体阻塞，不绕过权限。用户已要求开始任务；按 Loop 规范推进 OCTO-266 的状态及记录方案进展，不改变负责人。”
4. “输出可审阅的 SDD：接入范围和目标、现有链路与改造位置、身份/Token 与权限判定语义、业务数据访问边界、接口及依赖、迁移与发布安排、错误和安全语义、测试与验收场景、风险与待决事项；结论有代码或方案依据，需 owner 决策的事项在子区提出，并将设计与进度记录到 OCTO-266。”
5. 后续补充：“分析链接先忽略”；“现在按照假设已经完全齐备进行设计，通讯主要依赖 octo-auth-sdk, 现在需要进行 spec-design”。
6. 后续选择：新旧凭证“并行兼容、单独切换”。
7. 后续要求：“需要尽可能不侵入现有业务逻辑，最好能只通过中间件就完美解决问题”。
8. 后续澄清：“我们直对接 octo-cli 设计到的部分接口……token 是独立了，不应该影响之前业务，必要的话可以单独开设接口”。
9. 后续补充：“理论上只需要 bot 身份，octo-cli 会改版，理论上用不到 bot token”。
10. 后续确认：按中间件优先、SDK 接入、旧凭证与旧注册接口保留、新版 CLI 获取 Bot 身份的整体设计方向推进；跨项目待确认契约在实施前闭合。
11. 后续补充：“设计及规划中的接口需要符合 openapi 的规范 https://codex.mlamp.cn/dmwork/cookbook/octo-openapi-dev-skill”；“当前接入有哪些问题？接入后具体的形式和流程是什么样的？对现有系统侵入性如何？有没有中间件调用对应 module handler的伪代码可以看下？”
12. 后续补充：“为什么看起来侵入了现有的业务逻辑？还要整理下新的接口映射，因为需要统一改为/v2/xxxxxx, 跟之前接口独立，不在共用 router”。
13. 后续要求：“原有接口octo-cli 有部分调用的 user, 有部分调用的 bot, 新版接口理论上都应该统一，且需要辨识哪些接口修改资源 owner 应该取用户，哪些应该有 bot 取 bot，没 Bot 取用户。接口前缀统一改为/v2/cli/xxxx”。
14. 后续选择：无 Bot 时仅支持真实用户等价能力；不存在等价能力的接口明确返回不支持，其他接口仍按用户执行。
15. 后续选择：新版 CLI 建群的群主/creator 固定为已验证的 Token 用户，绑定 Bot 可作为执行者。
16. 后续要求：“中间件和 router/handler 路由如何实现呢？”；“以上确定的内容先更新到 spec 中，然后回答我 : 中间件和 router/handler 路由如何实现呢？”
17. 后续要求：“ok，写入到 spec 中，列清楚 能复用的接口，不能复用需要侵入修改的接口(直接读 token、依赖旧参数/响应格式，或只有 Bot 主体但新版需要用户主体的接口。), 期待需要待确认的去确认下”。
18. 后续确认：“限制每个 token 最多绑定一个 bot，且这是 cli 的实现，我们不负责这部分”；子区 creator 取有效执行者，有可信 Bot 时为 Bot，无 Bot 时为用户，合法 OBO 时按授权主体。
19. 后续要求：“忽略 app bot ，不提供支持，openapi 规范需要支持 /v2, 可信Bot 身份契约就当完成了，这部分空实现/自行实现即可，等待 sdk 准备好再接入，参数/响应需要确认的，先单独在文档接口列表处标记，另行处理，独立 router 装配需要验证，当前只是设计，不改代码”。
20. 后续确认：群 Markdown 接口使用 `/groups/{group_id}/md`；群资料更新使用 `/groups/{group_id}/info`；新 API 路径应保持内部风格一致。
21. 后续确认：Token/API Key 仅在其绑定的 Space 内有效；Space 成员接口为 `GET /v2/cli/space/members`。
22. 后续确认：消息搜索始终基于 Token 用户，搜索不使用 OBO；其他能力分别按既有决策处理。
23. 后续确认（替代第 14、18、22 项中与主体及 OBO 冲突的内容）：“首先，不可能用户不在该群，且 token 如果有绑定 bot，则 bot 身份，如果操作了不支持bot 的接口，则是用户身份，如果没绑定 bot，则都认为是用户身份，且理论上用户可以操作所有，没有 OBO”。
24. 后续确认：“commands、events、heartbeat v2 不需要支持”。
25. 后续要求：“1.子区 markdown 也使用 /md
2.补充完善逐接口主体矩阵
3.参数和响应、v2 错误协议 给出一个统一规范即可，最好整体跟 octo-server 一致且符合 给出的 openapi 规范，只有业务数据不一定一样，但外层包装应该一致
4.权限 action 确认
5.文件访问边界 与旧版一致，原有业务逻辑不应该有任何更改
6.现在的 auth sdk 已经有了初版了，可以看下最新代码”。
26. 后续澄清：“我的目的是假设已经都完成了，先实现，具体等实际完成了再对接”。
27. 后续确认：v2 文件下载复用旧业务逻辑，作为该接口的 Space 隔离例外并在代码中注明。

## 目标与范围

为 CLI 使用的新业务 Token 接入统一身份与功能授权，同时保留 Server 自己的数据访问判断。主体范围为用户和 User Bot；新 `/v2/cli` 不支持 App Bot，旧 `/v1` 不变。接口边界以 OCTO-266 附件 `octo-307-octo-cli-server-api-inventory.md` 为工作清单：39 个声明操作、2 个动态全局搜索路径；还有 `uk_` 对应的 `/v1/user/messages/` 搜索变体。清单是 CLI 代码定义，不等于生产调用或授权范围；`octo-cli api` 任意路径需另做入口排查。Loop、Drive、Docs、Marketplace 由各自服务接入；本设计不改造其 API，不设计新 Auth Server/SDK 或开发者平台。

## 已核实现状

1. `main.go:253-275` 注入用户会话 parser；`pkg/auth/parser.go:133-164,189-225` 读取现有 token 并返回 UID、Name、Role、Language。`modules/botfather/api_user.go:36-69` 的 `uk_*` 和 `modules/bot_api/auth.go:25-156` 的 `bf_*`/`app_*` 各自认证，不走这个 parser。`pkg/authtree/authtree.go:1-26,48-104` 描述两个非 session 路由树。对 session parser 一处修改不覆盖所有 CLI 入口。
2. 搜索在 session、User Key 和 Bot 三类入口共享子树，先按 principal 校验频道，再过滤消息（`modules/messages_search/route_subtree.go:28-102`、`predicate.go:26-136`、`visibility.go:195-225`）；单消息路径另有保护（`modules/message/api.go:443-499`）。Bot OBO 搜索还有 grant、scope、grantor 频道可读检查（`modules/bot_api/search_route.go:42-155`、`modules/messages_search/obo.go:24-140`）。
3. `pkg/space/middleware.go:146-205` 的 SpaceMiddleware 为 opt-in，未带参数就跳过；会员正缓存 60 秒、负缓存 30 秒。`pkg/authtree/authtree.go:151-162` 指出绑定 Space 的 path/query/header 约束不覆盖 body。不能将 Space 参数当作已验证，也不能把功能权限当作跨 Space/频道授权。
4. Server 另有 Bot OBO Resolve：`pkg/obo/resolver.go:20-49,100-165` 为 `bf_` 解析 Actor/Subject 和委托；resource 不用于 resolver 的授权匹配。Bot Project 路由选 action 后仍由 Project service 判数据权限（`modules/project/api_bot_read.go:18-24,37-66,112-124`）。`docs/obo-resolve-rollout.md:37-58` 说明新增 action 可能扩展已有 ALL 授权；该机制不能直接等同于新 Auth Server。
5. `pkg/authtree/authtree.go:164-192` 明确用户 key 文件下载签名有 `ScopeUnscoped` 风险；不宣称接入 action 自动建立对象归属。搜索对旧索引的空 `Visibles` 视为无额外门（`modules/messages_search/visibility.go:149-165`）；实际索引状态未核实。

## 组件契约与职责

以 GitLab main 的代码和文档为准，部署和线上版本待核实：

- **Auth SDK**：已核对 GitLab main `93052c45585acf67743f8ebc80f3e8fb4e473867`，Go 模块 `codex.mlamp.cn/dmwork/octo-auth-sdk`。`sdk.NewClient(base, serviceSecret)` 后调用 `client.Auth.Resolve(ctx, businessToken, domains)`，再用 `permission.HasAction(access.Permissions, action)`。SDK 将服务 Secret 放 Authorization、业务 Token 放请求体，固定 POST `/v2/internal/tokens/_resolve`；`nil` domains 表示全部，非 nil 空列表无效。返回 `contract.AccessContext` 的 Principal、Permissions、BotBindings；精确或整字段通配、大小写敏感且查询 action 必须具体。默认 5 秒、不重定向、不缓存，内网 Service/私网 HTTP 可用并校验连接目标；错误或无效响应不放行。依据 SDK `README.md:13-62`、`auth/client.go:32-50,53-98`、`permission/match.go:30-44`。
  **版本复核**：远端 `main` 和最新发布标签 `v0.2.0` 均指向 `93052c45585acf67743f8ebc80f3e8fb4e473867`，与本仓库当前模块依赖一致；Auth Server 远端 main `cf41fb3073cf1b39ef9ec2a4d53f043c5d5d4571` 使用相同 SDK 契约。源码一致不代表服务已部署或真实联调通过。
- **Auth Server**：已核对 main `8f4ef89989025031b088ed26e2f9863c19e12a4c`。`authentication/service.go:41-55,80-98` 先读数据库 revision 校验，再解析身份、取权限目录；Principal 当前仍以用户为 subject，`BotBindings` 由身份解析提供。`service_identity.go:11-45` 校验 Token 无 Bot 则绑定为空、有 Bot 则绑定唯一且 ID 相同；`internal/app/identity.go:14-33` 调用 octo-server 并拒绝缺失或停用的已配置 Bot。Auth Server 期望 `POST /v1/internal/auth/identity`（`internal/clients/octoserver/client.go:18,67`）；本工作树目前在 `modules/user/api.go:434-435` 仅发现同组 `/resolve`，尚未发现该 identity 路由，需对齐对应 Server 版本及契约。以上为源码而非已部署实测。
  **跨服务待对齐**：Auth Server `docs/auth-server.md:47-68` 要求 identity 调用复用 `OCTO_MEMBERSHIP_INTERNAL_TOKEN`。本仓库 `modules/internal_membership/config.go:16-29,54-69` 对该 Token 还要求 peer 的缓存上限声明，且该凭证原用于 Project membership；本工作树尚未提供上述 identity 路由。不能仅配置同名 Secret 就视为对接完成或扩大其既有授权范围；需跨服务 owner 确认 identity 接口的独立权限边界、凭据与启用条件。
  **最新源码复核**：Auth Server main `cf41fb3073cf1b39ef9ec2a4d53f043c5d5d4571` 的 `docs/auth-server.md` 和 `internal/clients/octoserver/client.go` 仍要求上述 identity 路由，并通过 `X-Internal-Token` 发送 membership Secret。当前 octo-server main `c97b3ab922e90e450ea83d0b6852daf30b5f9559` 未提供该路由，因此真实双向联调尚有前置依赖。通过 `/auth` 网关访问时，SDK 的 base URL 需包含 `/auth`；直接访问 Auth Server 服务时使用服务根地址。
  **测试环境实测（2026-11-12）**：`dmwork-test/octo-auth-server-test` 为 2/2 Ready，Service `octo-auth-server-test:8097` 在集群内的 `/v2/health` 和 `/v2/ready` 均返回 `is_ready=true`，未带凭据请求 `/v2/internal/tokens/_resolve` 返回 401。测试环境 `dmworkim-develop` 的 `/v1/internal/auth/identity` 未带凭据返回 401，同级不存在路径返回 404；该部署与本工作树并非同一构建，尚未验证带服务凭据的身份解析。公网 `im-test.deepminer.com.cn/auth/v2/health` 与 `/auth/v2/internal/tokens/_resolve` 返回 404。当前测试 Server Deployment 及其引用的 ConfigMap 未列出本方案所用的 `OCTO_AUTH_SERVER_URL` 和 `OCTO_AUTH_SERVICE_SECRET` 键；不能据此声称 v2 已在该环境启用或联调通过。
- **开发者平台** `octo-developer-platform/docs/developer-backend.md`：负责权限目录、Token 生命周期和设备流程，验证现有用户会话并核实 Space 成员及角色；`AUTH_AUTOMATIC_FULL=true` 的代码策略是创建当时所有启用 action 的具体快照，配置及线上启用未知。其迁移负责共享表，Auth Server 是运行时读取方；不能由 Server 自行签发 Token 或填造权限。
- **octo-server** 负责受控路由/action 映射、从已认证 Principal 推导操作身份、核实当前资源与 Space/频道/成员权限、向客户端返回既有约定错误。SDK 的权限只是功能上限而不是资源级授权；CLI 不能作为安全执行点。

### v2 启动配置

`OCTO_AUTH_SERVER_URL` 和 `OCTO_AUTH_SERVICE_SECRET` 同时配置时装配独立 v2 router，并使用共享 UID 限流；两者均未配置时不启用 v2，原有入口继续工作。只配置一项或 SDK 配置无效时启动失败。凭据仅通过部署环境提供，不写入文档或日志；配置成功不代表 Auth Server、权限目录和 CLI 的实际联调已完成。

## 中间件优先的接入设计

新 Token/API Key 仅在绑定的一个 Space 内有效，Space 由经认证的 Principal 确定，不使用用户任意提供的 Space 覆盖 Principal。相关业务资源遵循相应既有数据访问规则；文件接口按已确认要求保留旧业务边界，旧文件路径未证明有 Space 归属校验，不能将 Token 绑定 Space 等同于文件对象隔离保证。Server 通过 octo-auth-sdk 获取可信主体、Token 权限和 Bot 绑定，按身份类型接入相应业务数据门；新主体需携带完整授权上下文。此约束适用于新 `/v2/cli`，旧凭证行为保持不变。
新业务 Token 只进入独立 `/v2/cli` 路由器：SDK 验证 → 服务端根据新路由选择 action → `HasAction` → 解析可信用户及可选 Bot → 独立入口适配并调用经过验证的既有业务能力。旧 `/v1` 路由、认证链、行为和凭证不变；不同版本由入口或网关明确分发，不通过认证失败尝试另一版本。独立路由器如何与现有 HTTP engine / 网关接入待实施时核实，不能只更换 Gin 的 `Group` 名称并称作独立 router。

`/v2` 入口优先复用已运行 module 实例持有的业务能力，而非新建会注册全局 provider 或监听器的 module 实例（见 `pkg/authtree/authtree.go:10-24`）。现有 handler 多为未导出方法且依赖旧上下文、旧路径参数与响应格式；只有调用入口、上下文、参数、响应和权限检查都兼容时才能复用 handler。否则通过所属 module 的最小适配或抽取原 service 能力接入；不声称所有 handler 可原样透传，也不得绕过资源权限门。

### 独立 v2 路由伪代码

以下为设计示意，`v2` 与 `v1` 是独立 router；SDK 提供 Bot 绑定结果，真实身份上游需联调，实际 action 由权限目录确定。此示意不是可编译代码；`messageAdapter.Send` 在 module 边界复用既有业务能力，不以旧 router 二次分发代替授权。

```go
v1Router := buildExistingV1Router()  // 保持现有路由和认证链
v2Router := buildIndependentV2Router()

v2Router.Use(sdkAuthenticate)        // 仅新 Token
v2Router.Use(routeActionGate)        // 可信 method + 路由映射

v2Router.POST("/v2/cli/messages",    // 网关保留完整路径
    trustedActorContext,            // 按路由支持的主体选择可信 Bot 或用户
    existingResourceGuards,
    messageAdapter.Send,            // 同一 module 实例的业务能力
)

gateway.Route("/v1/*", v1Router)
gateway.Route("/v2/*", v2Router)
```

`existingResourceGuards` 是必须逐路由核实并保留的业务门，而不是当前已存在的通用中间件名。对于发送消息，现有 `sendMessage` 在 handler 内解析 body 并调用 `checkSendPermission`、`checkOBO`（`modules/bot_api/send.go:53-70,160-185,227-243`）；v2 需保留适用的频道权限检查，不能只把 SDK action 放行当作这些检查，也不接入 OBO。对于采用 `{group_id}` 而原 handler 读取 `{group_no}` 的路由，适配器须显式验证并转换参数；规范响应格式也不能通过直接调用旧 handler 假定兼容。

### 独立 router 的装配和执行顺序

当前 `main.go:242-245,533-534` 通过 `server.New(ctx)` 获得并保存一个 `WKHttp`，再调用一次 `module.Setup(ctx)`；依赖 `octo-lib/module/module.go:16-50` 为每个 module 构造 API 并对 `ctx.GetHttpRoute()` 调一次 `Route`。`server.Server` 内部持有 `WKHttp` 并调用其 `Run`，没有已核实的公共 handler 替换入口（依赖 `server/server.go:18-39,51-82,136-138`）。`wkhttp.WKHttp` 确实实现 `ServeHTTP`，但这不意味着可以直接把第二个 engine 塞进现有 server。

推荐的独立装配方案：同一进程只执行一次 module 初始化和迁移；创建一个独立的 v2 `WKHttp`，通过独立入口或网关将 `/v2/cli/*` 转发到它，保留完整请求路径。module 的现有实例在自身装配阶段将选定的可复用能力贡献到 v2 路由注册表；v2 启动时装配这些回调。注册表绑定应用实例/装配上下文，不通过再次 `module.Setup` 创建第二套 module；`pkg/authtree` 的“module 贡献绑定 handler、挂载方负责认证”的模式可参考，但现有表以 `(tree, *WKHttp)` 为键、`Add` 要求与注册方使用相同 router，不能把 v1 的 key 换成 v2 后假设回调自动跨 router 出现（`pkg/authtree/authtree.go:28-32,353-385`）。

**装配验证（设计阶段）**：使用当前 `octo-lib` 版本，在仓库外临时 Go 程序中创建两个 `wkhttp.New()`，经 `net/http.ServeMux` 按 `/v1/` 与 `/v2/` 保留完整路径分发；分别安装标识 middleware，v2 通过嵌入的 Gin `RouterGroup.PATCH` 配合 `WKHttpHandler` 注册 PATCH。`httptest` 验证 v1 GET 和 v2 PATCH 命中对应 engine 与 middleware，反向路径/错误方法返回 404，四个场景通过。临时程序已删除，未修改业务代码。

这只证明两个 `WKHttp` 作为 `http.Handler` 可隔离分发以及 PATCH 桥接可工作，不证明现有 `server.Server` 已支持这个装配：当前 `Server` 持有私有 `r` 并直接调用 `s.r.Run`（`octo-lib/server/server.go:18-39,51-96`），`module.Setup` 只把 API 安装到 `ctx.GetHttpRoute()`，并执行迁移（`octo-lib/module/module.go:16-50`）。实际入口、模块实例贡献方式、公共 middleware、服务关闭与部署转发仍需在实施时集成验证；不能用第二次 `module.Setup` 代替，也不能声称当前服务已支持双 router。

v2 需要独立装配公共能力：入口限流、trace、恢复、CORS、日志脱敏和错误渲染；v1 上的全局 middleware 不会自动作用到独立 v2 engine。身份与权限流程为：边界提取新 Token → `client.Auth.Resolve` → 确认 Token owner/Space 和可信 Bot → 建立请求级 principal → 身份维度限流 → 由路由注册项携带服务端 action 并调用 `permission.HasAction` → 适配器和对应业务权限门 → handler/service。认证/绑定/action 失败中止；只有可信契约确认无 Bot 才能按用户执行，依赖故障或 Bot 验证失败不能降级。

路由注册应是显式清单，每项包含 method、相对路径、具体 action、主体策略、所需资源 guard 和绑定到已有实例的 handler/adapter；未声明 action 不暴露为授权入口。新 v2 参数名及 OpenAPI envelope 与旧 handler 不一致时，由 v2 适配器完成请求/响应转换；绝不通过回调 v1 HTTP 路由伪造 Bearer 或跳过旧 guard。现有 `authtree.MountOn` 只分发 GET/POST/PUT/DELETE（`pkg/authtree/authtree.go:420-430`），而候选表包含 PATCH，因此不能假定现有 mounter 能直接承载全部 v2 方法。

示例：群创建的 v2 adapter 将可信 `principal.OwnerID` 作为 `group.CreateGroupServiceReq.Creator`，将已验证绑定的 User Bot ID 作为 `BotUID`（无 Bot 时为空），同时使用可信 Space；调用现有 group service，并保留输入、成员准入、Space 和通知/IM 等原有业务语义。当前 `modules/bot_api/groups.go:471-501` 在 handler 里还会处理人类成员和 creator，因此直接跳到 service 前必须迁移或复用对应检查，不能只调用 `CreateGroup` 就认为语义完整（service 的 Space 检查见 `modules/group/service.go:1245-1265`）。

### 统一主体和资源归属

SDK 认证成功后保留 `user`（Token owner）和 `bot`（可信绑定的可选 User Bot）。每条路由声明是否支持 Bot 主体：存在可信绑定且该接口支持 Bot 时有效执行者 E = Bot；接口不支持 Bot 时 E = 用户；确认没有绑定 Bot 时所有接口 E = 用户。主体由服务端按路由策略选择，不由客户端任意指定，也不是 Bot 权限不足时改用用户重试。消息搜索属于用户主体接口，因此无论有无 Bot 均使用 U。

每个 Token 最多绑定一个 Bot，此限制由 CLI 实现；新 v2 不支持 App Bot。绑定无效、身份或权限服务失败、Bot 不存在/不可用时拒绝，不能当成“无 Bot”回退用户。Token 功能权限和选定主体的资源权限独立判定；不从客户端提交的 UID/owner 获取可信身份。v2 不提供或调用 OBO，不接受调用方通过委托参数选择其他主体；旧 v1 的 OBO 行为不变。

资源 owner 不一律等于执行者：创建群的群主/creator 固定为 Token 用户；子区 creator、消息发送者、子区加入者及已读主体按 E。新写操作不隐式转移已有资源 owner。其余逐接口的 Bot 主体支持情况及 owner 分类须逐项核实；查看类操作和签名 URL 不应凭空假设存在资源 owner。

目标是用户主体理论上可操作 v2 范围内的所有接口，仍须满足 Token action 和资源权限；不能把用户 UID 冒充 robot。commands、events（包括 poll 和 ack）、heartbeat 不纳入 v2，既有 v1 接口保持不变。

### 侵入边界

| 位置 | 预计改动 | 约束 |
| --- | --- | --- |
| 装配与认证 | 新建独立 v2 router、SDK client、身份适配及 action 映射 | 旧 v1 router 和既有 session、User Key、Bot 认证链不变；版本由入口或网关分发。 |
| 原业务 handler / service | 优先复用现有 handler 和资源校验 | 只有身份上下文和 body/资源归属等价得到证明才可不改；逐路由验收，无法预先保证全部零侵入。 |
| CLI 身份解析 | 返回经验证的用户及可选 Bot 身份 | 原 `/v1/bot/register` 及其 Bot/IM token 行为不变；新接口不返回旧 token，也不重置 IM token。 |
| 搜索 | 复用搜索 handler、用户可读频道和消息过滤 | v2 始终使用 Token 用户及绑定 Space；单频道、全局搜索共用用户权限语义。 |
| 主体选择 | 路由声明 Bot 支持情况并选取可信主体 | v2 不提供 OBO；旧 v1 授权及调用行为不变。 |
| 旧调用方与数据 | 保持现有凭证、响应和限流语义 | v1/v2 由版本入口分流，不发生认证失败后跨版本回退；新旧凭证在各自入口独立验证。 |

### OpenAPI 规范与验收

设计及实施中的新增或变更接口必须遵守 [octo-openapi-dev-skill](https://codex.mlamp.cn/dmwork/cookbook/octo-openapi-dev-skill)（核对 main `46f9dd999c8fa6213918bf523db4a8351052df46`）：

- OpenAPI 3.1（R9）；资源路径、snake_case、路径参数和 `operationId` 按 R6/R7/R10。**目标规范须支持 `/v2`**；当前已查规范 R12 仍仅允许 `/v1`，规范更新及 v2 的 servers/basePath、生成和 lint 方案须由规范维护方落实，不能将现有规则描述为已更新。swag `@Router` 采用相对版本的路径，避免重复版本前缀。
- 新接口的 2xx 顶层 `data`、4xx/5xx 顶层 `error`（R1）；错误按 R2 枚举、结构化 details，鉴权接口声明 401/403（R4）；字段命名与时间格式按 R3/R8。完整 swag 注释和安全声明遵循 R13；分页、批量按适用的 R5/R11。
- 旧接口保持现有客户端 wire 契约；不以重命名路由或改变响应来让现有接口形式上通过规范。新增身份接口的具体 URL、operationId、字段、鉴权与返回模式在 Server/CLI 契约对齐时按上述规则定稿，并以实际返回与 OpenAPI 一致为准。
- 仓库的 `pkg/httperr/respond.go` 默认固定 400，`pkg/i18n/renderer.go` 输出本地化内部 code 及兼容字段，不能直接满足 R2 的固定 enum、英文 message 和语义状态码。v2 使用下述独立协议适配；真实状态码按仓库规则需维护者批准，v1 不变。
- 仓库已增加针对独立 v2 文档的 `make openapi-check`；实际执行结果以验收记录为准。修改现有契约时还需执行兼容性 diff；当前尚无 `make openapi-diff` 目标。
### v2 统一请求和响应协议

以下是所有 v2 接口共用的外层协议；各接口的业务字段仍见下方清单另行对齐。沿用 octo-server 的注册错误码、i18n 信息与脱敏规则作为内部错误来源，v2 边界负责转换为 OpenAPI R1/R2；不改变 v1 的渲染与客户端协议。

- 请求：Bearer 新 Token；path 参数用 `{resource_id}`，query/body 字段使用 snake_case；JSON body 为业务对象而非额外 envelope，文件上传使用 multipart。布尔字段 `is_`/`has_`/`can_`，时间 `_at` 且 RFC3339，URL `_url`。列表按 R5 选择 cursor（`cursor`、`page_size`）或 offset（`page`、`page_size`），默认 20、最大 100；每个操作注明选择。
- 成功 JSON：`{"data": <业务对象或数组>}`；无业务返回值用 `{"data": {}}`。分页在顶层增加 `pagination`：cursor 为 `{"has_more": true, "next_cursor": "..."}`，offset 为 `{"total": 0, "page": 1, "page_size": 20}`。业务字段由具体接口定义，不以不同主体改变外层。
- 失败 JSON：`{"error":{"code":"FORBIDDEN","message":"Access denied","details":{}}}`；`hint` 可选，`details` 为对象且仅含可公开字段。`code` 仅取 R2 的 12 项：`AUTH_REQUIRED`、`FORBIDDEN`、`NOT_FOUND`、`CONFLICT`、`DUPLICATE`、`VALIDATION_ERROR`、`PAYLOAD_TOO_LARGE`、`UNSUPPORTED_MEDIA_TYPE`、`CLIENT_VERSION_TOO_OLD`、`RATE_LIMITED`、`INTERNAL_ERROR`、`UPSTREAM_UNAVAILABLE`。分别使用规范对应的 401/403/404/409/400/413/415/426/429/500/503；内部业务 code 与安全可见详情映射到固定 enum 和 details，不能把所有 4xx 当 400，5xx 不泄露内部详情。
- R2 要求服务端 message 英文；v2 输出英文安全消息，内部仍可保留 octo-server 的注册 code/i18n 分类。当前 `ErrorRenderer` 会输出本地化 message、`err.*` code、`error.http_status` 和顶层 `msg/status`（`pkg/i18n/renderer.go:27-65`），不是本协议的直接实现；须做 v2 专用适配并取得真实状态码维护者批准，不能直接宣称现有 facade 已合规。
- 下载沿用旧逻辑的 HTTP 302 + `Location`，不包装为 2xx 的 `data`；实际 GET 重定向响应的 Content-Type 为 `text/html`。下载 handler 的错误使用统一 JSON error，其他 JSON 接口不使用 302 代替成功 envelope。


CLI 调用边界：附件第 9 行和 `octo-cli` 的 `internal/registry/specs/bot.json` 明确包含 `POST /v1/bot/register` 及 `im_token` 响应；`octo-cli/internal/cmdutil/factory.go:450-495` 还在 Bot 身份解析时调用该接口，但此调用仅消费 `robot_id`。这证明 CLI 声明和身份解析涉及 register，不证明线上实际调用量或所有调用方都不需要 IM token。

现有 register handler 直接解析旧 Bearer、查 Bot token，并把该 token 用作 IM token 返回（`modules/bot_api/register.go:303-335,371-379`）。保持原接口及 IM token 行为不变，为新 Token 提供独立身份解析接口；CLI 身份解析使用该接口，返回用户及可选可信 Bot，不返回旧 Bot token、不调用 UpdateIMToken。CLI 改版中确认新命令是否存在 IM 连接需求；若需求改变，须重新设计隔离凭证，不能假设现有 IM 能力。

## CLI 接口到 v2 的映射

来源为 OCTO-266 附件及后续确认。下表是 `/v2/cli` 的实施目标；具体实现与验收以代码和测试为准。`U` = 经 SDK 验证的 Token 用户；`B` = 经可信绑定验证的 User Bot（不支持 App Bot）；`E` = 有 B 且接口支持 Bot 时取 B，否则取 U；`—` = 无新建 owner。owner、actor 和数据访问权限分别判断；参数、业务数据和响应按统一 v2 协议与现有业务能力适配。

| 现有接口 | v2 接口 | 主体 / owner |
| --- | --- | --- |
| POST `/v1/bot/register` | GET `/v2/cli/identity` | U 及可选 B；仅返回可信身份，不返回旧 Bot/IM token。 |
| GET `/v1/bot/user/info` | GET `/v2/cli/users/{user_id}` | E 访问；—。 |
| GET `/v1/bot/space/members` | GET `/v2/cli/space/members` | E 访问 Token/API Key 绑定的 Space；—。 |
| POST `/v1/bot/typing` | POST `/v2/cli/typing` | E 为输入主体；用户对应 `/v1/message/typing`，参数与响应待确认。 |
| POST `/v1/bot/sendMessage` | POST `/v2/cli/messages` | E 为发送者。 |
| POST `/v1/bot/message/edit` | PATCH `/v2/cli/messages/{message_id}` | E 操作；按原消息身份及权限判断，不转移作者。 |
| POST `/v1/bot/messages/sync` | POST `/v2/cli/messages/_sync` | E 决定可见消息；—。 |
| POST `/v1/bot/readReceipt` | POST `/v2/cli/messages/_read_receipt` | E 为已读主体；不改消息 owner。 |
| POST `/v1/bot/messages/_search` | POST `/v2/cli/messages/_search` | U 的会话可见性；—。 |
| POST `/v1/bot/messages/_search_all` | POST `/v2/cli/messages/_search_all` | U 的消息/文件可见性；—。 |
| POST `/v1/bot/messages/_search_files` | POST `/v2/cli/messages/_search_files` | U 的文件可见性；—。 |
| POST `/v1/bot/messages/_search_media` | POST `/v2/cli/messages/_search_media` | U 的媒体可见性；—。 |
| POST `/v1/bot/messages/_search_around` | POST `/v2/cli/messages/_search_around` | U 的频道及上下文权限；—。 |
| POST `/v1/bot/messages/_search_global_groups` | POST `/v2/cli/messages/_search_global_groups` | U 的可读会话范围；—。 |
| POST `/v1/bot/messages/_search_global_messages` | POST `/v2/cli/messages/_search_global_messages` | U 的全局搜索可见性；—。 |
| POST `/v1/bot/messages/_search_global_files` | POST `/v2/cli/messages/_search_global_files` | U 的全局文件可见性；—。 |
| GET `/v1/bot/groups` | GET `/v2/cli/groups` | E 的群可见性；—。 |
| GET `/v1/bot/groups/{group_no}` | GET `/v2/cli/groups/{group_id}` | E 访问；—。 |
| GET `/v1/bot/groups/{group_no}/members` | GET `/v2/cli/groups/{group_id}/members` | E 群成员权限；—。 |
| GET `/v1/bot/groups/{group_no}/md` | GET `/v2/cli/groups/{group_id}/md` | E 访问；—。 |
| PUT `/v1/bot/groups/{group_no}/md` | PUT `/v2/cli/groups/{group_id}/md` | E 操作并校验写权限；不转移群 owner。 |
| POST `/v1/bot/createGroup` | POST `/v2/cli/groups` | 已确认群 creator/owner = U，B 为可选执行 Bot。 |
| PUT `/v1/bot/groups/{group_no}/info` | PATCH `/v2/cli/groups/{group_id}/info` | E 操作并校验管理权限；不转移群 owner。 |
| POST `/v1/bot/groups/{group_no}/members/add` | POST `/v2/cli/groups/{group_id}/members/_add` | E 操作；批量语义和 R11 待确认。 |
| POST `/v1/bot/groups/{group_no}/members/remove` | POST `/v2/cli/groups/{group_id}/members/_remove` | E 操作；不改变现有群 owner。 |
| POST `/v1/bot/groups/{group_no}/threads` | POST `/v2/cli/groups/{group_id}/threads` | E 为 creator；父群访问门。 |
| GET `/v1/bot/groups/{group_no}/threads` | GET `/v2/cli/groups/{group_id}/threads` | E 访问；—。 |
| GET `/v1/bot/groups/{group_no}/threads/{short_id}` | GET `/v2/cli/groups/{group_id}/threads/{thread_id}` | E 访问；—。 |
| GET `/v1/bot/groups/{group_no}/threads/{short_id}/members` | GET `/v2/cli/groups/{group_id}/threads/{thread_id}/members` | E 访问；—。 |
| POST `/v1/bot/groups/{group_no}/threads/{short_id}/join` | POST `/v2/cli/groups/{group_id}/threads/{thread_id}/join` | E 本人加入；不更改原 creator。 |
| POST `/v1/bot/groups/{group_no}/threads/{short_id}/leave` | POST `/v2/cli/groups/{group_id}/threads/{thread_id}/leave` | E 本人离开；不更改原 creator。 |
| GET `/v1/bot/groups/{group_no}/threads/{short_id}/md` | GET `/v2/cli/groups/{group_id}/threads/{thread_id}/md` | E 访问；—。 |
| PUT `/v1/bot/groups/{group_no}/threads/{short_id}/md` | PUT `/v2/cli/groups/{group_id}/threads/{thread_id}/md` | E 操作并校验写权限；不转移 owner。 |
| POST `/v1/bot/file/upload` | POST `/v2/cli/files` | E 操作；现有对象上传未证明持久化 owner。 |
| GET `/v1/bot/file/download/{path}` | GET `/v2/cli/files/_download` | E 操作；沿用旧对象访问边界和 302。 |
| GET `/v1/bot/upload/credentials` | GET `/v2/cli/uploads/credentials` | E 操作；临时凭证范围待核实；—。 |
| GET `/v1/bot/upload/presigned` | GET `/v2/cli/uploads/_presigned` | E 操作；签名不等于对象 owner。 |

### 逐接口主体矩阵

`B/U` 表示有已验证 B 使用 B，无 B 使用 U；`U/U` 表示无论是否绑定均使用 U。涉及同一行的每个 method/path 分别按该规则注册；`identity` 返回两种可信身份，不创建业务 owner。群 creator 是明确的 U 例外。所选主体仍受具体资源权限约束；旧用户/Bot handler 的请求、响应、权限并不总是等价，适配依据见下方逐接口复用表。

| v2 接口（省略 `/v2/cli`） | 有 B / 无 B | 作用主体及 owner |
| --- | --- | --- |
| GET `/identity` | U+可选 B / U | 返回已验证身份与绑定 Space。 |
| GET `/users/{user_id}`；GET `/space/members` | B/U | 查询主体 B 或 U；无新 owner。 |
| POST `/typing` | B/U | 输入状态发送主体 B 或 U。 |
| POST `/messages` | B/U | 消息发送者 B 或 U。 |
| PATCH `/messages/{message_id}` | B/U | 编辑者 B 或 U，原作者不变。 |
| POST `/messages/_sync`；POST `/messages/_read_receipt` | B/U | 分别为同步读取主体、已读记录主体。 |
| POST `/messages/_search`；`/_search_all`；`/_search_files`；`/_search_media`；`/_search_around`；`/_search_global_groups`；`/_search_global_messages`；`/_search_global_files` | U/U | 每条路径前缀均为 `/messages`；按 U 的频道和消息可见性搜索。 |
| GET `/groups`；GET `/groups/{group_id}`；GET `/groups/{group_id}/members`；GET `/groups/{group_id}/md` | B/U | 按 B 或 U 的群访问权限查询；不改变群 owner。 |
| PUT `/groups/{group_id}/md`；PATCH `/groups/{group_id}/info` | B/U | 操作者和 Markdown 更新者为 B 或 U；原群 owner 不变。 |
| POST `/groups` | B/U | 允许 B 作为执行 Bot；群 creator/owner 固定 U。 |
| POST `/groups/{group_id}/members/_add`；POST `/groups/{group_id}/members/_remove` | B/U | 成员管理操作者 B 或 U；原群 owner 不变。 |
| POST `/groups/{group_id}/threads` | B/U | 子区 creator 为 B 或 U。 |
| GET `/groups/{group_id}/threads`；GET `/groups/{group_id}/threads/{thread_id}`；GET `/groups/{group_id}/threads/{thread_id}/members`；GET `/groups/{group_id}/threads/{thread_id}/md` | B/U | 按 B 或 U 校验父群并查询；不改变子区 creator。 |
| POST `/groups/{group_id}/threads/{thread_id}/join`；POST `/groups/{group_id}/threads/{thread_id}/leave` | B/U | 加入或离开者 B 或 U；creator 不变。 |
| PUT `/groups/{group_id}/threads/{thread_id}/md` | B/U | 更新者 B 或 U；creator 不变。 |
| POST `/files`；GET `/files/_download`；GET `/uploads/credentials`；GET `/uploads/_presigned` | B/U | 按 B 或 U 路径复用旧文件业务；未证明持久化文件 owner 或 Space 归属。 |

确认依据：旧 Bot 实现及会话用户实现见下方逐接口表；消息搜索固定 U 和群/子区 creator 是已确认产品规则。上述 B/U 为新 v2 目标策略，并不声称旧 Bot handler 已能直接接受 U 或两种旧业务语义完全等价；具体 wire 数据仍需适配。

### 权限 action 设计

11 项 `im.*` 权限已写入测试库。目录写入不代表目标 Token 的 action 快照已包含这些权限，也不代表相关服务已部署或端到端判权已验证。每条 `/v2/cli` 路由固定一个具体 action，并继续直接调用 `octo-auth-sdk/permission.HasAction`；业务资源权限仍由既有业务路径检查。

以下路径均以 `/v2/cli` 为前缀；同一行列出的接口使用同一 action。`im.groups:write` 为 `high`，其他 action 为 `normal`。

| action | 接口范围 | 分级 |
| --- | --- | --- |
| `im.identity:read` | GET `/identity` | normal |
| `im.users:read` | GET `/users/{user_id}` | normal |
| `im.spaces:read` | GET `/space/members` | normal |
| `im.messages:read` | POST `/messages/_sync`、POST `/messages/_read_receipt`；POST `/messages/_search`、`/_search_all`、`/_search_files`、`/_search_media`、`/_search_around`、`/_search_global_groups`、`/_search_global_messages`、`/_search_global_files` | normal |
| `im.messages:write` | POST `/typing`、POST `/messages`、PATCH `/messages/{message_id}` | normal |
| `im.groups:read` | GET `/groups`、GET `/groups/{group_id}`、GET `/groups/{group_id}/members`、GET `/groups/{group_id}/md` | normal |
| `im.groups:write` | POST `/groups`、PATCH `/groups/{group_id}/info`、PUT `/groups/{group_id}/md`、POST `/groups/{group_id}/members/_add`、POST `/groups/{group_id}/members/_remove` | **high** |
| `im.threads:read` | GET `/groups/{group_id}/threads`、GET `/groups/{group_id}/threads/{thread_id}`、GET `/groups/{group_id}/threads/{thread_id}/members`、GET `/groups/{group_id}/threads/{thread_id}/md` | normal |
| `im.threads:write` | POST `/groups/{group_id}/threads`、POST `/groups/{group_id}/threads/{thread_id}/join`、POST `/groups/{group_id}/threads/{thread_id}/leave`、PUT `/groups/{group_id}/threads/{thread_id}/md` | normal |
| `im.files:read` | GET `/files/_download` | normal |
| `im.files:write` | POST `/files`、GET `/uploads/credentials`、GET `/uploads/_presigned` | normal |

`im.groups:write` 的 high 范围覆盖全部群写操作：创建群、更新群信息或 Markdown、添加成员和移除成员。由于 read/write 粒度合并，不能只对移除成员单独设 high；获授该 action 的 Token 同时具备上述全部群写功能。其余 action 使用 normal。

消息搜索统一使用 `im.messages:read`，包括全局群/文件搜索；已读回执也归 `read`。上传凭证和预签名 URL 虽为 GET，仍使用 `im.files:write`；下载沿用既有旧业务路径及其 Space 隔离例外，使用 `im.files:read`。


原用户搜索的八条路径单列如下；它们与上述 Bot 搜索项映射至同一 `/v2/cli/messages/` 路径。无论是否绑定 Bot，v2 搜索均以可信 Token 用户 U 和绑定 Space 作为数据权限主体。

| 原用户搜索接口（POST） | 候选统一接口（POST） |
| --- | --- |
| `/v1/user/messages/_search` | `/v2/cli/messages/_search` |
| `/v1/user/messages/_search_all` | `/v2/cli/messages/_search_all` |
| `/v1/user/messages/_search_files` | `/v2/cli/messages/_search_files` |
| `/v1/user/messages/_search_media` | `/v2/cli/messages/_search_media` |
| `/v1/user/messages/_search_around` | `/v2/cli/messages/_search_around` |
| `/v1/user/messages/_search_global_groups` | `/v2/cli/messages/_search_global_groups` |
| `/v1/user/messages/_search_global_messages` | `/v2/cli/messages/_search_global_messages` |
| `/v1/user/messages/_search_global_files` | `/v2/cli/messages/_search_global_files` |

`_search`/`_search_all` 无会话时动态映射全局消息，`_search_files` 无会话时映射全局文件。`octo-cli api` 的任意路径不属于可穷举清单。目标 OpenAPI 规范需支持 `/v2`。

**路径风格（评审原则）**：统一 `/v2/cli` 前缀；资源集合用复数名词，标识符用 `{resource_id}`；`/space` 表示凭证绑定的唯一 Space 作用域，`/space/members` 中的 `members` 是资源集合；群和子区 Markdown 子资源均使用 `/md`；群资料更新范围限定为 `/info`。CRUD 优先用 HTTP 动词，非 CRUD 操作遵循同一动作命名规则。现行 OpenAPI R6 对非 CRUD 动作采用 `_action`、状态机动词使用普通动词；现有候选中的 search、sync、join/leave、批量成员操作尚需按该分类统一评审，不能只依据名称改变业务语义。

### 接口参数与响应待确认清单

状态：外层统一遵循上述 v2 协议；下列业务字段、各操作错误集合与 `operationId` 仍需逐项对齐。路径以 `/v2/cli` 为共同前缀；同一格中的每个 method/path 都是独立操作。

| 候选接口 | 待确认的参数与响应 |
| --- | --- |
| GET `/identity` | 用户和可选 User Bot 身份字段、无 Bot 的表示、Space 字段。 |
| GET `/users/{user_id}` | user ID、可见字段、用户不存在及跨 Space 的响应。 |
| GET `/space/members` | keyword、分页方式、字段与总数；Space 取已验证凭证的绑定值。 |
| POST `/typing` | 频道和成功响应。 |
| POST `/messages` | 用户/Bot 发送 body、payload 与消息 ID/序列响应。 |
| PATCH `/messages/{message_id}` | 路径 ID 与消息序列、卡片编辑参数和成功响应。 |
| POST `/messages/_sync`；POST `/messages/_read_receipt` | 频道标识、序列/分页、批量消息 ID 及对应结果。 |
| POST `/messages/_search`、`/_search_all`、`/_search_files`、`/_search_media`、`/_search_around`、`/_search_global_groups`、`/_search_global_messages`、`/_search_global_files` | 后七项的前缀同为 `/messages`；分别确认频道/过滤条件、排序、分页、around 窗口及全局结果结构；用户与 Bot 同一 wire contract。 |
| GET `/groups`；GET `/groups/{group_id}`；GET `/groups/{group_id}/members` | 群可见集合、返回字段、成员过滤/分页与分页元数据。 |
| GET、PUT `/groups/{group_id}/md` | content、version、updated_at/updated_by 及冲突响应。 |
| POST `/groups`；PATCH `/groups/{group_id}/info` | 群名、成员、Space 等请求与群信息响应；creator 不由客户端指定。 |
| POST `/groups/{group_id}/members/_add`；POST `/groups/{group_id}/members/_remove` | 成员 ID 列表、跳过/失败结果、批量原子性及 R11 适配。 |
| POST、GET `/groups/{group_id}/threads` | 创建名称/source message 与 creator 响应；列表 status、分页与返回。 |
| GET `/groups/{group_id}/threads/{thread_id}`；GET `/groups/{group_id}/threads/{thread_id}/members` | thread ID、详情及个人状态字段、成员列表和分页。 |
| POST `/groups/{group_id}/threads/{thread_id}/join`；POST `/groups/{group_id}/threads/{thread_id}/leave` | 成功响应、幂等及身份/状态错误。 |
| GET、PUT `/groups/{group_id}/threads/{thread_id}/md` | content、version、写入者字段与冲突响应。 |
| POST `/files`；GET `/files/_download` | multipart 字段、上传结果、下载对象引用与错误；下载保持旧 302。 |
| GET `/uploads/credentials`；GET `/uploads/_presigned` | 文件名、类型、大小、凭证范围、响应字段及过期时间。 |

### 逐接口复用与改动清单

本表说的是实现边界，不是已完成的复用验证。`适配` 指新增 v2 请求/响应、可信身份及数据门接线，尽量复用现有业务能力；不等于可直接调用旧 HTTP handler。`业务改动` 指旧 handler 内的主体、权限或副作用必须抽取/改造，或现有能力不足。由于 v2 要求新 principal 和 R1 响应，**不能仅因旧 handler 功能相似就认定可原样挂载**；旧 handler 返回响应后也不能假定可安全补写 envelope。表中的接口对应上方逐行映射；最终能否原样复用需实际运行验证。

| 原接口（方法及路径） | 可复用的能力 | v2 必需适配 / 业务改动与用户主体 |
| --- | --- | --- |
| POST `/v1/bot/register` | 无旧 register handler 可直接复用；可信身份来自 SDK 和 Bot 绑定 | 新身份接口。旧 handler 直接读 Bearer、按旧 token 查表、重置 IM token 并返回旧凭证；必须独立实现，不调用该 handler（`modules/bot_api/register.go:303-335,371-379`）。 |
| GET `/v1/bot/user/info` | 用户资料查询能力 | Bot handler 是全局 UID 查询，用户详情有共同 Space 等权限门；v2 需绑定 Space 和用户/Bot 数据权限并适配响应，不能原样透传（`modules/bot_api/commands.go:67-89`、`modules/user/api.go:1276-1335`）。 |
| POST `/v1/bot/typing` | Bot typing 和用户 `/v1/message/typing` | 两条 handler 分别读旧 Bot context 和用户 `uid`/`name`；适配可信主体、请求和响应，并保留所选主体适用的频道及好友门（`modules/bot_api/typing.go:19-85`、`modules/message/api.go:1571-1619`）。 |
| GET `/v1/bot/space/members` | Space 成员查询能力 | 旧 handler 取 Bot 身份并核对成员资格；v2 分别按可信 Bot/用户和可信 Space 执行成员门，适配分页与响应；不能采用旧接口省略 Space 时选择第一个 Space 的逻辑（`modules/bot_api/groups.go:293-375`）。 |
| POST `/v1/bot/sendMessage` | Bot 发送与用户发送各自的权限和业务能力 | 两者字段和响应不同；用户 `/v1/message/send` handler 直接解析 body token，不能将新 token 塞入旧字段，需抽出以可信 actor 调用的业务路径。保留适用的频道权限检查（`modules/bot_api/send.go:169-243`、`modules/message/api.go:505-535`）。 |
| POST `/v1/bot/message/edit` | Bot 和用户各自的消息编辑业务 | 适配路径 ID、body、响应与可信 actor；保留消息作者、频道和卡片编辑各自规则，用户编辑不等价于 Bot 卡片编辑（`modules/bot_api/send.go:713-810`、`modules/message/api.go:1441-1535`）。 |
| POST `/v1/bot/messages/sync` | Bot 和用户的频道同步能力 | 请求和返回分别不同；保留群、DM、thread 和 Space 的主体门及用户数据过滤，不能 Bot 失败后改用用户（`modules/bot_api/sync.go:14-158`、`modules/message/api.go:935-1015`）。 |
| POST `/v1/bot/readReceipt` | Bot 和用户已读业务 | 适配 actor 与响应；分别保留频道授权与 Bot/用户已读记录身份（`modules/bot_api/send.go:874-1024`、`modules/message/api.go:1148-1205`）。 |
| POST `/v1/{bot,user}/messages/_search` | `messages_search.Handler.searchMessages` | 适配可信搜索 Principal 和 Space；保留单频道可读及消息可见性。 |
| POST `/v1/{bot,user}/messages/_search_all` | `Handler.searchAll` | 同上；保留消息与文件混合结果语义。 |
| POST `/v1/{bot,user}/messages/_search_files` | `Handler.searchFiles` | 同上；保留文件过滤和可见性。 |
| POST `/v1/{bot,user}/messages/_search_media` | `Handler.searchMedia` | 同上；保留媒体过滤和可见性。 |
| POST `/v1/{bot,user}/messages/_search_around` | `Handler.searchAround` | 同上；额外适配旧窗口响应到规范 envelope，保留 anchor 本身可见性检查。 |
| POST `/v1/{bot,user}/messages/_search_global_groups` | `Handler.searchGlobalGroups` | 适配主体和 Space；保留可读频道集合及预览过滤。 |
| POST `/v1/{bot,user}/messages/_search_global_messages` | `Handler.searchGlobalMessages` | 适配主体和 Space；保留全局频道集合与请求过滤交集。 |
| POST `/v1/{bot,user}/messages/_search_global_files` | `Handler.searchGlobalFiles` | 同上；保留文件类型过滤。 |
| GET `/v1/bot/groups` | 现有 Bot 群可见性查询和用户群列表各自的查询逻辑 | 两者集合与字段不相同；需按可信 E 和 Space 适配，不直接套用用户“我保存的群”（`modules/bot_api/groups.go:33-68`、`modules/group/api.go:968-1110`）。 |
| GET `/v1/bot/groups/{group_no}` | 群详情数据与成员校验 | 适配 `{group_id}`、字段与错误；保留有效主体的群成员门（`groups.go:70-115`、`modules/group/api.go:886-915`）。 |
| GET `/v1/bot/groups/{group_no}/members` | 群成员数据查询 | Bot 返回完整数组，用户版本有分页及更多字段；需定义 v2 分页/响应并保留调用者成员门（`groups.go:117-160`、`modules/group/api.go:437-480`）。 |
| GET `/v1/bot/groups/{group_no}/md` | `GetGroupMd` 数据读取 | 适配可信主体和参数，保留群成员门及 thread ID 区分（`groups.go:163-208`）。 |
| PUT `/v1/bot/groups/{group_no}/md` | 群描述更新服务 | Bot admin 与用户群主/管理员门不同，需分别校验并以可信 E 记录更新者；保留 AI 容器及解散检查（`groups.go:210-291`、`modules/group/api.go:4367-4425`）。 |
| POST `/v1/bot/createGroup` | `groupService.CreateGroup` 和现有成员准入 | 必须改入口：creator 固定可信 U，不接受旧 body creator；B 为可选执行者，保留 U/B/成员的 Space、IM 和通知逻辑（`groups.go:405-517`、`modules/group/service.go:1216-1265`）。 |
| PUT `/v1/bot/groups/{group_no}/info` | `UpdateGroupInfo` | Bot admin 与用户按字段区分的权限不等价；适配 PATCH 字段和响应，保留解散及 AI 容器检查（`groups.go:518-594`、`modules/group/api.go:1351-1592`）。 |
| POST `/v1/bot/groups/{group_no}/members/add` | `AddGroupMembers` | 按 E 保留对应成员、邀请及 Space 门；旧 Bot 返回 added/skipped，用户响应不同；批量事务语义先确认（`groups.go:595-681`、`modules/group/api.go:1595-1823`）。 |
| POST `/v1/bot/groups/{group_no}/members/remove` | `RemoveGroupMembers` | Bot admin 和用户角色/本人 Bot 等移除规则不同；适配身份及结果，不能仅更换 operator（`groups.go:683-799`、`modules/group/api.go:3214-3458`）。 |
| POST `/v1/bot/groups/{group_no}/threads` | `CreateThread` | 适配 E；保留所选主体的父群活跃成员门、source message 差异（`modules/bot_api/threads.go:74-141`、`modules/thread/api.go:259-316`）。 |
| GET `/v1/bot/groups/{group_no}/threads` | `GetThreads` | 适配分页和主体；Bot 默认 active，用户支持其他 status，v2 能力待确认（`threads.go:142-171`、`modules/thread/api.go:387-460`）。 |
| GET `/v1/bot/groups/{group_no}/threads/{short_id}` | `GetThread` | 适配两个路径参数及主体；Bot 与用户传入的 login UID 不同，影响个人 mute 字段（`threads.go:173-187`、`modules/thread/service.go:817-845`）。 |
| GET `/v1/bot/groups/{group_no}/threads/{short_id}/members` | `GetMembers` | 适配可信主体、参数与响应；保留父群活跃成员门（`threads.go:225-239`）。 |
| POST `/v1/bot/groups/{group_no}/threads/{short_id}/join` | `JoinThread` | 将可信 E 作为加入者，保留父群活跃成员与 AI 容器门；适配参数及响应（`threads.go:241-255`）。 |
| POST `/v1/bot/groups/{group_no}/threads/{short_id}/leave` | `LeaveThread` | 将可信 E 作为离开者，保留 creator、解散等规则；适配参数及响应（`threads.go:257-271`）。 |
| GET `/v1/bot/groups/{group_no}/threads/{short_id}/md` | `GetThreadMd` | 适配可信主体、路径和响应；保留父群活跃成员门（`threads.go:273-301`）。 |
| PUT `/v1/bot/groups/{group_no}/threads/{short_id}/md` | `UpdateThreadMd` 数据写入 | service 不是完整授权门：Bot admin 与用户 thread creator/群管理员门不同，须在适配层保留正确判定（`threads.go:303-373`、`modules/thread/service.go:1053-1068`）。 |
| POST `/v1/bot/file/upload` | 文件大小、扩展名策略和 File 上传服务 | 按所选主体复用原业务路径及其大小、扩展名和路径处理，不变更旧文件业务逻辑；仅适配 v2 身份与外层响应（`file.go:58-122`）。 |
| GET `/v1/bot/file/download/{path}` | 文件签名服务及路径处理 | 保持旧路径清理、签名及 302 语义；仅适配 v2 身份与路径参数。旧实现未核验对象 Space 归属（`file.go:125-151`）。 |
| GET `/v1/bot/upload/credentials` | 现有上传凭证生成能力 | 按所选主体保留其原有签发和存储范围，不改原业务逻辑；适配身份与外层响应（`file.go:174-254`）。 |
| GET `/v1/bot/upload/presigned` | 现有预签名上传能力 | 按所选主体保留其原有签名和 object key 规则，不改原业务逻辑；适配身份与外层响应（`file.go:256-323`、`modules/file/authtree_guard.go`）。 |

搜索表中的 `{bot,user}` 仅为合并两条旧路径的记法。八项搜索已有 session、Bot、User Key 共用的 `messages_search.Shared` 实例及 handler；多数响应已有 `data`/pagination，但仍须逐字段核对新契约。`MountSubtree` 会一次挂载所有搜索路由；v2 需构造可信用户搜索 Principal 并提供绑定 Space，保留用户的 Space 成员、频道权限、消息过滤、限流、审计和 backend gate，且不能因整树挂载漏掉逐路由 action（`modules/messages_search/route_subtree.go:33-84`）。v2 消息搜索不采用 Bot 或 OBO 搜索主体；不修改既有 v1 的搜索身份及 OBO 行为。搜索限流主体及 Bot 调用审计字段与 CLI 另行确认。

产品前提：绑定 Bot 所在的群中 Token 用户也在群内，用户搜索覆盖其有权查看的 Bot 消息。实施前应验证该前提在绑定、群成员变更及历史数据上成立；现有搜索代码按用户和 Bot 分别检查活跃父群成员（`modules/messages_search/authz.go:33-42`、`predicate.go:71-85`），仅凭现有搜索代码不能证明该前提。结果仍遵守用户的撤回、删除、清空和消息可见范围。

### 现有 OBO 使用现状

以下记录旧 Server 能力；v2 不采用 OBO，v1 行为保持不变。

| 现有能力 | OBO 用途 | 当前设计处理 |
| --- | --- | --- |
| `POST /v1/bot/sendMessage` | `on_behalf_of` 经 grant、scope 和频道权限校验后以用户身份发消息；特定 Bot 与授权者本人私聊回复分支仍以 Bot 发送；卡片消息对 OBO 有额外限制（`modules/bot_api/send.go:40-47,118-120,187-245`）。 | v2 以路由选定的 E 发送，不接入 OBO。 |
| `POST /v1/bot/typing` | `on_behalf_of` 经权限检查后以授权用户身份发送输入状态；本人私聊回复分支另有处理（`modules/bot_api/typing.go:19-35,57-123`）。 | v2 以路由选定的 E 发送，不接入 OBO。 |
| `POST /v1/bot/groups/{group_no}/threads` | `on_behalf_of` 通过 OBO 后使用户成为子区 creator；否则由 Bot 创建（`modules/bot_api/threads.go:73-130`）。 | v2 的 creator 为 E，不接入 OBO。 |
| Bot Project 列表、详情、成员 `GET /v1/bot/projects...` | 使用 `pkg/obo` 解析 Bot actor 与用户 subject，再由 Project service 检查数据权限（`modules/project/api_bot_read.go:18-35,37-65,112-122`）。 | 是独立的 Project 读接口，不在当前附件映射范围；是否接入 v2 另行确认。 |
| `GET /v1/bot/obo-grant`、`GET /v1/bot/obo-grants/self`；`/v1/obo/grants`、`/scopes`、`/delegations` 等 | Bot 查询自身授权；用户管理 Persona grant/scope 和通用 delegation（`modules/bot_api/bot_api.go:461-469`、`obo_api.go:101-127`）。 | v1 授权管理保持现状，v2 不提供 OBO。 |
| Persona 消息 fan-out | 按授权向 Bot 投递用户频道消息副本及事件；属于旧消息接收链（`modules/bot_api/obo_fanout.go:1-12,34-60`）。 | 保持既有行为，不作为 v2 主体选择的依赖。 |

消息同步 `/v1/bot/messages/sync` 与已读 `/v1/bot/readReceipt` 当前按 Bot 身份执行，不接受 `on_behalf_of`（`modules/bot_api/sync.go:93-97`、`send.go:738-741`）。

补充暴露面：Server 还有 `/v1/bot/upload`、`/v1/botfile/upload` 和 `/v1/botfile/*path`；它们不在附件逐项范围，CLI 仓库本次未检出这些完整路径的字面引用，不等于线上无调用。用户 Bot 管理 `/v1/user/bots*` 是不同的用户管理面，旧 `agent_ref` 是调用方不透明字符串，不是新体系的可信 Token↔Bot 绑定证明（`modules/botfather/api_user.go:615-721`）；是否纳入 CLI 接口另行核实，不凭相似名称扩大范围。

## 用户与 Bot 身份的目标契约

设计前提：可信 Token↔User Bot 绑定和主体解析能力按已具备进行接口设计。每个 Token 最多绑定一个 Bot，由 CLI 负责这一限制；Server 不实现多 Bot 选择，不支持 App Bot。Server 保留经验证的用户和可选 User Bot，按接口支持情况选取有效执行者；资源 owner 与执行者可不同，资源/Space/频道权限仍由 Server 复查。v2 不使用 OBO。

最新 SDK 已有初版，目标实现应接 `client.Auth.Resolve` 和 `permission.HasAction`；可在隔离测试中注入可信身份提供者，但对外开放必须验证真实 SDK、Auth Server 及其 octo-server 身份解析依赖。Auth Server 当前 `Principal.Subject` 是用户，绑定 Bot 放在 `BotBindings`；v2 按路由策略选择 Bot 或用户，不能把 SDK 的 Subject 直接当成所有路由的执行者。业务 Token 不得转发给身份上游。

## 接口分类与实施依赖

- 用户信息和 Space 成员：按新主体在绑定 Space 的可见范围检查；不能把任意 UID 或跨 Space 查询当作目录权限。
- 消息发送、编辑、同步、回执：分别盘点会话类型、参与者、频道/父群和消息可见性；八种搜索按 Token 用户及绑定 Space 校验，动态 global 路由与列表、分页过滤均需覆盖；不得复用只做 action 的 handler。
- 群和子区：区分读、创建、修改、成员变更与加入/退出；核实 Bot 或用户的当前成员身份、管理权限以及 thread 父频道门。
- 注册：旧 `/v1/bot/register` 行为不变；新增身份解析接口供新 Token 下的 CLI 获取经验证的用户及可选 Bot 身份。新 CLI 当前按不需要 Bot token/IM 连接信息设计；typing、文件上传/下载核实凭证种类、Bot 归属及资源能力。
- 测试库已写入 11 项 `im.*` action；仍需验证目标 Token 的权限快照、各环境目录与服务部署及端到端联调。Auth SDK 发布/私有模块访问和 service credential 安全分发、Auth Server 提供强一致 DB 与业务 Token、开发者平台的签发及真实 session 网关，以及 CLI 的新 token 路由和旧 token 切换仍是联调依赖。`im.groups:write` 为 high，覆盖创建/更新群、群 Markdown 和成员增删全部群写能力。

## 错误、安全及可用性

功能权限拒绝、业务资源拒绝、无效/撤销/过期 Token、Auth Server/数据库不可用分开处理；新判定未完成或依赖故障不放行，且不以旧链路作为隐式兜底。v2 返回上文规定的语义状态码和统一 error envelope；具体业务错误与 enum 的映射需按已注册错误码逐项核对，真实状态码须按仓库规则获批。不可将依赖故障伪装为凭证无效；不输出业务 Token、服务 Token、Authorization 或敏感数据。

新链路不自行缓存权限/决策以维持上游撤权契约；Server 现有 Space 60 秒缓存及其他数据门与 token 撤销不是同一时效，必须分别测量和批准。对已经开始处理、之后才撤权的请求，不能承诺即时中止。

## 迁移发布（候选，非上线授权）

1. 对清单逐路由核对 action 和数据门；确认测试库目录记录、目标 Token action 快照、Token 类型、客户端版本、可信 Bot 绑定契约、v2 网关转发和规范版本策略。
2. 验证 Auth Server/SDK/service credential 的部署及端到端联调；确认认证主库一致性、可用性与指标，再在测试环境验证。测试目录已写入不代表 Token 快照或服务部署已就绪。
3. Server 增量部署但不默认切换；灰度受控调用者/路由，观察认证、授权、业务拒绝、依赖故障及客户端行为。扩范围前必须完成逐项映射与验收；旧凭证撤销和停用时间由 owner 决策。
4. 回滚 Server 路由/开关和 CLI 指向，同时保留旧凭证及原数据门；验证回滚不让新 Token 绕过权限或把错误当作旧凭证处理。若旧 token 已撤销则回滚需要另行恢复凭证，不能声称自动可逆。

## 验收矩阵

- SDK/Auth Server：有效、过期、撤销、失效权限、大小写和通配、domain 过滤、服务 token 与业务 token 混淆、服务断连、Redis 故障/DB 故障、权限更新及旧缓存回填；确认返回身份和撤权观察点。
- 每条清单：符合的 action 才可能进入 handler；无 action、错误 action、未映射路由直接拒绝；既有 Space/群/thread/消息数据门仍生效。验证跨 Space、跨成员、伪造 UID/Space/body、直接 HTTP 绕过 CLI、全局搜索分页和结果过滤；文件对象按上文记录的旧边界单独验证，不假称具备对象 Space 归属校验。
- 八种消息搜索：有无 Bot 绑定都以同一个 Token 用户及其绑定 Space 判定频道和消息可见性；验证 Bot 群成员对应用户同群的产品前提，以及用户可见的 Bot 消息。覆盖单频道、全局搜索及用户消息删除、清空、可见范围；v1 原路径行为不变。
- 主体选择：有可信 Bot 且接口支持 Bot 时使用 Bot；接口不支持 Bot 或确认无 Bot 时使用用户；验证无 Bot 的用户路径、绑定无效和绑定服务故障，失败不能隐式降级用户。逐项验证 v2 范围内的用户能力；v2 不使用委托参数或 OBO，v1 保持原有行为。CLI 的单 Token 最多一个 Bot 限制由 CLI 验证。
- 兼容及错误：新旧凭证按批准方案分别运行；鉴权服务不可用不回退放行；业务 4xx 与依赖故障可区分，旧客户端的状态与信封行为按确认契约验证。
- v2 wire：逐接口断言成功 `data`、分页字段和失败 `error.code/message/details`、语义状态码与 OpenAPI 一致；验证英文消息、详情脱敏和真实 JSON，与 v1 的本地化兼容 envelope 分别覆盖。下载复用原 302 与 `Location`，错误返回统一 JSON；上传、签名及下载的旧业务路径行为保持。
- 新 Token 的身份解析只返回用户及可选经绑定校验的 Bot 身份，不返回旧 Bot token，也不重置 IM token；旧 `/v1/bot/register` 保持原响应和行为。
- 在真实持久化依赖、授权服务和 CLI 联调环境验证主要链路及发布/回滚；代码/本地 fixture 不能证明生产环境部署或时效。

## 待决与风险

1. 可信身份和 Bot 绑定按最新 Auth Server 的代码契约设计；测试空实现仅用于隔离环境。须对齐 Auth Server 所需 `/v1/internal/auth/identity` 的 Server 实现、认证和部署，再以真实依赖联调。
2. 11 项 `im.*` 权限已写入测试库；目标 Token 是否包含这些 action、各环境目录和部署状态以及端到端判权仍需验证。`im.groups:write` 为 high，覆盖创建群、修改群信息/Markdown、添加和移除成员；其 high 分类因此适用于全部群写操作。
3. Auth Server/SDK owner：服务身份治理、限流、网络及部署可用性；上线级撤权与并发承诺。
4. Server/CLI owner：v2 凭证格式、旧凭证停用时间、错误兼容、对象下载风险处置。
5. 安全差异：`ScopeUnscoped` 对象签名、Space 成员缓存、搜索旧索引的可见性缺口均为独立数据门问题，评审时须明确实际暴露范围及处置责任。
6. 仓库已增加 OpenAPI 3.1 v2 文档及独立校验；共享规范 R12 的 `/v2` 更新仍需规范维护方落实。
7. Server/CLI owner：逐接口落实 Bot 主体支持矩阵和用户主体能力；验证绑定 Bot 群成员对应用户同群的前提及成员变更、历史数据的约束。
8. OCTO-276（Auth SDK）、OCTO-263（Auth Server）源码初版已查；对应环境的部署状态、Server identity 接口与端到端绑定行为仍需验证。
9. 文件下载是经确认的 Space 隔离例外：`/v2/cli/files/_download` 复用旧 object key 下载逻辑，不校验对象归属 Space。Token 身份和 `im.files:read` action 仍需通过认证授权；用户或 Bot 获得其他 Space 的 object key 时，该接口不阻止按旧逻辑生成下载 URL。

## 实施和验证记录

- 独立 `/v2/cli` 路由、SDK 身份与 action 校验、用户/Bot 主体选择、消息与搜索、群与子区、用户及 Space 成员、受限文件上传已接入；旧 `/v1` 入口保持独立。
- `/v2/cli/files/_download` 接受 `path` 和可选 `filename`，沿用旧下载逻辑的 302 和 `Location`；对象归属不按 Space 校验。Auth Server 与 CLI 的真实部署、权限目录和端到端链路尚未联调，不能据测试夹具认定已可上线。
- 适配层沿用用户与 Bot 各自的群和子区业务权限：Bot 子区 Markdown 写入使用 `bot_admin`；用户邀请 Bot 校验邀请人的有效 Space；Bot 群详情与列表中的 Markdown 写权限与实际写入门一致。Bot 创建子区不得提交源消息 payload。
- 用户私聊发送保留原双向好友门；其他消息操作保留原业务权限，并限制在 Token 绑定 Space。私聊编辑和已读在写入前检查目标消息的 Space；v2 全局搜索不纳入绑定 Space 外的真人好友，v1 搜索行为保持原样。
- 消息编辑适配在注入路径 ID 后以最终请求体进行权限检查，大小写不同的重复 JSON 字段不能使预检和 handler 使用不同目标。Bot 无标签私聊历史仅在 Token owner 的默认 Space 可读取或修改；带 Space 标签的消息按绑定 Space 判断。
- 使用隔离 MySQL、Redis 和真实 WuKongIM，对 `modules/bot_api` 的 CLI 回归及 `modules/user` 的 CLIV2 回归运行 `-race` 通过；`modules/message` 的 CLI 回归、`modules/messages_search` 的 v2 搜索回归和 `pkg/cliv2` 测试也通过 `-race`。部分消息、搜索和 Auth 场景使用本地服务 fixture，不能证明真实服务间联调。
- 根包路由配置和全局限流 v1/v2 错误信封测试通过 `-race`；`make i18n-extract-check`、`make i18n-lint`、`make openapi-check` 通过，OpenAPI 检查有 4 条 warning。真实服务的完整启动、独立路由转发及跨服务部署验收尚未完成。

来源：OCTO-266 任务及附件；当前 octo-server 所列路径；GitLab `octo-auth-server` main `8f4ef89989025031b088ed26e2f9863c19e12a4c`、`octo-developer-platform` main `34778f7e4db1af2e5825a1c71a1c8d2fbfc24443`、`octo-auth-sdk` main `93052c45585acf67743f8ebc80f3e8fb4e473867` 的源码和 README。各项为代码/文档证据，不代表环境已上线。