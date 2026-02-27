# DMWork Bot Skill

Connect to DMWork messaging platform as a bot. Receive messages from users and reply via REST API.

## Setup

1. You have a bot token and server URL provided by the user.
2. Register with the server to get your credentials.
3. Poll for messages and reply to users.

## Step 1: Register

```bash
curl -s -X POST {SERVER_URL}/v1/bot/register \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"name": "Claude Code Agent"}'
```

Save the response fields:
- `robot_id` — your bot's ID
- `im_token` — for WebSocket (optional)
- `owner_uid` — the user who created you
- `owner_channel_id` — DM channel to your owner
- `ws_url` — WebSocket URL (for real-time, optional)
- `api_url` — REST API base URL

## Step 2: Greet your owner

```bash
curl -s -X POST {SERVER_URL}/v1/bot/sendMessage \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "channel_id": "{OWNER_UID}",
    "channel_type": 1,
    "payload": {"type": 1, "content": "Hello! I am your AI assistant, ready to help."}
  }'
```

## Step 3: Poll for messages

```bash
curl -s -X POST {SERVER_URL}/v1/bot/events \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"event_id": 0, "limit": 20}'
```

Response format:
```json
{
  "status": 1,
  "results": [{
    "event_id": 12345,
    "message": {
      "message_id": 100,
      "from_uid": "user123",
      "channel_id": "...",
      "channel_type": 1,
      "payload": {"type": 1, "content": "Hello bot"},
      "timestamp": 1700000000
    }
  }]
}
```

After processing each event, acknowledge it:
```bash
curl -s -X POST {SERVER_URL}/v1/bot/events/{EVENT_ID}/ack \
  -H "Authorization: Bearer {BOT_TOKEN}"
```

## Step 4: Reply to messages

```bash
curl -s -X POST {SERVER_URL}/v1/bot/sendMessage \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "channel_id": "{FROM_UID_OR_CHANNEL_ID}",
    "channel_type": 1,
    "payload": {"type": 1, "content": "Your reply here"}
  }'
```

## Optional: Typing indicator

Show "typing..." to the user while processing:
```bash
curl -s -X POST {SERVER_URL}/v1/bot/typing \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"channel_id": "{CHANNEL_ID}", "channel_type": 1}'
```

## Optional: Streaming output

For long AI responses, use streaming to show text progressively:

```bash
# 1. Start stream
STREAM_NO=$(curl -s -X POST {SERVER_URL}/v1/bot/stream/start \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"channel_id": "xxx", "channel_type": 1, "payload": "eyJ0eXBlIjoxLCJjb250ZW50IjoiIn0="}' \
  | jq -r '.stream_no')

# 2. Send chunks (accumulated content each time)
curl -s -X POST {SERVER_URL}/v1/bot/sendMessage \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"channel_id\": \"xxx\", \"channel_type\": 1, \"stream_no\": \"$STREAM_NO\", \"payload\": {\"type\": 1, \"content\": \"Partial response...\"}}"

# 3. End stream
curl -s -X POST {SERVER_URL}/v1/bot/stream/end \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"stream_no\": \"$STREAM_NO\", \"channel_id\": \"xxx\", \"channel_type\": 1}"
```

## Optional: Read Receipt

Mark messages as read (shows double-checkmark to the sender). Pass `message_ids` as **string array** to avoid large-number precision loss:

```bash
curl -s -X POST {SERVER_URL}/v1/bot/readReceipt \
  -H "Authorization: Bearer {BOT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"channel_id": "{CHANNEL_ID}", "channel_type": 1, "message_ids": ["{MESSAGE_ID}"]}'
```

## Optional: Heartbeat (keep online status)

Send every 30 seconds to stay online:
```bash
curl -s -X POST {SERVER_URL}/v1/bot/heartbeat \
  -H "Authorization: Bearer {BOT_TOKEN}"
```

## Channel Types
- 1 = Direct Message (DM)
- 2 = Group Chat

## Message Types (payload.type)
- 1 = Text (payload.content = text string)
- 2 = Image (payload.url = image URL)
- 3 = GIF (payload.url = gif URL)
- 4 = Voice (payload.url = audio URL)
- 5 = Video (payload.url = video URL)
- 6 = Location (payload.latitude, payload.longitude)
- 7 = Card (payload.uid or payload.name)
- 8 = File (payload.url = file URL)

## Security
- NEVER share your bot_token publicly
- Only use the token in the Authorization header
- All API calls should be made server-side

## Typical Bot Loop

```
1. Register → save credentials
2. Send greeting to owner
3. Loop:
   a. Send heartbeat (every 30s)
   b. Poll events
   c. For each new message:
      - Send typing
      - Process message (call AI, etc.)
      - Reply with result
      - Ack event
```
