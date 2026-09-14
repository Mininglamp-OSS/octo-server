---
type: Journal
title: AI Team unread through existing conversation sync
description: Expose authorized AI session unread in an independent sync response field while preserving ordinary chat behavior.
tags: [ai-team, conversation-sync, unread, space-isolation]
timestamp: 2026-09-14T00:00:00Z
---

The existing conversation sync already fetches AI session unread from WuKongIM,
but filters those protected channels out of ordinary conversations. The change
adds a separate `ai_team_conversations` response containing absolute channel
counts and full/delta mode. Clients maintain session state and aggregate by bot;
AI Team metadata APIs and server unread storage are unchanged.

The authority lookup covers only AI session candidates in the returned IM batch.
It checks owner, Space seats, active identities, parent membership and ready,
non-deleted sessions. Missing optional metadata omits the new field without
failing normal sync. Empty deltas and absent fields must not clear client state.

The PR is based directly on main so it does not import the separate aggregate
AI Team group implementation. Tests seed an unrelated group purpose explicitly
instead of depending on that feature's schema. Validation compares every
original response field and IM parameter with the extension enabled/disabled,
and exercises real IM message, clear and delete flows.
