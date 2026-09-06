# Change log

Change history for this repo's `.octospec/`, following the
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
change-log convention (§7). Newest first.

## 2026-09-06 — project-p0-foundation (PR #841 第一轮 review：TDD 修复 blocker 与 Q 项)

- **Fixed (blocking)** — remove 批次中途解散丢弃已提交部分（errProjectGone 镜像 add 的
  anyApplied 契约）；join_mode 与 is_official 完全对称撤出全部客户端面（S-2：P2 上线自助
  加入前必须先消灭"现在写入的 join_mode=0 将追溯性变成开放加入"的存量数据）。
- **Fixed (non-blocking)** — anyApplied 改按 committed 判定（no-op 不冒充已提交）；
  successor/target 的 space_member 共享锁前置到 project 锁之前，消除三方死锁环（Q2）；
  projectMiddleware 的成员与角色合成一次 MemberRole 读取（Q3/Q8，banned 在下一请求即生效）；
  五条特权写路径事务内复核 actor 的 Space 席位（Q6，新增 requireActorSpaceSeatTx）；
  reconcile 分页改为 base 行 LIMIT + 每行 violating 标记（Q4：LIMIT 从此约束"检查行"而非
  "返回行"）；ForTest 全局函数指针改为实例字段（Q7）；orphan 扫描覆盖解散 Space；
  bumpMemberEpochTx 不再搅动 updated_at 且加 status 谓词；指标改名 write_rejected_total（S-3）；
  brief 成员配额验收措辞按批量契约修订（S-1）。
- **Learned** — 行为级测试必须做变异检查：reentrancy 测试重写为读 histogram SampleSum 后
  才杀得掉"删 guard"变异；两个源码 guard join 续行后立即抓到跨行绝对赋值变异。TDD 循环里
  新写的守卫（bumpMemberEpochTx 的 status 谓词）当场抓到既有代码的顺序缺陷（disband 先翻转
  后 bump 吞掉 epoch）——测试先行不是仪式，是捕获手段。
- **Learned** — 测试断言不能从 wire 读被刻意排除的字段（committed 是 json:"-"），必须断言
  其行为后果（403 vs 200 per-target report）。
- **Test hardening** — setup Redis helper Skipf→require；级联注册可断言（space 导出
  MemberRemovalCleanupStepNames）；级联 step 导出供外部测试恢复真实注册；缓存 seam 三分支
  与 corrupt-cache 回落补测；并发测试加 start barrier + loser 断言；RoutesReject 断言
  registered code；COUNT(1) 纳入黑名单；schema 测试要求两表齐备。

## 2026-09-06 — project-p0-foundation (P0 implemented, 4 review rounds)

- **Added** — `modules/project`：Space 内项目协作层 P0。`octo_project` / `octo_project_member`
  两表（`active_name` 生成列让解散释放重名），CRUD + 成员管理在请求事务内同步校验不变量 I1，
  `member_epoch` 与每次成员写在同一事务内 `+1`（写纪律由源码级 guard 钉死），Space 移除级联经
  反向注册清理步骤接入既有 outbox 工单，reconcile 只读对账（LIMIT+cursor，跨 tick 轮转），
  四类配额 + 审计 + 按 entry point 拆分的拒绝指标。`project_create_enabled` fail-closed。
  未触碰 group/thread/message；回滚 = 删两张表 + 去掉一个 blank import。
  See [journal](journal/shared/project-p0-foundation.md).
- **Learned** — 不变量只存在于它被强制执行的路径上，而每条新写路径都是一个新的执行点。
  I1 的校验在 addOneMember 有了、createProject 的 owner 席位没有过；转让路径复核了 Space 席位、
  直接 role change 却没有。brief 早已预言（「十一条群写路径证明 retrofit 会漏」），仍然发生。
  对策是把「枚举写路径并断言各自含不变量调用」做成源码级 guard。
- **Learned** — 「后续会有人补」必须写成代码。三处缺陷都是注释在断言代码不具备的性质：
  cascade 注释说 reconcile 是第一页之后席位的「backstop」（reconcile 按设计只读）；
  页数上限注释说「下一 tick 继续」（cursor 是局部变量，每 tick 从头开始，~25k 行之后
  永远不被扫描）；批次标签 `not_attempted` 在 remove 路径是真话、在 add 路径是谎话
  （service 先跑完整批，handler 事后标注）。
- **Fixed (4 rounds of review)** — 事务内 I1 校验（`FOR SHARE OF` 锁 space_member 行）、
  三个配额改为锁 space 行后计数（daily 维度跨 Space 的窄窗口为已接受例外）、特权写路径
  一律锁内重读 actor 角色、提升/继任均复核目标 Space 席位、级联每短事务复核 + 页预算
  耗尽返回可重试错误、abandoned 对账改为分页统计泄漏席位并排除在途工单、leave 只容忍
  io.EOF、所有权交接补继任者审计、部分提交批次返回 per-target 结果。
- **Deferred (产品待决，已记录进 brief Open questions)** — 唯一 owner 被移出 Space 后留下
  无 owner 项目：自动提拔与自动解散都是产品决策，P0 只打 Warn；与 group 模块
  `handOverGroupCreator` 无继任者时的既有终局一致。
- **范围收敛** — 按需求方指示回退三处超出 brief 的扩展：级联的自动转让/自动解散、
  Space admin 在用户侧列表对 unlisted 的放宽（brief 只授权 detail；枚举属 P2 admin surface）、
  `member_epoch_bumped_total` 指标。
- **Known gap** — 每日创建配额的跨 Space 并发窄窗口（per-space / per-creator 两个硬配额已在
  space 行锁内计数，跨 Space 的 daily 维度未加锁）；生产 collation 待人工确认
  （legacy 表未写 COLLATE，若生产默认非 `utf8mb4_general_ci`，对账 JOIN 会撞 1267）。

## 2026-09-04 (bot-agent-hosting · review round 4)

- **Learned** — **注释声称有区分力、实际没有的测试，比没有测试更糟：它会被相信。**
  reviewer 用变异法证明我两条关键测试杀不掉对应变异：时区测试把**旁观连接**调偏
  （register 走池里另一条，两个 TIMESTAMP 落同一 UTC 瞬间、偏差恒 0）；三条稀疏写入
  测试的带外 UPDATE 落在下一次 register **之前**（合并实现读到新值再写回，结果相同）。
  而我上一轮把"验了两处"表述成了"三处全部验证通过"。
- **Adopted** — 定为规则：**每个行为断言都要过变异检查，且测试注释写明它杀掉哪个变异。**
  本轮 6 个断言逐一实跑变异版验证（SQL 时钟 / 合并守卫 / legacy 空值跳过 /
  日志字段 / App Bot Warn / 全有或全无解码）。
- **Changed** — 稀疏写入的不变量改由**源码守卫**承担：真正的交错窗口在
  queryRobotByBotToken 与 UPDATE 之间，行为测试无法确定性插入。守卫区分**赋值目标** ——
  robot 行的值作为独立 stored 快照传参用于 skip 比较合法（不匹配时写调用方的值），
  赋回 req.Agent* 则禁止。
- **Learned** — **一条需要污染进程级状态才能运行的测试，在整包里就是不可靠的。**
  时区端到端测试要区分两种时钟，必须把连接池压到单连接（SetMaxOpenConns 是进程级），
  于是其它测试排队/超时、失败集每次不同。**删掉而非修好**：它守的不变量有确定性替代
  （断言语句里是字面 NOW()），杀同一变异且不碰共享状态。删除理由留在原代码位置。
- **Fixed** — brief 四处与实现矛盾，其中两处描述的是**前两轮被改掉**的行为
  （含两位 reviewer 都阻塞过的"形状非法落空串"）。上一轮只修了被点名那处、没修类别。
  Load-bearing 段里的过时规则比没有规则更糟：后来人当权威读，会把代码"修"回 bug。
- **Decided** — `agent_hosting` 的撤回改用保留 slug `none`，`""` 回归 no-op（四字段统一）。
  让 `""` 清空会给新列复制出老列被判为数据丢失的同一形状：从不填该字段但总是发 key 的
  客户端每次重连落进 `('', 非NULL)` —— 那个状态被三处文档定义为「曾上报后刻意撤回」。
  `none` 在存储层归一成空串，读取方无需知道这个 sentinel。
- **Fixed** — 去掉 skip-if-unchanged 的**理由已消失**（时间戳现在只在 hosting 上报时前进，
  于是版本-only 重连的那条 UPDATE 无任何可观察效果）。legacy 三列恢复跳过，比较用的
  stored 快照**永不被写进语句**；hosting 保持无条件写（撤回/再确认都是真实上报）。
- **Fixed** — recordingLog 原先只记 msg **丢掉 fields**，而泄露风险全在 fields 里；
  迁移测试原先断言"不得出现 INFORMATION_SCHEMA"，把无守卫写法变成了强制（且大小写敏感
  只是碰巧正确）—— 单条原子 ALTER 解决的是列级半应用，**不解决**可重入性，这一点上一轮
  说错了。迁移补 pin `ALGORITHM=INSTANT`（不 pin 会静默退化为 COPY 锁表）。
- **Learned（环境）** — **botfather 整包"失败集每次不同、耗时恰好 5s/30s"时，先重启
  WuKongIM 再怀疑代码。** 连续 8 天运行的容器状态劣化，UpdateIMToken 5s 超时
  → `err.server.bot_api.im_token_failed`，看起来极像测试污染。判据：串行跑
  （`-p 1 -parallel 1`）—— 真正的顺序依赖会变**稳定**，容量问题仍然随机。
  重启后整包 15.7s 全绿。排查中确实找出两个真污染并保留修复：register 的 per-IP 桶
  必须在 helper 里每次清（不是 setup 清一次，一条测试就能自己打满）、`SET time_zone`
  打在连接池上无作用域。

## 2026-09-03 (bot-agent-hosting · review round 2)

- **Fixed** — round 1 的修复引入的数据丢失回归（两位 reviewer 再次独立端到端复现）：
  三个 legacy 版本字段改 `*string` 后，指向空串的非 nil 指针被无条件写入，于是
  「报空」从 merge base 的「保留」变成「清空」。register 是重连路径，任何序列化器对
  未填字段输出 `""` 的客户端会每次重连擦一次，HTTP 200、无日志、事后与「从未上报」
  不可区分。现在 legacy 三列**空值也跳过**，`agent_hosting` 保持「报空即清空」。
- **Learned** — **「不在语句里」与「以空串在语句里」只在没有东西区分它们时才是同一件事。**
  为修并发引入指针，同时也引入了这个区分，而旧契约默默依赖它不存在。稀疏写入与
  「空值即不变」从来不冲突：丢更新的根源是「替换成刚读到的值」，不是「跳过该列」。
- **Learned** — 同一个 learning 在 brief 里重演：brief 同时留着新规则（四字段一律稀疏）
  和被它取代的旧规则（三个 legacy 保持 merge-then-write），**而过时那条描述的行为
  恰好能防住这次回归**。reviewer 直接引用了本 PR 新增的
  `a-rule-in-a-comment-is-not-applied.md`。取代一条规则 = 删掉旧的，不是并排放新的。
- **Adopted** — **变异测试作为常规手段**（跟 reviewer 学的）。他验证 round 1 的时区修复
  不是装饰：把 `dbr.Expr("NOW()")` 改回 `time.Now()`，确认两条测试红、其中一条报出实测
  `7h59m59s` 偏差。本轮推送前照做：去掉 legacy 空值跳过 → 端到端与 SQL 层两条都红；
  改回 `_ = c.ShouldBindJSON` → 部分采纳那条红。「测试过了」与「没这行就会红」是两个
  不同的断言，只有后者有价值。
- **Fixed** — `json.Decoder` 会先填好已解析字段再返回类型错误，所以忽略 bind 错误等于
  采纳一个**前缀**（`{"agent_platform":"OpenClaw","agent_version":123}` 存下 platform
  丢掉后面，无任何诊断）。改为全有或全无：解码到临时变量 + 要求干净 EOF 才采纳。
- **Guarded** — 空 `agent_hosting` 的两种含义（时间戳 NULL=从未上报 / 非 NULL=显式清空）
  此前被列 COMMENT 否认、被 `omitempty` 在 wire 上抹平；现在 COMMENT、字段注释、
  wire 说明三处对齐。补了 4 KiB body 上限的测试（本功能唯一没测的新行为）。
- **Fixed** — 测试里 `SET time_zone` 打在连接池上，deferred 复位可能落到另一条连接、
  把 `+08:00` 的连接留在池里污染后续测试。改为独占一条 `*sql.Conn` 并**关闭**而非复位。
- **Noted** — `normalizeAgentHosting` 的排序理由（「10MB 值不该付 10MB 折叠开销」）在
  4 KiB body 上限之后**已过时**。顺序仍对（零成本，且不让这个界依赖两层调用之外的限制），
  但理由随之改写 —— 论证过时和论证错误一样需要修。

## 2026-09-03 (bot-agent-hosting · review round 1)

- **Fixed** — PR #837 两位 reviewer 独立实测复现的两个阻塞项：
  ① `agent_reported_hosting_at` 原用 Go `time.Now()` 写，经驱动 `Config.Loc`（默认 UTC，
  DSN 未设 `loc`）转换，而它并列展示的 `bound_at` 由 MySQL `NOW()` 写、应用镜像固定
  `TZ=Asia/Shanghai` —— session 时区非 UTC 时两者相差 8 小时。改 `dbr.Expr("NOW()")`。
  生产 MySQL 是 UTC，故为潜伏未发生。
  ② 四个 `agent_*` 字段全改稀疏写入（`*string`，nil 即不进 `SetMap`）：初版只对新列稀疏，
  三个既有版本列仍回写读到的值，且把条件 UPDATE 改成无条件，等于新开一条丢更新路径。
- **Learned** — **注释里写下的规则不会因为被写下就生效。** 那段「不要把缺席字段解析成
  刚读到的值，会丢更新」的注释，正上方三行就是它否决的写法 —— 周边代码早于这句话存在，
  而没有任何东西在事后重新审视它。同时两种实现对**单写者**的所有 DB 可观察断言完全等价，
  所以推理产物（注释）与验证产物（测试）盲在同一处。可断言的是**发出的 SQL 里有哪些列**。
- **Learned** — `strings.ToLower` **不限于 ASCII**：`U+212A KELVIN SIGN`→`k`、
  `U+0130`→`i`，所以折叠排在 ASCII-only 正则之前时混淆字符能通过，让函数自己注释里
  「confusables all fail it」变成假的（测试恰好只挑了会失败的 `U+200B`）。ASCII 前置后
  才成立；且 ASCII 性质同时是**列宽不变量**的前提（`len()` 数字节 vs `VARCHAR` 数字符）。
- **Guarded** — 「被拒」与「清空」原本被文档成同一件事（PR 描述说 degrades to not
  reported，字段注释说 present overwrites）。定为**保持不变**：触发场景不需要恶意，
  `self-hosted`（连字符，正是所引用 GitHub Actions 的写法）就会被拒，一次客户端拼错
  会把全量 bot 的该列刷空。清空仍可做，但要显式报 `""`。
- **Learned** — 测试注释声称「能区分两种实现」时，那本身是个需要验证的断言。初版那条
  「值保留 + 时间戳前进」在被否决的写回实现下三条断言全过。改用**带外写入**（第三方在
  两次 register 之间改列，写回实现会覆盖它）+ sqlmock 直接断言 SQL 文本。
- **Changed** — `agent_reported_at` 更名 `agent_reported_hosting_at` 并收窄为只在 hosting
  被上报时前进：原实现任何 `agent_*` 上报都刷新它，等于替一份该次上报从未提及的数据背书
  新鲜度，而分歧场景正是用来论证指针语义的那个「新 runtime 漏报 hosting」。
- **Guarded** — register 的 body 加 4 KiB 上限（`binding.JSON` 无上限，本路由此前无任何
  body 界，而四个 sibling bot_api 路由都有）。App Bot 分支上那次解码只换来一行日志。
  超限按「未上报」处理，绝不让 register 失败（#696）。


## 2026-09-03 (bot-agent-hosting)

- **Added** — `robot.agent_hosting` + `robot.agent_reported_hosting_at`（单条原子 `ALTER`）。
  `POST /v1/bot/register` 的 User Bot 分支接收自报托管形态，`GET /v1/user/bots` 带出。
  App Bot 显式不支持（只解析 body 打 Warn，由源码守卫钉住 `app_bot` 无 `agent_*` 列）。
  无新 errcode / 端点 / i18n 条目 / 路由变更。
  See [journal](journal/shared/bot-agent-hosting.md).
- **Learned** — 「本地 vs 云上」在服务端**没有可信来源**，三个候选字段全部不成立：
  `user_api_key.client_id` 把桌面客户端与云端服务混装在同一个 `octopush` 下
  （`modules/integration/api.go` 明写桌面端只持业务后端自签 JWT，`/exchange` 硬编码单一
  client_id），从它推导会把本地标成云上；`bound_agent_ref` 是 bind 时客户端自填且
  「Octo 不解析其语义」；`agent_platform` 只有平台名。所以这个列是**观测**字段，
  永不可喂授权判定。
- **Learned** — **枚举白名单给的保证是虚假的。** 它校验「值在集合内」，不校验
  「你有资格声称这个值」——任何持 `bf_` token 的进程照样能报 `octo_hosted`。真正要挡的
  是引号/尖括号/空格/控制字符/Unicode 混淆字符，`^[a-z][a-z0-9_]*$` 全挡且不需要预知
  vendor。改为开放取值后，新托管方无需服务端发版，且 vendor 名不进这个开源仓。
  代价已明示：`cloud`/`local` 从此合法（约定降级为客户端约定，**刻意不做黑名单**）。
- **Guarded** — **校验上界高于列宽不是余量，是延迟的失败。** 初版 `maxAgentHostingLen=64`
  配 `VARCHAR(20)`：25 字节的值会过校验、写库撞 `1406`，而 `agent_*` 共用一条 UPDATE，
  会连带挡掉同一请求里的 `agent_platform/version/plugin_version`，且 register 仍返回 200。
  现在上界与列宽**严格相等**（测试从迁移文件正则提取列宽后断言 `Equal` 而非 `<=`）。
  另：长度上界必须排在 `ToLower` **之前**（后者按输入等大分配，body 无上限）。
- **Learned** — **规格核到「响应结构没这个字段」不等于核到「端点存在」。**
  原计划改的运维面 `GET /v1/manager/robots{,/:robot_id}` 整个是死代码：
  `modules/robot/api_manager.go` 的 `NewManager` 全仓无调用方、`Route()` 从未执行
  （`1module.go` 只注册 `New(ctx)`）。字段和测试都写完了，靠测试报 `404 page not found`
  才发现，随后整体 `git checkout` 撤回 —— 给死代码加字段没有调用方看得到，却会让
  下一个人以为运维面已有这个能力。
- **Learned** — 测试要放在**拥有 schema 的模块**里，不是拥有代码的模块。写入路径在
  `modules/bot_api`，但 `robot` 表的 `agent_*` 列归 `modules/botfather` 的迁移，而
  bot_api 的测试二进制不 link botfather 的 `init()`，那里 `NewTestServer` 建出的 robot 表
  没有这些列（`1054 Unknown column`）。App Bot 那条用例则要 blank import
  `modules/app_bot` 而**不能**裸 `CREATE TABLE IF NOT EXISTS` —— 绕过 sql-migrate 建表
  不写 `gorp_migrations`，会让下一个包的同名迁移撞「already exists」（`modules/message`
  大量 DB 测试被 skip 的已知根因）。
## 2026-08-24 (group-exit-notice-visibility)

- **Fixed** — 「某成员退出群聊」系统提示（`type=1021`）改为**全员可见 + RedDot:0**，
  与 bot 级联移除 Tip 同一套语义。此前它带 `visibles` 白名单只给一位管理员可见，
  非管理员看不到气泡却被计一格未读、且永远消不掉。实现走 octo-server 侧新增的
  `modules/group/group_exit_notice.go`，**不动 octo-lib 的 `SendGroupExit`**。
  See [journal](journal/shared/group-exit-notice-visibility.md).
- **Learned** — `visibles` 挡的是**内容**，挡不住 **seq**。IM 的未读是纯游标减法
  （`unread = latest_msg_seq − read_seq`），与 `red_dot`、`visibles` **都无关**
  （octo-im `build.go` / `sync.go`，octo-server 只原样透传）。所以「改红点字段」
  这个直觉修复**完全无效** —— 真正修好未读的是让消息可读。推论：单条共享 channel log
  里，「只给部分人看的持久气泡」与「不给其他人产生未读」不可兼得。
- **Guarded** — 可见性白名单里藏着一条**静默早退**：白名单为空（群里没有其他
  管理员）时整条提示不发。那不是产品规则，只是可见性实现的副作用。现已由
  `TestGroupExitTipSentWhenNoOtherAdmin` 钉死。`groupExit` 侧还连带去掉了一个
  查询失败即 500 中断整个退群的错误分支。
- **Learned** — 差分验证若只回退**改动面的一部分**，同样失真。第一轮只回退了 helper
  的 payload，两条测试确实红了，但那条早退门槛的断言从未被验证过；补做整段还原才
  跑出 `"[]" should have 1 item(s), but has 0`。移除了 N 个行为门槛，就得逐个还原。
  See [learning](learnings/pending/group-exit-notice-visibility.md).
- **Known gap** — `groupExit` handler 那处发送门槛的改动**无运行中的测试**：
  `api_test.go` 整片 HTTP handler 测试（19 处 skip）卡在 issue #17 的路由重复注册
  （解除 skip 会 `panic: handlers are already registered`）。补齐前置是先修 #17。
- **Out of scope** — 排查中确认的第二个问题（跨端已读不同步：`unreadClear` CMD
  `NoPersist:true` 离线丢失、`readed_to_msg_seq` 被 octo-lib 丢弃、iOS
  `reconcileServerSnapshot` 走 `MAX` 本地优先）未动，独立任务。

## 2026-08-23 (bot-owner-self-removal)

- **Implemented** — 普通群成员现在可以把自己名下（`robot.creator_uid`）的 bot 移出
  群聊。`memberRemove` 在调用方非 Creator/Manager 时落到一条窄口径自助分支：目标必须
  **全部**是本群内属于调用方的活跃 bot，否则整批拒绝。成员列表新增 per-viewer 的
  `bot_owned_by_me` 供前端逐行判权；「你被 X 移除群聊」换成 owner 视角的 Tip；两条
  移除路由挂上 `SharedUIDRateLimiter`（它们现在对普通成员开放）。
  上游 Mininglamp-OSS/octo-web#1511。
  See [journal](journal/shared/bot-owner-self-removal.md).
- **Guarded** — 移除侧的判据必须是默认拒绝的白名单（`QueryBotUIDsOwnedByUIDs`）。
  复用入群侧的 `checkBotOwnership` 会是提权漏洞：它对非 bot UID 返回 nil，搬到移除侧
  等于放开踢人权限。由 `TestBotOwnerSelfRemoval_RejectsHumanTarget` 钉死。
- **Fixed in review** — 三处只有 code review 才抓到的问题：授权谓词漏用活跃口径，
  让被拉黑成员拿到一个能改群成员表并写持久化 Tip 的写操作；`queryMemberWithGroupNoAndUID`
  漏选 `group_member.robot`，使 `bot_owned_by_me` 在 memberGet 上静默恒 false；
  前端只把 `removeAction` 透传给「查看全部」，而该入口在 19 人以下的群根本不渲染，
  功能等于没上。
- **Learned** — `register.GetModules` 用进程级 `sync.Once` 构造模块实例，一个测试
  二进制里 handler 永远持有第一个 `NewTestServer` 的 ctx。因此对系统消息的断言必须
  走 service 层，走 HTTP 路由时 IM 桩只对进程内第一个测试生效（表现为「单独跑绿、
  一起跑红」）。候选规则见 `learnings/pending/bot-owner-self-removal.md`。

## 2026-08-21 (space-member-removal-cleanup)

- **Implemented** — Space member removal now takes the member out of the Space's
  groups and sub-threads instead of only soft-deleting `space_member`. A
  transactional outbox (`space_member_removal_cleanup`) drives a leased, retried
  cascade that exits every group in the Space (full group-exit semantics, with
  creator handover first); the `SpaceMiddleware` and notify membership caches are
  invalidated inside the request. Covers all five removal paths — the
  owner-initiated `DELETE /v1/space/:space_id` was not in the original survey
  because it only flipped `space.status`.
  See [journal](journal/shared/space-member-removal-cleanup.md).
- **Split** — Person-channel (DM) isolation was originally part of this task and
  was moved to `space-member-dm-isolation` after eleven review rounds. Two
  measured reasons: WuKongIM's `whitelistOffOfPerson` defaults to `true`, so the
  DM half changes no delivery behaviour in any current deployment; and it did not
  converge by point fixes (a new escape in six consecutive rounds, with
  structural root causes). Keeping it would have blocked a group cascade that is
  live behaviour today behind a half that is inert.
- **Learning (pending)** — Deferred cleanup must be scoped with was-ever
  predicates: an is-currently predicate observes the state the trigger already
  destroyed and turns the job into a silent no-op. See
  [learning](learnings/pending/cleanup-predicate-tense.md).

## 2026-08-13 (cutover-framework)

- **Refactor** — Task `cutover-framework`: extracted the control plane the three
  one-way cutover mechanisms (#627 msgextra, #697 botevent, #733 token session)
  had hand-written separately into the new leaf package `pkg/cutover` —
  singleton state read, FOR UPDATE CAS flip with under-lock evidence and floor
  bounds, and the malformed-fails-closed expected-mode guard — and folded the
  two standalone operator tools into the server binary as
  `app cutover <domain> {preflight,activate,status}` so they finally ship in
  the image (the #733 precedent, generalized). Refusal conditions, floor
  semantics (#627 inclusive vs #697 strict), sentinel errors, and runtime hot
  paths are unchanged; the characterization tests moved with the code. The
  session rollout stays on its own five-phase surface and shares only the
  documented conventions (`docs/cutover-framework.md`: state-table template,
  `OCTO_<DOMAIN>_EXPECTED_MODE` naming, the flip-then-arm ordering invariant,
  evidence discipline, Down 3819 pattern). Runbooks now live in `docs/`
  (msgextra moved, botevent written for the first time). A review round fixed
  twelve findings on top, four of them operationally material: the endpoint
  print echoed the full MySQL DSN (password included) and is now redacted; a
  committed flip could be reported as a failure when releasing the pinned
  connection failed; msgextra never named the Redis instance whose scan sets its
  cutover floor; and `msgextra status` hard-failed on a missing state row,
  hiding the guard readout in exactly the state that fails every write closed.
  A second review round fixed twelve more, led by a regression from the first
  round's own fix: the signal handler added to make a wedged activation
  abortable had disabled default termination while no evidence phase could
  observe the context, so the command ignored every Ctrl-C. Interrupts are now
  two-stage (cancel, then restore default handling so a second signal
  terminates), the botevent score ceiling moved into the domain, and the
  msgextra mode constants alias the shared ones rather than restating them.
  A third round added a schema conformance test for every registered domain's
  state table (asserting the DDL from the live migrated schema and the inert
  seed from the migration source), and a fourth closed thirteen more: an
  operator interrupt was being reported as unreadable evidence, the interrupt
  notice raced process exit, two botevent MySQL reads still ignored the
  deadline, SIGTERM now exits 143 rather than sharing SIGINT's 130, and the
  operator commands say which config file they resolved again (on stderr, so
  `session-rollout status` stays parseable).
  See [journal](journal/shared/cutover-framework.md).

## 2026-08-12 (profile-visibility-system-bot-whitelist)

- **Task** — `profile-visibility-system-bot-whitelist`: took the public-bot
  exemption in the shared person-profile visibility decision off the writable
  `user.category` column and put it on the `pkg/space.SystemBots` whitelist, so a
  `category=system` row that is not a system bot — the superuser account, which
  has a fixed guessable UID — no longer skips the Space, friend, and common-group
  legs. The input field was renamed `SystemAccount` to `SystemBot` to close the
  wiring that caused the defect, and both endpoints moved together. Recorded as a
  narrowing of the authorization input, not an incident fix: no material
  disclosure was reproduced. The two endpoints differ, and the record states them
  separately — `/v1/users/:uid` withheld the short number via the `Follow` gate at
  `modules/user/api.go:1431-1436`, while `/v1/channels/:id/:type` has no such gate
  and did hand a stranger the superuser's `extra.short_no` plus online state. That
  value is a public seed constant in this repository and carries no PII and no
  capability; `username` and `vercode` were checked and never reached a stranger.
  Two inherited tests had asserted the defective behavior and were rewritten, with
  reverse regressions added on both endpoints for `system` and `customerService`.
  **Operators upgrading**: any human support account sitting on
  `category=customerService` with `robot=0` degrades to the minimal profile set on
  deploy; `SystemBots` is a compile-time literal, so the supported remedies are
  adding the UID to that whitelist, marking the account `robot=1`, or relying on a
  normal relationship path. Focused MySQL-backed runs and the prior authorization
  matrix pass, before and after rebase. See
  [journal](journal/shared/profile-visibility-system-bot-whitelist.md).

## 2026-08-12 (botfather-space-binding-hardening)

- **Task** — `botfather-space-binding-hardening`: made the server authoritative
  for conversational Bot-to-Space binding. Payload/channel Space IDs are now
  selectors only; active creator, Space, and membership are checked fail-closed
  before creation and again under row locks in the membership transaction.
  Missing selectors require exactly one active creator Space. Binding or core
  persistence failures compensate created artifacts and revoke credentials,
  while external failures remain generic and logs remain identifier-free.
  Review hardening scopes friendship compensation to the newly-created
  creator/Bot pair (indexed range access, not a full-table scan), documents the
  binding lock order, and covers zero-row insert and begin-failure paths.
  MySQL-backed regression, end-to-end Space-isolation, race, coverage, vet,
  lint, i18n, cleanup, and redaction checks passed. See
  [journal](journal/shared/botfather-space-binding-hardening.md).

## 2026-08-12 (login-audit-ip-spoofing)

- **Fix** — Task `login-audit-ip-spoofing`: login, account-creation, logout, and
  OIDC audit/guard paths now use the same validated proxy-aware client-IP source
  as shared rate limiting. The stacked server change covers all 15 user and four
  OIDC sources, preserves the audit/wire/quota contracts, and now pins merged
  octo-lib #119 commit `233dd6f`. Review follow-up closes empty-IP callback
  guard bypasses with a stable unknown bucket, directory-wide source guards,
  and a routed user audit assertion. Production CLB/direct-access checks remain
  rollout gates; independent incoming-webhook parsing is a follow-up. See
  [journal](journal/shared/login-audit-ip-spoofing.md).

## 2026-08-11 (token-session-rollout-simplify)

- **Task** — `token-session-rollout-simplify`: collapsed the #725 five-phase
  session rollout into a MySQL-authoritative control plane. A singleton owns
  floor/cap/version/pause, and append-only evidence commits in the same
  transaction as each floor or cap CAS; #725 Redis floor and legacy MODE/MAX are
  one-time takeover inputs only. Runtime mode publication fences issuance,
  applies local state, then atomically publishes writer state + lease; failure
  stays fenced without breaking existing-session reads. Observe, migrate and
  reconciler share a scan-owner lease and `run_id`-bound scanner that discards
  counters on failover. The writer registry still proves fleet convergence;
  empty token keyspace is valid absence evidence, while an empty writer set is
  a blocker. The reconciler ships disabled, and rollback to a Redis-floor-only
  artifact is forbidden after the MySQL floor or cap changes. Tooling remains in
  `app session-rollout`, reads MySQL state directly, and exposes an audited
  `set-cap` path rather than restoring env/Redis authority. See
  [journal](journal/shared/token-session-rollout-simplify.md) and
  [verification](tasks/token-session-rollout-simplify/verification.md). PR #733.
- **Learning (pending)** —
  [characterize-before-you-design](learnings/pending/characterize-before-you-design.md):
  the brief was written from code reading and verified afterwards; the
  verification found a defect that changed a design decision, so it landed as a
  patch. A runnable characterization of current behaviour belongs in Plan as an
  input, split into invariants that must stay green and tripwires that must go
  red.

## 2026-08-10 (token-lifecycle-hardening PR 2)

- **Task** — `token-lifecycle-hardening-pr2`: added an inert-by-default,
  monotonic v3 session rollout; absolute deadlines, generation/fence validation,
  generation-scoped bounded indexes, durable high-risk revocation intents, and
  a controlled legacy migration/admin tool. Review follow-up closed owned v3
  compensation/index leaks, disabled scan-code redemption, campaign resume,
  floor-evidence, lease and post-commit retry gaps; finite migration policy is
  now explicit rather than hardcoded. The production runbook pins Redis,
  connection, old-replica, required-floor and irreversible rollback gates.
  Production activation, device-scope completion, migration, enforce, and
  security retest remain explicit gates. See
  [journal](journal/shared/token-lifecycle-hardening-pr2.md).
- **Learning (pending)** —
  [scope-revocation-cleanup-to-generation](learnings/pending/token-lifecycle-hardening-pr2.md):
  a monotonic authority update does not make later shared-index cleanup safe;
  partition cleanup by the exact revoked generation and test secondary
  invariants such as caps after event replay.

## 2026-08-09 (token-lifecycle-hardening PR 1)

- **Task** — `token-lifecycle-hardening`: bounded all new and touched user HTTP
  bearers with the existing Token TTL, centralized readers/writers in a shared
  Redis Session Store, preserved deadlines atomically across replicas, added
  strict v3 forward validation, startup Lua compatibility probing, bounded pool
  metrics, and a rate-limited aggregate-only migration observer. This PR does
  not bulk-expire historical persistent v1/v2 sessions; v3 activation,
  generation/indexed revocation, migration apply/enforce, and final security
  closure remain PR 2 plus controlled operations. See
  [journal](journal/shared/token-lifecycle-hardening.md).
- **Learning (pending)** —
  [scope-explicit-empty-env-validation](learnings/pending/token-lifecycle-hardening.md):
  distinguish absent from explicitly empty security env values by reading the
  one documented key; never mutate shared configuration semantics as a local
  validation shortcut.

## 2026-08-09 (scan-login-authorization)

- **Task** — `scan-login-authorization`: added a deployment-wide
  `login.scan_enabled` policy gate, split scan from explicit confirmation, bound
  redemption to the browser-issued `poll_secret`, and made confirmation and
  redemption atomic across replicas. Review follow-up added a per-UUID claim so
  different auth codes for one displayed QR cannot both become redeemable,
  made the rollout gate default disabled and fail closed before the first
  successful settings load, and scrubbed confirmation credentials from logs.
  Anonymous polling exposes credential fields
  only to the matching browser and keeps strict per-IP limits; non-finite scan
  limiter values fall back to finite defaults. Incomplete post-consume login work
  emits a bounded warning, while failed QR-state publication restores the pending
  confirmation without overwriting a newer attempt. The OIDC-only production
  rollout keeps scan login disabled before new binaries start and blocks the
  routes during any old/new mixed-version or rollback window. See
  [journal](journal/shared/scan-login-authorization.md).
- **Learning (pending)** —
  [redis-write-errors-have-ambiguous-outcomes](learnings/pending/scan-login-authorization.md):
  a Redis write error does not prove the write was absent; compensation and
  clients must tolerate the committed-write/error-response branch without
  reopening an authentication credential.

## 2026-08-09 (channel-get-object-authz)

- **Task** — `channel-get-object-authz`: `GET /v1/channels/:channel_id/:channel_type`
  (`channelGet`) had no object-level authorization — `loginUID` was a render
  param, never an authz subject, so any authenticated caller could swap
  `channel_id` and read group detail (name/notice/member count/`space_id`) of
  groups they'd left or never joined, sub-channel metadata with no parent
  membership check, and any user's `short_no`/device flags/realname. Added
  per-type gating: GROUP/COMMUNITY_TOPIC require (parent) membership
  (non-member + missing → one `ErrGroupViewForbidden`, no existence oracle);
  PERSON is relationship-graded (self/friend/same-Space/common-group/bot/system/
  webhook → full; unrelated → a minimal whitelist DTO, *not* a 403, since this
  is the sole datasource for rendering arbitrary message senders). Mounted
  `SharedUIDRateLimiter`; fixed a missing-group nil-panic (500 empty body) that
  was itself an existence oracle. Traps: display-layer datasources are not an
  authz layer; a `group_member` row outlives a disbanded group (async cleanup)
  so `ExistCommonGroup` must `JOIN group` and exclude dissolved ones; a datasource
  error had stranded the handler's not-found branch (fixed via the
  `ErrorUserNotExist` sentinel → `ErrDatasourceNotProcess`); a minimal response
  needs its own DTO because `model.ChannelResp` has no `omitempty`. Origin: an
  internal security review (object-level read). `GET /v1/users/:uid` is the same
  root cause through a second door and is fixed in the same change: the decision
  now lives in `modules/channel/service` (dependency-free leaf that
  `modules/user` already imports) so the two endpoints cannot drift, with the
  common-group lookup injected via a registration hook because `modules/user`
  cannot import `modules/group`. The profile endpoint's minimal set keeps
  `follow` (the add-friend entry needs it) where the channel one omits it; both
  emit `status`, because clients read an absent `status` as their banned
  sentinel and persist it, so "absent" had to be decided per field rather than
  applied as a blanket rule.
  Review follow-ups: a public brief must not point at an unpatched sibling
  (fixed by landing both); `status = Normal` was too strict — admin-disabled
  groups are live everywhere else in the module, so the check is
  `<> GroupStatusDisband`; existence checks use `SELECT 1 ... LIMIT 1`. See
  [journal](journal/shared/channel-get-object-authz.md);
  learning [membership-row-outlives-its-parent](learnings/pending/membership-row-outlives-its-parent.md).

## 2026-08-07 (reminder-sync-membership-scope)

- **Task** — `reminder-sync-membership-scope`: `POST /v1/message/reminder/sync`
  returned every channel-level (`@所有人`) reminder in the system to any
  authenticated caller — `channel_id` / `publisher` / `message_id` /
  `message_seq` for channels they had never joined. `remindersDB.sync`'s
  `uid=''` branch had no membership predicate, and an empty client-supplied
  `channel_ids` meant "no filter" rather than "no channels". Channel-level rows
  are now scoped to the caller's active groups
  (`group.IService.ActiveMemberGroupNos`, mirroring `ExistMemberActive`'s
  `is_deleted=0 AND status=Normal`); the client's list can only narrow.
  The 2026-07-30 retest filed this as §4.11 "垂直越权 via X-Space-Id removal" —
  **that attribution would have produced a false fix**: the handler never reads
  the validated `space_id`, and a caller holding a header for a space they do
  belong to got the same dump. The primary regression test therefore keeps a
  valid `X-Space-Id` and still requires non-member rows to be absent. Scope
  covers Group and CommunityTopic (group membership) plus Person (party-hood —
  the recipient's uid *is* the `channel_id`, so no table is needed). Only
  CustomerService / Community / Info remain a knowingly-retained residual, and
  for a different reason: they have no producer anywhere in the server. The gate
  is an allowlist, so an unknown or future channel type fails closed. The
  residual set is pinned by `TestChannelLevelReminderChannelTypes`.
  Membership is matched with bind-parameter `IN`, not a join to `group_member`:
  the two tables can land on different collations per deployment (Error 1267),
  and pinning `COLLATE` costs the index — the trap documented in
  `20260711000001`. `SpaceMiddleware`'s fail-open and the same shape in DM Space
  filtering are deliberately out of scope. `EXPLAIN` is plan-neutral; the query
  was already a full scan on `main` (no `version` index), recorded as a
  follow-up rather than fixed here.
  See [journal](journal/shared/reminder-sync-membership-scope.md).
- **Learning (pending)** —
  [a-repro-is-not-a-root-cause](learnings/pending/a-repro-is-not-a-root-cause.md):
  a reproduction proves reachability, not which missing check allowed it; write
  the primary regression test against the variant that keeps the report's toggle
  intact.

## 2026-08-07 (scanlogin-poll-binding)

- **Task** — `scanlogin-poll-binding`: `loginstatus` no longer hands `auth_code`
  to any anonymous caller who knows the uuid. `loginuuid` mints a `poll_secret`
  (response body only, never in the QR payload) and `loginstatus` releases the
  credential fields only to a caller presenting it;
  everyone else gets the real status filtered through an allow-list. Both
  endpoints — unauthenticated by design, since the QR renders before any token
  exists — gained `StrictIPRateLimitMiddleware`; `auth_code` TTL 10min → 5min;
  the secret is revoked on redemption; the 10s long poll releases on disconnect
  and only reclaims its own channel. Cross-repo: octo-web replays the header.
  **This closes QR-observer hijack, not QRLJacking** — the attacker mints the
  uuid and so receives the secret too. The confirm-screen device context that
  *would* close it was pulled after review: every field of it is
  attacker-controlled today (gin trusts all proxies, so even the IP is
  forgeable), which would have turned weak evidence into false assurance.
  Tracked in octo-ios#71 / octo-android#116. Review also surfaced an auth-code
  expiry inversion that the TTL change had made reachable, a channel-displacement
  vector on the long poll, and post-login state (incl. Signal key material)
  outliving the session — all fixed here. The secret travels as a query
  parameter; a custom header was tried and reverted — it breaks cross-origin
  scan-login for a benefit that only holds against readers who already have
  Redis access. See [journal](journal/shared/scanlogin-poll-binding.md).

## 2026-08-07 (cardtmpl-reasoning-phase-tools-successor)

- **Task** — `cardtmpl-reasoning-phase-tools-successor`: published
  `ai.reasoning-process@0.4.0` carrying the front-end's per-phase collapsible
  tool panels and simplified header, adapted onto the bounded #667/#681 data
  contract instead of the handoff's unbounded schema. Registry default and Bot
  new-send cut to `0.4.0` by image release (not a runtime activation — Bot
  advertisement is a compile-time exact-version constant); `0.1.0`–`0.3.0` stay
  byte-frozen and exact-version editable. The handoff declared itself `0.3.0`,
  colliding with the live `0.3.0`, so the delta had to become a new exact
  version rather than an in-place edit. Three handoff defects were corrected
  rather than adopted: manifest-declared `submit_actions` (it is derived from
  the interaction report here), missing `owner`/`protocol`, and a schema with
  every bound stripped. See
  [journal](journal/shared/cardtmpl-reasoning-phase-tools-successor.md).
  Verify found two things no gate would have caught: the shipped interaction
  reports enumerated `${$index}`-generated toggle ids (true only for a 2-phase
  card — `assertInteractionReport` compares Submit/Input ids only, so an
  indexed id is an unverified published claim), and the "a matching golden
  cannot launder an injected `Action.Submit` in the `octo/v1` result frame"
  invariant had a generic-compiler test but none for this artifact.
- **Decision** — `phases[].thought` gets **two ceilings**: accept `4001`, display `400` with
  server-side truncation (`x-octo-constraints.truncateStrings`, new engine capability). It
  stops mirroring the producer's truncation length either way, and an over-long summary now
  renders clamped instead of failing the card. The display number is a **product decision**
  ("≤ 400 is fine, truncate above it, don't error"); the engine's real contribution is that
  frame size stops depending on caller input — 400 and 4001 produce byte-identical frames.
  Four review rounds to get here, and the fourth invalidated the previous three's arithmetic:
  they measured the render output, but `cardmsg.Finalize` afterwards adds a top-level `plain`
  worth **+47%**, and that is what gets stored. Re-measured on persisted bytes, `thought` turns
  out never to have been the dominant term (at `thought = 1` the worst frame is still **107%**
  of the 64 KiB column; the frame is dominated by 13 actions × (`tool` 81 + `detail` 192) plus
  `plain`), and the live `0.3.0` is **already at 121.6%** at its own bound — pre-existing debt,
  unfixable in place because its bytes are frozen. `tool` / `detail` / `errorMessage` keep their
  zero-headroom design and were knowingly left alone.
- **Decision** — The storage budget moved to the **write boundary**, since no single schema
  version can enforce it. `carddispatch.NormalizeFrameForPersistence` is now the one judge of
  "can this frame be stored", shared by `CardMutator.Mutate` and both `CardUpdater` paths,
  returning `ErrCardMutationTooLarge` (wrapping `ErrCardMutationInvalid`, so existing error
  mapping is unchanged) with the byte count. Covers `0.1.0`–`0.4.0` and every future template;
  turns MySQL `Data too long` / silent truncation into invalid JSON into a typed, logged refusal.
  Still open: an all-fields-at-maximum CJK frame persists at **92.6%** — under 5 KB of margin —
  so `MEDIUMTEXT` widening (own brief, hot table, `ALGORITHM=COPY, LOCK=SHARED`) or truncating
  `tool`/`detail` (a visual change, hence a product decision) remain the durable fixes.
- **Decision (reversed in review)** — D4a: the handoff's chevrons fetched from
  `api.iconify.design` — the repo's first outbound template dependency, permanent once the
  version's bytes freeze, and unreachable from mainland China networks. Now **inlined as vetted
  `data:image/svg+xml` bytes**. The first relaxation of `cardmsg`'s URL allowlist used a
  trusted-caller `ValidateOption` plus a substring denylist; review broke both — the option was
  applied at render but not at the three paths that re-validate before persisting (so every
  `0.4.0` card would send once and freeze on first edit), and the denylist had five reproduced
  bypasses (namespace prefixes, CSS identifier escapes, SVG 1.2 Tiny `<handler>`). Replaced by
  an **exact-byte allowlist**, which is smaller and makes all four problems unrepresentable
  rather than fixed. `data:` stays refused on non-image URL fields and for any unvetted bytes.
- **Learning (pending)** —
  [`schema-bound-must-not-mirror-producer-truncation`](learnings/pending/schema-bound-must-not-mirror-producer-truncation.md):
  a `maxLength` equal to `producer_cap + 1` is a coupling, not a bound — it is
  exactly saturated, and the two sides count in different units (JSON Schema
  code points vs JS UTF-16 code units vs graphemes, measured and confirmed).
  Over-limit means the whole card fails to send, not a display regression.
  Candidate rule: `trust-boundary`.
- **Review corrections (four rounds)** — the first draft of that learning ended "the bound was
  never protecting a real resource", and D3a claimed "~4× headroom" at `thought: 4001`. Round
  one showed both were measured against the wrong ceiling (512 KiB render gate vs 64 KiB storage
  column). Round two rejected keeping `4001` with a boundary refusal for *declared field length*
  — a contract published to *every* bot via `/v1/bot/card/profile` (which also advertises a
  512 KiB payload allowance) must not admit what the store cannot hold, however politely the
  write fails. Round three answered that with the accept/display split. Round four found the
  error under all three: **every byte figure had been measured on the render output, not on the
  bytes that get stored** — `cardmsg.Finalize` adds a top-level `plain` afterwards worth +47%.
  Correcting it reversed two conclusions (`thought` was never the dominant term; `0.3.0` is
  already over) and relocated the enforcement point from the schema to the write boundary. The
  same round found the inline-icon trust boundary applied at one call site out of four, with the
  gap invisible because the edit tests use a fake mutator that never re-validates.
- **Learning (pending)** —
  [`relax-validation-by-artifact-not-by-caller`](learnings/pending/relax-validation-by-artifact-not-by-caller.md):
  a validator relaxed via a trusted-caller flag has to be kept in sync across every path that
  re-validates the same artifact, and nothing types that coupling. Key the relaxation on the
  artifact (these exact bytes are vetted) instead — it cannot drift, it makes sanitizer bypasses
  unreachable, and it forces interpolated values out of the relaxed position by construction.

## 2026-08-06 (bot-setting-store)

- **Task** — `bot-setting-store`: added `bot_setting`, a generic per-bot config
  store, replacing "add another column to `robot`" as the way a bot-level switch
  gets stored. One registry backs the write whitelist, the owner-facing catalog,
  and the bot → `system_setting` → code-default resolution chain, so a new key
  is an entry rather than a migration. First consumer is the four card switches
  (`card_enabled` derived read-only from the deployment env; display /
  interaction / reasoning owner-editable, default true because the master switch
  is already fail-closed). `GET /v1/bot/card/profile` gained one additive
  `config` object; `sendMessage` enforces the same values independently.
  The precedent that looked right was the wrong one — `bot_mention_pref` is a
  table because it is two-dimensional, not because tables are preferred. See
  [journal](journal/shared/bot-setting-store.md).
  Three review rounds across four heads found three P1s, every one a sibling
  path or sibling consumer left behind when a new gate went in: the raw branch
  of `bot/message/edit`, the legacy robot send ingress, and the manifest that
  advertises what the gate accepts. The journal's "What review found that the
  author did not" section records the pattern rather than only the fixes.
- **Learning (pending)** —
  [`cleanalltables-does-not-reset-in-process-caches`](learnings/pending/cleanalltables-does-not-reset-in-process-caches.md):
  generalizes the existing "CleanAllTables does not clear Redis rate-limit
  buckets" note to every non-DB layer, after a process-wide `SystemSettings`
  snapshot leaked between cases and failed only under `-shuffle`.
  Candidate rule: `testing`.

## 2026-08-05 (bot-api-per-bot-ratelimit)

- **Task** — `bot-api-per-bot-ratelimit` (#696): moved bot rate limiting off the
  client-IP axis onto bot identity, and gave the two self-heal channels
  (`heartbeat`, `register`) quotas of their own. The reported cause was wrong in
  a way that changed the fix — the shared axis was IP, not "business vs
  heartbeat", so a bot was starved by a co-located neighbour rather than by
  itself. A follow-on incident showed protecting heartbeat alone is not enough:
  the reconnect path (`register`) was rate limited too, so the bot could notice
  it was down but never get back up. New `pkg/ratelimit` keeps quotas
  hot-tunable (lib's middlewares fix them at construction, which is what made
  the incident's own mitigation cost a 93-second oscillating rollout) and adds
  shadow mode so a candidate quota can be evaluated without touching clients.
  Every **per-bot** layer ships `enabled=false` + `dry_run=true`, so merging
  changes no limiting behaviour. The two pre-auth per-IP floors are the exception
  and are live on deploy — deliberately unswitchable, since a toggleable outer
  layer would leave the excluded heartbeat endpoint unprotected while the inner
  layer is off. Both were first sized at 2x the measured peak and revised upward
  before opening the PR: that is how you size a *quota*, whereas a floor only has
  to turn unbounded into bounded and needs an order of magnitude of headroom. See
  [journal](journal/shared/bot-api-per-bot-ratelimit.md).

## 2026-07-31 (bot-events-longpoll)

- **Feature** — Task `bot-events-longpoll` (card-message-interaction D5 / P3-2):
  `POST /v1/bot/events` gained an opt-in long poll. Bot delivery was cursor short
  polling (one `ZRangeByScore`, immediate return), so card interaction latency
  equalled the bot's poll cadence. **Every** producer that ZADDs into
  `robotEvent:{robotID}` now rings a per-bot doorbell — five sites, including
  the highest-volume `saveRobotMessage`; review found the first revision had
  wired only two, so the invariant is now held by a source guard rather than by
  the docstring that caused the miss — and a caller passing `wait` seconds parks
  on it via BLPOP. New leaf package `pkg/botevent` owns the key format, since
  `modules/bot_api` already imports `modules/robot` and either module would have
  meant an import cycle or a drifting copy.
  **The doorbell is a hint, never the event** — every wake-up re-reads the
  authoritative sorted set from the caller's cursor, so a lost, stolen or stale
  bell costs latency only. `wait` defaults to 0, keeping today's behavior
  field-for-field; the default was decided by the consumer, whose hard 10s client
  timeout would have made a default-on hold abort and log on every poll. Waiting
  uses a dedicated Redis client with an explicit `PoolSize` because BLPOP pins its
  connection and the shared pool has none. No new errcode, i18n entry, endpoint or
  migration — an expired hold reuses the existing OK empty-batch shape.
  Known bounds recorded rather than hidden: drain can be extended by one 5s chunk
  (no module shutdown hook), a whole page of undecodable members starves that one
  request (bounded, never spinning), and hold budgets are per process
  (`maxEventHolds × replicas` fleet-wide).
- **Review rounds 3–4 — the hold loop's progress guarantee.** Four review rounds
  each found a branch of `waitForEvents` that made no progress, so the loop was
  restated as one invariant rather than patched per finding: **every iteration
  either burns a chunk of wall clock or advances the queue cursor, never
  neither.** Concretely: a refused hold now pauses (round 3) under a budget of
  its own so back-pressure cannot become the resource sink (round 4); a failing
  BLPOP pays out its chunk and logs once per hold, instead of retrying at
  go-redis's ~8ms backoff (measured: **924 authoritative reads in 8s**); and the
  cursor advances from the read itself — before the App Bot filter, covering
  undecodable members — so progress no longer rests on an auto-ACK `ZREM` that
  only warns on failure, and the block is skipped only when the cursor actually
  moved (measured without that guard: **38,722 reads in 6s**). Both figures come
  from deleting the fix and re-running the regression tests, which count Redis's
  own `INFO commandstats`. Also: `readEventPage` is now the single seam both the
  immediate and held paths read through; the entry page is threaded into the hold
  so a pre-existing backlog drains instead of waiting for a bell that will never
  ring; `Ring` moved to `rd.NewScript` (EVALSHA) and its failures are logged at
  all five producers; `OCTO_BOT_EVENTS_MAX_HOLDS` is validated at boot and
  documented, with the per-replica connection budget (shared + wait 68 + ring 10)
  in the new `docs/bot-events-longpoll.md`.
- **Review round 5 — the invariant's own counterexamples.** All four reviewers
  approved round 4; the remaining findings were non-blocking and fixed anyway,
  because every one of them is the failure mode this task keeps reproducing: a
  written claim stronger than the code. The stated invariant had two
  counterexamples (a doorbell token whose event lands *below* the caller's
  cursor advances nothing; the entry page's skip was ungated while the in-loop
  one was gated), so it now reads *burns a chunk, **or** advances the cursor,
  **or** consumes a token* and both skip sites share one `eventPage.advanced`.
  The claim that the cursor covers undecodable members narrowed to the truth — it
  clears them only incidentally, when a decodable member with a higher id shares
  the page. `docs/bot-events-longpoll.md` said a hold overshoots by "less than one
  second"; go-redis's `timeout + 10s` command deadline makes the real worst case
  **~45s for a 30s hold**, which is the number an operator sizes a proxy idle
  timeout from. The `event_id == ZSET score` equality the cursor rests on is now
  written down. Finally, round 4's commit message claimed every new test was
  verified by deleting its fix, and that was untrue of one: the failed-ACK test
  re-seeded events after a *successful* ZREM, which is not the read-succeeds /
  write-fails state it named. It is now a real one via an `ackFilteredEvent`
  seam, and the anti-spin test's slack tightened from `+3` to `+1` so the entry
  gap fails it (3 reads where 2 suffice).
- **Review round 6 — the producer side.** Four rounds of hardening had all
  concentrated on the consumer loop; the one blocking finding this round was on
  the producer path, which had received a ring call in round 1 and no
  tail-latency analysis since. `botevent.Ring` was a **synchronous** network call
  inline in `saveRobotMessage`, which runs inside a `msgSem` slot the message
  listener acquires with a *blocking* send on its own goroutine (capacity 100) —
  so ring latency became held slots, and 100 held slots stop bot message fan-out
  process-wide, for every bot, including bots that never long-poll. The tail was
  bounded by nothing: a 10-connection pool against a path admitting 100
  concurrent callers, plus go-redis defaults of 5s dial / 3s read / 4s pool with
  one retry. Not patched with tighter timeouts — the shape changed. Producers now
  call `botevent.Notify`, which does **no I/O on their goroutine**: rings are
  coalesced per bot (`LTRIM 0 0` makes N rings for one bot indistinguishable from
  one, so the queue is bounded by distinct bots rather than message rate), handed
  to a bounded worker pool, and dropped-with-a-counter when saturated — losing a
  hint costs a waiter one chunk, blocking a producer costs delivery for everyone.
  `ringPoolSize` is now derived from `ringWorkers` rather than copied from an
  unrelated convention, with sub-second timeouts and no retries. The repo had
  already rejected the synchronous shape for the same reason in
  `modules/bot_api/auth.go`'s fire-and-forget registry warm-up.
  Also this round: `MaxRetries = 0` on the wait client (a retried BLPOP added
  roughly a chunk beyond the documented worst case); the cursor's stated
  invariant corrected from score **equality** to score **uniqueness**, which
  `GenSeq`'s block allocator does not guarantee across replicas and which would
  silently drop an event — recorded as assumed-and-unverified rather than
  claimed; the read-error exit's asymmetry justified with the client's actual
  backoff behaviour instead of left implicit; `ackFilteredEvent` moved off a
  mutable package global onto `BotAPI`; and `brief.md`'s Out-of-scope line, which
  still forbade self-building an `rd.Client` while the finalised decision
  required it.
- Brief/context under `.octospec/tasks/bot-events-longpoll/`; shared journal
  `.octospec/journal/shared/bot-events-longpoll.md`; learning candidates
  `.octospec/learnings/pending/bot-events-longpoll.md` (lower-bound assertions for
  timing promises → `testing`) and
  `.octospec/learnings/pending/loop-progress-invariant.md` (state the invariant
  instead of answering findings one at a time → `testing`). Consumer half is a
  sibling change in openclaw-channel-octo. PR #685.

## 2026-07-30 (space-join-apply-resubmit)

- **Recoverable join applications** — A pending Space join application now adopts
  a freshly submitted invite code (refreshing the application time and
  re-notifying admins) instead of staying bound to a spent/disabled/expired one,
  so an applicant can repair their own stuck application without an admin
  rejecting it first. Approval-time invite failures are classified (exhausted vs
  invalid), notify the applicant, and share one implementation across all three
  approval entry points. Invite-slot consumption stays at approval time — a
  tracked follow-up. Upstream #683. See
  [journal](journal/shared/space-join-apply-resubmit.md).

## 2026-07-30 (cardtmpl-reasoning-controls-hidden-successor)

- **Safe reasoning successor** — Added immutable
  `ai.reasoning-process@0.3.0` from the bounded V2 contract, removing unsupported
  stop/retry Submit controls while preserving five states, the local reasoning
  toggle, and active/error=`octo/v2`, result=`octo/v1`.
- **Version cutover** — Registry default and Bot new sends now select V3;
  V1/V2/V3 remain available only for same-exact-version historical edits.
  Runtime static reconciliation claims V3 and dynamic same-key collision stays
  fail-closed.
- **Visual-profile contract** — Raw Bot send/edit callers no longer need to
  provide `render_profile`; the Bot API authors `octo-chat/v1` on effective
  frames. Registry callers still cannot override the server-authored manifest
  value. See
  [journal](journal/shared/cardtmpl-reasoning-controls-hidden-successor.md) and
  [brief](tasks/cardtmpl-reasoning-controls-hidden-successor/brief.md).
- **Boundary** — No OpenClaw release, active-run stop/retry, dynamic grant, or
  production gate enablement is included. V3-capable server rollback support is
  required after the first V3 message is sent.
- **Review closure** — The runtime-catalog runbook now forbids routine
  static-to-static Activate/Rollback and requires active-pointer compatibility
  before binary rollback, preventing an older image from treating a persisted
  static V3 target as sticky global integrity failure. Protocol docs now include
  the Bot templating/empty-submit capability; focused tests reject legacy V3
  stop/retry IDs through the Submit and ActionContext gates and caller-owned
  Registry `render_profile`. The reported V1 self-check gap was mutation-tested
  and is already closed by registration-time `cardmsg.Validate`; a regression
  now locks it, and the V3 RouteSpec test directly covers its intended branch.

## 2026-07-29 (inactive-hiding-user-control, Batch 1)

- **Consistency** — Archived 子区 now leave the conversation lists, not just
  `/v1/conversation/sync`: `dropArchivedThreadItems` converges
  `/v1/sidebar/sync`'s **recent tab** on the same `status=active` predicate,
  fail-open on an unknown status. The **follow tab deliberately keeps them** —
  its response is the clients' only source of `is_followed`, so filtering it
  server-side would report an already-followed archived thread as unfollowed and
  make unfollow impossible; that tab's display filtering stays with the clients.
  Extends the direction XIN-1135 set for the self-created
  exemption to the paths it left open. See
  [journal](journal/shared/inactive-hiding-user-control.md) and
  [brief](tasks/inactive-hiding-user-control/brief.md).
- **Config** — Thread auto-archive policy (`enabled` / `days`) moved from
  env-only to `system_settings` with DB → env → code-default resolution and no
  migration-written row, so rollout is behaviour-identical. The worker re-reads
  policy per tick (no restart to change it) and `effective_value` makes the
  running window queryable for the first time.
- **Invariant** — `archive_days >= recent_filter_thread_days` enforced on both
  keys' write entry points against the post-merge state: the two windows are a
  two-stage decay, and inverting them makes the recent-tab window silently
  unobservable.
- **Safety** — Unread / pinned / system-bot conversations are now exempt from
  the inactivity window on both endpoints. A window that can swallow unread
  cannot be handed to users, so this is a precondition for the per-user windows
  in Batch 2, not a follow-up.
- **Learning (pending)** — A cross-key `system_setting` merge guard must resolve
  an empty payload as reset-to-default; carrying the current value forward lets
  a clearing write land in the state the guard exists to reject. See
  [learning](learnings/pending/system-setting-empty-payload-is-reset-not-keep.md).

## 2026-07-29 (cardtmpl-runtime-catalog-overlay, roadmap E3 PR-B server)

- **Runtime overlay** — Added one composition-root RuntimeCatalog over frozen
  built-ins and the immutable MySQL artifact store, with authoritative
  exact/default resolution, bounded compiled caching, and fail-closed dynamic
  authorization. See [journal](journal/shared/cardtmpl-runtime-catalog-overlay.md),
  [brief](tasks/cardtmpl-runtime-catalog-overlay/brief.md), and Issue #672.
- **Audited state machine** — Added revision-CAS activate, explicit prior-active
  rollback, one-way emergency block with fallback/disable, manager detail/audit,
  and transactional state-plus-audit persistence.
- **Runtime consumers** — Migrated Bot template send/edit, notify, CardUpdater,
  and message action-context to the catalog interface with server-authored
  purpose/principal/Space context. Control and dynamic new-send remain dark and
  no production grants are installed.
- **Recovery hardening** — Startup reconciliation is asynchronous and retryable;
  integrity remains sticky while super-admin detail/audit diagnostics stay
  readable. Static interactive rollback targets no longer inherit the dynamic
  RouteSpec precondition, and notify catalog DB calls are deadline-bounded.
- **Current-head review closure** — Catalog startup state now participates in
  `/v1/ready`; active-target validation has a 128-target hard cap, independent
  per-target deadlines, and a bounded gauge. Notify preserves and fail-closes
  runtime catalog safety errors, Bot construction lookups are deadline-bounded,
  and audit paging/prior-active lookups have supporting indexes. Trusted
  producer provenance is explicitly deferred as a PR-C grant prerequisite.
- **Trust-boundary closure** — Runtime schemas now reject non-empty
  `patternProperties`; `additionalProperties=false` alone does not bound regex
  keyspaces.
- **Verification boundary** — Core cardtmpl/catalog coverage exceeds 80% and all
  focused, race, clean-DB integration, build, vet, lint, i18n, and diff gates are
  green locally. Legacy Bot/notify/message whole-package coverage remains below
  80%, so that literal brief checkbox stays open. PR #674 merge and the PR-B
  rebase are complete; post-rebase CI/current-head approval, E1d/E1e, PR-C
  grants, joint E2E, and production enablement remain pending.
- **Learning (pending)** — A fail-closed runtime must retain an authenticated,
  bounded diagnostic read path. See
  [learning](learnings/pending/cardtmpl-runtime-catalog-overlay.md).

## 2026-07-28 (cardtmpl-runtime-catalog, roadmap E3 PR-A)

- **Dark publishing foundation** — Added the shared strict JSON artifact
  compiler, deterministic canonical identity, immutable static/dynamic version
  claims and artifacts, transactional publish audit, and startup static
  inventory reconciliation. See
  [journal](journal/shared/cardtmpl-runtime-catalog.md),
  [brief](tasks/cardtmpl-runtime-catalog/brief.md), and PR #674 / Issue #669.
- **Control plane** — Added super-admin-only, authenticated and UID-rate-limited
  validate/publish endpoints with a 2 MiB body cap, localized safe errors, and
  bounded operation/compile/DB metrics. Published artifacts remain inactive and
  are not read by production render/send/edit paths in PR-A.
- **Fail-close hardening** — Runtime manifests now align identity lengths with
  persistence columns, reject ambiguous SemVer/Unicode identities, resolve local
  schema refs (including array items) with cycle detection, bind samples within
  their declaring view, and expose canonical manifest metadata.
- **Review hardening** — Added full static/runtime canonical parity coverage,
  contention-safe static-claim upsert plus bounded retry, and a distinct internal
  error/metric classification for persistent catalog-integrity failures. The
  bounded-schema proof is now a context-aware, visit-budgeted single traversal;
  publish DB work has a 10-second deadline, bounded whole-transaction retry for
  MySQL 1205/1213, and separate low-cardinality failure outcomes.
- **Final review closure** — Unified golden/runtime number semantics, made
  canonical integer limits notation-independent and recompile-safe, rejected
  open-keyspace `patternProperties`, preserved `allOf` traversal-abort signals,
  fixed mixed exact/positional sample assignment, made validation-document
  selection deterministic, removed UTF-16 sort allocations and a redundant
  envelope/bundle decode pass, and durably audited immutable/static-source
  publish conflicts without mutating artifacts. A real-MySQL concurrency test
  now verifies same-hash idempotency, different-hash immutability, audit results,
  and migration Up/Down cleanup.
- **Startup recovery** — Kept exact-key reconciliation fail-closed and documented
  the safe PR-A break-glass path in the
  [runbook](../docs/card-template-runtime-catalog-runbook.md): roll back or
  correct the conflicting image and assign a new built-in version; never delete,
  rewrite, or bypass permanent claim/artifact state.
- **Learning (pending)** — Artifact validation must prove both persistence
  compatibility and worst-case schema resource bounds; unknown or union schema
  forms cannot be treated as bounded by omission. See
  [learning](learnings/pending/cardtmpl-runtime-catalog.md).
- **Boundary** — activation/rollback/block/runtime overlay remain PR-B; grants,
  B1/B2, and Bot capability merge remain PR-C. Object keyspace bounds and strict
  untrusted render-field decoding must close before dynamic activation.

## 2026-07-24 (bot-card-template-consumption, roadmap E1b)

- **Protocol** — Added the explicit Bot Registry template catalog to
  `/v1/bot/card/profile` and Registry-backed `template_ref + state + data`
  modes to Bot send/edit. Server owns view/profile/render metadata/Space/plain;
  raw Model B remains supported under a total XOR. See
  [journal](journal/shared/bot-card-template-consumption.md) and
  [brief](tasks/bot-card-template-consumption/brief.md).
- **Mutation safety** — Registry edits retain immutable template id/version and
  reuse `CardMutator` ownership, lifecycle, positive `card_seq` CAS, revision,
  and CMD paths. Transient frames skip revision history. Server-authored
  `template_ref` provenance plus metadata equality prevents raw cards from
  entering the Registry edit path.
- **Fail-close** — Only explicitly catalogued templates are advertised/accepted;
  JSON-template interaction reports are checked against rendered v2 samples at
  registration; invalid or forged requests have zero dispatch/mutation effects.
- **Learning (pending)** — JSON mutual exclusion must use raw key presence, not
  decoded zero values; otherwise empty string/null can bypass the both-present
  guard. See
  [learning](learnings/pending/bot-card-template-consumption.md).
- **Rollout boundary** — frozen `ai.reasoning-process@0.1.0` remains registered,
  but a bounded successor + Bot catalog cutover (or an explicitly disabled Bot
  card sub-gate) is required before Model A production enablement.

## 2026-07-24 (bot-send-permission-error-classification)

- **Error classification** — Bot API sends to a missing group or missing thread
  parent now reuse `err.server.bot_api.group_not_found` (wire 400, semantic
  404); real group-status and membership query failures remain internal
  `query_failed` results. Existing DM, Space, disbanded-group, and non-member
  denials are unchanged.
- **Observability and privacy** — added the bounded
  `dmwork_bot_send_permission_failure_total{stage,reason}` counter and one
  request-correlated terminal log without raw Bot/user/channel/group/thread/
  Space identifiers. OBO friend-gate lookup failures retain `not_friend`
  fail-closed behavior while using the same sanitized observer.
- **Verification** — handler tests cover group and parent-group absence, real
  DB failure, D14 wire semantics, trace correlation, and zero dispatch; focused
  tests cover DM/App Bot/Space/OBO outcomes and metric cardinality. All build,
  test, vet, lint, and i18n gates are green. See
  [journal](journal/shared/bot-send-permission-error-classification.md).
- **Learning (pending)** — when fail-closed authorization intentionally
  collapses an infrastructure error into a business denial, preserve a bounded
  internal diagnostic signal and emit it once at the request boundary. See
  [learning](learnings/pending/fail-closed-diagnostic-signal.md).

## 2026-07-24 (cardtmpl-reasoning-progress-card, roadmap E1a)

- **Feature** — Onboarded `ai.reasoning-process@0.1.0` as the first live JSON-mode
  card on the E1 engine (#654), via `Registry.RegisterJSON` (no Go `Build()`).
  Decision A: the producer's action buttons are kept (`reasoning_stop` /
  `reasoning_retry` `Action.Submit` + toggle) under a fixed owner `ai` /
  action_type `reasoning.control` (added `ai` to the L2a allowlist); `active`/
  `error` are octo/v2, the display-only `result` view is octo/v1 (mirrors
  docs.access-request 0.3.0). `Submit.data` carries owner/action_type so the
  ActionContract self-check passes; goldens synced. The button handler +
  RouteSpec + bot streaming delivery are downstream — the card is registered and
  renderable only. See [journal](journal/shared/cardtmpl-reasoning-progress-card.md).

## 2026-07-23 (cardtmpl-json-template-engine, roadmap E1)

- **Feature** — Added a JSON-template render path to `pkg/cardtmpl` (roadmap E1):
  a bounded Adaptive Card Templating engine (`pkg/cardtmpl/jsontmpl/`) + a generic
  `jsonTemplate` + `Registry.RegisterJSON`, so a card can register and render via
  `Registry.Render` from a JSON handoff without a hand-written Go `Build()`.
  Validated byte-for-byte against the `ai.reasoning-process@0.1.0` goldens. Made
  `BuildResult.DeepLink` optional (D7) for cards with no canonical URL. The 5 Go
  cards are untouched. See
  [journal](journal/shared/cardtmpl-json-template-engine.md); learning candidate
  `learnings/pending/template-engine-literal-bind-validate-backstop.md`.

## 2026-07-23 (cardtmpl-l2a-summary-migration PR-2)

- **Feature** — Roadmap C second slice: `summary.completed@0.1.0` and
  `summary.failed@0.1.0` (v1 display cards) migrated onto `Registry.Render`
  reusing the PR-1 (#649) copy-directory shape and the PR-1
  `BuildSummaryResourceCardBodyWithLang` scaffold. `NotifyReq` /
  `SummaryCardFields` external shape unchanged (plan B);
  `deliverCardNotification` routes both kinds through Registry with F7
  fail-close and C1 preflight, matching the docs display cards. Both
  manifests declare `renderProfile` + `renderProfileCompatibility` (#647).
- **Milestone** — After PR-2 the Registry holds 5 L2a cards
  (docs.access-request v2 + docs.commented v1 + docs.shared v1 +
  summary.completed v1 + summary.failed v1); every legacy display-card
  deliver branch is Registry-backed. Only `generic.approval` remains legacy
  (dynamic-owner conflict, tracked separately).
- **Test** — `card_via_registry_summary_baseline_test.go` runs 4 fixtures
  through legacy `buildSummaryCard` vs `buildSummaryCardViaRegistry` and
  asserts canonical byte-equality after stripping
  `metadata.octo.{protocol,template}`; C1 preflight and F7 unwired assertions
  extend the pattern PR-1 established. `TestMain` grows to register the two
  new cards. See [journal](journal/shared/cardtmpl-l2a-summary-migration.md).

## 2026-07-23 (group-welcome-message)

- **Feature** — Group new-member welcome (群入群欢迎语): a group's
  creator/manager configures **one** welcome via `GET/PUT/DELETE
  /v1/groups/:group_no/welcome` (new `octo_group_welcome_config`, insert-then-lock
  `UpsertMerged`), and on a human member's **first** join it is **posted publicly
  into the group channel** (`channel_type=GROUP`), at most once per `(group_no,
  uid)` via the new `octo_group_welcome_delivery` ledger. The body supports a
  `{member}` placeholder rendered to the joiner's display name. Delivery mirrors
  the per-Space engine (reconciler + worker + rotating cursor + `FOR UPDATE SKIP
  LOCKED` + CAS/at-most-once) as a **parallel, group-scoped** copy — not a
  refactor of the reviewed space code. **No platform-global content fallback**: a
  group with no enabled row gets no welcome.
- **Config** — new master switch `onboarding.group_welcome_enabled` (bool,
  **default off** = dark launch), read via `SystemSettings.GroupWelcomeEnabled()`,
  checked at enqueue/reconcile/worker. Enablement only — no content fallback;
  flip-off is an instant, reversible kill that touches no per-group rows. Ships
  double-inert (master off AND per-group `enabled=false` by default).
- **Behavior** — burst coalescing: a batch invite (one `GroupMemberAdd` event → N
  rows) is delivered as **one** public post naming everyone (`{member}` → joined
  list; overflow → `…、nameK 等 N 人`) instead of N posts. Freshly-enqueued rows are
  held a short coalesce window via `next_retry_at`; the worker `claimBatch`es a
  group's due rows and posts once, preserving per-row at-most-once (shared
  `message_id`). One coalesced post per group per wake also rate-limits a single
  group's welcomes.
- **Test** — full `-race` suites green for `common`/`notify`/`group`/`space`;
  committed **live e2e** (`group_welcome_e2e_test.go`) drives the whole pipeline
  against real WuKongIM, confirming `notification` posts into a group channel it
  is not a member of and that a burst coalesces to one `message_id`. Incidental
  polish carried from #646 (Upsert doc caveat, `r.Enabled` read, request-context
  DB, DELETE→enabled-global-fallback test). See
  [journal](journal/shared/group-welcome-message.md).

## 2026-07-23 (cardtmpl-l2a-migration PR-1)

- **Feature** — Roadmap C first slice: `docs.commented@0.1.0` and
  `docs.shared@0.1.0` (v1 display cards) migrated onto `Registry.Render` via
  the pilot copy-directory shape. `NotifyReq`/`DocsCardFields` external shape
  unchanged (plan B); `deliverDocsCardNotification` routes both kinds through
  Registry with F7 fail-close, matching `access_requested`. Both manifests
  declare `renderProfile` + `renderProfileCompatibility` (#647).
- **Milestone** — §2.2-5 L2b hard gate ② is now met: `docs.access-request`
  (v2) + `docs.commented` (v1) + `docs.shared` (v1) = 3 L2a cards running
  the full Registry path across both wire profiles.
- **Base helpers** — `pkg/cardtmpl.SanitizeLine` is the single source of
  truth (G5, notify + pilot become wrappers).
  `BuildSummaryResourceCardBodyWithLang` added to scaffold PR-2.
- **Test** — `card_via_registry_display_baseline_test.go` runs 4 fixtures
  through legacy `buildDocsCard` vs `buildDocsDisplayCardViaRegistry` and
  asserts canonical byte-equality after stripping the two injected
  `metadata.octo` fields. `modules/notify/testmain_test.go` wires the
  Registry once for the whole test package (mirrors production wiring). See
  [journal](journal/shared/cardtmpl-l2a-migration.md).
- **Review fix (P1, PR #649)** — Over-length display fields were flipping to a
  400 zero-delivery under the C1 preflight where legacy truncated & delivered.
  `mapDocsCardFieldsToDisplayJSON` now server-side truncates title/actorName/
  excerpt/updatedAt to the schema/render caps before preflight (delivery
  preserved); `docId` stays a hard C1 400 (deep-link key). Exported
  `cardtmpl.MaxTitleRunes` + `cardtmpl.TruncateRunes` (single cap/impl, G9);
  added `TestSchemaCapsMatchRenderCaps` (closes the previously-dangling G9
  field-cap reference) + `TestMapDocsDisplayFields_TruncatesDisplayFields` +
  `docs_shared` en test; made the docs build-error log label kind-generic.

## 2026-07-22 (incoming-webhook-quota-per-thread)

- **Behavior change** — Incoming Webhook creation quota re-scoped from
  *per parent group* to *per delivery scope* `(group_no,
  thread_short_id)`: the group itself and each thread (子区) now hold
  independent webhook budgets instead of sharing one per-group cap.
  `insertWithQuota` narrows both the group-level and per-creator
  `COUNT(*)` by `thread_short_id`; the `FOR UPDATE` serialization lock
  stays on the parent `group` row (narrowing it would reintroduce the
  gap-lock deadlock). Motivated by Octo Loop provisioning a webhook per
  thread. Supersedes the `incoming-webhook-thread` task's locked
  "threads share `max_per_group`" decision.
- **Config** — `incomingwebhook.max_per_group` /
  `incomingwebhook.max_per_creator` keep their keys and defaults
  (10 / 5) but are reinterpreted "per delivery scope"; setting docs +
  admin schema descriptions + the two 409 quota messages (en-US markers
  + zh-CN) updated. No schema/data migration; existing rows
  (`thread_short_id=''`) fall into the group-self bucket.
- **Config (follow-on)** — added two precise-control knobs:
  `incomingwebhook.max_per_thread` (per-thread scope cap, decoupled from
  `max_per_group`; falls back to it when unset) and
  `incomingwebhook.max_total_per_group` (group-wide aggregate ceiling
  across the group + all threads; `0`=disabled default). `insertWithQuota`
  evaluates all three quota layers inside the one parent-group `FOR UPDATE`
  critical section (race-exact; verified by a concurrent aggregate-ceiling
  test under `-race`). New 409 `mgmt_total_quota_exceeded`. See
  [journal](journal/shared/incoming-webhook-quota-per-thread.md).

## 2026-07-22 (cardtmpl-interaction-closure)

- **Feature** — Closed the post-#633 interactive-card loop (roadmap group A).
  `CardUpdater` (`ReplaceView` + progress-frame `Append`) composes the existing
  `CardMutator` CAS/revision/CMD path; `docs.access-request@0.3.0` adds an
  `approved`/`rejected` `result` view (`octo/v1`) registered beside the frozen
  `0.2.0`; the docs finalizer now upgrades approved/denied cards in place to
  `0.3.0/result`. In-flight `0.2.0` pending cards upgrade too — missing
  decorative fields are omitted, not fabricated.
- **Contract** — Route-versioned callback envelope: `legacy` flat body remains
  the default (byte-compatible), `octo-card-v1` opt-in nested envelope carries
  `protocol`/`type=card.action`/`card.{…}`/`trigger_id`; `response_url` stays
  reserved (no authenticated response body defined in §7). See
  [journal](journal/shared/cardtmpl-interaction-closure.md).
- **Observability** — Bounded counters `dmwork_cardtmpl_callback_total`
  (`ok|rejected|error`) + `dmwork_cardtmpl_update_total` (`ok|error`); labels
  only from registered metadata + declared interactions.
- **Learning (pending)** — `card_seq` for authoritative updates must come from
  a monotonic source; the docs finalizer reuses `event.EventID`, an implicit
  contract now documented. See
  [learning](learnings/pending/cardtmpl-interaction-closure.md).

## 2026-07-22 (cardtmpl-registry-pilot)

- **Feature** — Introduced the octo-card@1.0 platform base
  (`pkg/cardtmpl.Template` + `Registry` + `Registry.Render`
  8-step pipeline) and migrated `docs.access-request@0.2.0` as the
  first L2a pilot. `metadata.octo.{protocol,template}` are now
  injected by the base on every payload rendered through the registry
  (docs approval-request cards, initially).
- **Contract** — `docs/platform-card-base.md` is added as the L0
  authoritative contract; `docs/l2b-owners.md` reserves the empty L2b
  owner allowlist. Handoff artefacts (manifest / contract /
  samples / reports) live at
  `pkg/cardtmpl/docs_access_request/handoff/docs.access-request@0.2.0/`
  and are the machine-readable cross-repo reference.
- **Behavior change** — For docs `access_requested` cards with the
  approval gate on, schema-level field errors returned by
  `Registry.Render` (typed `cardtmpl.ErrFieldsInvalid`) now become
  **HTTP 400 zero-delivery** rather than degrading to a plain-text DM
  (C1 policy).
- **Fix / hardening** — Rewrote the pilot `pending.interaction.json`
  to match real Go action IDs and dataKeys, so the A15c interaction
  contract lock is code-vs-report equality instead of a
  design-phase-vs-code superset check.
- **Learning** — Deposited
  `cardtmpl-registry-pilot.md` under `.octospec/learnings/pending/`:
  a handoff schema authored for a *full compiled card* is NOT the
  same as a caller-input schema and should not be wired unchanged as
  the Registry input contract.

## 2026-07-21 (space-welcome-per-space-admin-crud)

- **Feature** — The onboarding welcome message became **per-Space and
  self-service**. Space admins (Role>=1) CRUD one config per Space via
  `GET/PUT/DELETE /v1/space/:space_id/welcome` (new `octo_space_welcome_config`
  table + `common.SpaceWelcomeConfigStore`). Follows #604/#606 which shipped a
  single platform-designated Space, superadmin-only; lifts that task's
  out-of-scope "per-Space admin self-service" item into scope.
- **Precedence** — A present per-Space row wins over the platform-global config
  outright, even when disabled (opt a Space out of a global campaign); no row →
  the global config applies iff it names the Space; else off. Global config kept
  as a superadmin fallback. Ships `enabled=false` per Space (no behavior change).
- **Delivery driver** — notify's event/reconciler/worker went single-Space →
  all-enabled-Spaces, resolving the per-Space effective config each cycle. Both
  reconciler and worker rotate a per-replica cursor over the enabled set for
  fairness (a greedy in-order worker starved tail Spaces under sustained load —
  see [learning](learnings/pending/multi-space-worker-rotating-cursor.md)).
  Cross-space sweep added (`idx_sweep`); ledger state machine / at-most-once /
  sender identity unchanged.
- **Verified** — all gates + `-race` suites green; real-wire e2e against live
  MySQL/Redis/WuKongIM confirmed actual receipt and no cross-Space mixing (each
  recipient's channel read back from the IM). See
  [journal](journal/shared/space-welcome-per-space-admin-crud.md).

## 2026-07-20 (route-missing-retry)

- **Fix** — Card-action dispatch (`internal/cardactiondispatch`) now **defers** a
  `route_missing` at dispatch time (no attempt consumed) instead of dead-lettering on
  the first attempt. An event only enters the queue when its route existed at enqueue
  time, so a miss at dispatch means the process restarted into a run whose
  `OCTO_CARD_ACTION_ROUTES` lacked the route while the durable queue carried the event
  across — previously a permanent, non-self-healing DLQ that read at the UI as docs
  approve/deny cards never updating. Deferring (rather than nacking) matters: a nack
  spends `route.MaxAttempts`, so the event would trip `attempts_exhausted` the moment
  its route returned. Within `routeMissingMaxWindow` (15m) the event waits and then
  dispatches on its original attempt budget; past the window it dead-letters
  (`reason=route_missing`) so a genuine misconfiguration stays visible. The attempt-budget
  interaction was caught by an `xhigh` code review of the first (nack-based) cut. See
  [brief](tasks/route-missing-retry/brief.md) · [journal](journal/shared/route-missing-retry.md).
- **Learning (pending)** — `durable-queue-registry-divergence`: a durable/shared work
  queue consumed against per-process, startup-loaded config can dead-letter valid work
  across a config-divergent restart; treat "config absent at consume time" as a bounded
  retry, not a first-attempt DLQ.
- **Change (config)** — Card-action DLQ retention is now configurable via
  `OCTO_CARD_ACTION_DLQ_RETENTION_DAYS` (whole days, 1–365) through a shared
  `cardactiondispatch.DLQRetentionFromEnv` resolver used by both `main.go` and
  `tools/card-action-dlq` (so they can't drift). **Default stays 30 days** (the pre-change
  value), so an upgrade that doesn't set the override keeps the existing recovery window and
  never prunes older DLQ entries on first deploy; set the env to a smaller value (e.g. `7`) to
  opt into a shorter window. Doc updated.
- **Fix (review round, PR #621, 4 reviewers)** — three blocking corrections folded in:
  (1) a `route_missing` event with a non-positive `ActedAt` now **dead-letters immediately**
  instead of deferring forever (the wait is bounded by elapsed-since-`ActedAt`, so an unset
  timestamp had nothing to measure against and re-deferred every 5s indefinitely);
  (2) the DLQ-retention default was kept at **30 days** rather than lowered to 7 (the running
  server's lazy prune would otherwise silently delete 8–30-day-old DLQ entries on first deploy);
  (3) the `card-action-dlq` CLI's read-only `depth` no longer prunes (new `DepthsNoPrune`), so
  inspecting the DLQ can't delete recoverable entries. The metric-noise nit (per-re-check
  `observeError`) was left as documented-intentional.
- **Fix (review round 2, PR #621 re-reviews)** — two further blocking corrections folded in:
  (4) the bounded route-missing window is now anchored on the **first observed miss** (a durable
  per-event `route_missing_since` marker via `RouteMissingSeenAt`), not on `Event.ActedAt` — an
  event that dwelt in the durable queue past the window before its first dispatch (long
  restart/outage/backlog) now still defers on its first transient miss instead of dead-lettering
  immediately; this supersedes round 1's `ActedAt<=0` special-case (the marker is always a real
  stamp, so that edge is gone by construction), and `ReplayDLQ` clears the marker so a replayed
  event starts fresh; (5) the `card-action-dlq replay` path is now **non-destructive** — an entry
  past the CLI's resolved retention is refused without being deleted, so the running server stays
  the single pruning authority (a shorter CLI window can no longer silently destroy a
  server-retained entry).
- **Fix (review round 3, PR #621 re-review)** — the round-2 first-miss marker
  (`route_missing_since`) leaked: it is one shared Redis hash with a whole-hash TTL (no per-field
  expiry), refreshed on every miss, so under sustained route-missing traffic a field per COMPLETED
  event accumulated unbounded (it was cleared only on replay, not on delivery or dead-letter).
  Fixed by `HDEL`-ing the marker on every exit transition (`ackScript`, `nackScript`
  requeue+dead-letter, and the existing `replayDLQScript`); a new Redis-backed lifecycle test proves
  the field is gone after Ack and after terminal dead-letter. Also folded in two doc-drift fixes (a
  stale CLI "refuses (and prunes)" comment; the pending learning's `ActedAt`-based deadline →
  first-observed-miss, plus a new marker-lifecycle-vs-whole-key-TTL point).

## 2026-07-20 (github-webhook-parity)

- **Feature** — GitHub `pull_request`/`issues` InteractiveCards gained
  Source/Target branch (PR) + Labels(N) FactSet rows, mirroring the GitLab
  MR/Issue cards from `gitlab-mr-issue-cards` earlier the same day.
- **Behavior change** — GitHub adapter no longer filters
  `pull_request`/`issues`/`issue_comment`/`release` events by action
  (explicit product decision, mirroring the GitLab one); every action now
  renders on both text and card paths.
- **Fix** — Applied the `gitlab-mr-issue-cards` task's pending learning
  (whitelist-gate-as-implicit-sanitizer) proactively: every field the filter
  removal exposed was escaped in the same commit (verified by enumerating
  and grepping every call site before committing, not discovered via a
  later review round). Also folded in a pre-existing, previously-unfixed
  escaping gap in `ghLogin`/`ghWithRepo` (GitHub's twin of GitLab's already-
  fixed `glActor`/`glWithRepo`), for adapter parity. Renamed the shared
  `glCappedFactValue` helper to `cappedFactValue` since GitHub's new Labels
  fact now calls it too. See
  [journal](journal/shared/github-webhook-parity.md).

## 2026-07-20 (gitlab-mr-issue-cards)

- **Feature** — GitLab merge_request/issue InteractiveCards gained a
  Source/Target branch (MR) + Labels(N) FactSet, mirroring the existing
  pipeline card. Card-only; text degrade path unchanged.
- **Behavior change** — GitLab adapter no longer filters MR/Issue events by
  action or pipeline events by status (explicit product decision); every
  action/status now renders on both text and card paths.
- **Fix** — A follow-up code review found the filter-removal had silently
  reopened a markdown/link injection: `glActionVerb`'s raw-passthrough
  fallback for unmapped actions was interpolated unescaped. Fixed by escaping
  at every call site; also deduped the pipeline Jobs / new Labels fact
  cap-and-join logic.
- **Fix** — A PR review (lml2468, PR #610) then found the exact same bug
  class on the sibling field the first fix missed: GitLab pipeline `status`
  also lost its whitelist gate in the same commit, and was still interpolated
  raw on the text path. Fixed identically. See
  [journal](journal/shared/gitlab-mr-issue-cards.md) and the pending learning
  on whitelist-gates-as-implicit-sanitizers (updated with this recurrence).
- **Fix** — Re-review (yujiawei, PR #610) found the same class of bug a third
  time, pre-existing in `glActor`'s `username` branch (byte-identical to
  `main`, not introduced by this task, but folded into the same fix pass):
  it assumed GitLab's restricted username charset made escaping unnecessary,
  which does not hold at this trust boundary (the endpoint only checks a
  shared secret, not that the payload is genuinely from GitLab). Also
  addressed two non-blocking review nits (mochashanyao, PR #610): a
  distinguishing `>` prefix when `formatPipelineDuration` clamps a hostile
  value, and a dedicated `cardFactItemMax` constant instead of reusing the
  actor-name clamp for Jobs/Labels fact items (yujiawei, PR #610).

## 2026-07-17 (docs-approval-card-enrich)

- **Feature** — Enriched the docs access-request approval card (header + colored
  status, big title, requester row with optional avatar, boxed reason) across
  pending + terminal states, and added a reviewer deny-reason dialog whose value
  rides a declared hidden `deny_reason` input through
  `DecisionRequest.Inputs` to the docs backend. Additive optional
  `DocsCardFields.actor_avatar_url` (https-validated). Cross-repo (octo-web deny
  dialog). See [journal](journal/shared/docs-approval-card-enrich.md).

## 2026-07-16 (space-new-user-welcome-message)

- **Feature** — At-most-once Space welcome DM from the `notification` bot on a
  human user's first join to a designated Space. New `octo_space_welcome_delivery`
  ledger (migration in `modules/notify/sql/`; `notify/1module.go` gains
  `//go:embed sql` + `SQLDir`), a 60s reconciler and a single-row send worker
  (claim via `FOR UPDATE SKIP LOCKED`, CAS guarded by `status + claim_owner`,
  `attempts` grows only on pre-IM failure with backoff {5s,30s,120s}→failed,
  any post-dispatch failure → `unknown` never retried). Config is five
  `system_setting` keys under `onboarding`; `modules/common` gains an atomic
  `SpaceWelcomeConfig()` snapshot accessor + prospective composite validation on
  the manager write path + i18n code `err.server.common.space_welcome_config_invalid`.
  A notify-local 15s context-aware HTTP sender replaces octo-lib's timeout-less
  helper (octo-lib unmodified). `active_from` vs `space_member.created_at`
  compared via `UNIX_TIMESTAMP` (mirrors `modules/opanalytics`). Observability
  kept minimal (in-process counters + logs). Ships `enabled=false`; three
  product/ops sign-off items gate turning it on. Brief under
  `.octospec/tasks/space-new-user-welcome-message/`; shared journal
  `.octospec/journal/shared/space-new-user-welcome-message.md`.

## 2026-07-16 (card-action-internal-http-actions)

- **Follow-up** — Two small extensions to #588 plus one bundled config
  collapse. `OCTO_CARD_ACTION_ROUTES[].url` now accepts `http://` in addition
  to `https://`; hostname form is intentionally not inspected (route
  registration = operator authorization). URL validator tightened at the same
  time: `Hostname() != ""` (blocks `http://:8080/x`), `ForceQuery` (blocks
  trailing `?`), raw-`#` prefilter (blocks trailing/embedded `#`).
  `OCTO_CARD_ACTION_ALLOWED_URLS` is deleted from code paths and emits a
  structured deprecation WARN if still set, so rolling upgrades do not fail.
  `approval_card.actions` grew an optional 1..5 bounded slice: server-derived
  action IDs, reserved metadata enforced, control-character-in-title checked
  on the raw string, `nil` preserves byte-for-byte legacy approve/deny while a
  non-nil empty slice is rejected as a caller bug. Callback wire contract
  (states, requester notification, HMAC canonical) is deliberately unchanged.
  Coverage — cardactiondispatch 81.5%, cardtmpl 89.9%, notify 71.2%. Brief/context
  under `.octospec/tasks/card-action-internal-http-actions/`; shared journal
  `.octospec/journal/shared/card-action-internal-http-actions.md`.

## 2026-07-16 (webhook-cardmsg-adapter)

- **Feature** — The GitHub/GitLab incoming-webhook adapters render their event
  subset as `InteractiveCard` (=17) octo/v1 cards (structured header + body + a
  "View on {GitHub|GitLab}" `Action.OpenUrl`) when `OCTO_CARD_MESSAGE_ENABLED`
  is on, and degrade to the untouched markdown text path when off (flag-off wire
  byte-identical). New `adapter_card.go` holds the shared card anatomy + one leaf
  escaper + http(s) allowlist + self-validate/degrade selector, used by both
  adapters (trust-boundary parity). GitLab pipeline cards render a
  Branch/Status/Duration/Jobs FactSet (parses `duration` + `builds[]`, card-only).
  Server-only: octo-web already ships the octo/v1 renderer + `iwh_` sender trust.
  Brief/context under `.octospec/tasks/webhook-cardmsg-adapter/`; shared journal
  `.octospec/journal/shared/webhook-cardmsg-adapter.md`.

## 2026-07-13 (card-message-appbot-trust)

- **Fix** — Closed the P0 App Bot card trust split without changing the send
  pipeline: added a cache-free `modules/botidentity` authority over active
  `robot` and published `app_bot` rows (same-statement ambiguity detection,
  `user.robot` never authorizes), moved `cardtrust` display masking onto it while
  retaining the 60-second bounded cache, and made `card/action` resolve sender
  identity live before enqueueing through the unchanged robot event queue. Added
  push/search projection coverage plus App Bot unpublish/republish and full
  action -> poll -> ACK lifecycle tests. `internal/carddispatch` remains a
  separate task. Brief/context under
  `.octospec/tasks/card-message-appbot-trust/`; shared journal
  `.octospec/journal/shared/card-message-appbot-trust.md`.

## 2026-07-09 (sticker-oversized-store-guard)

- **Fix** — Task `sticker-oversized-store-guard` (code-review fix on
  `sticker-oversized-default`): close the regression where the compress-aware
  gate admitted >512 jpg/png trusting compression to downscale, but every
  fail-open path (nil compressor, skipped:concurrency_saturated/timeout, failed,
  or compress_max_dimension > upload_max_dimension) stored the original oversized
  image up to 1024² and served it to peers — reachable under load / attackable by
  saturating the compress slots. Added `stickerCompressResult.OutMaxDim` (actual
  post-compression dimension) + an `api.go` post-block guard that rejects
  (`compress_oversized_rejected`, new pre-warmed terminal metric) when the final
  stored dimension exceeds `upload_max_dimension` — dimension fail-CLOSED while
  compression quality stays fail-OPEN. Deduped the cross-package 1024 literal
  (exported `common.StickerUploadMaxDimensionHardCap`, referenced by modules/file).
  Schema note recommends `compress_max_dimension ≤ upload_max_dimension`; test
  helper reuse cleanup. Four guard regressions (nil/failed/timeout/mis-config) +
  unbroken happy path. No new errcode / i18n / DB / appconfig change. Briefs
  `.octospec/tasks/sticker-oversized-store-guard/`.

## 2026-07-09 (sticker-oversized-default)

- **Change** — Task `sticker-oversized-default` (follow-up to
  `sticker-downscale-store`): make ">512px static jpg/png auto-shrinks to 512" the
  built-in default once compression is enabled, without turning compression on for
  every deployment. `compress_max_dimension` default flips 0(=ceiling)→**512**,
  decoupled from `upload_max_dimension`, clamp `[1,1024]` (getter collapsed to the
  shared `stickerClampIntUpper`). New compress-aware dimension gate
  (`stickerLimitsSnapshot.effectiveGateDim`): jpg/png accept up to the **1024**
  hard cap when `compress_enabled=true` (then shrink to `compress_max_dimension`),
  gif/webp and compress-off stay gated at `upload_max_dimension` (512).
  `compress_enabled` default stays **false** (gray-scale rollout preserved);
  `upload_max_dimension` default and the appconfig `StickerUploadLimits`
  client contract stay **512/unchanged** (compress-aware gate avoids the
  appconfig ripple a 1024 default would cause). Zero-impact when compression off
  (gate = 512 for all formats, compressor never runs). Known edge: APNG (ext
  `.png`) passes the widened gate but can't be shrunk (`skipped:animated`) — later
  fail-closed **rejected** by `sticker-oversized-store-guard` if >
  `upload_max_dimension` (this entry's pre-guard "stored un-shrunk" no longer
  holds). Getter tests rewritten; gate integration tests added; fake made
  faithful to the 512 default. No new errcode / i18n / DB / migration / appconfig
  field. Brief `.octospec/tasks/sticker-oversized-default/brief.md`.

## 2026-07-09 (sticker-downscale-store)

- **Change** — Task `sticker-downscale-store` (phase two of
  `sticker-upload-compression`): decouple the compressor's `imaging.Fit` downscale
  target from the upload dimension gate. New server-side key
  `sticker.compress_max_dimension` (int, `Positive:true`, read-side clamped to
  `≤ upload_max_dimension`, unset ⇒ `= upload_max_dimension` ⇒ no downscale). Swap
  `stickerLimitsSnapshot.compressParams().MaxDim` from `maxDim` (accept gate) to a
  new `compressMaxDim` field so static jpg/png larger than the target but within
  the unchanged accept ceiling are downscaled before re-encode+store, instead of
  the Fit branch being unreachable (gate/target were same-source, so it never
  fired). Accept hard cap stays 1024 (decompression-bomb envelope unchanged);
  webp/gif still validate-only; not exposed via appconfig. Zero-impact default,
  byte-for-byte identical to `main` when unset. New getter clamp tests (no-infra)
  + api-level downscale/regression tests. No new errcode / i18n / DB / migration.
  Brief `.octospec/tasks/sticker-downscale-store/brief.md`.

## 2026-07-09 (P3-3)

- **Change** — Task `card-message-p3-rich-inputs` (card message P3-3): extend the
  octo/v2 input whitelist with `Input.Number/Date/Time` (all AC 1.0, within the
  pinned `card_version:"1.5"` — additive, no version bump). Submit-time value
  validation added (format/type only: Number = finite JSON number; Date =
  `YYYY-MM-DD`; Time = `HH:MM`; `""` = unfilled; declared min/max range NOT
  server-enforced — delegated to bot, same class as `isRequired`/`regex`, which
  likewise stay unenforced). Refactored the element
  whitelist into a single `pkg/cardmsg` authority (`whitelist.go`:
  `displayElements`/`inputElements` + `DisplayElements()`/`InputElements()` +
  `isInputElement`) that send-time validation, submit-time collection, action
  dispatch, and the D12 manifest all derive from — no drifting literals. D12
  `GET /v1/bot/card/profile` additively advertises `elements`/`inputs` for
  element-granularity feature detection. Review-caught fixes folded in: reject
  non-finite `Input.Number` (NaN/±Inf bypass `ParseFloat`); strict JSON-number
  grammar so the server's "valid number" matches the bot's JSON parser (reject
  `ParseFloat`'s Go-only superset — `1_000`/`0x1p4`/leading-`+`/leading-zero —
  which would silently corrupt the value the bot re-parses); `default`
  fail-closed arm in the submit-time type switch; `Column` dropped from the
  manifest `elements` (it is a `ColumnSet` child, not a top-level element the
  validator accepts — advertising it lied about capability); and symmetric
  `inlineAction` dispatch for the new types (no dead buttons). No new errcode /
  i18n / DB / migration / endpoint; additive-only wire contract. Brief
  + journal under `.octospec/tasks/card-message-p3-rich-inputs/` and
  `.octospec/journal/shared/card-message-p3-rich-inputs.md`; learning candidate
  in `.octospec/learnings/pending/`.
- **Change** — Same task/PR, follow-on: **AC 1.5 display-element completion (Tier 1)** —
  added `ImageSet`(1.0) / `RichTextBlock`(1.2) / `Table`(1.5) / `ActionSet`(1.2) to the
  octo/v2 display whitelist (versions verified against adaptivecards.io). Each covers
  send-time validation (structure + URL allowlist + recursion budget), dispatch symmetry
  (`findSubmitInElements` walks ActionSet.actions / Table cells / ImageSet images /
  RichTextBlock inlines for Submit — no dead buttons), plain derivation, and D12 manifest
  `elements` (auto via the displayElements single authority). Corrected the pre-existing
  `TestValidateWhitelistRejections` which mislabeled Table as "AC 1.6, reject" (Table is
  1.5, now supported) → replaced with still-unsupported Media(1.1)/ToggleVisibility(1.2).
  Still out (later, on demand): Media, Action.ShowCard/ToggleVisibility/Execute, templating,
  AC 1.6.
- **Change** — Same task/PR, review hardening (PR#556 review of head `7559c526`): fixed a
  **send-time URL-allowlist bypass (P1)** in the two Tier-1 flat-leaf handlers — `imageChild`
  (`ImageSet.images[]`) and the `RichTextBlock.inlines[]` object branch accepted a child
  without enforcing its declared `type` and never recursed its `items`, so a mislabeled child
  (`{"type":"Container","url":"http://ok","items":[TextBlock with javascript:]}`) passed
  `Validate` with the nested `javascript:` link unchecked. Now both enforce a *leaf* contract —
  reject a present `type` ≠ `Image`/`TextRun` (same discipline as `column()`) AND reject any
  child-collection field (`items`/`columns`/… via `rejectLeafSubtree`), which also closes the
  **typeless-child residual** a conditional `if type present` check leaves open (a no-`type`
  child with a nested subtree) — restoring "校验面 ≥ 渲染面" (`TestTier1MislabeledChildRejected`
  covers typed + typeless). Also completed `TableRow.selectAction` (P2): added it to
  validation (`w.selectAction(row)`) and dispatch (`findSubmitInElements` reads
  `row.selectAction`) symmetrically — row was the only node whose `selectAction` was neither
  validated nor dispatched. Brief updated; `inputs` manifest field confirmed in-scope.
- **Change** — Same task/PR, review hardening cont'd (heads `2c8f1003`→`85baabdf`, three
  reviewers): the foreign-typed-child bypass turned out to recur one child collection at a time
  (ImageSet → its typeless variant → `Table` rows/cells), so generalized the fix into one shared
  discipline instead of patching each instance. New `checkConstrainedChild` (type-pin via a shared
  `childTypeMatches` predicate + closed-set `rejectForeignSubtree`) is now applied to **every**
  flat-validated child position — `ColumnSet.columns[]`, `ImageSet.images[]`,
  `RichTextBlock.inlines[]`, `Table.rows[]`/`cells[]`, `FactSet.facts[]` — closing the `Table`
  send-time bypass (mislabeled cell as `Image` with a `javascript:` url; mislabeled/typeless row
  hiding an un-recursed `items` subtree) plus the Column/Fact instances of the same class. The
  dispatch walker (`findSubmitInElements`) reuses the same `childTypeMatches` predicate to skip
  foreign-typed children, so validate-surface == dispatch-surface can't drift (P2). Tests:
  `TestTier1MislabeledChildRejected` (Table/Column/Fact, typed + typeless) +
  `TestTier1DispatchSkipsMislabeledChild`. Lesson: patch the class, not the flagged instance.

## 2026-07-08 (PR-D)

- **Change** — Task `card-message-p2-capability-manifest` (PR-D, card message P2
  D12): producer capability discovery. New read-only `GET /v1/bot/card/profile`
  (bot-token, existing `authBot()` chain — no new rate limiter, no Space
  middleware) returning the deployment's card capability manifest
  (`enabled` / `card_version` / `profiles` / `limits`) so producers feature-detect
  instead of send-probing. All values sourced from `pkg/cardmsg` constants; the
  `profiles` set comes from a new single-authority `cardmsg.AcceptedProfiles()`
  that `interactiveByProfile` now derives from too (a drift-guard test asserts
  the manifest can't advertise a profile the validator rejects). `enabled:false`
  still returns 200 with the full manifest (a both-halves test pins manifest-200
  + send-still-rejects together). Additive-only wire contract (contract test pins
  the field set). No new errcode / i18n / DB / migration. Independent of PR-B/PR-C
  (both merged). Journal:
  `.octospec/journal/shared/card-message-p2-capability-manifest.md`;
  learning: `.octospec/learnings/pending/card-message-p2-capability-manifest.md`.

## 2026-07-08 (PR-C)

- **Change** — Task `card-message-p2-revision-history` (PR-C, card message P2
  D10): card revision history. New `octo_message_card_revision` side table +
  `pkg/cardrevision` shared store (written by bot_api on edits/clear, read by
  message), `GET /v1/message/card/revisions` (summary / full=1) reusing the
  extracted `authorizeCardChannelMember` gate, bot revision clear + auditable
  tombstone, `transient` frame flag (progress frames skip history), and revoke
  cleanup. Verify caught two P1s (fixed): the query path lacked the
  revoke/deleted/user-local-delete visibility gate, and the revoke cleanup was
  mis-ordered after the notify step. Code-review (B1) then caught that the query
  still enforced a *subset* of the canonical read — missing the `visibles`
  allowlist / read-offset / channel-offset / expiry layers `card/action` carries;
  fixed by extracting `cardCanonicalVisibleToViewer` and sharing it across both
  endpoints (+ `TestCardRevisionsCanonicalVisibility`). Stacked on PR-B; zero
  octo-im changes. Journal:
  `.octospec/journal/shared/card-message-p2-revision-history.md`;
  learning: `.octospec/learnings/pending/card-message-p2-revision-history.md`.

## 2026-07-08

- **Change** — Task `card-message-p2-action-loop` (PR-B, card message P2
  interaction): shipped the interaction closed loop (contract
  `card-message-interaction` D3–D9/D11 + octo/v2 whitelist). New
  `POST /v1/message/card/action` (authz + anti-IDOR + D11 input validation + D4
  Redis idempotency), typed `card_action` bot event on the existing robot queue,
  type-17 `botMessageEdit` unlock (cardmsg validation + D9 `card_seq` CAS in
  `message_extra`), and the `pkg/cardmsg` octo/v2 whitelist filled into the
  merged-P1 seams. Verify caught a real InnoDB deadlock in the D9 CAS under
  concurrent frames (fixed via bounded 1213/1205 retry). Zero octo-im changes.
  D10 revision history / D12 capability manifest split to sibling PRs C/D.
  Journal: `.octospec/journal/shared/card-message-p2-action-loop.md`;
  learning: `.octospec/learnings/pending/card-message-p2-action-loop.md`.

## 2026-07-02

- **Change** — Task `conv-space-catchall-484` (issue #484 follow-up): closed the
  two deterministically reproducible cross-Space paths in the recent-conversation
  list. (1) The default-Space DM catch-all no longer lists a bare DM whose
  `dm_space_presence` rows point exclusively at other Spaces (positive-evidence
  post-pass; legacy no-presence DMs keep the catch-all; system bots exempt; any
  query failure disables the pass). (2) Groups with empty `group.space_id` — and
  their topics, in the conv filter AND sidebar thread-ext filter — now show only
  in the user's default Space instead of every Space (same policy as #337 bare
  DMs / #484 untagged history). This branch also carries the base
  `dm-space-isolation-484` fix (merged in — see the 2026-06-27 entry below), so
  the presence infra is authored once here. Journal:
  `journal/shared/conv-space-catchall-484.md`.
- **Remove** — Task `incoming-webhook-remove-name-prefix`: dropped the
  server-enforced `Webhook-` name prefix that was force-prepended to
  non-admin (member/bot) submitted incoming-webhook display names
  (originally added anti-impersonation, PR #340 review). Members can now
  set any name, same as admins. Kept: avatar lock for non-admins, default
  auto-naming (`Webhook-xxxxxx`) when no name is submitted, and the
  push-time `Username`/`AvatarURL` override block for non-admin webhooks
  (separate control, unaffected). Paired frontend change in octo-web
  removed the now-stale hint text. Brief under
  `.octospec/tasks/incoming-webhook-remove-name-prefix/`.

## 2026-06-29

- **Change** — Task `group-avatar-name-no-text` (client-coordination; repurposes
  `group-avatar-icon-default` S2): newly created groups now default to the
  two-person icon — the group **name is never rendered as avatar text**; text
  appears only when the user sets a custom `avatar_text`. Implemented by changing
  **who gets `is_named=1`**, not the render rule (`writeGroupDefaultAvatar`
  unchanged: `avatar_text > is_named==1 name-text > icon`). `is_named` is
  repurposed from "user named it" to "**pre-cutover legacy group**": all new
  inserts (`CreateGroup`/`AddGroup`/`event.go` system+org+dept) persist
  `is_named=0`, and rename no longer flips it; existing groups keep `is_named=1`
  (already backfilled by migration `20260629000001`) so they are **grandfathered**
  onto their current name-text avatar (no historical group flips to an icon).
  `is_named` stays load-bearing (not deprecated) as the legacy/new discriminator;
  `GroupResp.is_named` re-documented as 1=legacy/0=new predictor. No render-version
  bump, no new migration. Brief under `.octospec/tasks/group-avatar-name-no-text/`.
- **Add** — Task `common-builtin-emoji-manifest`: public, cacheable
  `GET /v1/common/emojis` returning the built-in custom emoji manifest
  (`{version, list:[{key,name,url}]}`) from an embedded JSON single source of
  truth, mirroring the `avatar_palette` (#500) pattern (content ETag +
  `must-revalidate` + 304). Clients fetch + cache instead of hardcoding the
  `[xxx]` emoji list. `url` optional per item (built-ins reuse client bundle);
  no DB / errcode / i18n added. New `modules/common/emoji.go`,
  `modules/common/emojis/manifest.json`, `emoji_test.go`, swagger entry.

## 2026-06-27

- **Add** — Task `default-avatar-text-rule`: script-aware 2-glyph text rule for
  group + personal default avatars. Mixed script → Han only; pure English →
  initials (camelCase/sep split, ≤2, upper); pure digits → 2; empty/symbol/emoji
  → icon (group two-person) / ascii (personal) fallback. New
  `avatarrender.GroupNameText` (前2) + rewritten `IndividualText` (后2) over a
  shared core; `GroupText` kept as the custom-`avatar_text` normalizer (≤4) and
  `writeGroupDefaultAvatar` splits custom-text vs auto-name. Cache-version bumped
  `group-name-v3→v4` and `name-v4→v5` (ETag + CacheKey). Brief + context under
  `.octospec/tasks/default-avatar-text-rule/`, journal
  `.octospec/journal/shared/default-avatar-text-rule.md`.
- **Fix** — Task `dm-space-isolation-484` (#484): authoritative per-Space DM
  presence index (`dm_space_presence`, written at the WuKongIM message webhook,
  read by the conversation Space filter) — fixes cross-Space DM history leak
  (symptom 1, via default-Space policy for untagged messages) and DMs mutually
  hiding between Spaces (symptom 2, window-independent visibility OR-ed with the
  legacy Recents scan). Server-only; no client change.

## 2026-06-25

- **Add** — Task `incoming-webhook-mention-config`: moved the incoming-webhook
  `@mention` from a caller-supplied push-body param to webhook create/update
  config (new `mention_uids` column + `AllowMention*` switches). The push
  endpoint no longer reads `mention` from the body; targets are validated at the
  management boundary and re-filtered to current members at push time. Removing
  the body-source also removed the native-only `allowMention` gate, so mention
  now applies across **all** adapter endpoints (native + github/wecom/gitlab/
  feishu/multica). Deleted the now-dead caller-supplied entity machinery. Brief +
  context under `.octospec/tasks/incoming-webhook-mention-config/`, journal
  `.octospec/journal/shared/incoming-webhook-mention-config.md`.
- **Add** — Task `appbot-token-revocation-redis` (#309): replace the per-process
  in-memory App Bot auth registry with a shared Redis write-through cache so
  token revocation (rotate/unpublish/delete) takes effect on every replica
  immediately; DB stays authoritative (auth fails safe to DB on Redis error).
  Safety-net TTL via system_settings (`app_bot.auth_cache_ttl_seconds`, no new
  env var). Regression test asserts a revoked token is rejected on a peer replica.
- **Update** — Task `group-default-avatar` (increment 4, final): removed the
  member-avatar 9-grid composite chain now that avatarGet renders on demand —
  all 5 publish sites + `beginAvatarUpdateEvent`, the `GroupAvatarUpdate` event
  handler/const/db-helpers, `queryGroupAvatarIsUpload`, dead `memberCount`
  guards, and two obsolete tests. Kept DownloadAndMakeCompose (other use) and
  the CMDGroupAvatarUpdate client-refresh CMD. Historical composite groups fall
  through to the rendered default with no backfill. Feature backend complete;
  only the placeholder group-icon SVG remains to be swapped.
- **Update** — Task `group-default-avatar` (increment 3): group-info update
  (`PUT /v1/groups/:group_no`) now accepts `avatar_text`/`avatar_color`
  (set/clear, validated), persisted via a dedicated `UpdateGroupAvatarCustom`
  service + `db.updateAvatarCustom`; clients refreshed via
  `SendChannelUpdateToGroup`. Composite teardown still pending.
- **Update** — Task `group-default-avatar` (increment 2): `avatarGet` now
  server-renders the default group avatar (colored circle + group-name initials,
  2×2 for CJK / single-line for Latin, group-icon fallback) with weak-ETag/304,
  keyed on `is_upload_avatar`; uploaded avatars still redirect. `pkg/avatarrender`
  gains `RenderGroup`/`GroupAvatarLines`, `RenderIcon` (+ placeholder glyph), and
  shared `ETag`/`IfNoneMatch`. Member-avatar composite teardown still pending.
- **Creation** — Task `group-default-avatar` (increment 1): create-group API gains
  optional `avatar_text`/`avatar_color` params persisted via new `group` columns;
  `pkg/avatarrender` gains `GroupText`/`VisibleRuneCount`/`ColorByIndex`. Brief +
  journal under `.octospec/tasks/group-default-avatar/`. Follow-ups: avatarGet
  server-render branch, group-update keys, composite-avatar teardown.

## 2026-06-24

- **Add** — Task `incomingwebhook-webhooks-alias` (#455): `/v1/webhooks/{id}/{token}`
  push-route alias for the canonical `/v1/incoming-webhooks/...` (native + 5
  adapters), reusing the identical middleware chain. Generalized `pkg/accesslog`
  token scrubbing (`ScrubPath` + panic-dump regex) to mask BOTH prefixes (#246
  parity). Brief + context under `.octospec/tasks/incomingwebhook-webhooks-alias/`,
  journal `.octospec/journal/shared/incomingwebhook-webhooks-alias.md`.
- **Add** — Task `incoming-webhook-mention-broadcast` (#448 item ②): broadcast-pill
  auto-compose on the native incoming-webhook push endpoint. When a permitted
  `mention.all`/`mention.bots` is set, the server prepends the canonical broadcast
  literal (`@所有人`/`@所有AI`) + a space to the text content so all three clients
  render a pill; directed-entity (#449) offsets shift by the prefix's UTF-16
  length. Text-path only; routing / red-dot / bot-summon unchanged. Brief +
  context + journal under `.octospec/tasks/incoming-webhook-mention-broadcast/`
  and `.octospec/journal/shared/incoming-webhook-mention-broadcast.md`.
- **Add** — Task `incoming-webhook-mention-directed-render` (#448 item ① b):
  opt-in server-side directed @mention name-resolution. `mention.render:true`
  resolves each member uid → `user.name`, prepends `@<name> ` to text content, and
  generates the UTF-16 `mention.entities`. Refactored the broadcast compose into one
  `composeMentionContent`. Adversarial review added a forged-broadcast guard (skip
  names that are broadcast labels or contain `@`), incremental budget tracking, and
  cap/iOS/byte-size docs. Ships in the same PR as the broadcast half (#450) → the
  two close #448. Brief + context + journal under
  `.octospec/tasks/incoming-webhook-mention-directed-render/` and
  `.octospec/journal/shared/incoming-webhook-mention-directed-render.md`.

## 2026-06-23

- **Add** — Task `upstream-dep-metrics` (#440 P0-a): upstream-dependency
  observability. Added `dmwork_dependency_duration_seconds` (object-storage
  `DownloadURL` latency) and connection-pool metrics (`go_sql_*` via
  DBStatsCollector + `dmwork_redis_pool_*` via a scrape-time collector). No
  background goroutine, no `octo-lib` change, no business-logic change. Brief +
  context + journal under `.octospec/tasks/upstream-dep-metrics/` and
  `.octospec/journal/shared/upstream-dep-metrics.md`.

## 2026-06-19

- **Update** — Adopted OKF v0.1 compatible frontmatter across all repo rules
  (`commit-style`, `error-handling`, `rate-limit`, `space-isolation`,
  `testing`): added `type`, `title`, `description`, `tags`, `timestamp`. The
  octospec orchestration fields are retained as OKF extension fields.
- **Update** — Bumped global inheritance pin to `octo-spec@1.1.0`.
- **Creation** — Added `.octospec/index.md` (human-readable rule catalog) and
  this `.octospec/log.md` change log.

## 2026-06-18

- **Creation** — octospec pilot scaffolding: rules `error-handling`,
  `rate-limit`, `space-isolation`, `testing`, `commit-style`; manifest, task
  templates, slash commands (PR #418).
- **Creation** — Dogfood task `member-list-name-fallback` (#344 → PR #420).

## 2026-07-13 (card-message-internal-dispatch P2)

- **Pilot** — Enabled the first `internal/carddispatch` producer
  (`summary-notify`): dedicated `summary` bot + producer spec + `NotifyReq.Card`
  structured branch building `octo/v1` DM cards via `cardtmpl` and dispatching
  through the bound `Sender` (per-recipient fan-out, `NotifyResp` preserved).
  Stacked on the P1 foundation branch, not main. Cross-repo (octo-web route,
  octo-smart-summary switch) tracked in the summary-notify contract. See
  [journal](journal/shared/summary-notify-pilot.md).

## 2026-06-19 (tooling)

- **Update** — Synced OKF-aware slash commands, workflow skill, and task brief
  template from octo-spec 1.1.0 so generated briefs/journals stay conformant.

## 2026-08-14 (notification-pause-manual-mode)

- **Implemented** — Added explicit manual/timed pause state, server-side fixed
  durations, unified REST/CMD responses, migration, validation, and tests.

## 2026-08-22 (cleanup-membership-predicate)

- **Fixed** — The two removal-cleanup rejoin guards were asking one question
  with two different predicates: a disbanded Space silently voided cleanup
  (orphan `space_member` row), a banned Space wrongly triggered it. Both now use
  `CheckMembershipForCleanup` (`sm.status=1 AND s.status <> 0`).
  `CheckMembership` is deliberately **unchanged** — #797's original proposal to
  relax it would have admitted banned Spaces across all **36** of its non-test
  call sites, `SpaceMiddleware` — the primary auth gate — included. A behavioural truth table now pins both predicates' answers
  across `{disbanded, normal, banned} x {active, removed}`, including the one
  cell where they must disagree. (Corrected 2026-08-23: this entry originally
  claimed a *source guard*; that guard was deleted on the same branch for
  passing the regression it was named for.) Closes two #797 items.
  See [journal](journal/shared/cleanup-membership-predicate.md).

## 2026-08-22 (cleanup-queue-durability)

- **Fixed** — Two silent-failure items from #797. The cleanup queue's retry budget
  was only enforced in `releaseCleanupJob`, which a `SIGKILL` never reaches, so a
  process-killing job was re-claimed forever and head-of-lined the whole queue;
  the budget now gates the claim itself and a 1-minute sweep pushes exhausted rows
  to `abandoned`, with three gauges so the new terminal state is not just as silent
  as the old loop. Separately, a failed membership-cache `DEL` now returns, is
  logged, and is overwritten with a negative entry — a total Redis outage was
  already safe, but a DEL-only failure let a removed member keep passing
  `SpaceMiddleware` for 60s with nothing logged.
  See [journal](journal/shared/cleanup-queue-durability.md).

## 2026-08-23 (space-member-removal follow-ups · wrap-up)

- **Reverted** — The durable IM-unsubscribe outbox (`eb74529`) was implemented and
  then withdrawn (`78e46d3`) after five-lens adversarial review. Three reviewers
  independently found it reintroduced the exact leak it targets (an `abandoned` row
  became a permanent tombstone that silently swallowed every later enqueue while the
  log claimed "queued for retry"), and it added a new one (firing without
  re-validating membership turns blacklist→un-blacklist into a permanent cutoff of an
  active, visible member). The problem statement and the measured broker evidence
  stand; the design does not. Corrected requirements are written into
  `.octospec/tasks/im-pending-outbox/brief.md`.
- **Fixed** — Two guard tests that could not fail for what they existed to check
  (mutation-proven, then re-verified with the reviewers' own mutations), and a sweep
  that took next-key locks across the whole pending range — reproduced as
  `ERROR 1205` on a brand-new non-conflicting insert, which is the removal-cleanup
  enqueue inside the removal transaction. See
  [journal](journal/shared/cleanup-queue-durability.md).
- **Learning** — `learnings/pending/mutation-testing-must-be-adversarial.md`: an
  author-chosen mutation only proves the test catches what the author already thought
  of. The same guard was green on the real security regression and red on whitespace.

## 2026-08-26 — file-extension-policy-dynamic-config

- **Shipped** — Upload extension allow/block lists and the single-file size cap
  are runtime-configurable through `system_setting`; blocking a format no longer
  needs a configmap edit and a pod restart. Both extension keys are `env ∪ DB`
  unions ("allow" only adds, "block" only removes) over a non-revocable built-in
  blocklist, and the effective limits are served from `/v1/common/appconfig`.
  See [journal](journal/shared/file-extension-policy-dynamic-config.md).
- **Fixed during review** — Four blocking findings on my own first revisions: the
  provider was never mounted (feature inert while reporting success), the
  `bot_api` / `robot` multipart paths had no extension gate at all, the snapshot
  cache key could collide (emergency block silently serving the stale policy),
  and the extension CSVs were unbounded on a response served from an
  unauthenticated endpoint.
- **Learning** — `learnings/pending/assembly-path-must-be-tested.md`: tests that
  inject a dependency through a helper cannot show that production wiring exists.
  A missing registration call passed the entire suite.


## 2026-09-02 — oidc-oauth2-provider-abstraction

- **Shipped** — `modules/oidc` is no longer hard-wired to OpenID Connect. The
  provider is an `AuthProvider` interface with two implementations selected by
  `OCTO_OIDC_PROVIDER_KIND` (default `oidc`, so deployed configurations are
  untouched), so an enterprise IdP that speaks plain OAuth2 — no Discovery, no
  `id_token`, no JWKS, a vendor envelope around `/userinfo`, an app id in a path
  segment for single logout — can drive login, logout and profile claims.
  Business branches read `Capabilities()`, never the kind. Two exchange endpoints
  serve clients that complete SSO themselves and arrive holding a credential:
  `/exchange` (upstream `access_token` → `/userinfo`) and `/exchange-jwt`
  (locally verified HS256 JWT, no outbound call). See
  [journal](journal/shared/oidc-oauth2-provider-abstraction.md).
- **Guarded the one irreversible decision** — `(issuer, subject)` is the identity
  key and cannot be changed after go-live, and the vendor docs contradict
  themselves about whether `subject` is an internal long id or the employee
  number. Since employee numbers are reused between leavers and joiners, guessing
  wrong would log a new hire into a former employee's account. A shape guard
  refuses short numeric subjects at the trust boundary before any row is written,
  turning an unrecoverable data problem into a recoverable failure — and removing
  "capture a real userinfo response first" from the critical path.
- **Fixed during self-review** — Two CRITICALs of my own making: logout read
  `device_flag` by decoding the raw token (a UUID with no fields), so it always
  fell through to disconnecting *every* device instead of the calling one; and
  `/exchange-jwt` lacked the nil-provider guard, so a boot-time provider failure
  turned into a panic. Plus credential-leak closures at three points (`*url.Error`
  embeds a URL that carries `client_secret`, redirects were being followed with
  those credentials attached, and accesslog scrubbing missed the panic-dump sink).
- **Trimmed the config surface** — New environment variables went from 8 to 5.
  Endpoint rate limits became constants (matching `modules/user`), and the
  bearer-JWT issuer namespace is now derived from the upstream issuer rather than
  taking a second environment marker, which inherits per-environment isolation
  instead of asking operators to keep two markers consistent.
- **Learning** — `learnings/pending/build-does-not-compile-tests.md`: `go build
  ./...` skips `_test.go`, so a rename done by string substitution left the entire
  `modules/oidc` test binary uncompilable while the build gate stayed green; ~70
  cases silently stopped running across a session boundary.
- **Learning** — `learnings/pending/refusing-is-cheaper-than-an-immutable-wrong-key.md`:
  when an identifier becomes an immutable primary key, fence the ambiguity instead
  of resolving it first — refusal is a constant you can edit, a polluted identity
  table is not.

## 2026-09-02 — oidc-oauth2-provider-abstraction (review round 2)

- **Fixed six blocking defects found by review**, all re-derived from source before
  acting. Per-device logout had never worked in production: the device flag was read
  after `InvalidateCurrentToken` had deleted the record it comes from, and `0` was
  used as the "unresolved" sentinel while `config.APP` *is* 0. Both exchange
  endpoints discarded the race-recovery winner and handed the client a session for a
  ghost account with no identity row. The bearer-JWT anchor accepted any non-empty
  secret and had `exp` as its only freshness control. `RequireEmailVerified=false`
  was an account-takeover primitive under a kind whose provider cannot assert
  verification. And a provider-kind typo could take **every** login path down at
  once. See [journal](journal/shared/oidc-oauth2-provider-abstraction.md).
- **Removed the mirror rather than updating it** — the login-lockout path existed
  because `modules/common` hand-copied the module's boot validation and the copy
  never gained the five new fatal conditions. The refusal rules now live in
  `pkg/oidcboot`, a stdlib-only leaf package both sides import, and
  `oidcboot.RefusedScenarios` pins both sides' tests to one table. Deleting the
  delegation makes all nine scenarios fail, so we know the drift was live.
- **Landed the de-duplication that the brief had already claimed** — both exchange
  endpoints now share one post-validation tail. While it was duplicated, the
  race-recovery defect existed in both copies and the phone-masking fix reached only
  one; that is the class the shared tail exists to prevent.
- **Reverted my own phone-number inference.** Bare 11-digit inference was added on
  the strength of a documented example that is not a valid mainland number, and
  roughly seven eighths of North American numbers are byte-identical to a valid
  mainland mobile — `13861234567` is both. It was storing strangers' numbers.
- **Corrected four places where the brief or PR body asserted properties the code
  did not have**, each with the reason the original wording was wrong.
- **Learning** — `learnings/pending/a-double-must-model-the-failure-you-fear.md`:
  a double written to satisfy the assertion certified a fix production could not
  perform; model what the collaborator destroys, and confirm the test fails without
  the fix.
- **Learning** — `learnings/pending/delete-the-mirror-instead-of-syncing-it.md`:
  a comment saying "keep in sync with" is not a mechanism; extract the rule into a
  leaf package and pin both sides' tests to one shared table.

## 2026-09-02 — oidc-oauth2-provider-abstraction (review round 5)

- **Fixed a regression I introduced two commits earlier.** Extracting provider
  construction into a shared factory silently deleted the neighbouring block that
  wired the RP-Initiated Logout id_token cache, so the constructor had zero
  production callers and the field stayed nil: logout never emitted an
  `id_token_hint` and the upstream IdP session was never ended. A user who logged
  out of DMWork remained signed in at the IdP. The suite stayed green because all
  ten affected tests assign the store by hand — a double can be perfectly faithful
  and still prove nothing about assembly. Restored, plus tests that build the
  module through `New()` and assert the wiring exists.
  See [journal](journal/shared/oidc-oauth2-provider-abstraction.md).
- **Closed the same guard gap on the second consumer.** The byte-exact
  `(issuer, subject)` recheck existed only on the login path; `modules/integration`
  called the raw query, so under the table's `ci` collation a subject differing
  only in case authenticated as another user and minted an API key against their
  account — reproduced in a test. Fixed structurally rather than by adding a second
  call: the raw query is now unexported and `QueryIdentityExact` is the only way in
  from outside the package, so a future third consumer is stopped at compile time.
- **Stopped forwarding our own rejected tokens to the third-party IdP.** The
  credential fall-through was unconditional, so a business JWT with a valid HMAC
  that failed only on freshness was sent upstream in a URL query string, landing in
  the vendor's access logs together with its signature. Now only "not ours"
  (malformed / bad alg / bad signature) falls through; "ours but rejected" is a
  local 401.
- **Split the freshness policy by purpose.** The ten-minute ceiling was justified
  by one-shot redemption but was also applied to a standing per-request
  authenticator, where the desktop client reuses one long-lived token — so the
  integration endpoints would have worked for ten minutes after login and then
  returned an indistinguishable 401 forever. `VerifyForRedemption` keeps the
  ceiling; `VerifyForAuthentication` honours the token's own `exp`. My own test had
  pinned the broken behaviour.
- **Added the subject upper bound the brief already claimed existed**, and folded
  the duplicated app-id pattern into `pkg/oidcboot` so boot and runtime cannot
  disagree (they did: `_tenant` passed boot and was refused at runtime, where the
  error is swallowed and logout degrades to local-only).
- **Learning** — `learnings/pending/extracting-a-helper-can-silently-drop-a-sibling.md`:
  after lifting anything out of a constructor, diff the *removed* region and check
  every `newXxxStore` still has a non-test caller.

## 2026-09-02 — round 6: the classification boundary, and writing the matrix down

Two independent `CHANGES_REQUESTED` converged on the same two blockers, both of
them the round-5 defect reached through routes that fix had not enumerated.

- **A sentinel cannot carry a stage judgement.** "Is this credential ours?" was
  keyed on error identity, but `ErrJWTMalformed` is returned on both sides of
  `hmac.Equal` — payload JSON, non-integer `exp`, claims decoding all fail *after*
  the signature matched. A token bearing our own valid HMAC with a mistyped payload
  was therefore forwarded to an IdP that takes credentials in the URL query. Fixed
  by marking only the pre-/at-signature sites and inverting the predicate to an
  allowlist, so a check added later defaults to "ours" and is refused locally.
  The trigger is mundane (`iat: Date.now()/1000` unfloored) and the client reuses
  the token for its whole life, so the leak was continuous.
- **The guard's own construction failure was the unguarded cell.**
  `modules/integration` logged and continued with a nil verifier — under a comment
  saying it must not — and because the classification sits behind that nil check,
  every desktop JWT went upstream unconditionally. Now every credential is refused
  while the construction error is set; an absent secret stays a legal shape, pinned
  by a paired negative test.
- **Hoisting a guard hoists its code, not its justification.** Moving the subject
  bound to a protocol-neutral funnel was right; taking the employee-number
  heuristic with it was not — that path's subject is our own DB primary key, never
  reused, and three existing tests went red. Split by what each half is a property
  of (column vs producer); the producer half is now pinned by the conformance table
  for both providers, which is how the standard-OIDC provider got it at all.
- Also: `Capabilities().IDToken` + `EndSessionEndpoint()` on the interface instead
  of a `*oidcProvider` assertion (any decorator silently lost RP-logout — including
  the decorator design considered for the bounds guard this same round);
  `AppIDPattern` from a mutable exported `var` to a function.
- **Wrote the matrix I had dismissed** — `tasks/oidc-oauth2-provider-abstraction/guard-matrix.md`.
  I retracted a reviewer's request for it on the grounds that it "produces no code
  change", having measured it against the findings already in hand. Both of this
  round's blockers sat in columns that request had named. Filling it in corrected
  one imprecise cell and disproved one suspected gap, and it lists the cells still
  open rather than closed.
- **Learnings** — `a-sentinel-cannot-carry-a-stage-judgement.md`,
  `a-guard-carries-its-justification-not-just-its-code.md`,
  `when-a-defect-recurs-the-matrix-is-underspecified.md`.

## 2026-09-02 — round 7: the taxonomy stopped at credentials we don't issue

Two blocking findings, both reproduced before fixing.

- **"Ours" is bigger than "the JWTs we sign".** Round 6's provenance guard answers
  "is this an HS256 JWT under our secret" — true, and a strict subset of the
  question that matters. A session token or a `uk_` API key is not a JWT, so it
  failed at the segment split, was classified foreign, and went to the vendor's
  `/userinfo` **in a URL query**. Reproduced: both appear verbatim in the upstream
  request URL. Not exotic either — this PR's own global `BearerTokenCompat` is what
  makes `Authorization: Bearer <session token>` the house convention, and
  `userAPIKeyAuth` reads the same header on sibling routes in the same group.
  New in this branch and specific to `kind=oauth2`: `main` verified locally and
  never called out, and `kind=oidc` still doesn't (verified by closing the mock
  server and getting a parse error, not a connection failure).
- **A write-side guard doesn't cover values written by the previous binary.** Bind
  snapshots cross the deployment boundary by design (there is a test pinning that
  legacy snapshots still decode), so a snapshot issued before the bounds existed
  still reproduced the orphan-user / truncation-collision shapes through
  `Confirm`/`Create`. Both now re-validate after decode, before any mutation.
- **The matrix I wrote last round had the blind spot that caused the first one.**
  C1–C3 were all credentials the *upstream* issues; there was no row for the ones
  *we* issue. Added C4, plus a lifetime column for artefacts that outlive the binary
  that wrote them, plus a correction where it overstated rate-limit coverage.
- Also: `HTTP_TIMEOUT` was silently ignored under `kind=oauth2`; and the two boolean
  env readers disagreed on an unparseable primary — one falling through to the
  legacy alias, the other returning the default — with the second carrying a comment
  claiming it matched the first. Deleted the copy into `pkg/oidcboot.EnvBool` and
  added the drift as a shared refusal scenario. **I had deferred this weighing
  trigger difficulty; the reviewer was right that blast radius is the measure** —
  the conjunction leaves an SSO-only deployment with no login path at all.
- **Learning** — `own-is-larger-than-the-subset-you-can-verify.md`.

## 2026-09-02 — round 8: a denylist of remembered types, and a vendor fact applied as a protocol fact

- **The guard was mounted on one of two consumers. Again.** `/exchange` got the
  own-credential detector but not the HMAC stage, so a business JWT went out whole —
  signature included — into the vendor's URL query. The comment saying *this endpoint
  is the likelier misdirection* was written in the commit that left the gap. Writing
  down the reason is not the same as acting on it.
- **`app_` was missed, and would have been missed again.** The fix that mattered was
  not the entry, it was `own_credential_coverage_test.go`: scan the credential-minting
  packages for prefix constants and fail naming any the detector does not know. It
  found `app_` on its first run. A prefix list is a denylist of types someone
  remembered; the omission has to become a CI failure, because "read it more carefully"
  has now failed three rounds running.
- **I applied a vendor fact as a protocol fact.** Round 6 said "the subject cap only
  covers one provider" and I made the whole check protocol-neutral — correct for the
  storage bound, wrong for the short-numeric heuristic. That one comes from *one IdP's*
  documented employee-number reuse. `kind=oidc` is the generic client existing
  deployments point at any IdP; a self-hosted IdP with `sub=1001` would have lost login
  for every user, no override, redeploy-only recovery — the exact cost I argued was
  unacceptable in the `local_off` section. **The module already made this argument one
  axis over** (bearer JWT excluded because our own keys are not reused). The axis I
  chose — derived vs upstream-asserted — was not the axis the argument needs, which is
  per-deployment: does *this* IdP's subject come from a reused personnel identifier.
  Now a capability bit, default false.
- **Enumerated one cell rather than closing it**: a non-HS256 JWT shape is
  unattributable and gets forwarded; closing it would break JWT-shaped upstream access
  tokens, which is what these endpoints exist for. Nothing we issued leaves, so the
  invariant holds; what is accepted is written down, and a test skips-with-explanation
  if anyone "fixes" it.
- **Learning** — `a-generalisation-can-widen-a-vendor-specific-rule.md`.

## 2026-09-02 — round 9: four findings, three of them mine from the round before

Two reviewers converged on the same headline; all four verified against code before any fix.

- **P1.1 — `/exchange` failed open exactly where `modules/integration` fails closed.** The
  round-8 verifier stage was written `if o.bearerJWT != nil`, and `New()` logged the
  construction error and threw it away. So a 31-byte secret gave a nil verifier meaning both
  "not configured" (legal) and "misconfigured" (must refuse), and the stage was skipped
  silently. **I had fixed this exact fail-open in `modules/integration` in round 6** and kept
  the error on the struct there — this round I copied the guard and not the failure direction.
  The premise I wrote in the comment was also wrong: "no secret ⇒ no C3 credential can exist"
  holds for an *absent* secret, not an *invalid* one, because the client backend signs with
  the same configured value and HMAC does not care about key length.
- **P1.2 — a regression I introduced: a doomed `/userinfo` call on every request.** Moving
  `oidcAuth` onto `IdentityFromClientCredential` means the `TokenSet` carries only the
  id_token, so `needUserInfo` fires with an empty `AccessToken`. go-oidc's `StaticTokenSource`
  validates nothing, so a GET with `Authorization: Bearer ` goes out, 401s, and is swallowed.
  A path that was purely local before this branch now blocks on an IdP round trip per request.
  The suite was green because the mock matches `HasPrefix(auth, "Bearer ")` and 401s into the
  swallow branch.
- **P1.3 — the mirror was deleted, the *normalisation* was not.** Rules were unified into
  `pkg/oidcboot`; one reader trimmed its env values, the other did not, and the rules compare
  against `""`. Whitespace-only `BASE_URL` therefore reached the two sides as different
  inputs → module 404s + helper reports configured + `local_off` honoured = no login path.
  Fixed by normalising inside `ValidateKind`. **My first attempt encoded this as a
  `RefusedScenario`, which was wrong** — after the fix the correct verdict is *accept* on both
  sides. What needed pinning was agreement, and `RefusedScenarios` only pins one direction, so
  the accepting direction became a second shared table (`AcceptedScenarios`), replacing the
  local copy each side kept.
- **P1.4 — two unauthenticated session-minting endpoints were on by default.** `main` exposes
  three routes; every deployment with `DM_OIDC_ENABLED=true` silently gained `/exchange` and
  `/exchange-jwt`, with no way to decline. Under `kind=oidc` that turns an id_token — a
  front-channel artefact that legitimately appears in browser history and Referer headers —
  into an unlimited session mint until `exp`. Now behind `OCTO_OIDC_EXCHANGE_ENABLED`,
  default false, and the opt-out also skips constructing the limiter's Redis pool.
- **Declined, with reasons recorded**: folding `validateLogoutURL` into `oidcboot` (reviewer
  agrees it is pre-existing; making `ValidateKind` stricter risks creating a *new* lockout);
  single-use redemption (adds Redis to the auth path and a new failure mode — and with the
  opt-in flag defaulting off, existing installs are no longer exposed).

## 2026-09-03 — oidc-auto-join-initial-space

- **Shipped** — One admin setting (`space.oidc_initial_space_id`, empty = off)
  makes every account created through OIDC — browser callback and
  `/bind/create` — an ordinary member of that Space right after its identity row
  lands. This unblocks `POST /v1/integrations/oidc/exchange`, which requires an
  active Space and membership: SSO users previously belonged to no Space at all,
  logged in fine, and failed exchange forever with no reachable remedy. The join
  never affects the login — it runs after the session is issued, returns no
  error, contains panics, and reports through
  `oidc_initial_space_join_total{result=...}`.
  See [journal](journal/shared/oidc-auto-join-initial-space.md).
- **Fixed during review** — The new settings validator stopped at the first plan
  naming its key while the write loop applies every plan in order, so a batch
  carrying the key twice was judged on the first value and stored the second:
  `200` for a configuration pointing at a Space that does not exist, with no
  downstream component able to report it.
- **Fixed proactively** — `atomicAddMemberIfNotFull` ran its
  `COUNT ... FOR UPDATE` over all active member rows *before* testing
  `maxUsers > 0`, so an unlimited Space paid an O(N) scan and a space-wide lock
  for a decision the count could not inform. Sparse joins hid it; an initial
  Space holding the whole company would not, since an SSO rollout puts every
  employee's first login on the same space_id, on the login response path.
- **Coverage gap closed late** — `/bind/create` is the second account-creating
  entry point and carried the hook with nothing asserting it; every test drove
  the callback, so the line could have been deleted silently.
- **Known limitation, deliberately not fixed** — Nothing removes Space
  membership when an IdP account is disabled. The hole predates this work but was
  empty while SSO users belonged to no Space; auto-join fills it.
- **Learning** — `learnings/pending/batch-write-validate-what-lands.md`: reduce a
  batch to the state it will actually produce before validating it; a validator
  that stops at the first match for a key approves one value and persists another.

## 2026-09-06 — project P0：PR #841 第二轮 review 修复

两位 reviewer 对新 head 重审，独立收敛到同一组三个 blocker。三条均为"本 PR 自己引入的
保证，在除一处以外的全部调用点生效，而遗漏未被记录"：`addOneMember` 漏了 actor 的 Space
席位复核、list 路由丢弃 `MemberRole` 的 `ok`、`createProject` 的锁序与 `modules/space` 记录
的死锁事故顺序相反。

- **B-3 的实测比预测更糟**：复现出 Error 1213，InnoDB 选中的受害者是**解散事务**而非
  create —— 正是 `modules/space/db.go:71-88` 那条注释担心的一侧，而解散是成员移除安全
  级联的一步。
- **修 B-3 时引入的第二个缺陷，被既有验收测试当场抓住**：锁序交换让六个并发 create 全部
  通过 `MaxPerSpace=1`。根因不在锁，在 read view —— `FOR SHARE OF sm` 里**不在 OF 列表中
  的 JOIN 表按一致性读处理，而一致性读会打开事务的 read view**，于是快照冻结在 `space`
  行锁之前，三个创建配额全按旧快照计数。在 MySQL 8.0.33 上做了有/无 JOIN 的对照实验坐实。
  修法是给 create 单独一个无 JOIN 的席位锁；丢掉的 JOIN 不损失保证，Space 活性由紧随其后
  的排他锁更强地复核。为此加了源码 guard，因为把两个 helper "统一"回去会静默重现它。
- **一个 code 的文案也是分类的一部分**：actor 级失败复用 target 级 code，文案会说"目标用户
  不是该空间的有效成员"——对调用者自己而言仍指错方向。新注册 actor 级 code；create 端点
  本来就无 target，那处是既有误标。
- **非回归证据从推理升级为实测**：建基线 worktree 对照 brief 点名的四个包。`modules/group`
  初次 5 个 FAIL 定位为 WuKongIM 容器老化（同一份基线代码两次跑失败集 3 → 39，全是 IM
  context deadline），重启容器后 HEAD 整包全绿；`modules/thread` 两侧同构 flaky（迁移竞态）。
- **方法论修正**：`timeout` 在 macOS 不存在，批量跑包时命令静默失败、测试根本没执行，而
  grep 无输出被读成"无失败"。凡是"没有输出即通过"的判断都要先确认命令真的跑了。

## 2026-09-06 — project P0：PR #841 第三轮 review 修复（改为结构化而非点状补丁）

两位 reviewer 都在真 MySQL 上执行并发断言，独立收敛到一个 P0 + 两个 P1，并都指出同一个
模式：连续三轮，每轮的修复都在**上一轮修复没走到的路径**上留下同类问题。

- **本轮的教训是范围而不是原因**。第二轮我把 read view 陷阱的原因写对了（甚至写进 guard 的
  失败消息），却把 guard 指向了一个调用点。另外五条写路径的首条语句仍是 JOIN 版 helper，
  后果比配额失效严重得多：**两个 owner 并发退出让项目变成 0 owner**，P0 无修复路径、四个
  对账扫描无一检测。实测复现（service 方法在 project 行锁上真实排队 ~700ms，拿到锁后
  `queryMemberTx` 是 FOR UPDATE 读新值、授权聚合读旧快照——所以 code review 看不出来）。
- **两个纪律都做成结构性的**：读可见性 guard 改为解析每条在 `*dbr.Tx` 上执行的 SELECT
  （新代码自动覆盖）；所有席位锁改为一条 `uid IN (...)` 语句让 InnoDB 定序（排序无效，
  disband 扫描按 id 排不按 uid 排）；七个写入口统一经有界 1213/1205 重试。
- **险情，与第二轮同一失败模式**：把写方法重命名为 `...Once` 后，三个源码 guard 开始检查空的
  重试包装器——一个响亮失败，**两个变绿却什么都没检查**。能被一次重命名打败的 guard 不是
  guard；现在统一经 `implBody()` 解析实现体，并在包装器背后改实现重做了变异验证。
- **guard 的判据要精确到执行上下文**：读可见性 guard 初版用"文件里是否出现裸 COUNT"，误报了
  列表端点的 `member_count` 列和指标采集——那两处在 session 上、不在写事务里、不授权任何
  东西。改判据为"是否在 `*dbr.Tx` 上执行"。
- **顺带发现统一 helper 能免费修好另一条 review 项**：所有路径都走同一个席位锁 helper 之后，
  actor/target 分类自动补齐 6/6，update/disband 不再把授权拒绝渲染成 Internal 500。
- **P2 拆后续 PR**（reviewer 建议）：缓存 cache-aside stale positive、对账随历史行增长。
  collation 按指示本轮不查，PR 明确标注未在具名部署验证。

## 2026-09-06 — project P0：PR #841 第四轮 review 修复

第三轮那个"结构化"的修复 commit 自己引入了两个 blocker：给四个 handler 加 actor arm 时
**全都忘了 `return`**。Go 的 case 不贯穿到下一个 case，但会掉出 switch，于是控制流走上成功
路径——update **真的 panic**（nil model 进 toResp），disband 给一次**被拒绝的解散**写了审计并
在错误信封上叠了第二个 JSON body。reviewer 只报了这两个；leave/role 也缺 return，无害纯粹
因为它们的 switch 恰好是函数最后一句。

- **单点变异不够，要变异"写错"而不只是"删掉"**。第三轮我给这个 arm 写了 guard，但只是
  `assert.Contains(sentinel)`，且只变异验证了「删掉 arm」。substring 检查看不出 arm 是否终止。
- **判据要精确到语境**。强化 guard 的第一版要求每个 arm 都 return，立刻标红三个——核对后是
  要求错了：leave/role 以 switch 结尾（掉出去即函数结束），addMembers 的 switch 在逐目标循环
  里、不终止正是"继续下一个 uid"。改为「switch 之后有代码时才要求终止」。
- **guard 家族被系统性审了一遍，四条成立**：复现测试驱动重试包装器（P0 级复现变成抛硬币，
  同类问题我自己还漏了 createProject 那个）；`LessOrEqual(n,1)` 允许 0；
  `strings.Count("reconcileMaxPages")<4` 因函数名含同名子串而不可能失败；一个测试用自己手写的
  SQL 当 oracle、从不调用产品代码，而那条 SQL 正是代码注释说明"错了三重"的废弃形态——测试把
  删掉的 bug 编码成了断言。
- **写新 guard 时当场又犯同一类错**：禁止 `p.Error(` 的检查被 `zap.Error(err)` 误报（za**p.Error(**）。
  子串匹配的 guard 要用词边界。
- **`cursors` 是包级全局，reset 从未在 setup 调用，而且漏了我自己第三轮加的两个字段**。CI 跑
  `-shuffle=on`，所以"期望 0"的对账断言可能因为前一个用例留下的截断轮转而通过。已在 setup
  调用 + 加字段覆盖 guard。
- **collation 有实测答案了**：生产四列全是 `0900_ai_ci`。根因是 dump 导入（mysqldump 对
  collation 等于源库默认的表省略 COLLATE），所以 `general_ci` 是意图、`0900` 是导入事故。
  **不能逐表转**——那三张表还和 robot/group_member/group/app_bot/opanalytics 维表 JOIN，只转
  它们会打破现在兼容的那些。方案是把 dump 那批一次转完。顺带发现：默认 collation 的库上
  **迁移无法从零重放**（category 的 `group_setting ⋈ group_category` 1267），会咬到下个新环境。

## 2026-09-06 — project P0：PR #841 第五轮 review 修复

三位 reviewer 收敛到同一组：两个 blocker + 一处记录不一致。两人各自在真 MySQL 上验证。

- **我上一轮亲手写的安全声明不成立**。迁移注释写着「create flag 默认 false 使这成为 pre-enable
  检查项而非故障」——对 HTTP 面成立，对后台任务不成立：`startReconcileWorker()` 是 `Route()` 第
  一句，不看任何 flag。两人独立确认**空表也失败**（collation 在语句**解析**时聚合）。而且三个
  gauge 只在完整轮转时 publish → **永远不 publish** → 监控读起来是「健康、零违约」，实际从未跑过。
- **没照搬 reviewer 的门控建议，两处故意不同**：① 不复用 create flag——写开关关掉时（止血）恰恰
  最需要不变量监控，耦合会在那一刻让它变瞎；② 不门控整个 worker——ownerless/epoch/分布指标只碰
  本模块自己的表，一刀切白丢能工作的观测，而 ownerless 检测的是 P0 无法修复的状态。测试**双向**
  钉住范围（过度门控同样变红）。
- **让门控诚实而非遮盖**：flag 关时启动 Warn 点名 env 变量（没人宣布的「监控缺失」比「监控坏了」
  更糟）；新增 `reconcile_scan_failures_total`（此前「从未跑」与「跑了没发现」在看板上同形，
  这正是漂移会呈现的样子）。
- **又一次：我的测试固定了错误行为**。I1 扫描对 banned Space 豁免过宽，而那个测试的 fixture 用
  `removeSpaceMember`(status=0) 再 ban——cleanup 语义下那人**不是成员**，cascade 不会跳过他，
  所以测试恰好在两者**不一致**的场景上断言了「一致」。一个 cleanup 语义谓词替掉原来的两个。
- **删函数会让 guard 变 vacuous**：删掉 `checkSpaceMembershipForWriteTx` 后，`assert.NotContains`
  那句永远成立。改为点名**仍存在**的 JOIN 版 helper。它的测试不是删而是**改指在用的函数**。
- **实施中自己抓到三个**：指标标签不一致（`abandoned_leak` vs histogram 的 `abandoned`，看板
  join 不上，已加一致性 guard）；既有 helper `histogramSum` 只 return 第一个 series 而非求和
  （加第五个标签后整包跑失败、单独跑通过）；leave/role 缺 return 今天无害只因 switch 恰在函数末尾。
- **collation 的正确 guard 形态**：CI 建库就是 general_ci，所以断言 legacy 列是 general_ci 的
  guard 靠继承通过、永不失败。改成**故意制造漂移**：独立库（共享表不能在 shuffle 下改）+
  `CREATE TABLE ... LIKE` 从真实 schema 复制（零手写 DDL）+ 调产品查询方法。它同时是转换的验收证据。
- **Jerry-Xin 撤回了他自己第四轮的建议**：只转 space/space_member/user 会让 `modules/space` 与
  `modules/user` 的现网 join 报 1267（robot/group_member 仍 0900）。坐实「不能逐表转」。
## 2026-09-06 — oidc-bearer-jwt-redemption-ledger

- **The anchor, not the number** — `/exchange-jwt` judged a bearer JWT's freshness
  by `now - iat <= 10min`. `iat` is when the upstream signed the token, which says
  nothing about when the user came to redeem it, so the ceiling refused a client
  that exchanged 36 minutes after login (a 401 indistinguishable from "invalid
  credential") while still admitting anyone who captured the token inside the
  window. Freshness now comes from the redemption itself: a Redis ledger keyed by
  `sha256(token)`, bounding how late a **first** redemption may arrive (F, default
  24h) and how long a token may sit unused **between** redemptions (T, default 7d).
- **A sliding window is not a TTL** — the first design set `TTL = T` and read
  expiry as "abandoned". Backwards: an expired key is indistinguishable from a
  never-seen token, so the record's death would readmit the token as a first
  redemption. The record must outlive the window (TTL = the token's own remaining
  life) and idleness is computed from the stored `last_at`.
- **A count cap was dropped before it shipped** — capping redemptions per token
  stops nothing (an attacker needs one), and would refuse a client that restarts
  often, with exactly the 401 this task removes. Kept as the `admit_repeat`
  metric, which answers whether the client reuses one token instead of enforcing
  an unfounded limit.
- **Verification stayed side-effect-free** — `api_exchange.go` reuses the same
  verify method to classify a credential posted to the wrong endpoint. A ledger
  inside the verifier would have given that misdelivered token an idle window it
  never earned; the side effect lives in the one caller that redeems.
- **Degraded path keeps F** — with Redis down we cannot tell first from repeat, so
  the fallback applies F alone: bounded, never "anything within exp", and not a
  self-inflicted login outage. `degraded_*` labels are pre-warmed so the alert can
  exist before the first outage.
- **Deliberately not done** — true one-shot redemption. Whether the client redeems
  once or on every launch is not recorded anywhere in the repo; making it one-shot
  now would break the second case with the same indistinguishable 401. `T` is
  configurable, so tightening later is a decision, not a redesign.

## 2026-09-06 — oidc-bearer-jwt-redemption-ledger (review round)

- **The same outage, through another door** — `normalized()` guarded against
  `F <= 0` because `F=0` refuses every login, then let `F=500ms` through: the Lua
  script compares whole seconds, so it arrived as `0`. A guard written against one
  spelling of a value missed the other. Bounds are now truncated to whole seconds
  with a 1s floor, which incidentally fixed a real divergence between the Go
  degraded path (Durations) and the Lua path (seconds).
- **A defensive branch that could never run** — `admitRedemption` refuses a nil
  credential, but the caller dereferenced it one line earlier in a log field. The
  guard read as safety and was decoration; on an unauthenticated endpoint it would
  have been a panic.
- **A cap that silently rewrote a configured value** — `T` longer than the
  record's max TTL could not fire, and the resulting refusal was attributed to `F`,
  which is the knob an operator would then tune. Capped where it is read so the
  startup log prints what actually applies.
- **Close() wrote a field the request path reads** — mirrored an existing pattern
  without noticing the new field is read on the hot path. The closable client now
  lives in its own field.
- **Verified against real infrastructure** — MySQL 8.0.46 + Redis 7.0.15 brought
  up locally (this environment has no docker): the whole `modules/oidc` package
  passes under `-race`, including the ledger's decision table and a new
  concurrency case proving the Lua decision-and-write is one atomic round trip
  (8 concurrent redemptions of one token yield exactly one `admit_first`).
- **Learnings** — `learnings/pending/a-sliding-window-is-not-a-record-ttl.md`
  (absence cannot mean both "never seen" and "expired"; the record must outlive
  the window) and
  `learnings/pending/validate-in-the-representation-the-consumer-uses.md`
  (a guard protects the value as its consumer sees it, not as its author wrote it).

## 2026-09-06 — oidc-bearer-jwt-redemption-ledger (PR #843 review)

- **An argument written for one bound and not turned around on its twin** — the
  file argued at length why `T` cannot exceed the record's lifetime, then left `F`
  uncapped, where the same overflow silently defeats `T` entirely. Both are capped
  now. Worth looking for whenever a rule is written next to its sibling.
- **A degraded path is only justified while it is never looser** — applying `F`
  alone made a Redis outage more permissive than normal operation for any
  deployment configuring `T < F`. The fallback bound is `min(F, T)`; on the
  defaults it is unchanged, which is why nothing caught it.
- **Two failure states sharing one metric label hide the worse one** — "ledger
  never configured" and "Redis is down" both reported `degraded_*`. The first does
  not self-heal and disables `T` outright; on a dashboard it read as flapping
  Redis. Separate labels, plus a `New()` wiring test, since every handler test
  injects a double and would stay green if the constructor were dropped.
- **A regression test that stubs the mechanism does not pin the fix** — the
  36-minute case proved the handler no longer applies a ceiling, not that the
  shipped default admits it. The default could have been reverted to ten minutes
  with the suite green.
