package botfather

import "fmt"

func generateSkillMD(apiURL, _ string) string {
	return fmt.Sprintf(`---
name: octo
version: deprecated
description: Deprecated Octo Bot skill pointer. Use create-openclaw-octo install instead.
metadata: {"octo":{"category":"messaging","api_base":"%s","deprecated":true}}
---

# Octo Bot Skill (Deprecated)

This server-generated skill document is deprecated and no longer receives Bot API updates.

Use the canonical OpenClaw Octo plugin and skill instead:

    npx -y create-openclaw-octo install

Canonical skill source:
https://github.com/Mininglamp-OSS/openclaw-channel-octo/blob/main/skills/octo-bot-api/SKILL.md

Raw skill file:
https://raw.githubusercontent.com/Mininglamp-OSS/openclaw-channel-octo/main/skills/octo-bot-api/SKILL.md

Bot API guidance, including file upload recommendations, now lives in that repository.
`, apiURL)
}
func generateCLIGuideMD() string {
	return `---
name: octo-setup-install
description: Octo 插件安装和更新指南
---

# Octo 插件安装/更新

## 安装或更新插件

` + "```" + `
npx -y create-openclaw-octo install
` + "```" + `

- 未安装 → 自动安装最新版
- 已安装且有新版本 → 自动更新
- 已安装且是最新 → 提示已是最新

## 配置 bot

插件安装完成后，使用以下命令配置 bot：

**单个 bot（/newbot 创建后）：**
` + "```" + `
npx -y create-openclaw-octo bind --bot-token <token> --api-url <url> --account-id <id> --agent <agent标识>
` + "```" + `

**批量创建（/quickstart）：**
` + "```" + `
npx -y create-openclaw-octo quickstart --api-key <uk_key> --api-url <url>
` + "```" + `

## 其他命令

| 命令 | 用途 |
|------|------|
| ` + "`npx -y create-openclaw-octo install`" + ` | 安装或更新插件 |
| ` + "`npx -y create-openclaw-octo bind`" + ` | 配置单个 bot 并绑定 agent |
| ` + "`npx -y create-openclaw-octo quickstart`" + ` | 批量创建 bot 并绑定所有 agent |
| ` + "`npx -y create-openclaw-octo doctor`" + ` | 诊断健康状态 |
| ` + "`npx -y create-openclaw-octo doctor --fix`" + ` | 自动修复问题 |
| ` + "`npx -y create-openclaw-octo info`" + ` | 显示版本信息 |
| ` + "`npx -y create-openclaw-octo uninstall`" + ` | 卸载插件 |
`
}

func generateSetupNewbotMD(_ string) string {
	return `---
name: octo-setup-newbot
description: 收到 BotFather /newbot 创建的 bot 信息后，执行 bind 命令完成配置和绑定。
---

# Octo Newbot 绑定流程

收到 BotFather 的 bot 信息后（Bot Token、API Server、Account ID），执行以下命令：

` + "```" + `
npx -y create-openclaw-octo bind --bot-token <Bot Token> --api-url <API Server> --account-id <Account ID> --agent <agent标识>
` + "```" + `

默认绑定到当前 Session 的 Agent，agent 标识通过 /status 查看。
如果用户指定了其他 Agent，使用用户指定的标识替换。

## 前提条件

Octo 插件必须已安装。未安装请先执行：
` + "```" + `
npx -y create-openclaw-octo install
` + "```" + `

## bind 命令会做什么

1. 检查插件是否已安装（未装则报错）
2. 写入 bot 配置到 openclaw.json
3. 设置会话隔离（session.dmScope）
4. 添加 agent → bot 绑定
5. 等待 Octo channel 热重载
6. 给 bot 创建者发上线通知

不需要手动重启 gateway。

完成后请在 Octo 中给 bot 发一条消息验证联通。

## Bot API 文档

权威 Bot API skill 文档已迁移到 openclaw-channel-octo 仓库：
https://github.com/Mininglamp-OSS/openclaw-channel-octo/blob/main/skills/octo-bot-api/SKILL.md
`
}

func generateSetupQuickstartMD(_ string) string {
	return `---
name: octo-setup-quickstart
description: 收到 BotFather /quickstart 的 API Key 后，执行 quickstart 命令为所有 agent 批量创建并绑定 bot。
---

# Octo Quickstart 流程

收到 BotFather 的 User API Key 和 API Server 后，执行以下命令：

` + "```" + `
npx -y create-openclaw-octo quickstart --api-key <User API Key> --api-url <API Server>
` + "```" + `

## 前提条件

Octo 插件必须已安装。未安装请先执行：
` + "```" + `
npx -y create-openclaw-octo install
` + "```" + `

## quickstart 命令会做什么

1. 检查插件是否已安装（未装则报错）
2. 获取 OpenClaw 的所有 agent 列表
3. 为每个 agent 创建一个 Octo bot
4. 一次性写入所有 bot 配置和绑定
5. 设置会话隔离（session.dmScope）
6. 等待 Octo channel 热重载
7. 给 bot 创建者发上线通知
8. 输出结果清单

不需要手动重启 gateway。
quickstart 是一次性初始化工具，面向首次接入 Octo 的用户。

完成后请在 Octo 中给 bot 发一条消息验证联通。

## Bot API 文档

权威 Bot API skill 文档已迁移到 openclaw-channel-octo 仓库：
https://github.com/Mininglamp-OSS/openclaw-channel-octo/blob/main/skills/octo-bot-api/SKILL.md
`
}
