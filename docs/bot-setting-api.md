# Bot 配置项接口对接说明

面向两类接入方，**鉴权方式和用途都不同，别混用**：

| 接入方 | 端点 | 鉴权头 |
|---|---|---|
| Bot 管理 UI（bot 属主） | `/v1/robot/:robot_id/settings` | `token`（用户会话） |
| Bot / 适配器（生产者） | `/v1/bot/card/profile` | `Authorization`（bot token） |

---

## 一、Bot 管理 UI：配置读写

三个端点都**只有 bot 的创建者能访问**，且都挂了按登录用户的限流。

### 1. 读取配置目录 `GET /v1/robot/:robot_id/settings`

```json
{
  "list": [
    {
      "key": "bot.card_enabled",
      "type": "bool",
      "value": null,
      "effective_value": true,
      "source": "env",
      "editable": false
    },
    {
      "key": "bot.display_enabled",
      "type": "bool",
      "value": false,
      "effective_value": false,
      "source": "bot",
      "editable": true
    },
    {
      "key": "bot.interaction_enabled",
      "type": "bool",
      "value": null,
      "effective_value": true,
      "source": "default",
      "editable": true
    },
    {
      "key": "bot.reasoning_enabled",
      "type": "bool",
      "value": null,
      "effective_value": false,
      "source": "global",
      "editable": true
    }
  ]
}
```

**这是一份「可配置项目录」，不是「已设置项列表」。** 所有已注册的键都会返回，没设过的 `value` 为 `null`。服务端加新键时客户端不发版就能看到它——**遇到不认识的 `key` 请跳过，不要阻塞整个页面渲染**。展示文案由客户端按 key 自持，服务端只给结构。

#### 三个值字段的区别（渲染「恢复默认」的前提）

| 字段 | 含义 |
|---|---|
| `value` | 这个 Bot 的**显式覆盖**。`null` = 没设过 |
| `effective_value` | **实际生效的能力**，可以直接渲染 |
| `source` | `value` 这一层的**来源**，不是「为什么 effective_value 是这个」 |

`source` 取值：

- `bot` —— 该 Bot 的显式覆盖
- `global` —— 服务端全局默认（`system_setting`）
- `default` —— 代码默认
- `env` —— 派生自部署环境变量，**不可写**

**别把 `value` 和 `effective_value` 合并成一个值。** 合并之后就分不出「我显式设成了 false」和「我没设、上层默认就是 false」——两者 `effective_value` 相同，但「恢复默认」按钮该不该亮、点了之后变成什么，完全不同。

#### `effective_value` 已由服务端支配，客户端不要再自己 AND

`effective_value` 就是**发卡时真正生效**的那个值：服务端已经把部署级总闸和「展示是交互的下限」这两条规则算进去了，与 `/v1/bot/card/profile` 的 `config` 同源同值。直接渲染即可。

具体来说，读到的 `effective_value` 一定满足：

- 总闸（`bot.card_enabled`）为 false 时，其余三项一律为 false；
- `bot.interaction_enabled` 已经是 `display AND interaction` 的结果——`octo/v2` 是 `octo/v1` 的严格超集，展示能力是交互能力的下限。

**注意 `source` 不随之改变。** 它标注的是 `value` 来自哪一层，不解释 `effective_value` 为何是这个值。所以完全可能出现 `source:"bot"` 且 `value:true`，而 `effective_value:false`——含义是「你设了开，但总闸把它压住了」。UI 想解释这个状态，依据是同一份响应里的 `bot.card_enabled` 那一行（`source:"env"`、`editable:false`），用它把其余三项置灰。

**一处已知的例外，仅涉及 `bot.reasoning_enabled`。** profile 的 `config.reasoning_enabled` 还会额外 AND 一次「本部署的模板目录是否广告了推理模板」，而 owner 目录看不到这件事——它是部署组装事实，不是配置策略，把它镜像过来会让配置模块反向依赖卡片模块。因此在**没有广告推理模板**的部署里，owner 目录可能报 `true` 而 profile 报 `false`。标准部署都广告它，两边一致；如果你的部署裁剪过模板目录，请以 profile 为准。

### 2. 批量写入 `PUT /v1/robot/:robot_id/settings`

```json
{
  "items": [
    { "key": "bot.display_enabled",     "value": false },
    { "key": "bot.interaction_enabled", "value": true  },
    { "key": "bot.reasoning_enabled",   "value": null  }
  ]
}
```

- `value` 接受 **JSON bool**（`false`）、**字符串字面量**（`"false"` / `"0"` / `"FALSE"` / `" true "`，大小写与首尾空白容错），以及 **`null`**。
- **`null` 是 no-op：跳过这一项，不写也不删。** 读接口对未配置项下发的正是 `null`。
- **`null` 不是「删除覆盖」。** 删除用 `DELETE`。如果 `null` 表示删除，那么整份回灌就会**静默清空用户没碰过的每一个覆盖**——一个无害动作产生破坏性后果。
- **全批原子**：任一 item 非法则整批拒绝，不存在「三项里生效两项」的中间态。
- 同一次请求里 `key` 重复会被拒。
- `items` 为空、缺失（`{}`）或整份 body 无法解析都会被拒。
- 整批都是 `null` 时直接返回成功（没有任何要写的东西），也不推变更事件。

#### 「读目录 → 改一项 → 整份写回」是受支持的流程

把 `GET` 返回的 **完整 `list` 原样** PUT 回去即可，**不需要任何客户端侧过滤**：

- 未配置项的 `null` 会被跳过，不会误写；
- **只读键 `bot.card_enabled` 携 `null` 也会被跳过**，不必先把它剔掉。

唯一仍会被拒的是给 `bot.card_enabled` 送一个**真实值**——那会让库里存下与部署环境相矛盾的值。换句话说：原样回灌一定成功，只有你**主动去改**只读键才会失败。

（未注册的 key 即便携 `null` 仍然会被拒。服务端对那个键没有任何定义，无法确认自己理解了你的意图。）

### 3. 删除覆盖 `DELETE /v1/robot/:robot_id/settings/:key`

**删除 = 回落到上一层，不是「设为 false」。** 这是「恢复默认」按钮该调的接口。

幂等：删一个本来就不存在的覆盖返回成功。

### 写入限制

- `bot.card_enabled` **不可写**，写它会 400。它派生自部署环境变量，做成只读是为了防止库里写着「开」而环境是「关」，导致清单和发卡门禁互相矛盾。
- 未注册的 key 会 400。

---

## 二、错误处理（⚠️ 有一处反直觉）

**HTTP 状态码在这几个端点上不统一，请一律按 `error.code` 分支，不要按 HTTP 状态码分支。**

| 场景 | `error.code` | HTTP 状态 | `error.http_status` |
|---|---|---|---|
| bot 不存在 | `err.server.robot.not_found` | **404** | 404 |
| 不是该 bot 的属主 | `err.server.robot.creator_only` | **403** | 403 |
| 参数非法（未注册 key / 非法 value / 空 items / 重复 key / 写只读键） | `err.server.robot.request_invalid` | **400** | 400 |
| 读取失败（服务端） | `err.server.robot.query_failed` | **400** ⚠️ | 500 |
| 写入失败（服务端） | `err.server.robot.store_failed` | **400** ⚠️ | 500 |
| profile 读配置失败（服务端） | `err.shared.internal` | **400** ⚠️ | 500 |

标 ⚠️ 的三行是历史兼容约定（D14）：这类错误的**线路状态码被钉成 400**，真实状态在响应体的 `error.http_status` 里。所以：

- **看到 400 不代表是你参数错了** —— 可能是服务端 500。判断依据是 `error.code`。
- 服务端错误应当**重试**，不要当成「参数有问题」提示用户改输入。

`err.server.robot.request_invalid` 会在 `error.details.field` 里带上出错字段（`items` / `key` / `value`），可用于定位。

响应体形状：

```json
{
  "error": {
    "code": "err.server.robot.creator_only",
    "http_status": 403,
    "message": "…（已按请求语言本地化）",
    "details": {}
  }
}
```

属主校验先于 body 解析：对**他人的**或**不存在的** bot 发任何形状的请求（包括 `{}` 与不可解析的 body），拿到的都是 403 / 404，不会是 400。

---

## 三、缓存

两个读接口都返回：

| 端点 | 响应头 |
|---|---|
| `GET /v1/robot/:robot_id/settings` | `Cache-Control: private, no-store`、`Vary: token` |
| `GET /v1/bot/card/profile` | `Cache-Control: private, no-store`、`Vary: Authorization` |

响应是**单个 Bot 的私有配置**，而 URL 对所有调用方字节相同——区分调用者的只有鉴权头。两个端点的 `Vary` 值不同，因为它们用的鉴权头本来就不同（用户会话走 `token`，bot 走 `Authorization`）。请不要在客户端或任何中间层缓存这两个响应——改完配置立刻重读必须看到新值。

---

## 四、Bot / 适配器侧：`GET /v1/bot/card/profile`

该接口**新增一个 `config` 对象**，其余字段全部不变（纯增量）。

```json
{
  "config": {
    "card_enabled": true,
    "display_enabled": true,
    "interaction_enabled": false,
    "reasoning_enabled": true,
    "reasoning_template_ref": { "id": "ai.reasoning-process", "version": "v3" }
  }
}
```

### 语义

- 这里的值**已经和总闸 AND 过**，可以直接用。
- `display_enabled` —— 允许发展示型 raw 卡（`octo/v1`）。
- `interaction_enabled` —— 允许发交互型 raw 卡（`octo/v2`）。**注意它已经是 `display AND interaction` 的结果**：`octo/v2` 是 `octo/v1` 的严格超集，所以展示能力是交互能力的下限。不要拿这两个字段自行重新组合布尔，直接用。
- `reasoning_enabled` —— 允许发服务端模板推理卡。
- `reasoning_template_ref` —— 服务端解析出的模板 ref。**`reasoning_enabled` 为 false 时它一定是 `null`**；为 true 时它一定是本部署在同一响应的 `templating.templates` 里也广告了的那个版本，即发送门一定接受它。请直接用这个 ref，不要自己写死版本号。

这四个布尔与 owner 目录 `GET /v1/robot/:id/settings` 的 `effective_value` **同源同值**（`reasoning_enabled` 有一处例外，见第一节末尾）。

### ⚠️ 这个接口现在会失败了

改动前它是鉴权之后的一个常量，不可能出错；**现在它要读数据库，因此可能返回 `err.shared.internal`**。

**请重试，不要把错误当成「能力关闭」缓存下来。** 把一次数据库抖动记成「这个 bot 不能发卡」，会让 bot 在故障恢复后仍然长时间不发卡，直到下一次重新拉取。

同样注意上面第二节那条：这个错误的线路状态码也是 400，真实的 500 在 `error.http_status` 里——**按 `error.code == "err.shared.internal"` 判断重试**，不要按 HTTP 状态码。

### 配置变更通知

owner 改动配置后，服务端会给该 bot 推一个事件：

```json
{ "type": "bot_setting_updated", "data": { "scope": "bot_setting" } }
```

**事件不携带任何配置值，只是一个「去重新拉取」的信号。** 收到后重新请求 `/v1/bot/card/profile` 即可。不要试图从事件里读值——那样会让适配器拿一个可能过时的形状去覆盖权威结果。

事件是尽力投递的，不保证到达；它的作用是**让配置改动立刻生效而不必等缓存 TTL**，不是唯一的更新途径。

### 发送侧仍会独立校验

`profile` 只是能力清单。生产者可能持有过期副本、也可能压根不读，所以**每一条发送路径都会按有效配置独立校验一次**。被拒时统一返回一个泛化的 `card_invalid`（防枚举），具体原因只进服务端日志——请以 profile 为准提前判断，不要靠「发一条试试」来探测能力。

---

## 五、已知限制

**服务端全局层的传播窗口。** 通过 `system_setting` 调整的全局默认由各副本按快照刷新，最长约 60s 生效，期间不同副本可能给出不同结论；启动时若加载失败会落到代码默认（对这三个开关是 `true`）。要确保某项能力**立刻且确定地关闭**，请用部署级环境总闸，或给该 Bot 写一条显式覆盖——这两条都是即时且 fail-closed 的。

**App Bot 暂不支持。** App Bot 在独立的表里，没有 `robot` 记录，因此 `PUT /v1/robot/<appBotUID>/settings` **只会返回 404**，无法为 App Bot 写任何覆盖。这三个开关对 App Bot 取代码默认值（全部为 true），部署级的全局开关对它们仍然生效。

（不是回归：改动前也没有 per-Bot 开关。是否支持 App Bot 待定。）
