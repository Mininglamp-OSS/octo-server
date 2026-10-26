# Testing Octo Server with WuKongIM v3.0.0-beta.24

`wukongim-v3-compat` is an opt-in companion executable for the official
`v3.0.0-beta.24` commit `4d44b39dda6fdaa7dafb637ca6cfb718e9530f77`.
The normal Server entrypoint and its pinned octo-lib dependency are unchanged.
Both executables are included in the Server image built by the root Dockerfile.

The adapter supplies two private interfaces:

- **API:** Octo Server → adapter → IM Core. Set Server `wukongIM.apiURL` to
  the adapter Service and set `wukongIM.managerToken` to the adapter API token.
  This covers octo-lib calls and the independent Space welcome sender.
- **Callbacks:** IM Core → local adapter → signed Server `/v1/webhook`.
  The adapter and Core must share a network namespace (the same Kubernetes
  Pod). The unsigned callback socket only accepts a numeric loopback bind and
  loopback peers. Never route an ingress or a Service to that socket.

Run Server/MySQL/Redis/MinIO in a separate companion Pod with fresh storage and
credentials. Run IM Core and the adapter in their own Pod with a new v3 volume.
Do not reuse v2 data, an existing Server's IM configuration, or a mixed v2/v3
cluster for this experiment.

## Build and start

```sh
go build -o wukongim-v3-compat ./cmd/wukongim-v3-compat
./wukongim-v3-compat \
  --api-listen=0.0.0.0:5001 \
  --core-url=http://127.0.0.1:5002 \
  --webhook-listen=127.0.0.1:5004 \
  --server-webhook-url=http://companion-server:38100/v1/webhook \
  --api-token-file=/run/im-secrets/api-token \
  --webhook-secret-file=/run/im-secrets/webhook-secret \
  --manager-url=http://127.0.0.1:5300 \
  --manager-user=compat \
  --manager-password-file=/run/im-secrets/manager-password
```

In the image the executable is `/home/wukongim-v3-compat`; override the
entrypoint only for the adapter container. Generate independent random secrets
of at least 32 bytes for the API token and Webhook signing key. Supply them as
mounted Secret files. Set Server `TS_WEBHOOK_SECRET_KEY` to the same Webhook
key; existing Server HMAC verification stays enabled.

The Manager account only needs `cluster.channel` / `r`. Keep Manager
authentication enabled and its listener private. The adapter uses login and
the exact channel runtime metadata query for maximum-sequence requests; it
does not receive Manager write permissions or forward Manager credentials to
API callers. Omit all three Manager options to disable this compatibility route.

Configure the corresponding beta.24 TOML sections:

```toml
[api]
listen_addr = "127.0.0.1:5002"
external_ws_addr = "ws://test-im:5200/ws"

[manager]
listen_addr = "127.0.0.1:5300"
auth_on = true
# Supply an instance-specific jwt_secret and a compat user with only
# permissions = [{ resource = "cluster.channel", actions = ["r"] }].

[gateway]
token_auth_on = true
listeners = [
  { name = "tcp", network = "tcp", address = "0.0.0.0:5100", transport = "gnet", protocol = "wkproto" },
  { name = "ws", network = "websocket", address = "0.0.0.0:5200", path = "/ws", transport = "gnet", protocol = "wsmux" }
]

[webhook]
http_addr = "http://127.0.0.1:5004/webhook"
```

This is a fragment, not a complete cluster configuration. Set node identity,
fresh data path, cluster identity/nodes/replica counts and webhook enablement
using beta.24's own TOML schema. Remove obsolete v2 `WK_` variables and
`--ignoreMissingConfig` instead of passing them into the v3 process.

## Adapted contracts

| Contract | Behavior |
| --- | --- |
| `/message/send` | Wrap successful v3 fields in legacy `data`; preserve integer IDs; make `reason != 1` an HTTP 400 failure even when Core returned 200. Malformed or unrepresentable acknowledgements fail instead of becoming zero IDs. |
| `/message/sendbatch` | Bounded personal-message fanout (up to 1000 recipients). Retain `fail_uids` and corresponding `reason` entries. Any partial failure returns HTTP 400 because octo-lib ignores failed recipients on HTTP 200. No automatic send retry. |
| `/channel/max_message_seq` | Authenticated exact Manager lookup of the committed maximum; return legacy `message_seq`. Missing channel observations, wrong channel identity, and values above the old uint32 range fail instead of returning 0. |
| `/health` | Alias to Core `/readyz` for legacy dependency probes. `/healthz` and `/readyz` also pass through without the service token. |
| Other Core API routes | Pass through to the fixed Core destination after service-token authentication, including real failures and 404s. `/varz` is not fabricated. |
| `msg.notify`, `msg.offline` | Change a combined Person channel ID into the peer UID before signing. Preserve other fields, group/thread identifiers, payload bytes and 64-bit IDs. Reject ambiguous Person identifiers. |
| `user.onlinestatus` | Validate the bounded event array and sign the original body. |
| Callback acknowledgement | Wait for the Server response. `msg.notify` requires all expected message IDs; offline/status require `status=200`. Failed, malformed or partial acknowledgements return 502 to Core. |

API requests require the existing `token` header. User authentication, Space
authorization, localized client errors and rate limiting remain in Server.
The adapter is a private machine-protocol boundary; it adds no user-facing
Octo routes. Request bytes, event counts, in-flight requests and upstream
timeouts are bounded. Redirects are not followed, and forwarded destinations
cannot be selected by the caller.

## Failure and verification boundaries

The adapter does not make batch fanout atomic or make Webhooks durable. A send
timeout may follow a committed message. Retry only with appropriate message
identity/outcome handling; never automatically replay an entire partial batch.
beta.24 callbacks use bounded best-effort queues: exhausted retries or an IM
crash can lose callbacks before they reach the adapter. The adapter returns
success only after Server acknowledgement, but has no independent replay log.

Server still consumes uint32 message sequences. A v3 value outside that range
is an explicit compatibility error. A channel absent from Manager runtime
metadata is unavailable, even if it may be a newly created empty channel.
Old plugins, legacy streaming interfaces and v2 data migration need separate
qualification. Monitor real `/metrics` and `/top/v1/snapshot` interfaces rather
than reading missing `/varz` values as zero.

```sh
go test ./internal/imcompat ./cmd/wukongim-v3-compat
go test -race ./internal/imcompat ./cmd/wukongim-v3-compat
go build ./...
```

Before calling a deployment usable, verify the running image/commit, all
dependency readiness checks, SDK send/receive, Server send acknowledgements,
and actual Server message persistence under the correct Person conversation
key. A short single-node smoke test is not capacity or failover qualification.
