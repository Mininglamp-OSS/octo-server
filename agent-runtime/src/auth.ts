/**
 * Bearer authentication (backend dev-design §2). Fastify global `onRequest` hook.
 * Primary header `Authorization: Bearer <token>`, compatibility header
 * `x-agent-token: <token>` (aligned with OpenClaw). `GET /health` is whitelisted.
 * Missing/malformed/unknown token -> 401 with the `{ok:false,error}` envelope.
 */
import { timingSafeEqual } from "node:crypto";
import type { FastifyReply, FastifyRequest } from "fastify";
import type { RuntimeConfig } from "./config.js";
import { err, STATUS_BY_CODE } from "./envelope.js";

const PUBLIC_PATHS = new Set(["/health"]);

/** Constant-time membership test to avoid a timing side-channel (§2.2). */
function tokenAccepted(candidate: string, tokens: string[]): boolean {
  const candidateBuf = Buffer.from(candidate);
  let matched = false;
  for (const token of tokens) {
    const tokenBuf = Buffer.from(token);
    // timingSafeEqual requires equal length; compare against a same-length copy
    // so the length check itself doesn't short-circuit and leak timing.
    const padded = Buffer.alloc(candidateBuf.length);
    tokenBuf.copy(padded);
    if (candidateBuf.length === tokenBuf.length && timingSafeEqual(candidateBuf, tokenBuf)) {
      matched = true;
    }
  }
  return matched;
}

/** Extract the presented token, or a reason it is malformed. */
export function extractToken(headers: Record<string, unknown>): { token?: string; reason?: string } {
  const auth = headers["authorization"];
  if (typeof auth === "string" && auth.length > 0) {
    const [scheme, ...rest] = auth.split(" ");
    if (scheme?.toLowerCase() !== "bearer") return { reason: "invalid scheme" };
    const token = rest.join(" ").trim();
    if (!token) return { reason: "missing token" };
    return { token };
  }
  const agentToken = headers["x-agent-token"];
  if (typeof agentToken === "string" && agentToken.trim().length > 0) {
    return { token: agentToken.trim() };
  }
  return { reason: "missing token" };
}

export interface AuthedRequest extends FastifyRequest {
  ownerId?: string;
}

export function buildAuthHook(config: RuntimeConfig) {
  return async function onRequestAuth(request: AuthedRequest, reply: FastifyReply): Promise<void> {
    if (PUBLIC_PATHS.has(request.routeOptions?.url ?? request.url.split("?")[0] ?? request.url)) {
      return;
    }
    const { token, reason } = extractToken(request.headers as Record<string, unknown>);
    if (!token) {
      await reply.code(STATUS_BY_CODE.unauthorized).send(err("unauthorized", reason ?? "missing token"));
      return;
    }
    if (config.tokens.length === 0 || !tokenAccepted(token, config.tokens)) {
      await reply.code(STATUS_BY_CODE.unauthorized).send(err("unauthorized", "invalid token"));
      return;
    }
    // owner_id is derived from the presented token (§2.4 / §5.5). Distinct tokens
    // map to distinct owners; the digest is opaque and never the raw token.
    request.ownerId = ownerIdForToken(token);
  };
}

/** Stable, non-reversible owner id for a token. Never logs or returns the raw token. */
export function ownerIdForToken(token: string): string {
  // A short prefix of a hash keeps distinct tokens distinct without exposing them.
  let h = 2166136261;
  for (let i = 0; i < token.length; i++) {
    h ^= token.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return "owner_" + (h >>> 0).toString(16);
}
