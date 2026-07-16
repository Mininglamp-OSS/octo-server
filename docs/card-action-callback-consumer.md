# Card action callback consumer integration

This guide is for any first-party service that consumes interactive card
actions from octo-server. A consumer implements one signed decision endpoint;
it does not poll a Bot queue, hold a Bot token, or call message/card APIs. The
contract is language- and owner-independent.

For octo-server route configuration, monitoring, DLQ replay, and rollout, see
[`card-action-callback-dispatch.md`](./card-action-callback-dispatch.md).

## Onboarding contract

Before enabling a producer:

1. Deploy an exact HTTPS endpoint with no redirect. An in-cluster or local
   `http://` endpoint is also allowed when it is declared explicitly in the
   octo-server `OCTO_CARD_ACTION_ROUTES` registry; the operations guide covers
   its confidentiality and network-isolation requirements.
2. Provision a callback HMAC secret of at least 32 random bytes in both
   services. For generic approval ingress, provision a second, distinct notify
   bearer token of the same minimum length.
3. Give octo-server operators the exact URL, sender UID, owner, action type,
   timeout, retry policy, and secret environment-variable name.
4. Verify HMAC over the raw request body before JSON decoding.
5. Enforce timestamp freshness and durable idempotency by `event_id`.
6. Re-check the operator's current authorization in the consumer domain.
7. Apply the domain decision with a compare-and-swap and persist the exact typed
   response in the same transaction.

The callback is at-least-once. A timeout, process crash, or lost response can
cause the same `event_id` to be delivered again. Keep clocks synchronized and
reject timestamps outside a bounded freshness window; the reference
implementation uses five minutes.

## Standard action-card onboarding

For a consumer using the standard terminal visual, no owner-specific
octo-server code is required:

1. Add the exact sender-bound callback route to `OCTO_CARD_ACTION_ROUTES` and
   set `notify_token_env`. The route's `url` is itself the exact allowlist
   entry — no separate URL list to maintain.
2. Implement this signed decide contract and return a typed result.
3. Call `/v1/internal/notify` with the route-bound token and an
   `approval_card` containing `action_type`, display text, bounded domain
   identifiers, and optional bounded actions.

Example initial-card request:

```json
{
  "space_id": "space-1",
  "service": "tasks",
  "targets": ["user-b"],
  "actor_uid": "user-a",
  "approval_card": {
    "action_type": "task.execute.decision",
    "title": "Execute task",
    "description": "Choose how to handle this task",
    "data": {"task_id": "task-1"},
    "actions": [
      {"decision": "execute", "title": "Execute"},
      {"decision": "reject", "title": "Reject"},
      {"decision": "cancel", "title": "Cancel"}
    ]
  }
}
```

Send `X-Internal-Token: <the value named by notify_token_env>`. The token fixes
`sender_uid` and `owner`; callers cannot select either. It can mint cards only
for action types whose route repeats that `notify_token_env`. The server owns
action IDs, reserved metadata, layout, escaping, and profile. The request
accepts neither callback URLs nor arbitrary card JSON.

`actions` is optional. Omit it (or send explicit JSON `null` — equivalent on
the wire) to preserve the localized Allow/Deny actions and their
`approve`/`deny` decisions. Sending `"actions": []` (an explicit empty array)
is treated as a caller bug and rejected — the fallback path is nil, not an
empty slice. When present, it contains 1-5 entries:

- `decision`: a unique stable value matching `[a-z][a-z0-9_.-]{0,47}`. The
  tokens `approve` and `deny` are reserved for the legacy 2-button template
  and rejected as custom decisions so their derived action IDs stay
  collision-free with `approval-approve` / `approval-deny`;
- `title`: trimmed display text containing 1-80 Unicode code points; control
  characters (including tabs, newlines, and the BEL byte) are rejected so the
  server never emits an unrenderable button label.

octo-server derives each action ID as `approval-<decision>` and injects the
route-bound owner/action type into every submit payload. Per-action data,
styles, URLs, inputs, and caller-authored card JSON are not supported. Put
bounded shared domain identifiers in the card's `data` map.

Custom action titles are caller-provided display text. Supply the language
appropriate for the target audience; octo-server validates and renders the
text but does not translate consumer-specific wording.

The generic request exposes consumer-specific identifiers in `data`. The
docs-only `doc_id` and `request_id` top-level conveniences may be absent. For a
standard approval result, `display.title` is optional but recommended; octo
uses only that reviewed display field, removes all actions, renders the status,
and sends a v1 requester outcome. It ignores callback-authored URLs, card JSON,
reasons, and arbitrary display fields.

Docs uses the same callback transport contract but keeps its existing
`DocsCard`/`OCTO_DOCS_NOTIFY_TOKEN` ingress and specialized deep-link/template
binding. A genuinely new visual family still requires octo-server review.

## HTTP request

octo-server sends `POST` to the configured exact path with:

| Header             | Value                                                           |
| ------------------ | --------------------------------------------------------------- |
| `Content-Type`     | `application/json`                                              |
| `X-Octo-Timestamp` | Unix timestamp in seconds                                       |
| `X-Octo-Event-ID`  | Decimal event ID; must exactly match the body `event_id` string |
| `X-Octo-Signature` | `v1=<lowercase hex HMAC-SHA256>`                                |

Example request body:

```json
{
  "event_id": "9007199254740993",
  "action_id": "approval-execute",
  "decision": "execute",
  "operator_uid": "user-b",
  "inputs": {},
  "data": {
    "owner": "tasks",
    "action_type": "task.execute.decision",
    "decision": "execute",
    "task_id": "task-1"
  },
  "message_id": "190001234567890",
  "channel_id": "notification",
  "channel_type": 1,
  "space_id": "space-1",
  "acted_at": 1784073600
}
```

`operator_uid` is an authenticated identity assertion from octo-server, not an
authorization grant. The consumer must still verify that this user can decide
the referenced request. `data`, `channel_id`, and display fields must not be
used as substitutes for consumer-owned ACL or request state.

Treat `decision` as an enum owned by the consumer endpoint. Reject values that
were not configured for this business action; never dispatch it as a method
name, SQL fragment, URL, or other executable input.

`event_id` is deliberately encoded as a decimal string because its full `int64`
range exceeds JavaScript's safe integer range. Store and compare it as a string;
do not coerce it to a JavaScript `number`.

`inputs` is always a JSON object; actions without form inputs receive `{}`.

## Signature verification

The canonical UTF-8 bytes are:

```text
v1\nPOST\n<escaped-path>\n<timestamp>\n<event-id>\n<sha256-of-exact-body>
```

The signature is `v1=` followed by the lowercase hex HMAC-SHA256 of that
canonical value. Hash the exact bytes received on the wire. Re-serializing JSON
before verification changes the signature.

### Language-neutral test vector

Use this fixed non-production vector to verify any implementation. The body is
the single UTF-8 line shown below with no trailing newline.

```text
secret:    0123456789abcdef0123456789abcdef
method:    POST
path:      /v1/card-actions/decide
timestamp: 1784073600
event_id:  9007199254740993
body:      {"event_id":"9007199254740993","action_id":"approval-execute","decision":"execute","operator_uid":"user-b","inputs":{},"data":{"owner":"tasks","action_type":"task.execute.decision","decision":"execute","task_id":"task-1"},"message_id":"190001234567890","channel_id":"notification","channel_type":1,"space_id":"space-1","acted_at":1784073600}
body_sha256: e5f9edc7558b6dbac6f754308b161d79a84e9d4635377a8afd6f95b6baa4c6cc
X-Octo-Signature: v1=77d6abe3e80bd90d70545ce90d8c87daafd65a22b62919cee71b450613d6e50f
```

Changing any body byte, the escaped path, timestamp, or event ID must produce a
different signature.

The following Express example uses a fixed callback path and a five-minute
freshness window. Mount `express.raw` for this route before any global
`express.json` middleware.

```ts
import { createHash, createHmac, timingSafeEqual } from "node:crypto";
import express, { type Request, type Response } from "express";

const app = express();
const callbackPath = "/v1/card-actions/decide";
const maxSkewSeconds = 300;
// Bind this name to the secret_env configured for this route.
const configuredSecret = process.env.OCTO_CARD_ACTION_SECRET;

type DecisionRequest = {
  event_id: string;
  action_id: string;
  decision: string;
  operator_uid: string;
  doc_id?: string;
  request_id?: string;
  inputs: Record<string, unknown>;
  data?: Record<string, unknown>;
  message_id: string;
  channel_id: string;
  channel_type: number;
  space_id?: string;
  acted_at: number;
};

type DecisionResult = {
  disposition: "applied" | "replayed" | "forbidden" | "conflict" | "not_found";
  state: "pending" | "approved" | "denied" | "cancelled";
  requester_uid?: string;
  display?: Record<string, string>;
};

declare function decideIdempotently(
  request: DecisionRequest,
): Promise<DecisionResult>;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseDecisionRequest(value: unknown): DecisionRequest {
  if (!isRecord(value)) throw new Error("request must be an object");
  const requiredStrings = [
    "action_id",
    "decision",
    "operator_uid",
    "message_id",
    "channel_id",
  ];
  if (
    requiredStrings.some(
      (key) => typeof value[key] !== "string" || value[key] === "",
    )
  ) {
    throw new Error("missing required string");
  }
  if (
    typeof value.event_id !== "string" ||
    !/^[1-9][0-9]*$/.test(value.event_id)
  ) {
    throw new Error("invalid event_id");
  }
  if (
    typeof value.channel_type !== "number" ||
    !Number.isInteger(value.channel_type) ||
    typeof value.acted_at !== "number" ||
    !Number.isSafeInteger(value.acted_at)
  ) {
    throw new Error("invalid numeric field");
  }
  if (
    !isRecord(value.inputs) ||
    (value.data !== undefined && !isRecord(value.data))
  ) {
    throw new Error("invalid action data");
  }
  for (const key of ["doc_id", "request_id", "space_id"]) {
    if (value[key] !== undefined && typeof value[key] !== "string") {
      throw new Error(`invalid ${key}`);
    }
  }
  return value as unknown as DecisionRequest;
}

if (!configuredSecret || Buffer.byteLength(configuredSecret) < 32) {
  throw new Error(
    "OCTO_CARD_ACTION_SECRET must contain at least 32 bytes",
  );
}
const secret = configuredSecret;

function verifyOctoSignature(
  rawBody: Buffer,
  timestamp: string,
  eventID: string,
  signature: string,
  nowSeconds = Math.floor(Date.now() / 1000),
): boolean {
  if (!/^[1-9][0-9]*$/.test(eventID) || !/^[0-9]+$/.test(timestamp))
    return false;
  if (!/^v1=[0-9a-f]{64}$/.test(signature)) return false;

  const sentAt = Number(timestamp);
  if (
    !Number.isSafeInteger(sentAt) ||
    Math.abs(nowSeconds - sentAt) > maxSkewSeconds
  ) {
    return false;
  }

  const bodyHash = createHash("sha256").update(rawBody).digest("hex");
  const canonical = [
    "v1",
    "POST",
    callbackPath,
    timestamp,
    eventID,
    bodyHash,
  ].join("\n");
  const expected = createHmac("sha256", secret).update(canonical).digest();
  const provided = Buffer.from(signature.slice(3), "hex");
  return (
    provided.length === expected.length && timingSafeEqual(provided, expected)
  );
}

app.post(
  callbackPath,
  express.raw({ type: "application/json", limit: "64kb" }),
  async (req: Request, res: Response) => {
    const rawBody = Buffer.isBuffer(req.body) ? req.body : Buffer.alloc(0);
    const timestamp = req.header("X-Octo-Timestamp") ?? "";
    const eventID = req.header("X-Octo-Event-ID") ?? "";
    const signature = req.header("X-Octo-Signature") ?? "";

    if (!verifyOctoSignature(rawBody, timestamp, eventID, signature)) {
      res.status(401).json({ error: "unauthorized" });
      return;
    }

    let request: DecisionRequest;
    try {
      request = parseDecisionRequest(JSON.parse(rawBody.toString("utf8")));
    } catch {
      res.status(400).json({ error: "invalid_request" });
      return;
    }
    if (request.event_id !== eventID) {
      res.status(401).json({ error: "unauthorized" });
      return;
    }

    try {
      const result = await decideIdempotently(request);
      res.status(200).json(result);
    } catch {
      // Log the internal cause with event_id; never expose it to octo-server.
      res.status(503).json({ error: "temporarily_unavailable" });
    }
  },
);
```

`DecisionRequest`, runtime validation, and `decideIdempotently` are
consumer-owned. The example validates required transport fields and permits
additive request fields; add consumer-domain bounds before accessing storage.
The transaction must follow this order:

```text
BEGIN
  SELECT stored_response FROM card_action_receipt WHERE event_id = ?
  if found: COMMIT and replay stored_response

  validate the expected action_type and decision enum
  lock the domain request identified by consumer-owned data (for example task_id)
  verify the request belongs to the expected Space/resource
  re-check operator_uid is currently authorized
  apply the selected business operation with a CAS (first valid decision wins)
  INSERT card_action_receipt(event_id, stored_response) with UNIQUE(event_id)
COMMIT
return stored_response
```

If concurrent requests race on the same `event_id`, the unique-key loser must
reload and return the already stored response. Never perform the domain update
outside the transaction that records the idempotency result.

## Typed response

Return HTTP 200 with exactly these top-level fields:

```ts
type DecisionResult = {
  disposition: "applied" | "replayed" | "forbidden" | "conflict" | "not_found";
  state: "pending" | "approved" | "denied" | "cancelled";
  requester_uid?: string;
  display?: Record<string, string>;
};
```

Applied action example:

```json
{
  "disposition": "applied",
  "state": "approved",
  "requester_uid": "user-a",
  "display": { "title": "Execute task" }
}
```

An idempotent replay must return the exact stored result for the same
`event_id`. If the original result was the applied action above, repeat that
same result; do not rewrite its disposition merely because this HTTP delivery
is a replay. `replayed` is valid when it was the authoritative result initially
stored for that event, for example when the business transition had already
been committed by another decision path:

```json
{
  "disposition": "replayed",
  "state": "approved",
  "requester_uid": "user-a",
  "display": { "title": "Execute task" }
}
```

Business rejection is also a typed HTTP 200 response, not an HTTP 403:

```json
{ "disposition": "forbidden", "state": "pending" }
```

The selected `decision` and returned `state` are separate contracts. A custom
decision such as `execute`, `reject`, or `cancel` does not create a new state:

- return `approved` when the requested operation was accepted/completed;
- return `denied` when it was authoritatively rejected;
- return `cancelled` when the underlying request was cancelled;
- use `pending` only to report the current authoritative non-terminal domain
  state. octo-server still removes the actions and renders the standard
  unavailable terminal visual; it does not leave the card interactive.

Every valid typed response finalizes and ACKs this card action. If these four
states or the standard terminal wording cannot represent the result, the
consumer needs a separately reviewed finalizer/template rather than inventing
callback response fields.

`requester_uid` is required whenever `state` is `approved` or `denied`, because
octo-server must notify the applicant. It must be the consumer-authoritative
request initiator, not the operator or an unverified callback field. Responses
are limited to 64 KiB and the current decoder rejects unknown top-level fields.
`display` accepts at most 32 string fields; keys are non-empty and at most 64
bytes, and values are at most 500 Unicode code points. The standard finalizer
currently consumes only `display.title`. Coordinate schema additions with an
octo-server release.

For standard approval routes, the originating card must carry an authoritative
`space_id`; terminal requester notification fails closed without it.

## HTTP and retry behavior

| Consumer response                             | octo-server behavior                               |
| --------------------------------------------- | -------------------------------------------------- |
| `2xx` + valid typed body                      | Finalize card; approved/denied also notify requester; then ACK |
| `408`, `429`, or `5xx`                        | Retry with bounded exponential backoff             |
| Other `4xx`                                   | Permanent rejection; move to DLQ                   |
| `3xx`                                         | Redirect rejected; move to DLQ                     |
| Timeout / transport failure                   | Retry                                              |
| Invalid, oversized, or unknown-field response | Retry, then DLQ after exhaustion                   |

Do not return HTTP 403/404 for normal domain outcomes; use the typed
`disposition` values so octo-server can render the authoritative state.

## Consumer test checklist

- valid signature over exact raw body;
- one-byte body mutation fails verification;
- stale timestamp fails verification;
- header/body `event_id` mismatch fails verification;
- duplicate `event_id` replays one stored response without a second transition;
- an unknown `decision` is rejected without a domain transition;
- concurrent decisions produce one domain winner;
- operator removed from ACL before click returns `forbidden`;
- terminal `approved`/`denied` always includes `requester_uid`;
- transient `5xx` can be retried safely.
