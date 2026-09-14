# AI Team unread through conversation sync

Use the existing `POST /api/v1/conversation/sync` request for unread data. AI Team
agent/session list, detail, create and add responses keep their original schema.
There is no new route, Redis projection or extra IM request.

Protected AI sessions were already excluded from normal `conversations`. The
response therefore adds one independent field, leaving the original fields,
filters, request parameters, cursor and clear-unread processing unchanged:

```json
{
  "conversations": [],
  "ai_team_conversations": {
    "mode": "full",
    "items": [
      {
        "bot_id": "bot_123",
        "session_id": "session_123",
        "space_id": "space_123",
        "channel_id": "parent____session_123",
        "channel_type": 5,
        "unread": 3,
        "version": 42,
        "last_msg_seq": 12,
        "timestamp": 1789344000
      }
    ]
  }
}
```

Only authorized ready, non-deleted sessions in the current user's requested
Space appear. Archived/muted sessions are included. Shared AI Team groups,
private parent groups, ordinary groups and their unrelated threads are excluded.
No message bodies or unauthorized session metadata are exposed in this field.

## Client contract

The frontend owns unread state, keyed by login user, Space and session channel:

- `mode=full`: replace that Space's AI conversation snapshot with `items`.
- `mode=delta`: replace the absolute count for returned channels; preserve
  omitted channels. Empty delta means no changes, not zero unread.
- Missing `ai_team_conversations`: do not clear existing client state. The
  feature may be disabled, the request unscoped/`msg_count=0`, or the optional
  metadata lookup unavailable.
- A successful empty full response contains `items: []` explicitly.
- Group locally maintained session counts by `bot_id` to display agent totals.
  Never sum only the current delta or the currently visible session page, and
  never use the shared-team or private-parent group's count.
- Continue handling live IM messages and existing clear-unread events in client
  conversation state. Ignore stale responses after account/Space switches;
  coordinate overlapping sync and local read operations as for normal chats.
  Remove deleted sessions from local state; full sync also removes them.

Current Web sends `{"msg_count":1,"recent_filter":true}` with the current Space
and no cursor. Under the default `MessageSaveAcrossDevice=true`, this is full
sync. The mode uses the effective server-side version and message cursors,
including any pre-existing device offsets, rather than assuming every POST is
incremental. Values are the same WuKongIM absolute unread counts used for chat.

The frontend must consume this additional field in its existing sync callback;
the old callback only processes `conversations`. This backend change does not
implement badge UI or introduce another frontend request/polling loop.

## Compatibility and cost

The original `conversations`, `users`, `groups`, `channel_status`,
`space_memberships`, IM fetch and cursor flow remain intact. AI extraction runs
while assembling the final response and does not mutate the raw conversation
slice. Lookup failure logs a warning and omits the optional field.

It adds one indexed association/authority query for AI session IDs present in
this IM batch, plus linear in-memory mapping. There is no all-session scan,
per-session HTTP query or extra IM/Redis operation. When no AI candidates are in
the response, no metadata lookup is performed. IM query cost is unchanged;
total latency is not claimed identical, and no production load test was run.

The data scope follows the existing WuKongIM conversation-sync limits. This
change does not fetch older channels outside that scope separately.
