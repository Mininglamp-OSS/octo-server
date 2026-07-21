import { afterEach, describe, expect, it } from "vitest";
import type { FastifyInstance } from "fastify";
import { buildApp } from "../src/app.js";
import { loadConfig } from "../src/config.js";
import { FakeAgentBackend } from "../src/backends/fake.js";

const TOKEN = "test-secret-token";

function makeApp(): FastifyInstance {
  const config = loadConfig({ AGENT_RUNTIME_TOKEN: TOKEN } as NodeJS.ProcessEnv);
  return buildApp(config, { factory: async (key) => new FakeAgentBackend(key) });
}

let app: FastifyInstance | undefined;
afterEach(async () => {
  if (app) await app.close();
  app = undefined;
});

describe("bearer auth (§2)", () => {
  it("returns 401 with unauthorized envelope when the token is missing", async () => {
    app = makeApp();
    const res = await app.inject({ method: "POST", url: "/v1/agent", payload: { message: "hi" } });
    expect(res.statusCode).toBe(401);
    const body = res.json();
    expect(body.ok).toBe(false);
    expect(body.error.code).toBe("unauthorized");
    expect(body.error.message).toBe("missing token");
  });

  it("returns 401 for a non-Bearer Authorization scheme", async () => {
    app = makeApp();
    const res = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: { authorization: "Basic Zm9vOmJhcg==" },
      payload: { message: "hi" },
    });
    expect(res.statusCode).toBe(401);
    expect(res.json().error.message).toBe("invalid scheme");
  });

  it("returns 401 for a token that is not in the allow set", async () => {
    app = makeApp();
    const res = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: { authorization: "Bearer wrong-token" },
      payload: { message: "hi" },
    });
    expect(res.statusCode).toBe(401);
    expect(res.json().error.message).toBe("invalid token");
  });

  it("accepts a valid Bearer token", async () => {
    app = makeApp();
    const res = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: { authorization: `Bearer ${TOKEN}` },
      payload: { message: "hi" },
    });
    expect(res.statusCode).toBe(200);
    expect(res.json().ok).toBe(true);
  });

  it("accepts the x-agent-token compatibility header", async () => {
    app = makeApp();
    const res = await app.inject({
      method: "POST",
      url: "/v1/agent",
      headers: { "x-agent-token": TOKEN },
      payload: { message: "hi" },
    });
    expect(res.statusCode).toBe(200);
    expect(res.json().ok).toBe(true);
  });

  it("does not require auth on /health", async () => {
    app = makeApp();
    const res = await app.inject({ method: "GET", url: "/health" });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ ok: true, data: { status: "ok", ready: true } });
  });
});
