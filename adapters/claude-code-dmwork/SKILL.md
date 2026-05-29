# DMWork Bot Skill

Connect to DMWork messaging platform as an AI bot via the OpenClaw DMWork adapter plugin.

## Setup

### 1. Install the DMWork plugin

```bash
openclaw plugins install octo
```

### 2. Get a Bot Token

In DMWork, find **BotFather** in your contacts and send:
1. `/newbot` — create a new bot
2. Set bot name and username
3. Copy the bot token (starts with `bf_`)

### 3. Configure OpenClaw

Add to your `openclaw.json`:

```json
{
  "channels": {
    "dmwork": {
      "enabled": true,
      "botToken": "bf_your_token_here",
      "apiUrl": "http://your-dmwork-server:8090",
      "wsUrl": "ws://your-dmwork-server:5200"
    }
  }
}
```

### 4. Restart OpenClaw

```bash
openclaw gateway restart
```

Your bot is now online and will:
- **Private chat**: respond to all messages
- **Group chat**: respond only when @mentioned (configurable)

## Configuration Options

| Option | Default | Description |
|--------|---------|-------------|
| `botToken` | required | Bot token from BotFather |
| `apiUrl` | required | DMWork API URL |
| `wsUrl` | required | WuKongIM WebSocket URL |
| `requireMention` | `true` | Group chat: only respond when @mentioned |

## Features

- Real-time WebSocket connection (no polling needed)
- Mention gating for group chats
- History context: unmentioned messages are prepended when bot is @mentioned
- Multi-bot support: each bot token = one OpenClaw instance

## Channel Types
- 1 = Direct Message (DM)
- 2 = Group Chat

## Message Types (payload.type)
- 1 = Text
- 2 = Image
- 3 = GIF
- 4 = Voice
- 5 = Video
- 6 = Location
- 7 = Card
- 8 = File
