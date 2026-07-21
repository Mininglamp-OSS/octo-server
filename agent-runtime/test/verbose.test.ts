import { describe, expect, it } from "vitest";
import { buildTrace, project } from "../src/verbose.js";
import type { BackendEvent } from "../src/types.js";

const toolStart: BackendEvent = {
  type: "tool_start",
  toolCallId: "c1",
  toolName: "read_file",
  args: { path: "/etc/hostname" },
};
const toolEnd: BackendEvent = { type: "tool_end", toolCallId: "c1", toolName: "read_file", result: "octo", isError: false };
const textDelta: BackendEvent = { type: "text_delta", delta: "hi" };
const thinking: BackendEvent = { type: "thinking_delta", delta: "hmm" };
const messageEnd: BackendEvent = { type: "message_end", text: "done", usage: { input: 1, output: 2, totalTokens: 3 } };

describe("verbose projection (§4)", () => {
  it("off: keeps only final text + usage, drops tools/text/thinking", () => {
    expect(project(toolStart, "off")).toBeNull();
    expect(project(toolEnd, "off")).toBeNull();
    expect(project(textDelta, "off")).toBeNull();
    expect(project(thinking, "off")).toBeNull();
    expect(project(messageEnd, "off")).toEqual({ type: "message_end", text: "done", usage: { input: 1, output: 2, totalTokens: 3 } });
  });

  it("on: adds a human-readable tool summary and text deltas, still no raw args or thinking", () => {
    const start = project(toolStart, "on");
    expect(start).toEqual({ type: "tool_start", toolCallId: "c1", summary: "read_file(/etc/hostname)" });
    expect(start).not.toHaveProperty("args");
    expect(project(toolEnd, "on")).toEqual({ type: "tool_end", toolCallId: "c1", isError: false });
    expect(project(textDelta, "on")).toEqual({ type: "text_delta", delta: "hi" });
    expect(project(thinking, "on")).toBeNull();
  });

  it("full: passes raw tool args/result and thinking through", () => {
    expect(project(toolStart, "full")).toEqual({
      type: "tool_start",
      toolCallId: "c1",
      toolName: "read_file",
      args: { path: "/etc/hostname" },
    });
    expect(project(toolEnd, "full")).toEqual({
      type: "tool_end",
      toolCallId: "c1",
      toolName: "read_file",
      result: "octo",
      isError: false,
    });
    expect(project(thinking, "full")).toEqual({ type: "thinking_delta", delta: "hmm" });
  });

  it("buildTrace is empty for off and excludes terminal frames otherwise", () => {
    const events = [thinking, toolStart, toolEnd, textDelta, messageEnd, { type: "settled" } as BackendEvent];
    expect(buildTrace(events, "off")).toEqual([]);
    const onTrace = buildTrace(events, "on");
    expect(onTrace.some((f) => f.type === "message_end")).toBe(false);
    expect(onTrace.some((f) => f.type === "settled")).toBe(false);
    expect(onTrace.some((f) => f.type === "tool_start")).toBe(true);
    const fullTrace = buildTrace(events, "full");
    expect(fullTrace.some((f) => f.type === "thinking_delta")).toBe(true);
  });
});
