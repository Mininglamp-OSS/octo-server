import { afterEach, describe, expect, it } from "vitest";
import type { AddressInfo } from "node:net";
import type { FastifyInstance } from "fastify";
import { SseBuffer } from "../src/sse.js";
import type { BackendEvent } from "../src/types.js";
import { buildApp } from "../src/app.js";
import { loadConfig } from "../src/config.js";
import { FakeAgentBackend } from "../src/backends/fake.js";

const toolStart: BackendEvent = { type: "tool_start", toolCallId: "c1", toolName: "read_file", args: { path: "/x" } };
const thinking: BackendEvent = { type: "thinking_delta", delta: "t" };
const text = (delta: string): BackendEvent => ({ type: "text_delta", delta });

describe("SseBuffer event-id + replay contract (§3.6)", () => {
  it("assigns gapless monotonic ids and does not consume ids for dropped events", () => {
    const buf = new SseBuffer(512);
    // At level "on", thinking_delta is dropped and must NOT consume an id.
    expect(buf.append(thinking, "on")).toBeNull();
    const a = buf.append(toolStart, "on");
    const b = buf.append(text("hi"), "on");
    expect(a?.id).toBe(1);
    expect(b?.id).toBe(2);
    expect(buf.maxId).toBe(2);
  });

  it("replays exactly the (lastEventId, tail] window", () => {
    const buf = new SseBuffer(512);
    buf.append(text("a"), "on"); // id 1
    buf.append(text("b"), "on"); // id 2
    buf.append(text("c"), "on"); // id 3
    const { frames, resync } = buf.replay(1, "on");
    expect(resync).toBeUndefined();
    expect(frames?.map((f) => f.id)).toEqual([2, 3]);
    expect(frames?.map((f) => (f.frame as any).delta)).toEqual(["b", "c"]);
  });

  it("returns an empty replay when the client is already current (out-of-order / stale-high id)", () => {
    const buf = new SseBuffer(512);
    buf.append(text("a"), "on"); // id 1
    // Client claims to have seen id 5 though max is 1 (out-of-order reconnect): nothing to resend.
    expect(buf.replay(5, "on").frames).toEqual([]);
  });

  it("re-projects the replayed tail at the reconnect verbose level (full -> on)", () => {
    const buf = new SseBuffer(512);
    buf.append(toolStart, "full"); // id 1, raw args at full
    const { frames } = buf.replay(0, "on");
    expect(frames).toHaveLength(1);
    // Reconnecting at "on" must yield the compact summary, not the raw args.
    expect(frames?.[0]?.frame).toEqual({ type: "tool_start", toolCallId: "c1", summary: "read_file(/x)" });
    expect(frames?.[0]?.id).toBe(1);
  });

  it("emits resync_required when the requested id has slid out of the ring window", () => {
    const buf = new SseBuffer(3); // tiny window
    for (const d of ["a", "b", "c", "d", "e"]) buf.append(text(d), "on"); // ids 1..5, only 3..5 retained
    expect(buf.earliestId).toBe(3);
    const { resync, frames } = buf.replay(1, "on"); // id 2 was evicted
    expect(frames).toBeUndefined();
    expect(resync).toEqual({ type: "resync_required", from: 3 });
  });

  it("still replays when the requested id is the last retained boundary", () => {
    const buf = new SseBuffer(3);
    for (const d of ["a", "b", "c", "d"]) buf.append(text(d), "on"); // ids 1..4, retained 2..4
    const { frames, resync } = buf.replay(2, "on"); // next needed is 3, which is retained
    expect(resync).toBeUndefined();
    expect(frames?.map((f) => f.id)).toEqual([3, 4]);
  });
});

// ---- End-to-end SSE over a real socket: stream, disconnect, reconnect, replay ----

const TOKEN = "test-secret-token";
let app: FastifyInstance | undefined;
afterEach(async () => {
  if (app) await app.close();
  app = undefined;
});

function parseSse(text: string): Array<{ id?: number; data: any }> {
  const out: Array<{ id?: number; data: any }> = [];
  for (const record of text.split("\n\n")) {
    if (!record.trim()) continue;
    let id: number | undefined;
    let data: any;
    for (const line of record.split("\n")) {
      if (line.startsWith("id:")) id = Number.parseInt(line.slice(3).trim(), 10);
      else if (line.startsWith("data:")) data = JSON.parse(line.slice(5).trim());
    }
    out.push({ id, data });
  }
  return out;
}

describe("SSE end-to-end stream + reconnect replay (§3.6)", () => {
  it("streams a full turn with ids and a terminal done frame, then replays on reconnect", async () => {
    const config = loadConfig({ AGENT_RUNTIME_TOKEN: TOKEN } as NodeJS.ProcessEnv);
    app = buildApp(config, { factory: async (key) => new FakeAgentBackend(key) });
    await app.listen({ host: "127.0.0.1", port: 0 });
    const { port } = app.server.address() as AddressInfo;
    const base = `http://127.0.0.1:${port}`;

    // First connection: stream the turn at verbose=on.
    const res = await fetch(`${base}/v1/agent`, {
      method: "POST",
      headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" },
      body: JSON.stringify({ message: "hello", stream: true, verbose: "on" }),
    });
    expect(res.headers.get("content-type")).toContain("text/event-stream");
    const frames = parseSse(await res.text());
    const ids = frames.filter((f) => f.id !== undefined).map((f) => f.id!);
    // ids must be strictly increasing and gapless.
    expect(ids).toEqual(Array.from({ length: ids.length }, (_, i) => i + 1));
    expect(frames.some((f) => f.data.type === "tool_start")).toBe(true);
    expect(frames.some((f) => f.data.type === "text_delta")).toBe(true);
    const done = frames.at(-1)!;
    expect(done.data.type).toBe("done");
    expect(done.data.result).toBe("echo: hello");

    // Reconnect with Last-Event-ID after the turn settled: buffered tail is replayed.
    const midId = ids[Math.floor(ids.length / 2)]!;
    const replayRes = await fetch(`${base}/v1/agent`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${TOKEN}`,
        "content-type": "application/json",
        "last-event-id": String(midId),
      },
      body: JSON.stringify({ resume: true, stream: true, verbose: "on" }),
    });
    const replayFrames = parseSse(await replayRes.text());
    const replayedIds = replayFrames.filter((f) => f.id !== undefined).map((f) => f.id!);
    // Only ids strictly greater than midId are resent, in order.
    expect(replayedIds.every((id) => id > midId)).toBe(true);
    expect(replayedIds).toEqual([...replayedIds].sort((a, b) => a - b));
    expect(replayFrames.at(-1)!.data.type).toBe("done");
  });

  it("emits resync_required on reconnect when the buffer window has overflowed", async () => {
    const config = loadConfig({ AGENT_RUNTIME_TOKEN: TOKEN, AGENT_RUNTIME_SSE_BUFFER_SIZE: "2" } as NodeJS.ProcessEnv);
    app = buildApp(config, { factory: async (key) => new FakeAgentBackend(key) });
    await app.listen({ host: "127.0.0.1", port: 0 });
    const { port } = app.server.address() as AddressInfo;
    const base = `http://127.0.0.1:${port}`;

    await fetch(`${base}/v1/agent`, {
      method: "POST",
      headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" },
      body: JSON.stringify({ message: "hello world how are you", stream: true, verbose: "full" }),
    }).then((r) => r.text());

    // Ask to replay from id 1, which has long since been evicted from the 2-frame ring.
    const replayRes = await fetch(`${base}/v1/agent`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${TOKEN}`,
        "content-type": "application/json",
        "last-event-id": "1",
      },
      body: JSON.stringify({ resume: true, stream: true, verbose: "full" }),
    });
    const frames = parseSse(await replayRes.text());
    expect(frames.some((f) => f.data.type === "resync_required")).toBe(true);
  });
});
