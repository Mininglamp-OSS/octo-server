# @octo/agent-runtime

A lightweight, resident HTTP runtime that embeds the [`@earendil-works/pi-coding-agent`](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
kernel behind an authenticated API with verbose-tiered output and resumable SSE
streaming.

This is a **new, additive orchestration-layer service**. It does not fork pi and
does not modify the Go kernel of octo-server — it depends on pi via npm and lives
entirely under `agent-runtime/`. It implements the backend dev-design
(`research/DESIGN-backend-devdesign.md`, reviewed APPROVED).

## What it does

- **Single resident Node process** = a thin Fastify HTTP server + one or more
  resident pi agent sessions, sharing the orchestration-layer memory state.
- **Bearer auth** (`Authorization: Bearer <token>`, compat `x-agent-token`) on a
  global `onRequest` hook; `GET /health` is public. Missing/malformed/unknown
  token → `401` with the `{ok:false,error}` envelope.
- **Verbose three-tier projection** (`off` / `on` / `full`) over the pi event
  stream, without touching the kernel.
- **SSE streaming** with a per-session monotonic event-id, a bounded ring buffer,
  `Last-Event-ID` replay, and a `resync_required` frame when a reconnect falls
  outside the buffer window.

## Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/v1/agent` | ✅ | Run a turn. Sync by default; `stream:true` → SSE. |
| `GET`  | `/v1/sessions` | ✅ | List the caller's resident sessions. |
| `GET`  | `/v1/sessions/:key` | ✅ | Session state (mirrors pi `RpcSessionState`). |
| `POST` | `/v1/sessions/:key/abort` | ✅ | Abort the in-flight turn. |
| `GET`  | `/health` | ❌ | Liveness/readiness probe. |

`POST /v1/agent` body:

```jsonc
{
  "message": "string, required (unless resume:true)",
  "sessionKey": "string?, gated by allowRequestSessionKey",
  "verbose": "off | on | full",   // overrides the global default
  "stream": "boolean?, default false",
  "resume": "boolean?, reconnect to an existing session's stream",
  "timeoutSeconds": "number?"
}
```

Response envelope: `{ ok, data? , error? }`. In sync mode, `data` is
`{ sessionKey, result, usage, trace }` (`trace` is populated per verbose tier and
empty at `off`). In stream mode, each SSE frame carries an `id:` line plus a
`data:` line whose JSON `type` names the event; the terminal frame is
`{ "type":"done", "result":..., "usage":... }`.

## Configuration (environment)

| Var | Default | Meaning |
|---|---|---|
| `AGENT_RUNTIME_TOKEN` / `AGENT_RUNTIME_TOKENS` | — | Accepted bearer token(s); comma list for rotation. No token ⇒ every authed route returns 401 (fail closed). |
| `AGENT_RUNTIME_HOST` | `127.0.0.1` | Listen host. |
| `AGENT_RUNTIME_PORT` | `8787` | Listen port. |
| `AGENT_RUNTIME_VERBOSE_DEFAULT` | `off` | Global verbose tier. |
| `AGENT_RUNTIME_ALLOW_REQUEST_SESSION_KEY` | `false` | Accept a client `sessionKey` (OpenClaw-aligned default off). |
| `AGENT_RUNTIME_ALLOWED_SESSION_KEY_PREFIXES` | — | Required prefixes when the above is `true`. |
| `AGENT_RUNTIME_SSE_BUFFER_SIZE` | `512` | Per-session SSE ring capacity. |
| `AGENT_RUNTIME_BACKEND` | `fake` | `pi` for the in-process kernel (route ①); `fake` for the deterministic backend. |

## Kernel backends (the `AgentBackend` seam)

The HTTP / auth / verbose / SSE layers sit on the `AgentBackend` interface
(`prompt` / `abort` / `getState` / `subscribe`). Three implementations:

- **`PiAgentBackend`** (route ①) — in-process `createAgentSession`. Running real
  turns requires a configured model provider (pi `auth.json` / `models.json`).
- **`FakeAgentBackend`** — deterministic, no model provider; emits a pi-shaped
  event stream. Used by the test suite and for local smoke runs, so the full HTTP
  contract, auth, verbose projection and SSE replay are exercisable offline.
- Route ② (a `./rpc-entry` subprocess) is reserved behind the same interface.

## Develop

```bash
npm install
npm test        # vitest: auth 401, endpoint contracts, verbose tiers, SSE replay
npm run typecheck
AGENT_RUNTIME_TOKEN=dev npm start   # boots on 127.0.0.1:8787 with the fake backend
```

## Scope

MVP + streaming increment per dev-design §10. Post-MVP (deferred, jointly signed
off): SQLite/FTS5 cross-session query ACL, rich diff/checkpoint/rollback
endpoints, cron/subagent/template orchestration modules, route ② isolation.
