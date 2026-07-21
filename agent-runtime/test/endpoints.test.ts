import { afterEach, describe, expect, it } from "vitest";
import type { FastifyInstance } from "fastify";
import { buildApp } from "../src/app.js";
import { loadConfig } from "../src/config.js";
import { FakeAgentBackend } from "../src/backends/fake.js";

const TOKEN = "test-secret-token";
const AUTH = { authorization: `Bearer ${TOKEN}` };

function makeApp(env: Record<string, string> = {}): FastifyInstance {
  const config = loadConfig({ AGENT_RUNTIME_TOKEN: TOKEN, ...env } as NodeJS.ProcessEnv);
  return buildApp(config, { factory: async (key) => new FakeAgentBackend(key) });
}

let app: FastifyInstance | undefined;
afterEach(async () => {
  if (app) await app.close();
  app = undefined;
});

describe("endpoint contracts (§3)", () => {
  it("POST /v1/agent returns the {ok,data} envelope with result and usage", async () => {
    app = makeApp();
    const res = await app.inject({ method: "POST", url: "/v1/agent", headers: AUTH, payload: { message: "ping" } });
    expect(res.statusCode).toBe(200);
    const body = res.json();
    expect(body.ok).toBe(true);
    expect(body.data.result).toBe("echo: ping");
    expect(body.data.usage.totalTokens).toBe(59);
    expect(body.data.sessionKey).toMatch(/^default:/);
  });

  it("POST /v1/agent rejects an empty message with invalid_request", async () => {
    app = makeApp();
    const res = await app.inject({ method: "POST", url: "/v1/agent", headers: AUTH, payload: { message: "  " } });
    expect(res.statusCode).toBe(400);
    expect(res.json().error.code).toBe("invalid_request");
  });

  it("ignores a client sessionKey by default (allowRequestSessionKey=false)", async () => {
    app = makeApp();
    const res = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: AUTH,
      payload: { message: "ping", sessionKey: "attacker-owned" },
    });
    expect(res.statusCode).toBe(200);
    expect(res.json().data.sessionKey).toMatch(/^default:/);
    expect(res.json().data.sessionKey).not.toBe("attacker-owned");
  });

  it("honors a prefixed sessionKey when allowRequestSessionKey=true", async () => {
    app = makeApp({
      AGENT_RUNTIME_ALLOW_REQUEST_SESSION_KEY: "true",
      AGENT_RUNTIME_ALLOWED_SESSION_KEY_PREFIXES: "hook:",
    });
    const okRes = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: AUTH,
      payload: { message: "ping", sessionKey: "hook:abc" },
    });
    expect(okRes.json().data.sessionKey).toBe("hook:abc");

    const badRes = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: AUTH,
      payload: { message: "ping", sessionKey: "other:abc" },
    });
    expect(badRes.statusCode).toBe(400);
    expect(badRes.json().error.code).toBe("invalid_session_key");
  });

  it("GET /v1/sessions lists only the caller's sessions and GET /v1/sessions/:key maps state", async () => {
    app = makeApp();
    await app.inject({ method: "POST", url: "/v1/agent", headers: AUTH, payload: { message: "ping" } });

    const list = await app.inject({ method: "GET", url: "/v1/sessions", headers: AUTH });
    expect(list.statusCode).toBe(200);
    expect(list.json().data.sessions).toHaveLength(1);
    const key = list.json().data.sessions[0].sessionKey;

    const state = await app.inject({ method: "GET", url: `/v1/sessions/${encodeURIComponent(key)}`, headers: AUTH });
    expect(state.statusCode).toBe(200);
    expect(state.json().data.state.messageCount).toBe(1);
    expect(typeof state.json().data.state.isStreaming).toBe("boolean");
  });

  it("GET /v1/sessions/:key returns not_found for an unknown key", async () => {
    app = makeApp();
    const res = await app.inject({ method: "GET", url: "/v1/sessions/nope", headers: AUTH });
    expect(res.statusCode).toBe(404);
    expect(res.json().error.code).toBe("not_found");
  });

  it("POST /v1/sessions/:key/abort aborts a live session", async () => {
    app = makeApp();
    await app.inject({ method: "POST", url: "/v1/agent", headers: AUTH, payload: { message: "ping" } });
    const list = await app.inject({ method: "GET", url: "/v1/sessions", headers: AUTH });
    const key = list.json().data.sessions[0].sessionKey;
    const res = await app.inject({ method: "POST", url: `/v1/sessions/${encodeURIComponent(key)}/abort`, headers: AUTH });
    expect(res.statusCode).toBe(200);
    expect(res.json().data.aborted).toBe(true);
  });

  it("owner isolation: a different token cannot see another token's session", async () => {
    app = makeApp({ AGENT_RUNTIME_TOKENS: `${TOKEN},second-token` });
    await app.inject({ method: "POST", url: "/v1/agent", headers: AUTH, payload: { message: "ping" } });
    const otherList = await app.inject({
      method: "GET",
      url: "/v1/sessions",
      headers: { authorization: "Bearer second-token" },
    });
    expect(otherList.json().data.sessions).toHaveLength(0);
  });
});
