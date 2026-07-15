# Card action callback consumer integration

This guide is for first-party services that consume interactive card actions
from octo-server. A consumer implements one HTTPS decision endpoint; it does not
poll a Bot queue, hold a Bot token, or call message/card APIs.

For octo-server route configuration, monitoring, DLQ replay, and rollout, see
[`card-action-callback-dispatch.md`](./card-action-callback-dispatch.md).

## Onboarding contract

Before enabling a producer:

1. Deploy an exact HTTPS endpoint with no redirect.
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
cause the same `event_id` to be delivered again.

## Standard approval onboarding

For a consumer using the standard terminal visual, no owner-specific
octo-server code is required:

1. Add the exact sender-bound callback route to `OCTO_CARD_ACTION_ROUTES`, set
   `notify_token_env`, and add its URL to `OCTO_CARD_ACTION_ALLOWED_URLS`.
2. Implement this signed decide contract and return a typed result.
3. Call `/v1/internal/notify` with the route-bound token and an
   `approval_card` containing `action_type`, display text, and bounded domain
   identifiers.

Example initial-card request:

```json
{
  "space_id": "space-1",
  "service": "smart-summary",
  "targets": ["user-b"],
  "actor_uid": "user-a",
  "approval_card": {
    "action_type": "summary.publish.decision",
    "title": "Publish summary",
    "description": "Review before publishing",
    "data": {"task_no": "task-1"}
  }
}
```

Send `X-Internal-Token: <the value named by notify_token_env>`. The token fixes
`sender_uid` and `owner`; callers cannot select either. It can mint cards only
for action types whose route repeats that `notify_token_env`. The server owns
the Allow/Deny labels, action IDs, reserved metadata, layout, escaping, and
profile. The request accepts neither callback URLs nor arbitrary card JSON.

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
  "action_id": "approve",
  "decision": "approve",
  "operator_uid": "user-b",
  "doc_id": "doc-1",
  "request_id": "request-1",
  "inputs": {},
  "data": {
    "owner": "docs",
    "action_type": "access_request.decision",
    "decision": "approve",
    "doc_id": "doc-1",
    "request_id": "request-1"
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

The following Express example uses a fixed callback path and a five-minute
freshness window. Mount `express.raw` for this route before any global
`express.json` middleware.

```ts
import { createHash, createHmac, timingSafeEqual } from "node:crypto";
import express, { type Request, type Response } from "express";

const app = express();
const callbackPath = "/v1/card-actions/decide";
const maxSkewSeconds = 300;
const configuredSecret = process.env.OCTO_DOCS_CARD_ACTION_SECRET;

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
    "OCTO_DOCS_CARD_ACTION_SECRET must contain at least 32 bytes",
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

  lock the domain request identified by request_id/doc_id
  verify the request belongs to the expected Space/resource
  re-check operator_uid is currently authorized
  CAS pending -> approved/denied (first valid decision wins)
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

Applied approval example:

```json
{
  "disposition": "applied",
  "state": "approved",
  "requester_uid": "user-a",
  "display": { "title": "Roadmap" }
}
```

An idempotent replay must return the exact stored result for the same
`event_id`. If the original result was the applied approval above, repeat that
same result; do not rewrite its disposition merely because this HTTP delivery
is a replay. `replayed` is valid when it was the authoritative result initially
stored for that event, for example when the business transition had already
been committed by another decision path:

```json
{
  "disposition": "replayed",
  "state": "approved",
  "requester_uid": "user-a",
  "display": { "title": "Roadmap" }
}
```

Business rejection is also a typed HTTP 200 response, not an HTTP 403:

```json
{ "disposition": "forbidden", "state": "pending" }
```

`requester_uid` is required whenever `state` is `approved` or `denied`, because
octo-server must notify the applicant. Responses are limited to 64 KiB and the
current decoder rejects unknown top-level fields. Coordinate schema additions
with an octo-server release.

For standard approval routes, the originating card must carry an authoritative
`space_id`; terminal requester notification fails closed without it.

## HTTP and retry behavior

| Consumer response                             | octo-server behavior                               |
| --------------------------------------------- | -------------------------------------------------- |
| `2xx` + valid typed body                      | Finalize card and applicant notification, then ACK |
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
- concurrent approve/deny produces one domain winner;
- operator removed from ACL before click returns `forbidden`;
- terminal `approved`/`denied` always includes `requester_uid`;
- transient `5xx` can be retried safely.
