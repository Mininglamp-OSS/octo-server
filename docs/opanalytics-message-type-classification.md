# Opanalytics P1B Message Type Classification Plan

Status: proposal for #320.

## Goal

P1B should add a production-safe message type dimension for the operation
analytics dashboard without changing the existing P0/P1A message totals.

The first implementation scope excludes call analytics. Voice/video calls should
not be surfaced as a dashboard category in P1B. If call-like payloads are seen
while classifying messages, they should be treated only as non-chat/system-like
traffic for governance decisions, or as `unknown` until a separate call analytics
scope is defined.

## Current State

The message shard tables persist the fields needed for an offline classifier:
`id`, `signal`, `from_uid`, `channel_id`, `channel_type`, `timestamp`,
`payload`, and deletion metadata.

`octo_fact_member_channel_daily.content_type` exists, but it is a reserved
column and the ETL currently writes `0` for every row. More importantly, the
current primary key is `(channel_id, stat_date, sender_uid)`, so the table
cannot represent one sender producing multiple content types in the same
channel on the same day. Reusing this column directly would collapse mixed
message types into one row and lose information.

Therefore P1B should add dedicated content facts rather than mutating the
existing member-channel daily fact shape.

## Product Scope

In scope:

- Message type distribution.
- Conversation type x message type matrix.
- Human/agent x message type counts.
- System/notification classification for future "effective chat message" count
  governance.
- Unknown coverage reporting for encrypted, malformed, and unsupported payloads.

Out of scope:

- Call analytics, call duration, call success rate, and call trend charts.
- Co-active member interaction analysis.
- Pair/bitmap facts.
- Export endpoints.
- Online queries against message shard tables.

## Classification Categories

Use stable dashboard categories instead of exposing raw `common.ContentType`
values directly.

| Category | Raw content types | Notes |
| --- | --- | --- |
| `text` | `Text=1` | Plain text messages. |
| `image` | `Image=2`, `GIF=3`, `VectorSticker=12`, `EmojiSticker=13` | Sticker/GIF are image-like for dashboard use. |
| `voice` | `Voice=4` | Voice messages, not voice call analytics. |
| `video` | `Video=5` | Video file/message, not video call analytics. |
| `file` | `File=8` | File messages. |
| `rich_custom` | `Location=6`, `Card=7`, `MultipleForward=11`, `RichText=14`, `InviteJoinOrganization=16`, unknown app custom types that still parse cleanly | Product-facing "rich/custom" bucket. |
| `system_notification` | `CMD=99`, `FriendApply=1000`, `GroupCreate=1001`, `GroupMemberAdd=1002`, `GroupMemberRemove=1003`, `FriendSure=1004`, `GroupUpdate=1005`, `RevokeMessage=1006`, `GroupMemberScanJoin=1007`, `GroupTransferGrouper=1008`, `GroupMemberInvite=1009`, `GroupMemberBeRemove=1020`, `GroupMemberQuit=1021`, `GroupUpgrade=1022`, `HotlineAssignTo=1200`, `HotlineSolved=1201`, `HotlineReopen=1202`, `Tip=2000` | Countable as system/notification, and optionally excluded from effective chat totals. |
| `unknown` | `signal=1`, missing `payload.type`, malformed JSON, non-numeric `type`, `ContentError=97`, `SignalError=98`, unsupported raw types | Never guess. Track coverage so operators can evaluate dashboard completeness. |

`VideoCallResult=9989` and payload commands such as `room.invoke` /
`rtc.p2p.invoke` are intentionally not productized in P1B. The classifier can
record them as `system_notification` or `unknown` internally, but P1B APIs must
not expose a dedicated `call` bucket.

## Classifier Rules

Input:

- `signal`
- `payload`
- message metadata used by the existing opanalytics ETL

Rules:

1. If `signal=1`, return `unknown` with reason `signal_encrypted`.
2. Decode `payload` as JSON with `UseNumber`.
3. If `payload.type` is absent, non-numeric, or outside the supported mapping,
   return `unknown` with a reason such as `type_missing`, `type_invalid`, or
   `type_unsupported`.
4. Map known raw content types to the stable categories above.
5. Do not log raw payload content. Logs may include message shard, message id,
   raw type, category, and reason.

The classifier should be a pure Go helper with table-driven tests. It should not
depend on HTTP/webhook request context.

## Storage Design

Add new facts instead of changing the existing P0/P1A fact tables.

### `octo_fact_member_channel_content_daily`

One row per day, channel, sender, and dashboard category.

```sql
CREATE TABLE `octo_fact_member_channel_content_daily` (
  `stat_date`    DATE         NOT NULL COMMENT '统计日(报告时区自然日)',
  `channel_id`   VARCHAR(100) NOT NULL COMMENT '会话ID',
  `channel_type` TINYINT      NOT NULL COMMENT '1=私聊 2=群',
  `space_id`     VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '群=group.space_id; 私聊=""',
  `conv_type`    TINYINT      NOT NULL DEFAULT 0 COMMENT '1.HH群 2.HA群 3.HH私聊 4.HA私聊',
  `msg_category` TINYINT      NOT NULL DEFAULT 0 COMMENT '0=unknown 1=text 2=image 3=voice 4=video 5=file 6=rich_custom 7=system_notification',
  `sender_uid`   VARCHAR(40)  NOT NULL COMMENT '发送者uid',
  `sender_type`  TINYINT      NOT NULL DEFAULT 1 COMMENT '1=human 2=agent',
  `msg_count`    INT          NOT NULL DEFAULT 0 COMMENT '当日该成员该类型消息数',
  `last_msg_at`  BIGINT       NOT NULL DEFAULT 0 COMMENT '当日该类型最后一条消息时间戳',
  `created_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`channel_id`,`stat_date`,`sender_uid`,`msg_category`),
  KEY `idx_date_category` (`stat_date`,`msg_category`,`sender_type`),
  KEY `idx_space_date_category` (`space_id`,`stat_date`,`msg_category`,`sender_type`),
  KEY `idx_sender_date_category` (`sender_uid`,`stat_date`,`msg_category`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci
  COMMENT='看板事实表-成员×会话×日×消息类型';
```

### `octo_fact_channel_content_daily`

One row per day, channel, and dashboard category. It is recomputed from the
member-channel content fact for dirty days.

```sql
CREATE TABLE `octo_fact_channel_content_daily` (
  `stat_date`       DATE         NOT NULL COMMENT '统计日(报告时区自然日)',
  `channel_id`      VARCHAR(100) NOT NULL COMMENT '会话ID',
  `channel_type`    TINYINT      NOT NULL COMMENT '1=私聊 2=群',
  `space_id`        VARCHAR(40)  NOT NULL DEFAULT '',
  `conv_type`       TINYINT      NOT NULL DEFAULT 0,
  `msg_category`    TINYINT      NOT NULL DEFAULT 0,
  `human_msg_count` INT          NOT NULL DEFAULT 0,
  `agent_msg_count` INT          NOT NULL DEFAULT 0,
  `last_msg_at`     BIGINT       NOT NULL DEFAULT 0,
  `created_at`      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`space_id`,`stat_date`,`channel_id`,`msg_category`),
  KEY `idx_date_category` (`stat_date`,`msg_category`),
  KEY `idx_conv_category_date` (`conv_type`,`msg_category`,`stat_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci
  COMMENT='看板事实表-会话×日×消息类型';
```

## ETL And Backfill

Incremental ETL:

- Keep the current total-message facts unchanged.
- In the same chunk pass, classify each accepted source message and accumulate
  content facts by `(day, channel, sender, category)`.
- Use the existing channel metadata resolution so `space_id`, `conv_type`, and
  sender type are consistent with current opanalytics totals.
- Mark touched days dirty for both channel total recompute and content-channel
  recompute.

Backfill:

- Add a separate content backfill cursor, not the production incremental cursor.
- Scan each message shard by primary-key keyset pagination.
- Run from a read replica when available; do not backfill from the online primary
  for large deployments.
- Support pause/resume and rate limits.
- Report progress by shard, last id, rows scanned, rows classified, unknown
  count, and error count.
- API rollout must either wait for backfill completion or return a clear
  `data_complete=false` signal.

Rollback:

- API rollback is safe because existing P0/P1A fields are unchanged.
- ETL rollback can stop writing content facts while leaving old content rows
  unused.
- A down migration may drop the new content fact tables only if operators accept
  losing backfilled analytics data.

## Runtime Shape

This work is analytics-heavy and should not keep growing inside the online IM
request path.

Short term:

- Keep the code in this repo for shared models, migrations, auth, and release
  simplicity.
- Run ETL/backfill as a separate worker process or deployment.
- The HTTP API reads only analytics tables.
- The worker reads message shards from a replica and writes analytics facts.

Medium term:

- Split opanalytics worker/API into an independent service if query volume,
  backfill cost, or pair/bitmap interaction facts become operationally heavy.
- Reuse octo-server auth or an internal auth boundary for superAdmin-only
  dashboard access.

## API Contract Draft

All endpoints stay under `/v1/manager/dashboard`, use superAdmin checks,
`SharedUIDRateLimiter`, and i18n error envelopes.

### `GET /v1/manager/dashboard/message-types`

Query parameters:

- `start_date`
- `end_date`
- `space_ids`

Response:

```json
{
  "data_complete": true,
  "unknown_msg_count": 12,
  "unknown_rate": 0.0034,
  "items": [
    {
      "msg_category": 1,
      "category_key": "text",
      "human_msg_count": 100,
      "agent_msg_count": 20,
      "total_msg_count": 120
    }
  ]
}
```

The response should always include all fixed categories with zero-filled counts.

### `GET /v1/manager/dashboard/message-type-matrix`

Query parameters:

- `start_date`
- `end_date`
- `space_ids`

Response:

```json
{
  "data_complete": true,
  "items": [
    {
      "conv_type": 1,
      "msg_category": 1,
      "category_key": "text",
      "human_msg_count": 100,
      "agent_msg_count": 0,
      "total_msg_count": 100
    }
  ]
}
```

The response should zero-fill all `conv_type=1..4` x fixed categories. When
`space_ids` is present, private `conv_type=3/4` rows should be zeroed, matching
the existing dashboard behavior.

## Implementation Split

1. Classifier and tests.
   - Add pure helper and table-driven tests covering known types, system types,
     malformed payloads, `signal=1`, and unsupported raw types.
2. Schema and ETL write path.
   - Add content fact migrations.
   - Write content facts during incremental ETL.
   - Recompute channel content facts for dirty days.
3. Backfill worker.
   - Add isolated cursor and rate-limited shard scan.
   - Add progress logs and resumability.
4. Read APIs.
   - Add message type distribution and matrix endpoints.
   - Add Swagger and focused API tests.
5. Rollout.
   - Deploy schema and inert code first.
   - Run backfill in a controlled worker.
   - Enable APIs after data completeness is acceptable.

## Open Decisions

- Whether `system_notification` should be excluded from headline message totals
  or only shown as a separate category.
- Whether unknown-rate thresholds should trigger dashboard warnings.
- Whether rich/custom should remain one bucket or split once product has a clear
  visualization need.
- Whether call-like payloads should be silently folded into
  `system_notification` or left `unknown` until a separate call scope exists.
