---
type: Learning
title: "Keep dual directory SQLs on the same visible-bot predicate"
description: A keyword subquery and an agent-detail query that claim to describe the same visible-bot set must share hosting and human-owner joins, not just a non-empty creator_uid check.
tags: ["space", "directory", "sql", "testing"]
timestamp: 2026-09-09T21:00:00+08:00
# --- octospec extension fields ---
source: self
origin_task: space-directory-octo-hosted-only
status: pending
candidate_rule: space-isolation
---

# Keep dual directory SQLs on the same visible-bot predicate

## Context

`GET /v1/space/directory` loads humans and agents in two statements. The agent
query already required `octo_hosted` plus an eligible human owner in the same
Space. The keyword owner-match subquery originally only required
`creator_uid <> ''` and relied on the outer human list. A bot whose creator
was itself a bot could still match by name even though it could never appear
in `agents`.

## Rule of thumb

When two queries define one presentation set:

1. Copy the full visibility predicate (hosting, owner human-ness, Space
   membership, system-account exclusion) into both statements.
2. Pin a creator-is-bot / empty-creator case on both the default listing and
   `keyword`.
3. Do not treat “the outer query is humans-only” as enough; that only hides
   the owner row, it does not stop a name match from being computed on an
   ineligible bot.

## Why worth a rule

The contacts directory will keep growing filters (`keyword`, caps, hosting).
Each new filter that is applied in only one of the two SQLs becomes a silent
visibility leak or a count mismatch.
