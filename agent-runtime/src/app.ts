/**
 * Fastify application: auth hook + routing + response envelope + SSE (backend
 * dev-design §1-§4). Sits entirely on the `AgentBackend` seam; no pi imports here.
 */
import Fastify, { type FastifyInstance } from "fastify";
import type { RuntimeConfig, VerboseLevel } from "./config.js";
import { buildAuthHook, type AuthedRequest } from "./auth.js";
import { err, ok, STATUS_BY_CODE } from "./envelope.js";
import { SessionRegistry, type SessionRecord } from "./session-registry.js";
import { serializeSse } from "./sse.js";
import { buildTrace, project } from "./verbose.js";
import { FakeAgentBackend } from "./backends/fake.js";
import type { AgentBackend, BackendEvent, BackendFactory, Usage } from "./types.js";

interface AgentRequestBody {
  message?: unknown;
  sessionKey?: unknown;
  verbose?: unknown;
  stream?: unknown;
  resume?: unknown;
  timeoutSeconds?: unknown;
}

function resolveVerbose(requested: unknown, fallback: VerboseLevel): VerboseLevel {
  if (requested === "off" || requested === "on" || requested === "full") return requested;
  return fallback;
}

/** Default backend factory: pi (route ①) when configured, else the deterministic backend. */
function defaultFactory(config: RuntimeConfig): BackendFactory {
  if (config.backend === "pi") {
    return async (sessionKey) => {
      const { PiAgentBackend } = await import("./backends/pi.js");
      return PiAgentBackend.create(sessionKey);
    };
  }
  return async (sessionKey) => new FakeAgentBackend(sessionKey);
}

export interface BuildAppOptions {
  /** Override the backend factory (tests inject deterministic backends). */
  factory?: BackendFactory;
}

export function buildApp(config: RuntimeConfig, options: BuildAppOptions = {}): FastifyInstance {
  const app = Fastify({ logger: false });
  const factory = options.factory ?? defaultFactory(config);
  const registry = new SessionRegistry(config, factory);

  app.decorate("registry", registry);
  app.addHook("onRequest", buildAuthHook(config));
  app.addHook("onClose", async () => {
    await registry.disposeAll();
  });

  // ---- Health (public) ----
  app.get("/health", async () => ok({ status: "ok", ready: true }));

  // ---- Resolve the effective sessionKey under the allowRequestSessionKey gate (§2.4) ----
  function resolveSessionKey(
    requested: unknown,
    ownerId: string,
  ): { key?: string; error?: string } {
    if (typeof requested === "string" && requested.length > 0) {
      if (!config.allowRequestSessionKey) {
        // Ignored by policy — route to the token's default resident session.
        return { key: registry.defaultKeyFor(ownerId) };
      }
      const allowed =
        config.allowedSessionKeyPrefixes.length === 0 ||
        config.allowedSessionKeyPrefixes.some((p) => (requested as string).startsWith(p));
      if (!allowed) return { error: "sessionKey not in an allowed prefix" };
      return { key: requested };
    }
    return { key: registry.defaultKeyFor(ownerId) };
  }

  // ---- POST /v1/agent ----
  app.post("/v1/agent", async (request: AuthedRequest, reply) => {
    const ownerId = request.ownerId!;
    const body = (request.body ?? {}) as AgentRequestBody;

    const resume = body.resume === true;
    if (!resume) {
      if (typeof body.message !== "string" || body.message.trim().length === 0) {
        return reply.code(STATUS_BY_CODE.invalid_request).send(err("invalid_request", "message is required"));
      }
    }

    const resolved = resolveSessionKey(body.sessionKey, ownerId);
    if (resolved.error || !resolved.key) {
      return reply.code(STATUS_BY_CODE.invalid_session_key).send(err("invalid_session_key", resolved.error ?? "invalid session key"));
    }
    const sessionKey = resolved.key;
    const level = resolveVerbose(body.verbose, config.verboseDefault);
    const record = await registry.getOrCreate(sessionKey, ownerId);

    const lastEventIdRaw = request.headers["last-event-id"];
    const lastEventId = typeof lastEventIdRaw === "string" ? Number.parseInt(lastEventIdRaw, 10) : NaN;
    const isReconnect = resume || Number.isFinite(lastEventId);

    const timeoutMs =
      typeof body.timeoutSeconds === "number" && body.timeoutSeconds > 0 ? body.timeoutSeconds * 1000 : undefined;

    if (body.stream === true) {
      return streamTurn(reply, record, level, {
        message: typeof body.message === "string" ? body.message : "",
        isReconnect,
        lastEventId: Number.isFinite(lastEventId) ? lastEventId : undefined,
        timeoutMs,
      });
    }
    return syncTurn(reply, record, sessionKey, level, {
      message: typeof body.message === "string" ? body.message : "",
      timeoutMs,
    });
  });

  // ---- GET /v1/sessions ----
  app.get("/v1/sessions", async (request: AuthedRequest) => {
    return ok({ sessions: registry.list(request.ownerId!) });
  });

  // ---- GET /v1/sessions/:key ----
  app.get<{ Params: { key: string } }>("/v1/sessions/:key", async (request, reply) => {
    const ownerId = (request as AuthedRequest).ownerId!;
    const record = registry.get(request.params.key, ownerId);
    if (!record) return reply.code(STATUS_BY_CODE.not_found).send(err("not_found", "session not found"));
    return ok({ sessionKey: record.sessionKey, state: record.backend.getState() });
  });

  // ---- POST /v1/sessions/:key/abort ----
  app.post<{ Params: { key: string } }>("/v1/sessions/:key/abort", async (request, reply) => {
    const ownerId = (request as AuthedRequest).ownerId!;
    const record = registry.get(request.params.key, ownerId);
    if (!record) return reply.code(STATUS_BY_CODE.not_found).send(err("not_found", "session not found"));
    await record.backend.abort();
    return ok({ sessionKey: record.sessionKey, aborted: true });
  });

  return app;
}

interface TurnParams {
  message: string;
  timeoutMs?: number;
}

/** Synchronous mode: aggregate the projected turn, then reply once (§3.2). */
async function syncTurn(
  reply: import("fastify").FastifyReply,
  record: SessionRecord,
  sessionKey: string,
  level: VerboseLevel,
  params: TurnParams,
): Promise<void> {
  const backend: AgentBackend = record.backend;
  const events: BackendEvent[] = [];
  let finalText = "";
  let usage: Usage | undefined;

  try {
    await runTurn(backend, params, (event) => {
      events.push(event);
      if (event.type === "message_end") {
        finalText = event.text;
        usage = event.usage;
      }
    });
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    if (message === "timeout") {
      await backend.abort().catch(() => {});
      return void reply.code(STATUS_BY_CODE.timeout).send(err("timeout", "turn timed out"));
    }
    return void reply.code(STATUS_BY_CODE.internal).send(err("internal", message));
  }

  return void reply.send(
    ok({ sessionKey, result: finalText, usage, trace: buildTrace(events, level) }),
  );
}

/** Streaming mode: SSE with per-session event-id, ring buffering and replay (§3.6). */
async function streamTurn(
  reply: import("fastify").FastifyReply,
  record: SessionRecord,
  level: VerboseLevel,
  params: TurnParams & { isReconnect: boolean; lastEventId?: number },
): Promise<void> {
  reply.hijack();
  const raw = reply.raw;
  raw.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-cache, no-transform",
    connection: "keep-alive",
    "x-accel-buffering": "no",
  });

  const writeDone = () => raw.write(serializeSse(null, { type: "done", result: record.lastResult?.text ?? "", usage: record.lastResult?.usage }));

  // Reconnect: replay the buffered tail (or emit resync_required) before going live.
  if (params.isReconnect && params.lastEventId !== undefined) {
    const { resync, frames } = record.ring.replay(params.lastEventId, level);
    if (resync) {
      raw.write(serializeSse(null, resync));
    } else if (frames) {
      for (const f of frames) raw.write(serializeSse(f.id, f.frame));
    }
  }

  // Reconnect to an already-settled turn: deliver the terminal done frame and end (§3.6).
  if (params.isReconnect && !record.streaming) {
    writeDone();
    raw.end();
    return;
  }

  // Reconnect to a still-live turn: tail subsequent ring appends without re-appending,
  // re-projecting each raw event at this connection's verbose level.
  if (params.isReconnect) {
    await new Promise<void>((resolve) => {
      const remove = record.addListener(({ id, event }) => {
        const frame = project(event, level);
        if (frame) raw.write(serializeSse(id, frame));
        if (event.type === "settled" || event.type === "error") {
          remove();
          writeDone();
          raw.end();
          resolve();
        }
      });
    });
    return;
  }

  // Fresh turn: this connection is the driver. It owns ring appends and fan-out.
  record.streaming = true;
  let settled = false;
  const finish = (unsub: () => void, timer?: NodeJS.Timeout) => {
    if (settled) return;
    settled = true;
    record.streaming = false;
    if (timer) clearTimeout(timer);
    unsub();
    writeDone();
    raw.end();
  };

  await new Promise<void>((resolve) => {
    let timer: NodeJS.Timeout | undefined;
    const unsub = record.backend.subscribe((event) => {
      const emitted = record.ring.append(event, level);
      if (emitted) raw.write(serializeSse(emitted.id, emitted.frame));
      if (event.type === "message_end") record.lastResult = { text: event.text, usage: event.usage };
      record.notify({ id: emitted ? emitted.id : null, event });
      if (event.type === "settled" || event.type === "error") {
        finish(unsub, timer);
        resolve();
      }
    });

    if (params.timeoutMs) {
      timer = setTimeout(() => {
        record.backend.abort().catch(() => {});
        if (!settled) {
          raw.write(serializeSse(null, { type: "error", message: "turn timed out" }));
          finish(unsub, timer);
        }
        resolve();
      }, params.timeoutMs);
    }

    record.backend
      .prompt(params.message, { timeoutSeconds: params.timeoutMs ? params.timeoutMs / 1000 : undefined })
      .catch((e) => {
        raw.write(serializeSse(null, { type: "error", message: e instanceof Error ? e.message : String(e) }));
        finish(unsub, timer);
        resolve();
      });
  });
}

/** Drive one prompt to settlement, forwarding each event to `onEvent`. */
function runTurn(backend: AgentBackend, params: TurnParams, onEvent: (event: BackendEvent) => void): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let done = false;
    let timer: NodeJS.Timeout | undefined;
    const settle = (fn: () => void) => {
      if (done) return;
      done = true;
      if (timer) clearTimeout(timer);
      unsub();
      fn();
    };
    const unsub = backend.subscribe((event) => {
      onEvent(event);
      if (event.type === "settled") settle(resolve);
      if (event.type === "error") settle(() => reject(new Error(event.message)));
    });
    if (params.timeoutMs) {
      timer = setTimeout(() => settle(() => reject(new Error("timeout"))), params.timeoutMs);
    }
    backend.prompt(params.message, { timeoutSeconds: params.timeoutMs ? params.timeoutMs / 1000 : undefined }).catch((e) =>
      settle(() => reject(e instanceof Error ? e : new Error(String(e)))),
    );
  });
}

declare module "fastify" {
  interface FastifyInstance {
    registry: SessionRegistry;
  }
}
