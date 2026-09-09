# Verification: ai-container-default-no-mention

Date: 2026-09-08

Baseline: `origin/main` at `df7ef1d1` (worktree `ai-group-default-no-mention`).

## Automated checks

| Check | Command | Result |
| --- | --- | --- |
| Build | `go build ./modules/bot_api/` + full `go build .` | PASS |
| Static analysis | `go vet ./modules/bot_api/` | PASS |
| Lint | `golangci-lint run ./modules/bot_api/... ./modules/ai_team/...` | PASS |
| Unit truth table | `go test ./modules/bot_api/ -run TestResolveEffectiveNoMention` | PASS — 9 cases (4 ordinary, 4 container, 1 other-purpose) |
| Endpoint tests | `go test ./modules/bot_api/ -run TestGetMentionPref` | PASS |
| bot_api regression | `go test ./modules/bot_api/` (whole package, 53.9s) | PASS |
| Cross-module contract | `go test ./modules/ai_team/ -run 'TestAIContainer\|TestOrdinaryGroupStillRequiresMention'` | PASS |
| ai_team regression | `go test ./modules/ai_team/` (whole package) | PASS |
| i18n extraction consistency | `make i18n-extract-check` | PASS |
| i18n lint | `make i18n-lint` | PASS — no new direct error responses, no inline codes |

Local test runs follow CI's per-package protocol from `ci/run-e2e-shard.sh`:
`DROP DATABASE test; CREATE DATABASE test ... COLLATE utf8mb4_general_ci` before
each package, because a single-package `go test` only registers the migrations of
the modules it imports and sql-migrate aborts with `unknown migration in
database` otherwise.

## Falsification check

The tests are pinned against a real regression, not vacuously true. Temporarily
disabling the container branch (`if false && groupPurpose == ...`) makes all
three fail:

- `TestResolveEffectiveNoMention` — FAIL
- `TestGetMentionPref_AIContainerNeedsNoOwnerRecord` — FAIL
- `TestAIContainerAnswersWithoutMention` (contract) — FAIL (`Should be true`,
  `no_mention expected: 1 actual: 0`)

Restored afterwards; the branch is back in place.

## Live end-to-end, including the real OpenClaw channel

Fully isolated stack, created for this run and torn down afterwards — the existing
`octo-ai-team-*` / `octo-bottask-e2e-*` stacks were not touched:

| Component | Where |
| --- | --- |
| octo-server (this branch, compiled) | `127.0.0.1:18190`, gRPC `16979` |
| MySQL | shared `127.0.0.1:3306`, dedicated database `octo_e2e_nomention_wt` |
| Redis | dedicated container `octo-nomention-redis`, `127.0.0.1:16379` |
| WuKongIM | dedicated container `octo-nomention-wukongim`, `15001/15100/15200` |
| OpenClaw host | `openclaw@2026.6.9` installed under a private `OPENCLAW_STATE_DIR` (the checkout at `~/Projects/octo/openclaw` is 2026.6.1, below the plugin's `compat.pluginApi >= 2026.6.9`; the shared checkout was left alone) |
| Octo channel plugin | `openclaw-channel-octo` current build, loaded as `octo 1.4.1` |
| Model | loopback OpenAI-compatible scripted provider on `19123` — only the model OUTPUT is scripted; the plugin's inbound path, its HTTP calls, the agent runtime and the outbound send are all real |

Setup drove the real APIs: `POST /v1/ai-team/agents/e2e_bot` (returns no group —
containers are created lazily), then `POST /v1/ai-team/agents/e2e_bot/sessions`
→ container `9d1dd49d…`, thread channel `9d1dd49d…____2097285736419037184`,
`state=2`.

Gateway came up with `1 plugin: octo`, `bot registered as e2e_bot`,
`WebSocket connected to ws://127.0.0.1:15200`, `agent model: octo-e2e/scripted`.

Each probe is a message sent as the USER straight into WuKongIM
(`POST /message/send`, `channel_type=5`) with a payload carrying **no `mention`
field at all**.

| # | Server decision | Probe | Adapter behaviour | Model calls |
| --- | --- | --- | --- | --- |
| 1 | `effective: true` (container) | non-@ message | `typing` → agent → `[deliver] final text sent immediately` → `Bot reply done` | 1 |
| 2 | `effective: false` (purpose temporarily changed to `not_a_container`, gateway restarted to drop the 30s cache) | non-@ message | `[HISTORY] 非@消息已缓存` — dropped into history, no reply | 0 |
| 3 | `effective: true` (purpose restored, cache expired) | non-@ message | `[deliver] final text sent immediately` → `Bot reply done, lastAnsweredSeq=4` | 1 |

Same gateway process, same group, same shape of message — the only variable is the
server's decision. That is the direct evidence that this change is what unblocks
the session.

Read back from WuKongIM (`/channel/messagesync`), the thread holds both sides:

- `seq=1 from_uid=e2e_owner` → `{"type":1,"content":"没有 at 你，能回我吗？"}` (no `mention` key)
- `seq=2 from_uid=e2e_bot` → `{"content":"NOMENTION_OK 我在 AI 会话里没有被 @ 也收到并回复了这条消息。","type":1}`

Also verified live against the running server:

- container: `{"effective":true,"group_allow_no_mention":1,"no_mention":1}` while
  `bot_mention_pref` held 0 rows and `group.purpose = ai_session_container`
- ordinary group, same bot: `{"effective":false,...}`
- owner write on the container: `403 err.server.ai_team.container_protected`

## Teardown

`octo-nomention-redis` and `octo-nomention-wukongim` removed; `octo_e2e_nomention_wt`
dropped; server, gateway and scripted provider stopped. Pre-existing containers and
databases untouched.

## Known gaps (not introduced here)

- `/v1/bot/events` drops `space_id` / `session_key` / `input_id`: the read path in
  `modules/bot_api/events.go` re-declares a local `robotEvent` struct without
  them, so an adapter polling events cannot see AI-session metadata. Messages
  reach the adapter over the WuKongIM WebSocket, so this does not affect the
  behaviour fixed here.
- The plugin's `compat.pluginApi` (`>= 2026.6.9`) is ahead of the openclaw
  checkout in this workspace (2026.6.1). Not a product issue; noted so the next
  live run does not rediscover it.
