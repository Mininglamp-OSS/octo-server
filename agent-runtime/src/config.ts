/**
 * Runtime configuration. Source priority: environment variables > config file
 * (backend dev-design §2.2). Tokens never touch logs, sessions or metadata.
 */

export type VerboseLevel = "off" | "on" | "full";

export interface RuntimeConfig {
  host: string;
  port: number;
  /** Accepted bearer tokens. Missing/empty means auth cannot succeed (fail closed). */
  tokens: string[];
  /** Global default verbose level, overridable per request (§4.4). */
  verboseDefault: VerboseLevel;
  /**
   * When false (OpenClaw-aligned default), a client-supplied `sessionKey` in the
   * request body is ignored and the server routes to the token's default session
   * (§2.4). When true, the key must match one of `allowedSessionKeyPrefixes`.
   */
  allowRequestSessionKey: boolean;
  allowedSessionKeyPrefixes: string[];
  /** SSE ring buffer capacity per session (§3.6). */
  sseBufferSize: number;
  /** Backend selection: "fake" (deterministic) or "pi" (in-process kernel, route ①). */
  backend: "fake" | "pi";
}

function parseVerbose(value: string | undefined, fallback: VerboseLevel): VerboseLevel {
  if (value === "off" || value === "on" || value === "full") return value;
  return fallback;
}

function parseList(value: string | undefined): string[] {
  if (!value) return [];
  return value
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

function parseIntOr(value: string | undefined, fallback: number): number {
  const n = value === undefined ? NaN : Number.parseInt(value, 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): RuntimeConfig {
  // Support a single token (AGENT_RUNTIME_TOKEN) or a comma list (AGENT_RUNTIME_TOKENS)
  // for rotation, aligned with the design's `auth.token` / `auth.tokens[]`.
  const tokens = [
    ...parseList(env.AGENT_RUNTIME_TOKENS),
    ...(env.AGENT_RUNTIME_TOKEN ? [env.AGENT_RUNTIME_TOKEN] : []),
  ];

  const backend = env.AGENT_RUNTIME_BACKEND === "pi" ? "pi" : "fake";

  return {
    host: env.AGENT_RUNTIME_HOST ?? "127.0.0.1",
    port: parseIntOr(env.AGENT_RUNTIME_PORT, 8787),
    tokens,
    verboseDefault: parseVerbose(env.AGENT_RUNTIME_VERBOSE_DEFAULT, "off"),
    allowRequestSessionKey: env.AGENT_RUNTIME_ALLOW_REQUEST_SESSION_KEY === "true",
    allowedSessionKeyPrefixes: parseList(env.AGENT_RUNTIME_ALLOWED_SESSION_KEY_PREFIXES),
    sseBufferSize: parseIntOr(env.AGENT_RUNTIME_SSE_BUFFER_SIZE, 512),
    backend,
  };
}
