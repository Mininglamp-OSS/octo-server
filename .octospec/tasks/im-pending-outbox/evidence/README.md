# IM 订阅泄漏的影响面 —— 实测证据

日期: 2026-08-23
broker: wukongim v2.2.4-20260313 (ci.yml 钉的 tag)
探测器源码: scratchpad/wkleak/main.go

## 实测结果

| 阶段 | 被移除者能否发言 | 能否收到群消息 |
|---|---|---|
| 1. 正常群成员（基线） | ALLOWED | YES |
| 2. **泄漏态（退订失败）** | **ALLOWED** | **YES** |
| 3. 正确态（退订成功） | BLOCKED (SubscriberNotExist) | NO |

**泄漏态与正常群成员完全无差别。** 退订失败 = 群成员身份原封不动保留，收发全通。

## 源码佐证（同一 tag）

`internal/service/permission.go:147-154`：

```go
// 判断是否是订阅者
isSubscriber, err := Store.ExistSubscriber(realFakeChannelId, channelType, fromUid)
...
if !isSubscriber {
    return wkproto.ReasonSubscriberNotExist, nil
}
```

发言权直接由 broker 的订阅者表决定，与 octo-server 的 group_member 无关。

## 为什么没有自愈路径

1. 工单重跑无效 —— 重试范围由活跃 group_member 行推导，人已删除，范围为空
2. broker 不会重载 —— IMDatasource 回调是死代码（#797 已证）
3. 用户自己修不了 —— 客户端群列表里没有这个群（group_member 已删）
4. 管理员修不了 —— 群成员列表里没有这个人

四方都看不见、都改不了 ⇒ 永久。
