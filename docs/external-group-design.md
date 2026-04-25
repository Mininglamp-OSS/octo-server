# OCTO 外部群技术设计文档

> **版本**: v1.1  
> **日期**: 2026-04-25  
> **作者**: Coda (AI) + 余嘉伟  
> **状态**: 已合入（PR #1167）  
> **代码基线**: dmworkim `origin/develop` @ `13be878`

---

## 1. 概述

### 1.1 背景

OCTO 当前群与 Space 是 1:1 强绑定（`group.space_id`），建群和加人时强制校验 Space 成员资格。为支持跨组织协作场景，需要在不破坏 Space 隔离体系的前提下，允许外部 Space 用户加入群聊。

### 1.2 目标

- 外部 Space 用户可通过扫码或被邀请加入群
- 群自动识别为「外部群」（企微模式，无手动开关）
- 外部群在用户来源 Space 的会话列表中正确展示
- 外部成员可邀请自己 Space 的联系人和 Bot
- 零 WuKongIM 改动，Android/iOS 仅 UI 标识（无逻辑改动）

### 1.3 核心设计决策

| 决策项 | 结论 |
|--------|------|
| 外部群识别方式 | 自动分类（企微模式），有外部成员即为外部群 |
| 手动开关 | 不需要，已有 `invite=1` 作为安全阀 |
| source_space_id 来源 | `GetUserDefaultSpaceID(user)`，不需要客户端传参 |
| 外部 Bot | 允许，检查 Bot 在邀请人的 Space 内即可 |
| 跨第三方 Space 邀请 | 天然不可能（通讯录隔离） |
| 关闭外部群 | 所有外部成员退出后自动恢复为普通群 |

---

## 2. 数据库设计

### 2.1 Migration

文件：`modules/group/sql/group_20260424-01.sql`

```sql
-- +migrate Up
ALTER TABLE `group` ADD COLUMN `is_external_group` SMALLINT NOT NULL DEFAULT 0 COMMENT 'External group: 0=no, 1=yes (auto-maintained when external members join/leave)';
ALTER TABLE `group_member` ADD COLUMN `is_external` SMALLINT NOT NULL DEFAULT 0 COMMENT 'External member: 0=no, 1=yes';
ALTER TABLE `group_member` ADD COLUMN `source_space_id` VARCHAR(40) NOT NULL DEFAULT '' COMMENT 'Source Space ID for external members';
CREATE INDEX `idx_group_member_external` ON `group_member` (`uid`, `is_external`, `is_deleted`);

-- +migrate Down
DROP INDEX `idx_group_member_external` ON `group_member`;
ALTER TABLE `group_member` DROP COLUMN `source_space_id`;
ALTER TABLE `group_member` DROP COLUMN `is_external`;
ALTER TABLE `group` DROP COLUMN `is_external_group`;
```

### 2.2 字段说明

| 表 | 字段 | 类型 | 说明 |
|----|------|------|------|
| `group` | `is_external_group` | SMALLINT | 冗余字段。首个外部成员加入时设 1，最后一个退出时设 0 |
| `group_member` | `is_external` | SMALLINT | 此成员是否为外部成员（不在群的 space_id 对应 Space 中） |
| `group_member` | `source_space_id` | VARCHAR(40) | 外部成员的来源 Space。外部群在此 Space 的会话列表中显示 |

### 2.3 索引

`idx_group_member_external (uid, is_external, is_deleted)` 随 migration 创建，命中
以 `uid` 为起点的外部群查询：`QueryExternalGroupNosForUser` 与 `QuerySourceSpaceIDForMember`。
`QueryExternalMemberCountTx` 以 `group_no` 起点过滤，走既有的 `group_no` 索引；
此处列出只是说明「未漏建索引」，不是它们都用新复合索引。

---

## 3. 后端改动

### 3.1 modules/group/db.go — Model 扩展

```go
// Model 新增字段
type Model struct {
    // ... 现有字段 ...
    IsExternalGroup int // 外部群（自动维护）
}

// MemberModel 新增字段
type MemberModel struct {
    // ... 现有字段 ...
    IsExternal    int    // 外部成员
    SourceSpaceID string // 来源 Space
}

// MemberDetailModel 新增字段
type MemberDetailModel struct {
    // ... 现有字段 ...
    IsExternal    int    // 外部成员
    SourceSpaceID string // 来源 Space
}
```

会在 API 响应中暴露外部成员身份的 SELECT 路径均需补齐
`group_member.is_external, group_member.source_space_id` ——
实际补齐了 `SyncMembers`、`queryMembersWithKeyword`、`queryMembersWithGroupNo`、
`queryMemberWithGroupNoAndUID` 四处。仅服务于管理者过滤的 `queryManagersWithGroupNos`
无需这两列，保持原列表不动。

新增查询方法：

```go
// QueryExternalMemberCountTx 事务内查询群内外部成员数量（FOR UPDATE 行锁防并发）
func (d *DB) QueryExternalMemberCountTx(groupNo string, tx *dbr.Tx) (int64, error) {
    var count int64
    _, err := tx.SelectBySql(
        "SELECT COUNT(*) FROM group_member WHERE group_no=? AND is_external=1 AND is_deleted=0 FOR UPDATE",
        groupNo,
    ).Load(&count)
    return count, err
}

// QueryExternalGroupNosForUser 查询用户作为外部成员加入的群列表，返回 groupNo -> sourceSpaceID
// 用于 space_filter / search 的外部群放行判断及列表接口的批量 SetEffectiveSpaceID。
func (d *DB) QueryExternalGroupNosForUser(uid string) (map[string]string, error) {
    result := make(map[string]string)
    if uid == "" {
        return result, nil
    }
    var rows []struct {
        GroupNo       string `db:"group_no"`
        SourceSpaceID string `db:"source_space_id"`
    }
    _, err := d.session.SelectBySql(
        "SELECT group_no, source_space_id FROM group_member WHERE uid=? AND is_external=1 AND is_deleted=0",
        uid,
    ).Load(&rows)
    if err != nil {
        return result, err
    }
    for _, r := range rows {
        result[r.GroupNo] = r.SourceSpaceID
    }
    return result, nil
}

// UpdateIsExternalGroup 非事务更新 is_external_group，仅留作兜底/管理接口使用
func (d *DB) UpdateIsExternalGroup(groupNo string, value int) error {
    _, err := d.session.Update("group").
        Set("is_external_group", value).
        Where("group_no=?", groupNo).Exec()
    return err
}

// UpdateIsExternalGroupTx 事务内更新 is_external_group，业务主路径统一使用此版本，
// 保证成员写入与群标记一致提交/回滚
func (d *DB) UpdateIsExternalGroupTx(groupNo string, value int, tx *dbr.Tx) error {
    _, err := tx.Update("group").
        Set("is_external_group", value).
        Where("group_no=?", groupNo).Exec()
    return err
}

// QuerySourceSpaceIDForMember 查询某用户作为外部成员在指定群的 source_space_id
// 非外部成员或不存在时返回空字符串。用于详情接口单群 SetEffectiveSpaceID。
func (d *DB) QuerySourceSpaceIDForMember(groupNo, uid string) (string, error) {
    if groupNo == "" || uid == "" {
        return "", nil
    }
    var sourceSpaceID string
    err := d.session.SelectBySql(
        "SELECT source_space_id FROM group_member WHERE group_no=? AND uid=? AND is_external=1 AND is_deleted=0",
        groupNo, uid,
    ).LoadOne(&sourceSpaceID)
    if err != nil && err != dbr.ErrNotFound {
        return "", err
    }
    return sourceSpaceID, nil
}
```

> 成员明细由既有的 `QueryMemberWithUID(uid, groupNo)` 复用，无需新增 `QueryMemberByGroupNoAndUID`。

#### 3.1.1 recoverMemberTx 补齐外部字段

同一 UID 之前被软删除后再次加入时，`recoverMemberTx` 会复用原行并重置字段。
必须把 `is_external` / `source_space_id` 一并覆盖写入，否则重入群的外部成员会继承旧值
（例如曾以内部身份退出、再以外部身份扫码加入，会错误地保持 `is_external=0`）：

```go
func (d *DB) recoverMemberTx(member *MemberModel, tx *dbr.Tx) error {
    _, err := tx.Update("group_member").SetMap(map[string]interface{}{
        "remark":          member.Remark,
        "role":            member.Role,
        "version":         member.Version,
        "is_deleted":      0,
        "invite_uid":      member.InviteUID,
        "is_external":     member.IsExternal,
        "source_space_id": member.SourceSpaceID,
        "created_at":      dbr.Expr("Now()"),
    }).Where("group_no=? and uid=?", member.GroupNo, member.UID).Exec()
    return err
}
```

### 3.2 modules/group/api.go — 核心逻辑

#### 3.2.1 groupScanJoin（扫码入群）

在 `existMember` 检查之后、创建 `memberModel` 之前插入：

```go
// === 外部成员检测 ===
isExternal := 0
sourceSpaceID := ""
if group.SpaceID != "" {
    inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), group.SpaceID, scaner)
    if checkErr != nil {
        g.Error("检查 Space 成员失败", zap.Error(checkErr))
        c.ResponseError(errors.New("检查成员关系失败"))
        return
    }
    if !inSpace {
        // 不在群的 Space → 外部成员
        isExternal = 1
        sourceSpaceID = spacemod.GetUserDefaultSpaceID(g.ctx, scaner)
    }
}

memberModel := &MemberModel{
    GroupNo:       groupNo,
    UID:           scaner,
    Role:          MemberRoleCommon,
    Version:       version,
    Status:        int(common.GroupMemberStatusNormal),
    InviteUID:     generator,
    Vercode:       fmt.Sprintf("%s@%d", util.GenerUUID(), common.GroupMember),
    IsExternal:    isExternal,
    SourceSpaceID: sourceSpaceID,
}
```

首个外部成员加入时在同一事务内维护 `is_external_group`，确保成员写入与群标记一致提交：

```go
// === 事务内标记外部群 ===
markedExternal := false
if isExternal == 1 && group.IsExternalGroup == 0 {
    if updateErr := g.db.UpdateIsExternalGroupTx(groupNo, 1, tx); updateErr != nil {
        tx.Rollback()
        g.Error("更新 is_external_group 失败", zap.Error(updateErr))
        c.ResponseError(errors.New("更新群外部标记失败！"))
        return
    }
    markedExternal = true
}

if err := tx.Commit(); err != nil {
    tx.Rollback()
    c.ResponseError(errors.New("提交事务失败！"))
    return
}

// 事务提交后异步通知群成员刷新 channelInfo
if markedExternal {
    g.ctx.SendChannelUpdateToGroup(groupNo)
}
```

#### 3.2.2 addMembers / AddGroupMembers（邀请入群）

外部检测集中在 `Service.AddGroupMembers` 内完成。对每个待加入成员，先判定是否在群的 Space，
不在则构造 `externalMap` / `sourceSpaceMap` 记录外部标记与来源 Space；操作者的群成员
通过既有的 `QueryMemberWithUID(operatorUID, groupNo)` 获取：

```go
externalMap := make(map[string]bool)
sourceSpaceMap := make(map[string]string)
if groupModel.SpaceID != "" {
    var operatorMember *MemberModel
    if req.OperatorUID != "" {
        operatorMember, _ = s.db.QueryMemberWithUID(req.OperatorUID, req.GroupNo)
    }
    for _, uid := range uniqueUIDs {
        ok, err := spacepkg.CheckMembership(s.ctx.DB(), groupModel.SpaceID, uid)
        if err != nil {
            return nil, errors.New("failed to check space membership")
        }
        if ok {
            continue
        }
        externalMap[uid] = true
        if operatorMember != nil && operatorMember.IsExternal == 1 && operatorMember.SourceSpaceID != "" {
            // 外部成员邀请：同源 Space
            sourceSpaceMap[uid] = operatorMember.SourceSpaceID
        } else {
            // 内部成员邀请：使用被邀请人的默认 Space
            sourceSpaceMap[uid] = spacemod.GetUserDefaultSpaceID(s.ctx, uid)
        }
    }
}
```

> 与早期设计不同：Space 成员校验失败不再直接丢弃（`validUIDs` 过滤），而是保留并标记为外部成员。

在后续遍历构造 `MemberModel` 时填充：

```go
isExt, srcSpaceID := 0, ""
if externalMap[memberUser.UID] {
    isExt, srcSpaceID = 1, sourceSpaceMap[memberUser.UID]
}
newMember := &MemberModel{
    // ...
    IsExternal:    isExt,
    SourceSpaceID: srcSpaceID,
}
```

首次出现外部成员时，在同一事务内调用 `UpdateIsExternalGroupTx` 标记群为外部群，
事务失败会整体回滚；提交成功后再 `SendChannelUpdateToGroup` 通知前端刷新：

```go
markedExternal := false
if hasNewExternal && groupModel.IsExternalGroup == 0 {
    if err := s.db.UpdateIsExternalGroupTx(req.GroupNo, 1, tx); err != nil {
        return nil, errors.New("failed to update external group flag")
    }
    markedExternal = true
}
// tx.Commit() ...
if markedExternal {
    s.ctx.SendChannelUpdateToGroup(req.GroupNo)
}
```

Bot 校验逻辑（`api.go: memberAdd`）：以「邀请人的有效 Space」作为允许集合——
内部邀请人用群的 SpaceID，外部邀请人用其 `source_space_id`：

```go
if group.SpaceID != "" {
    inviterSpaceID := group.SpaceID
    operatorMember, opErr := g.db.QueryMemberWithUID(operator, groupNo)
    if opErr != nil {
        c.ResponseError(errors.New("查询操作者群成员失败"))
        return
    }
    if operatorMember != nil && operatorMember.IsExternal == 1 && operatorMember.SourceSpaceID != "" {
        inviterSpaceID = operatorMember.SourceSpaceID
    }
    for _, memberUID := range req.Members {
        // 查询 robot 标记...
        if isBot == 1 {
            inSpace, checkErr := spacepkg.CheckMembership(g.ctx.DB(), inviterSpaceID, memberUID)
            if checkErr != nil {
                g.Error("检查Bot Space成员失败", zap.Error(checkErr))
                c.ResponseError(errors.New("检查Bot Space成员失败"))
                return
            }
            if !inSpace {
                c.ResponseError(errors.New("该 Bot 不属于你的 Space"))
                return
            }
        }
    }
}
```

#### 3.2.3 退群/踢人时维护 is_external_group

在 `groupExit`（主动退群，api.go）与 `Service.RemoveGroupMembers`（踢人）的事务内完成：
使用 `UpdateIsExternalGroupTx` 并在失败时 rollback 整个操作，避免成员已被删除却残留
`is_external_group=1` 的不一致状态。

`groupExit`（退群者是外部成员时）：

```go
resetExternalGroup := false
if loginMember.IsExternal == 1 && groupInfo.IsExternalGroup == 1 {
    externalCount, countErr := g.db.QueryExternalMemberCountTx(groupNo, tx)
    if countErr != nil {
        g.Error("查询外部成员数量失败", zap.Error(countErr))
    } else if externalCount == 0 {
        if updateErr := g.db.UpdateIsExternalGroupTx(groupNo, 0, tx); updateErr != nil {
            tx.Rollback()
            c.ResponseError(errors.New("更新 is_external_group 失败"))
            return
        }
        resetExternalGroup = true
    }
}
// tx.Commit() ...
if resetExternalGroup {
    g.ctx.SendChannelUpdateToGroup(groupNo)
}
```

`RemoveGroupMembers`（遍历批量踢人，任一被踢者是外部成员则触发）：

```go
removedExternal := false
for _, m := range removableMembers {
    // ...删除成员...
    if m.IsExternal == 1 {
        removedExternal = true
    }
}

resetExternalGroup := false
if removedExternal && groupModel.IsExternalGroup == 1 {
    externalCount, countErr := s.db.QueryExternalMemberCountTx(req.GroupNo, tx)
    if countErr != nil {
        s.Error("query external member count failed", zap.Error(countErr))
    } else if externalCount == 0 {
        if err := s.db.UpdateIsExternalGroupTx(req.GroupNo, 0, tx); err != nil {
            return nil, errors.New("failed to update is_external_group")
        }
        resetExternalGroup = true
    }
}
// tx.Commit() ...
if resetExternalGroup {
    s.ctx.SendChannelUpdateToGroup(req.GroupNo)
}
```

### 3.3 modules/group/service.go — API 响应

#### GroupResp 新增字段

```go
type GroupResp struct {
    // ... 现有字段 ...
    IsExternalGroup int `json:"is_external_group"` // 是否外部群
}
```

#### from() / fromModel() 方法：携带 IsExternalGroup

```go
func (g *GroupResp) from(model *DetailModel) *GroupResp {
    resp := &GroupResp{
        // ... 现有赋值 ...
        IsExternalGroup: model.IsExternalGroup,
    }
    return resp
}

func (g *GroupResp) fromModel(model *Model) *GroupResp {
    resp := &GroupResp{
        // ... 现有赋值 ...
        IsExternalGroup: model.IsExternalGroup,
    }
    return resp
}
```

`InfoResp` 同样补 `IsExternalGroup`（`toInfoResp`）。

#### SetEffectiveSpaceID：单群与批量两种形态

外部成员看到的 `space_id` 应替换为其 source Space，Web 的 `shouldSkipChannelForSpace`
方能自然匹配。详情接口走单群查询；列表接口必须先批量拿外部群映射再逐条覆盖，避免 N+1：

```go
// 单群：详情/channelInfo 接口使用
func (g *GroupResp) SetEffectiveSpaceID(loginUID string, db *DB) {
    if g == nil || g.IsExternalGroup == 0 || loginUID == "" {
        return
    }
    sourceSpaceID, err := db.QuerySourceSpaceIDForMember(g.GroupNo, loginUID)
    if err != nil || sourceSpaceID == "" {
        return
    }
    g.SpaceID = sourceSpaceID
}

// 批量：列表接口使用，映射由调用方一次性查好
func (g *GroupResp) SetEffectiveSpaceIDFromMap(externalMap map[string]string) {
    if g == nil || g.IsExternalGroup == 0 || len(externalMap) == 0 {
        return
    }
    if sourceSpaceID, ok := externalMap[g.GroupNo]; ok && sourceSpaceID != "" {
        g.SpaceID = sourceSpaceID
    }
}
```

调用约定：

- `GetGroupDetail(groupNo, uid)` 返回前调用 `SetEffectiveSpaceID(uid, s.db)`；
- `GetGroupDetails(groupNos, uid)` 先 `QueryExternalGroupNosForUser(uid)` 拿 `externalMap`，
  再对每个 `GroupResp` 调用 `SetEffectiveSpaceIDFromMap(externalMap)`。

#### memberDetailResp 新增字段

```go
type memberDetailResp struct {
    // ... 现有字段 ...
    IsExternal      int    `json:"is_external"`       // 是否外部成员
    SourceSpaceID   string `json:"source_space_id"`   // 来源 Space ID
    SourceSpaceName string `json:"source_space_name"` // 来源 Space 名称（外部成员时填充）
}
```

`memberDetailResp.from(model)` 同步赋值 `IsExternal` / `SourceSpaceID`。
`SourceSpaceName` 由 `fillSourceSpaceNames` 在列表层统一补齐。

#### 填充 source_space_name

列表接口（`membersGet` / `syncMembers`）在序列化完成后调用 `fillSourceSpaceNames`，
批量查询外部成员对应的 Space 名称，避免 N+1。`resps` 以值切片传入并直接写回：

```go
func (g *Group) fillSourceSpaceNames(resps []memberDetailResp) {
    if len(resps) == 0 {
        return
    }
    // 1. 收集所有不重复的 source_space_id
    idSet := make(map[string]struct{})
    for _, m := range resps {
        if m.IsExternal == 1 && m.SourceSpaceID != "" {
            idSet[m.SourceSpaceID] = struct{}{}
        }
    }
    if len(idSet) == 0 {
        return
    }

    // 2. 批量查询 Space 名称
    ids := make([]string, 0, len(idSet))
    for id := range idSet {
        ids = append(ids, id)
    }
    var rows []struct {
        SpaceID string `db:"space_id"`
        Name    string `db:"name"`
    }
    _, err := g.ctx.DB().Select("space_id", "name").From("space").
        Where("space_id IN ?", ids).Load(&rows)
    if err != nil {
        g.Warn("查询来源 Space 名称失败", zap.Error(err))
        return
    }
    nameMap := make(map[string]string, len(rows))
    for _, r := range rows {
        nameMap[r.SpaceID] = r.Name
    }

    // 3. 填充
    for i := range resps {
        if resps[i].IsExternal == 1 {
            resps[i].SourceSpaceName = nameMap[resps[i].SourceSpaceID]
        }
    }
}
```

### 3.4 modules/message/space_filter.go — 会话过滤

`FilterConversationsBySpace` 新增 `externalGroupMap` 查询并作为入参传给纯函数版本：

```go
externalGroupMap, err := group.NewDB(ctx).QueryExternalGroupNosForUser(loginUID)
if err != nil {
    log.Warn("查询外部群失败，跳过外部群过滤", zap.Error(err))
    externalGroupMap = make(map[string]string)
}

return filterConversationsCore(
    conversations, filterSpaceID, defaultSpaceID,
    groupSpaceMap, externalGroupMap,
    botSet, botInSpace,
    skipGroupFilter, skipBotFilter,
)
```

`filterConversationsCore` 的 `spaceID != filterSpaceID` 分支按 channelType 展开：
群聊先查 `externalGroupMap`，source Space 匹配则放行（空值 fallback 到默认 Space），
此分支同时吸收了旧群（`spaceID == ""`）在所有 Space 可见的历史兼容逻辑。

```go
if spaceID == filterSpaceID {
    filtered = append(filtered, conv)
} else if conv.ChannelType == common.ChannelTypeGroup.Uint8() {
    if sourceSpace, ok := externalGroupMap[conv.ChannelID]; ok {
        effectiveSource := sourceSpace
        if effectiveSource == "" {
            effectiveSource = defaultSpaceID
        }
        if effectiveSource == filterSpaceID {
            filtered = append(filtered, conv)
            continue
        }
    }
    if spaceID == "" {
        // 旧群（无 space_id）在所有 Space 可见
        filtered = append(filtered, conv)
    }
} else if spaceID == "" && filterSpaceID == defaultSpaceID {
    // 裸 UID 旧会话 + Bot 过滤（保持原逻辑）
    // ...
}
```

### 3.5 modules/search/api.go — 搜索过滤

`shouldIncludeGroupForSpace` 扩展签名，引入外部群映射；`global` 接口在入口处批量查询映射：

```go
// 搜索入口：一次性查询用户的外部群映射
externalGroupMap, extErr := group.NewDB(s.ctx).QueryExternalGroupNosForUser(loginUID)
if extErr != nil {
    s.Warn("查询外部群失败，外部群将在搜索中不可见", zap.Error(extErr))
    externalGroupMap = map[string]string{}
}

// 过滤判断：群 Space 精确匹配 或 外部群 source Space 匹配
func shouldIncludeGroupForSpace(groupSpaceID, searchSpaceID string,
    groupNo string, externalGroupMap map[string]string) bool {
    if searchSpaceID == "" {
        return false
    }
    if groupSpaceID == searchSpaceID {
        return true
    }
    if sourceSpace, ok := externalGroupMap[groupNo]; ok && sourceSpace == searchSpaceID {
        return true
    }
    return false
}
```

---

## 4. 前端改动

### 4.1 Web — 群信息「外部群」标签

群设置页头部，根据 `channelInfo.orgData.is_external_group === 1` 显示标签：

```tsx
{channelInfo?.orgData?.is_external_group === 1 && (
    <Tag color="orange" size="small">外部群</Tag>
)}
```

### 4.2 Web — 成员列表「外部」角标 + 来源 Space

成员列表中，显示外部角标和来源 Space 名称：

```tsx
{subscriber.orgData?.is_external === 1 && (
    <span>
        <Tag color="purple" size="small">外部</Tag>
        {subscriber.orgData?.source_space_name && (
            <span className="text-gray-400 text-xs ml-1">
                来自 {subscriber.orgData.source_space_name}
            </span>
        )}
    </span>
)}
```

### 4.3 Web — 会话过滤兼容

**无需改动**。后端 `SetEffectiveSpaceID` 对外部成员替换了 `space_id` 返回值，Web 的 `shouldSkipChannelForSpace` + `channelSpaceMap` 自然匹配。

### 4.4 Android / iOS — UI 标识

会话列表**功能层面无需改动**（sync API 白名单机制自动放行），但需补充 UI 标识：

#### Android

1. **群设置页** — 读取 `channelInfo` 中 `is_external_group` 字段，为 `1` 时在群名旁显示「外部群」标签
2. **成员列表** — 读取成员 `is_external` 字段，为 `1` 时显示「外部」角标 + `source_space_name`（如「来自 XX」）
3. **系统消息** — 外部成员加入时展示「以外部成员身份加入群聊」

#### iOS

同 Android，读取相同 API 字段展示：
1. 群设置页「外部群」标签
2. 成员列表「外部」角标 + 来源 Space 名称
3. 系统消息差异化文案

#### 改动量评估

纯 UI 展示，无逻辑改动。数据来自已有 API 响应字段（`is_external_group`、`is_external`、`source_space_name`），无需新增接口。预计 Android/iOS 各 1-2 天。

---

## 5. 完整场景清单

### 5.1 加入群

| 场景 | 行为 |
|------|------|
| Space A 用户扫码进 Space A 群 | 内部成员，`is_external=0` |
| Space B 用户扫码进 Space A 群 | `is_external=1, source_space_id=B`，群 `is_external_group→1` |
| 扫码但群开了 `invite=1` | 拒绝（已有逻辑） |
| 同时在 A+B 的用户扫码进 A 群 | `CheckMembership(A)=true` → 内部成员 |
| 内部成员邀请 Space B 用户 | `is_external=1, source_space_id=B(默认Space)` |
| 外部成员邀请 Space B 同事 | `is_external=1, source_space_id=B(同源)` |
| 外部成员邀请 Space B 的 Bot | Bot 在邀请人 Space → 允许 |
| 外部成员邀请 Space C 用户 | 无法操作（通讯录隔离） |
| `invite=1` 管理员审批外部用户 | 审批通过后标记 `is_external=1` |

### 5.2 会话可见性

| 场景 | 行为 |
|------|------|
| 外部成员在 source Space 查看 | sync 放行 → 正常显示 |
| 外部成员切到其他 Space | 不显示 |
| 推送通知 | WuKongIM 直推，不经 Space 过滤 |
| Android/iOS 会话列表 | 白名单机制，信任 sync 结果 |
| Web `shouldSkipChannelForSpace` | 后端替换 space_id → 自然匹配 |
| 搜索群名 | `shouldIncludeGroupForSpace` 放行 |
| 外部成员离开 source Space | fallback 到默认 Space |

### 5.3 群内交互

| 场景 | 行为 |
|------|------|
| 发消息 / @全员 | 正常（WuKongIM 透明） |
| 查看成员列表 | 外部成员显示「外部」角标 + 来源 Space 名称 |
| 加好友 | 受 `forbidden_add_friend` 控制 |
| 修改群设置 | 非管理员/群主被拦截 |
| Thread / GROUP.md | 正常工作 |

### 5.4 退出与恢复

| 场景 | 行为 |
|------|------|
| 外部成员退群 | 事务内检查外部成员数，为 0 则恢复 `is_external_group=0` |
| 管理员踢外部成员 | 同上 |
| 群解散 | group_member 全清理，无需额外处理 |

---

## 6. 安全分析

### 6.1 不涉及的层

| 层 | 影响 |
|----|------|
| WuKongIM | 零改动。只管 channel 投递，不关心 Space |
| space_member 表 | 零改动。不写入 guest 记录，不污染 Space 成员体系 |
| Space 通讯录 | 零改动。外部成员不出现在 Space 成员列表或搜索中 |

### 6.2 信息暴露评估

| 外部成员可获取的信息 | 风险 |
|---------------------|------|
| 群内成员的 name / avatar / role | ✅ 可接受（加群即可见） |
| 群内成员的来源 Space 名称 | ⚠️ 仅在成员列表中显示（外部成员可见彼此来源） |
| Space 组织结构 / 通讯录 | ❌ 不暴露（外部成员不在 Space 内） |
| 群的 GROUP.md 内容 | ⚠️ 可见（需管理员控制 GROUP.md 内容） |

### 6.3 已有安全阀

| 控制 | 作用 |
|------|------|
| `invite=1` | 群开启邀请确认 → 管理员审批外部人 |
| `forbidden_add_friend=1` | 禁止群内互加好友 |
| 通讯录隔离 | 外部成员只能邀请自己 Space 的人 |

---

## 7. 改动文件清单

### 后端（11 个文件 + 1 个 migration）

| 文件 | 改动内容 |
|------|---------|
| `modules/group/sql/group_20260424-01.sql` | 3 个字段 migration + `idx_group_member_external` 复合索引 |
| `modules/group/1module.go` | `newChannelRespWithGroupResp` 将 `is_external_group` 写入 `channelInfo.Extra`，供前端 UI |
| `modules/group/db.go` | `Model`/`MemberModel`/`MemberDetailModel` 加字段；`recoverMemberTx` 覆盖外部字段；`SELECT` 列表补 `is_external,source_space_id`；新增 `QueryExternalMemberCountTx`/`QueryExternalGroupNosForUser`/`UpdateIsExternalGroup`/`UpdateIsExternalGroupTx`/`QuerySourceSpaceIDForMember` |
| `modules/group/api.go` | `groupScanJoin` 外部标记 + 事务内 `UpdateIsExternalGroupTx`；`memberAdd` Bot 校验用邀请人有效 Space；`groupExit` 事务内恢复外部群；`memberDetailResp` 加 `is_external`/`source_space_id`/`source_space_name`；`fillSourceSpaceNames` 批量补 Space 名称；`membersGet`/`syncMembers` 调用之 |
| `modules/group/external.go` | 新文件：`MsgGroupMemberScanJoinExt` 扩展事件载荷，携带 `is_external` |
| `modules/group/service.go` | `GroupResp`/`InfoResp` 加 `IsExternalGroup`；`from`/`fromModel`/`toInfoResp` 赋值；`SetEffectiveSpaceID` + `SetEffectiveSpaceIDFromMap`；`GetGroupDetail(s)` 调用替换 SpaceID；`AddGroupMembers` 外部检测 + 事务内标记外部群；`RemoveGroupMembers` 事务内恢复外部群 |
| `modules/message/event.go` | `handleGroupMemberScanJoinEvent` 解析扩展载荷，`is_external=1` 时系统消息文案改为「以外部成员身份加入群聊」，`payload` 透传 `is_external` |
| `modules/message/space_filter.go` | `FilterConversationsBySpace` 查询 `externalGroupMap` 并传入 `filterConversationsCore`；群会话分支放行外部群（source Space 匹配 / fallback 默认 Space / 旧群无 space_id 放行） |
| `modules/message/space_filter_test.go` | 新增 `TestFilterConversationsBySpace_ExternalGroupVisibleInSourceSpace` 等用例，补齐 `filterConversationsCore` 新增入参 |
| `modules/search/api.go` | `global` 先查询 `externalGroupMap`；`shouldIncludeGroupForSpace` 增加 `groupNo` + `externalGroupMap` 参数，外部群在 source Space 可搜到 |
| `modules/search/api_test.go` | `shouldIncludeGroupForSpace` 新签名的用例补齐 |

### 前端（Web 2 处 UI）

| 位置 | 改动内容 |
|------|---------|
| 群设置页 | 显示「外部群」标签 |
| 成员列表 | 外部成员显示「外部」角标 + 来源 Space 名称 |

### 客户端（Android / iOS 各 2 处 UI + 1 处文案）

| 位置 | 改动内容 |
|------|---------|
| 群设置页 | 显示「外部群」标签 |
| 成员列表 | 外部成员显示「外部」角标 + 来源 Space 名称 |
| 系统消息 | 外部成员加入时差异化文案 |

---

## 8. 补充说明

### 8.1 旧群兼容

`group.space_id` 为空的旧群（Space 功能上线前创建），所有外部检测逻辑被 `if group.SpaceID != ""` 跳过。旧群中所有成员均为内部成员，不触发外部群逻辑。

### 8.2 群系统消息

外部成员加入时，系统消息展示差异化文案：
- 内部成员加入：「"X" 通过 "Y" 的二维码加入群聊」（现有）
- 外部成员加入：「"X" 以外部成员身份加入群聊」（新增）

实现：`modules/group/external.go` 新增 `MsgGroupMemberScanJoinExt` 结构体
（嵌入 `config.MsgGroupMemberScanJoin` 并追加 `IsExternal int` 字段），
`groupScanJoin` 事件数据使用该扩展结构体。

`modules/message/event.go: handleGroupMemberScanJoinEvent`
改用匿名扩展 struct 解析事件（兼容旧事件数据），`IsExternal==1` 时切换 `content` 文案；
同时在 payload 中透传 `is_external` 字段供客户端 UI 使用。

### 8.3 Channel Update CMD

`is_external_group` 变更（0→1 或 1→0）后，调用 `SendChannelUpdateToGroup(groupNo)` 通知所有群成员刷新 channelInfo 缓存。

### 8.4 service.go 双序列化方法

`from(DetailModel)` 和 `fromModel(Model)` 均需补充 `IsExternalGroup` 字段赋值。

---

## 9. 测试要点

| # | 测试用例 | 预期结果 |
|---|---------|---------|
| T1 | Space B 用户扫码进 Space A 群（invite=0） | 成功，标记 is_external=1，群变外部群 |
| T2 | Space B 用户扫码进 Space A 群（invite=1） | 拒绝 |
| T3 | 外部成员在 source Space 会话列表 | 可见 |
| T4 | 外部成员切到其他 Space | 不可见 |
| T5 | 外部成员邀请自己 Space 同事 | 成功，is_external=1 |
| T6 | 外部成员邀请自己 Space Bot | 成功 |
| T7 | 外部成员邀请第三方 Space 用户 | 联系人列表不显示（天然隔离） |
| T8 | 所有外部成员退出 | is_external_group 恢复 0 |
| T9 | Web 端群详情 space_id | 外部成员看到 source_space_id |
| T17 | 成员列表显示来源 Space 名称 | 外部成员显示「外部 · 来自 XX Space」 |
| T18 | Android 群设置页外部群标签 | 显示「外部群」标签 |
| T19 | iOS 成员列表外部角标 | 显示「外部」+ 来源 Space |
| T10 | 搜索群名 | 外部群在 source Space 可搜到 |
| T11 | 推送通知 | 外部成员正常收到 |
| T12 | 并发退群 | is_external_group 一致（事务+行锁） |
| T13 | 旧群（无 space_id）扫码 | 正常加入，不标记 is_external |
| T14 | 外部成员加入系统消息 | 展示「以外部成员身份加入」 |
| T15 | is_external_group 变更后 | 所有成员收到 channelInfo 更新 |
| T16 | 外部成员 pin 外部群 | pin 存在 source Space 下，切 Space 不可见 |
