---
type: Learning
title: Preserve absolute channel state before aggregating incremental unread
description: An unread delta contains absolute values for returned channels but cannot alone determine a parent aggregate.
tags: [conversation-sync, unread, client-state]
timestamp: 2026-09-14T00:00:00Z
---

A sync endpoint may support both complete snapshots and incremental results.
Expose the effective mode when a new client-facing field reuses that data.
Clients replace full state, merge absolute per-channel delta values, and only
then aggregate by the business owner. Summing the current delta or a paginated
metadata list silently undercounts an agent's sessions. An omitted optional
field indicates unavailable data, not a successful empty snapshot.

Keep a compatibility test that compares existing response fields and upstream
request parameters when introducing an independent result on a shared endpoint.
