---
type: Journal
title: "Journal: bot-agent-hosting"
description: A self-reported hosting shape on robot (agent_hosting + agent_reported_at), written by POST /v1/bot/register and read back on GET /v1/user/bots. The value space is open — the server validates a lowercase-slug shape, not a set of literals — so a new hosting provider needs no server release. No new errcode, endpoint or i18n entry.
tags: ["bot-api", "wire-contract", "trust-boundary", "migration", "testing"]
timestamp: 2026-09-03T00:00:00+08:00
# --- octospec extension fields ---
task: bot-agent-hosting
upstream: "openclaw runtime hosting-shape distinction (verbal, no issue)"
source: user
---

# Journal: bot-agent-hosting

## What was done

The question was "how does a Bot tell us whether its OpenClaw runtime is
user-operated or platform-hosted". The answer turned out to be: **it cannot
today, and no existing field can be made to answer it** — so the work is a new
self-reported field plus the metadata needed to judge it.

- **`robot.agent_hosting`** (VARCHAR(64), `''` = never reported) and
  **`robot.agent_reported_at`** (TIMESTAMP NULL), added in one atomic `ALTER`.
- **Written by `POST /v1/bot/register`**, User Bot branch only, alongside the
  three existing `agent_*` columns it already writes.
- **Read back on `GET /v1/user/bots`** — the bot owner's own face, which already
  carries `agent_platform` / `agent_version` / `plugin_version` next to
  `bound_agent_ref`.
- **App Bot: explicit non-support.** `registerAppBot` now parses the body *only*
  to warn that the report was ignored. A source guard pins that `app_bot` has no
  `agent_*` column, so the non-support cannot silently become half-support.
- No new errcode, no new endpoint, no i18n entry, no route change.

## Decisions worth keeping

**Three candidate fields were investigated and all three fail.** Recording this
matters more than the feature: the next person will reach for the same three.
`user_api_key.client_id` looks decisive (`botfather` vs `octopush`) but
`modules/integration/api.go` states outright that the desktop client holds only
the business-backend JWT credential, and `/exchange` hardcodes one
`client_id` — so **local desktop clients and cloud services share `octopush`**,
and deriving hosting from it labels local as cloud. `robot.bound_agent_ref` is
filled by the client at bind time and `modules/botfather/model.go` says the
server does not interpret it. `robot.agent_platform` carries a platform name
only. Conclusion: there is no trustworthy source, so this column is an
**observability** field and must never feed authz — written into the column
COMMENT and every Go field comment.

**An enum was the wrong shape, and the question that broke it was one sentence
long.** The first implementation whitelisted `self_hosted` / `octo_hosted`. Then:
"can the client pass `<vendor>_hosted`?" Under a whitelist, no — it needs a server
code change and a release, and hosting providers are a thing that grows. Worse,
the guarantee a whitelist appears to give is **false**: it validates *the value
is in the set*, never *you are entitled to say it* — any holder of the bot's
`bf_` token can claim `octo_hosted` either way. What actually needs blocking is
quotes, angle brackets, whitespace, control characters and Unicode confusables,
and `^[a-z][a-z0-9_]*$` blocks all of those without knowing which vendors exist.
An enum blocked them too, but only as a side effect of blocking everything it had
not been told about. Open values also keep every provider name out of this
open-source repo — a provider's name now lives only in its own config and the
rows it writes.

**The cost is stated, not hidden.** `cloud` / `local` now pass. The reason for
rejecting them (on-premise deployments make "cloud" a lie, per the README's
"cloud is a choice, not a requirement") still holds, but it is now a client
naming convention rather than something the server enforces. Deliberately **no
blocklist** — it is never complete, and having given up value enumeration, a
half-hearted blocklist is the worst of both. The other half of the trade: under a
whitelist a typo becomes an empty string (silent data loss); with open values it
becomes a visible odd value (findable, fixable). The latter is easier to operate.

**`agent_reported_at` is not optional, and its tense is the point.** It means
"when we last *received* a report", not "when the value last changed" — so the
UPDATE fires even when every value is identical. That removed an existing
"skip the UPDATE if nothing changed" shortcut, on purpose. Without the column the
whole `agent_*` group is a set of bare values with no way to judge freshness:
`robot.updated_at` has no `ON UPDATE` clause, so it cannot answer it either. A
stale `self_hosted` from a runtime that was shut down months ago is a wrong
answer delivered confidently; the timestamp is what makes it a dated one.

**A pointer, not a string, and the pointer goes all the way to the SQL.** The
three existing version fields merge field-wise ("empty keeps the stored value"),
which is right for a version number and wrong here: on a
self-hosted → platform-hosted switch, a new runtime that omits the field would
leave the stale value in place. So `*string` distinguishes absent from
explicitly-empty. Crucially the pointer is carried down to the `SetMap` rather
than resolved into "the value we just read": an absent field must be absent
*in SQL*. Reading and writing back looks equivalent and loses concurrent updates
— two runtimes registering the same bot (nothing prevents that today) interleave
as "A reads old → B writes new → A writes old back".

## Gotchas worth remembering

**A validation bound above the column width is a delayed failure.** First
revision: `maxAgentHostingLen = 64` against `VARCHAR(20)`, justified as "room for
future shapes". But the DB runs `STRICT_TRANS_TABLES` and all `agent_*` fields
share **one** UPDATE statement — so a 25-byte value would pass validation, fail
the write with `1406`, and take the caller's `agent_platform` / `agent_version` /
`plugin_version` down with it, while register still returned 200. The bound is
now pinned *equal* to the column width, asserted by a test that regex-extracts
the width from the migration file. `Equal`, not `LessOrEqual`: too large fails as
described, too small pointlessly rejects slugs the column could hold.

**Reject cheaply: bound the length before folding case.** `strings.ToLower`
allocates a copy the size of its input, and register takes an unbounded JSON
body, so a 10MB value must be rejected *before* it. Order is `TrimSpace`
(sub-slice, zero allocation) → length bound → `ToLower` + regexp.

**Tests had to live in the module that owns the schema, not the one that owns the
code.** The write path is `modules/bot_api/register.go`, but the `robot` table's
`agent_*` columns belong to `modules/botfather`'s migrations — and the `bot_api`
test binary does not link botfather's `init()`, so `NewTestServer` builds a
`robot` table without them (`Error 1054: Unknown column 'agent_platform'`). The
HTTP/persistence tests therefore live in `modules/botfather`; only the pure
function and the source guards stayed next to the code. Blank-importing
`modules/app_bot` (rather than a bare `CREATE TABLE IF NOT EXISTS`) was needed
for the App Bot case: a table created outside sql-migrate leaves no
`gorp_migrations` row and makes the *next* package's migration collide — the
known root cause behind `modules/message`'s skipped DB tests.

## Known limitation (recorded, not fixed)

**One overlong `agent_*` field blocks the whole group.** The three pre-existing
string fields have no length validation and are `VARCHAR(50)`, under
`STRICT_TRANS_TABLES`, sharing one UPDATE. So a client reporting an overlong
`agent_platform` also prevents a perfectly valid `agent_hosting` from being
stored — and register still returns 200 (the failure only reaches the log, as
`1406`). This is **not a regression**: the same UPDATE failed before this change
too, and because the value never equals what is stored, it retried and failed on
every register. But the new column's writability now depends on the old columns'
hygiene, which is worth knowing. Fixing it means either per-field degradation for
the existing three (changing their established behaviour) or splitting the UPDATE
(trading atomicity for decoupling) — both deserve their own decision.
`TestRegisterOverlongPlatformBlocksTheWholeAgentUpdate` pins the current
behaviour as executable documentation: whoever fixes it will see that test go
red.

## Scope that was planned and then withdrawn

**`GET /v1/manager/robots{,/:robot_id}` — those endpoints do not exist.** The
brief planned to add the two columns to the ops-facing response, having checked
that `robotListResp` / `robotDetailResp` carried no `agent_*` fields. What it did
not check was whether the routes are mounted: `modules/robot/api_manager.go`'s
`NewManager` has **no caller anywhere in the repo** and `Route()` is never
executed — `1module.go` registers only `New(ctx)`. The whole `Manager` is dead
code, and `/v1/manager/robots`, `/v1/manager/robots/:robot_id` and
`/v1/manager/robot/menus` do not exist in production. The fields and tests were
written, the test reported `404 page not found`, and the change was reverted
whole (`git checkout modules/robot/`). Adding fields to dead code would never be
seen by any caller, but would convince the next person the ops surface already
has this. Real ops visibility needs the `Manager` routes mounted first — a whole
new admin route surface (auth + `SharedUIDRateLimiter`, which that group lacks),
i.e. its own task.

**Contract docs: there is nowhere in this repo to put them.**
`modules/botfather/swagger/api.yaml` is a 30-line placeholder endpoint list —
zero `requestBody`, zero `description`, zero `components/schemas`, and
`/v1/user/bots` is not even in it. The authoritative Bot API field contract is
not in this repo at all: the `/v1/bot/skill.md` generated by
`modules/botfather/skill.go` declares itself deprecated and "no longer receives
Bot API updates", pointing at `Mininglamp-OSS/openclaw-channel-octo`'s
`skills/octo-bot-api/SKILL.md`. So the contract carrier here is the code comments
(pointer semantics, the shape rule, and the two response fields' default
behaviour are all documented on the fields). **Client discoverability needs a
companion change in openclaw-channel-octo — outside this repo and not done.**
