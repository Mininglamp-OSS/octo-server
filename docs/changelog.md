# Changelog

## [Unreleased] - 2026-09-11

### Project 协作
- Project 支持同一 Space 内重名、字面名称搜索及 `X-Total-Count` 列表计数；GET 列表为纯读，仅返回已有且当前有权访问的显式创建 Project，空列表不产生写入。
- 成员批量入口 `POST /v1/projects/:project_id/members/add` 原子应用成员/管理员角色；新增专用 Owner 转让 `PUT /v1/projects/:project_id/owner`；退出 `POST /v1/projects/:project_id/leave` 按唯一 Owner 规则校验。
- 群 Project 关系支持 `GET/PUT/DELETE /v1/groups/:group_no/project` 的查询、绑定、换绑和解除，并支持 `POST /v1/group/create` 从 Project 创建群；原生群成员席位独立于 Project 关系并在解除关联后保留。
- 关联群支持个人置顶：新增 `PUT /v1/projects/:project_id/groups/:group_no/setting`，Project 成员可独立置顶关联群入口；列表返回 `pinned`，置顶项在分页前按服务端时间排序。解绑后隐藏、同一 Project 重绑恢复偏好，且不改变原生群成员关系或聊天权限。
- 新增 Project 成员单查 `GET /v1/projects/:project_id/members/:uid`：沿用 `MemberResp` 列表投影和 Project 角色，按 `project_id + uid` 直接有界读取；同一 RR 只读事务复核调用者组织/Project 成员资格，未知或已移除目标返回既有 not-found，目标账号/Space 状态不额外改变成员列表语义，响应仅是 Project 关系事实而非 Drive 授权决定；Drive 可用原始用户 session `token` 携带路径 selector 获取关系事实。
- 新增 Project 专用成员候选接口 `GET /v1/projects/:project_id/member-candidates`：仅返回当前组织有效真人，支持复用 Project 分页、字面姓名搜索和 `X-Total-Count`，并在服务端投影 `current_user`、`already_member`、`invitable` 三种状态；仅 Project Owner/Admin 可访问，候选结果不包含邮箱等额外个人信息。
- `GET /v1/group/my` 的 `role=owner|admin|owner,admin` 筛选与 `space_id` 组合收口，先校验 active Space 并排除解散群、非活跃成员、外部管理脏角色及已撤权 Space；无 `role` 保持旧查询，仅补齐群角色回填。
- PR887 review fixes：置顶配额与 membership-only 可见性对齐，移除/closing 的历史 pin 不再耗尽用户在该 Space 的槽位；达到上限后仍可置顶新的可见 Project。
- 统一 Sidebar 的 Project 内容与 `GET /v1/projects/:project_id/groups` 的
  `ProjectGroupRelation` 投影：返回 Project 的存活关联群，不再按调用者的原生
  `group_member` 席位过滤，同时保留 Project 成员授权、置顶排序和默认 50 条上限。
- Project-backed 建群将非成员、禁用目标和跨 Space 等预期拒绝返回本地化 D14 4xx envelope（legacy wire status 仍为 400），`POST /v1/group/create` 同时接入共享 UID 限流；原生群管理门和 active 角色冲突的整批原子拒绝补齐回归覆盖。
- Project 建群服务补齐 `BotUID` 的目标 Space 席位校验，与建群写入共用事务及既有锁顺序。跨 Space Bot 会拒绝整次建群；Space 内 Bot 无需先加入 Project，已有 Project Bot 成员保持单条原生记录及 `bot_admin`。
- 统一 Sidebar 的 Project 条目仅对有效 Project 成员可见；Space-listed 非成员的历史
  pin 不回填入口。`groups[]` 是不授予原生聊天权限的关系元数据，客户端字段切换需
  协调上线且本版本不声称已部署；原生群成员同步不属于本 Sidebar 契约，普通发起群和
  关联已有群保持既有独立行为。
- PR887 最新 review 收口：AI session container 关系 PUT/DELETE 在路由和 service 层均拒绝；实际 Group↔Project 关系变更推进 `group.version`，重复绑定/解绑不产生版本噪音；Project 关系列表、分页计数与 Sidebar 批量投影排除历史绑定的 AI 容器，普通群关系不受影响。预设群的原生成员模型与 Project 关系保持独立，允许并存且不增加警告或限制。
- 置顶 upsert 保留 `%w` 错误链，并在重复置顶时修复 `pinned=1,pinned_at=NULL` 的历史偏好；`GET /v1/group/my` 角色列表的成员计数查询失败返回 `query_failed`，不再以成功的 0 掩盖数据库错误。无引用关系辅助函数已删除。
- `joined_at` 采用 rolling expand：仅新增可空 `DATETIME(3)`，旧二进制可省略，读侧统一 `COALESCE(joined_at,created_at)`，新加入/重新加入写 UTC 时间；本版本不做回填或收缩为非空。Space cascade 保留 active human Owner，只有存在 active non-Owner agent rider 时才处理 rider，Owner-only 行不进入分页。
- 经授权的 Drive provisioning 以 `project_id` 管理且不要求 Project 返回 Drive ID：远端支持后使用 `POST /v1/internal/drive/spaces`、`X-Internal-Token` 及 Project 名称、`octo_space_id`、当前 Owner `super_admin_uid`、`project_id`；仅 exact same project 重复请求幂等成功，其他 `409/401/500` 按重试/失败策略处理。`OCTO_DRIVE_INTERNAL_TOKEN` 与 Fleet HMAC 分离，功能关闭时不出站；远端接口及 30 字符名称支持是部署前提。
- Project 对账恢复专属全员群 I4-A 缺群与 I4-B 缺员扫描，保留 `ReconcileEnabled` 昂贵扫描门禁，使用游标分页、宽限期、Space 撤权/移除/封禁/系统 bot 豁免并在完整轮转后发布 gauge；普通 `group.project_id` 关联群保持独立成员快照且不触发该缺员监控。
- Space 撤权时，在同一事务中降级并移除专属群的原生 Owner 成员，保留 Project Owner 身份；恢复按有效账号、Space、Project 和当前专属指针投影最新真人 Owner，兼容跨表混合 collation。Space 和 Project 重新加入在成员事务内写入 `reason=rejoined` 投影任务；迟到退订的补订阅失败由清理回调补写持久任务，投影 worker 的失败由原任务重试。仅合并尚未执行、无租约、无游标且零次尝试的 pending 任务，避免越过新请求对应的 Project；成功分页持久化游标并归还 attempt。普通关联群保持原生成员快照。
- Space 清理工单的立即入队与分页续跑时间按数据库毫秒精度截断，保证提交后的工单可立即认领；事件回归测试隔离进程级监听器，支持重复运行。
- 运维注意：`project_i2_violations_total`、`group_admission_rejected_total` 及全员群 guard failure 计数器已移除，旧面板/告警应删除或允许序列缺失。Bot/IM 提示、订阅及其他提交后通知均按 best-effort 处理，数据库提交事实权威，失败只记录并走既有补偿/重试路径；缺失上述指标本身不是服务故障。
- Space ID 解析改由数据库按 `space` 表实际 collation 返回存储值，旧 `rejoined` 任务在 worker 边界解析原始 selector；解析/查询失败沿原 lease 重试，`rejoined` 遇 Space 缺失或解散终止恢复，普通 Space removal 仍按原始 selector fail-safe 清理，避免大小写/PAD SPACE 或缺失 Space 导致错误跳过。
- I1 与 abandoned-cleanup 对账仅对正常 Project 豁免 Space 撤权后保留的 Owner；已解散或不存在 Project 的 active Owner 且无有效 Space 席位仍报告，普通成员与 `LIMIT` inspected base 分页语义不变。

## [v1.1.2] - 2026-03-05

### 新功能
- Bot 历史消息拉取接口 `POST /v1/bot/messages/sync` — Bot 可获取群聊/私聊历史消息，支持分页和方向控制 (#50)
- Bot skill.md 补全 — 新增 Groups、Event Ack、Messages Sync 等 5 个 API 文档 (#49)

### 基础设施
- Go 模块重命名 `TangSengDaoDao` → `dmwork-org`（167 个文件），解锁 GitHub Actions CI (#42)
- 创建 `Mininglamp-OSS/octo-lib` 公共核心库
- dmwork-adapters CI 流水线（tsc + vitest + build）
- 部署脚本 `deploy.sh` 支持 `server|web|adapter|all` 四种组件
- CD 改为手动触发（有在线用户，需控制部署窗口）

### 改进
- npm 包名冲突修复 — V2 版本 (1.0.0/1.0.1) 从 `octo` 下架，V2 独立为 `openclaw-channel-deepim`
- OpenClaw adapter 升级到 0.2.19（媒体消息、@mention 修复）
- 开发流程新增 Issue 认领步骤，避免重复开发

### 文档
- `docs/CI-CD.md` — CI/CD 流程说明
- `docs/WORKFLOW.md` — 完整运作流程（含认领步骤）

### 团队
- 邀请 `Jerry-Xin` 加入 dev 团队
- dmwork-adapters 分支保护启用（需 1 人 review）

## [v1.1.1] - 2026-03-04

### 新功能
- 搜索用户支持邮箱查找 — 输入完整邮箱可搜索添加好友
- Android 文本文件内置预览 — yaml/json/md/conf/代码文件点击后直接预览（等宽字体，可复制）
- Android 未知格式文件自动保存到下载目录（不再报错"格式不正确"）

### 修复
- Android 忘记密码验证码无效 — `emailSendCode` 未传 `code_type` 参数，默认 0（注册）但验证用 2（忘记密码），Redis key 不匹配
- Android 文件消息显示"未知消息" — `WKFileContent` 未注册到 WuKongIM SDK 消息管理器和视图提供器

### 改进
- Android App Logo 更新为网页版 Logo
- APK 下载地址统一到主域名 `https://api-test.example.com/download/dmwork.apk`

### 文档
- 添加团队协作流程规范 `docs/workflow.md`

### 团队
- 组织成员邀请：`lml2468`（dev/研发）、`yeejiaa`（product/产品）

## [v1.1] - 2026-03-04
### Security
- Bot token 吊销时正确撤销 IM token（cleanupBotConnection）
- 删除机器人时清理 IM 连接、Redis 心跳和事件队列
- WuKongIM 管理 API (5300) 限制为 127.0.0.1 访问
- robotList API 权限升级为 SuperAdmin (#36 → #37)
- Android 文件下载路径遍历防护（sanitizeFileName）(#21)
- Android 文件选择 100MB 大小限制 + 危险扩展名黑名单 (#22)

### Fixed
- Web Unicode emoji 显示为方块 — 添加 Segoe UI Emoji / Noto Color Emoji 字体回退 (#14)

### Infrastructure
- 仓库迁移到 dmwork-org GitHub 组织
- 添加 Feature Request / PR 模板
- 建立 Milestone v1.1 + Labels 体系
- OpenClaw adapter 升级到 0.2.17（BodyForAgent + 滑动窗口历史）

### Previous (v1.0 → v1.1)
- 邮箱验证码注册登录 (#35)
- Bot register 支持 force_refresh (#34)
- 全局搜索优雅降级 (#33)
- 本地默认头像生成 (#31)
- Bot HTTP API 压测脚本 (#30)
- API 测试 28/28 通过 (#29)
- 支持同时 @多个 Bot (#23)
- 安全加固: bcrypt 密码 + Webhook HMAC (#17)
- Bot 增强: @群聊路由 + 入群回调 + 自动已读 (#16)
- 文件模块安全增强 (#11)

## [v1.0] - 2026-02-28
- 初始版本
- 基于悟空IM (WuKongIM) 的即时通讯平台
- Web / Android 客户端
- Bot 系统 (BotFather 模式)
