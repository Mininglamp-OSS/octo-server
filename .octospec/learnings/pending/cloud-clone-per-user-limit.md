---
type: Learning
title: "Candidate: lock a stable owner row for global per-owner creation quotas"
description: A global per-owner quota must lock a row that exists before the first child resource, then check and create within that transaction.
tags: ["concurrency", "quota", "database"]
timestamp: 2026-09-09T19:40:00+08:00
---

# Candidate: lock a stable owner row for global per-owner creation quotas

For resources that are globally limited per owner but can be created in any
Space, lock the authoritative owner row with `FOR UPDATE`. Do not lock a
Space-scoped row and do not rely on locking an existing child row: neither
serializes two first-create requests that find no child rows. Keep the quota
check and child persistence inside that transaction, and release the lock only
after the creation has committed.
