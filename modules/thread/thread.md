# 群组子区 (Threads) 技术文档

## 概述

群组子区功能允许在群组内创建独立的讨论话题，类似 Discord Threads。子区继承父群的成员权限，所有父群成员都可以查看和参与子区讨论。

## 数据模型

```
┌─────────────────────────────────────────────────────────────┐
│  group (父群)                                                │
│  ├── group_no: 32位十六进制                                  │
│  └── group_member: 群成员列表                                │
│                                                              │
│  thread (子区)                                               │
│  ├── short_id: snowflake ID                                 │
│  ├── group_no: 所属父群                                      │
│  ├── channel_id: {group_no}____{short_id}                   │
│  ├── channel_type: 5 (ChannelTypeCommunityTopic)            │
│  └── thread_member: 主动加入的成员                           │
└─────────────────────────────────────────────────────────────┘
```

### channelID 格式

```
{groupNo}____{shortId}
```

- `groupNo`: 32位十六进制（如 `04f51b141553442ca63d7d10b1274be5`）
- `shortId`: snowflake ID（如 `2039626171074744320`）
- 分隔符: `____`（四个下划线，WuKongIM 不支持 `@` 等特殊字符）

### 数据库表

#### thread

```sql
CREATE TABLE `thread` (
    `id` BIGINT AUTO_INCREMENT PRIMARY KEY,
    `short_id` VARCHAR(32) NOT NULL COMMENT 'snowflake ID',
    `group_no` VARCHAR(40) NOT NULL COMMENT '父群编号',
    `name` VARCHAR(100) NOT NULL COMMENT '子区名称',
    `creator_uid` VARCHAR(40) NOT NULL COMMENT '创建者',
    `source_message_id` BIGINT DEFAULT NULL COMMENT '来源消息ID',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1=活跃,2=归档,3=删除',
    `version` BIGINT NOT NULL DEFAULT 0,
    `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    `updated_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY `uk_short_id` (`short_id`),
    INDEX `idx_group_no` (`group_no`)
);
```

#### thread_member

```sql
CREATE TABLE `thread_member` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `thread_id` BIGINT UNSIGNED NOT NULL COMMENT '子区ID',
    `uid` VARCHAR(40) NOT NULL COMMENT '用户UID',
    `role` TINYINT NOT NULL DEFAULT 0 COMMENT '0=普通成员, 1=创建者',
    `version` BIGINT NOT NULL DEFAULT 0,
    `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_thread_uid` (`thread_id`, `uid`),
    KEY `idx_uid` (`uid`)
);
```

## 权限模型

| 操作 | 权限 |
|------|------|
| 查看子区列表 | 父群成员 |
| 创建子区 | 父群成员 |
| 查看子区消息 | 父群成员（全部可见） |
| 发送消息 | 父群成员 |
| 加入/离开子区 | 父群成员 |
| 归档/取消归档 | 创建者 或 群管理员 |
| 删除子区 | 创建者 或 群管理员 |

## API 接口

### 完整路由（需要 group_no）

| Method | Path | 说明 |
|--------|------|------|
| POST | `/v1/groups/{group_no}/threads` | 创建子区 |
| GET | `/v1/groups/{group_no}/threads` | 列出子区 |
| GET | `/v1/groups/{group_no}/threads/{short_id}` | 获取详情 |
| GET | `/v1/groups/{group_no}/threads/{short_id}/members` | 成员列表 |
| POST | `/v1/groups/{group_no}/threads/{short_id}/join` | 加入子区 |
| POST | `/v1/groups/{group_no}/threads/{short_id}/leave` | 离开子区 |
| POST | `/v1/groups/{group_no}/threads/{short_id}/archive` | 归档 |
| POST | `/v1/groups/{group_no}/threads/{short_id}/unarchive` | 取消归档 |
| DELETE | `/v1/groups/{group_no}/threads/{short_id}` | 删除 |

### 简化路由（只需 short_id）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/v1/threads/{short_id}` | 获取详情 |
| POST | `/v1/threads/{short_id}/join` | 加入子区 |
| POST | `/v1/threads/{short_id}/leave` | 离开子区 |

### 请求/响应示例

#### 创建子区

```bash
POST /v1/groups/{group_no}/threads
Content-Type: application/json

{
  "name": "讨论话题",
  "source_message_id": 12345  // 可选，从某条消息创建
}
```

#### 响应

```json
{
  "short_id": "2039626171074744320",
  "group_no": "04f51b141553442ca63d7d10b1274be5",
  "channel_id": "04f51b141553442ca63d7d10b1274be5____2039626171074744320",
  "channel_type": 5,
  "name": "讨论话题",
  "creator_uid": "xxx",
  "status": 1,
  "member_count": 1,
  "created_at": "2026-04-02 16:49:08",
  "updated_at": "2026-04-02 16:49:08"
}
```

## IMDatasource 集成

子区通过 IMDatasource 回调与 WuKongIM 集成：

| 回调 | 说明 |
|------|------|
| `HasData` | 检查子区是否存在，返回支持的数据类型 |
| `Subscribers` | 返回父群所有成员（允许查看和发送消息） |
| `Blacklist` | 继承父群黑名单 |
| `Whitelist` | 继承父群禁言白名单（禁言时返回管理员列表） |
| `ChannelInfo` | 返回子区状态（`ban=1` 表示已归档/删除） |

## 消息流程

### 创建子区

```
1. 验证是父群成员
2. 生成 short_id (snowflake)
3. 插入 thread 表
4. 插入 thread_member (创建者, role=1)
5. 创建 IM 频道 (subscribers = 父群所有成员)
6. 发送通知消息到父群 (type=1100)
```

### 子区创建通知消息

创建子区时向父群发送通知消息：

```json
{
  "type": 1100,
  "content": "guobin 创建了子区「测试子区」",
  "from_uid": "xxx",
  "from_name": "guobin",
  "short_id": "2039626171074744320",
  "channel_id": "04f51b14...____2039626171074744320",
  "channel_type": 5,
  "thread_name": "测试子区"
}
```

客户端需识别 `type: 1100` 并渲染为可点击的子区入口卡片。

### 发送消息

```
1. WuKongIM 调用 IMDatasource.Subscribers
2. 返回父群所有成员 → 允许发送
3. (待实现) webhook 回调 → 自动加入 thread_member
```

### 接收消息

```
1. 父群成员都在 subscribers 列表
2. 所有父群成员都能收到子区消息
```

## 实现状态对比

### 当前实现 vs Discord Threads

> 参考: [Discord Threads API](https://discord.com/developers/docs/topics/threads) | [Discord Threads FAQ](https://support.discord.com/hc/en-us/articles/4403205878423-Threads-FAQ)

| 功能 | Discord | 当前实现 | 状态 |
|------|---------|----------|------|
| **子区类型** |
| Public Thread | ✅ 所有频道成员可见 | ✅ 父群成员可见 | 完成 |
| Private Thread | ✅ 邀请制私密子区 | ❌ | 未实现 |
| Forum Channel | ✅ 专用帖子频道 | ❌ | 未实现 |
| Media Channel | ✅ 媒体帖子（Beta） | ❌ | 未实现 |
| **基础功能** |
| 创建子区 | ✅ | ✅ | 完成 |
| 从消息创建子区 | ✅ | ✅ 字段已有 | ⚠️ 前端未实现 |
| 子区列表 | ✅ | ✅ | 完成 |
| 删除/归档 | ✅ | ✅ | 完成 |
| **成员模型** |
| 发消息自动加入 | ✅ | ❌ | 需 webhook |
| 主动加入/离开 | ✅ | ✅ | 完成 |
| 成员列表 | ✅ `/thread-members` | ✅ `/members` | 完成 |
| 成员计数 | ✅ | ✅ | 完成 |
| **实时事件** |
| threadCreate | ✅ Gateway 事件 | ⚠️ 父群通知消息 | 不同机制 |
| threadUpdate | ✅ Gateway 事件 | ❌ | 未实现 |
| threadDelete | ✅ Gateway 事件 | ❌ | 未实现 |
| threadMembersUpdate | ✅ 成员变动推送 | ❌ | 未实现 |
| **消息** |
| 父群通知消息 | ✅ 可点击入口 | ✅ type=1100 | ⚠️ 前端渲染 |
| 历史消息可见 | ✅ 所有父群成员 | ✅ | 完成 |
| **生命周期** |
| 自动归档 | ✅ 1h/24h/3d/7d 四档 | ❌ | 未实现 |
| 自动锁定 | ✅ 归档后禁止回复 | ⚠️ ban=1 禁止发送 | 部分实现 |
| 自动隐藏已归档子区 | ✅ | ❌ | 未实现 |
| **高级特性** |
| 慢速模式 | ✅ `rate_limit_per_user` | ❌ | 未实现 |
| 置顶消息 | ✅ | ❌ | 未实现 |
| **通知** |
| @提及通知 | ✅ | ❌ | 未实现 |
| 未读计数 | ✅ | ❌ | 未实现 |
| 只通知已加入成员 | ✅ | ❌ | 未实现 |
| **同步** |
| 父群成员退群同步 | ✅ | ❌ | 未实现 |
| 父群删除级联删除 | ✅ | ❌ | 未实现 |

### 核心差异说明

1. **Private Thread** — Discord 支持邀请制私密子区，DMWork 当前只有公开子区（所有父群成员可见）

2. **自动归档** — Discord 提供 1h/24h/3d/7d 四档自动归档选项，DMWork 需要手动归档

3. **实时事件** — Discord 通过 Gateway 推送 `threadCreate`/`threadUpdate`/`threadMembersUpdate` 等事件，DMWork 当前通过父群消息通知 + 客户端轮询

4. **发消息自动加入** — Discord 原生支持发送消息自动加入 thread members，DMWork 需要通过 WuKongIM webhook 实现

### 优先级建议

**P0 - 核心体验**
1. 发消息自动加入 thread_member（通过 WuKongIM webhook）
2. 前端渲染子区入口消息（type=1100）

**P1 - 重要功能**
3. 父群成员退群 → 移除子区订阅
4. 未读计数（基于 thread_member）
5. 从消息创建子区（前端交互）

**P2 - 增强功能**
6. 自动归档（定时任务，7天无消息）
7. @提及通知
8. 父群删除级联

## 常量定义

```go
// 子区状态
const (
    ThreadStatusActive   = 1 // 活跃
    ThreadStatusArchived = 2 // 已归档
    ThreadStatusDeleted  = 3 // 已删除
)

// 成员角色
const (
    MemberRoleNormal  = 0 // 普通成员
    MemberRoleCreator = 1 // 创建者
)

// channelID 分隔符
const ChannelIDSeparator = "____"

// 消息类型
const ContentTypeThreadCreated = 1100 // 子区创建通知
```

## 参考

- [Discord Threads FAQ](https://support.discord.com/hc/en-us/articles/4403205878423-Threads-FAQ)
- [Discord Threads API](https://discord.com/developers/docs/topics/threads)
- [GitHub Issue #867](https://github.com/Mininglamp-OSS/octo-server/issues/867)
