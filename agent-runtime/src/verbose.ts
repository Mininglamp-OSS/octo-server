/**
 * Verbose three-tier projection (backend dev-design §4). Orchestration-layer
 * concern: reads the normalized backend event stream and decides which SSE
 * frames survive per level. The pi kernel is never modified.
 *
 *   off  -> final assistant text + usage only
 *   on   -> + human-readable tool summaries (+ streaming text deltas) + status
 *   full -> + reasoning/thinking + raw tool args/result + full event stream
 *
 * `project` returns the frame to emit, or `null` to drop the event. The frame's
 * `type` travels inside the JSON payload (there is no separate SSE `event:` line;
 * the frontend parses `type` from the data — dev-design review §3, frame-shape note).
 */
import type { VerboseLevel } from "./config.js";
import type { BackendEvent } from "./types.js";

export type Frame = Record<string, unknown> & { type: string };

/** Compact, human-readable one-liner for a tool call (the `on` tier / "explain"). */
function toolSummary(toolName: string, args: unknown): string {
  if (args && typeof args === "object") {
    const obj = args as Record<string, unknown>;
    const hint =
      (typeof obj.path === "string" && obj.path) ||
      (typeof obj.file === "string" && obj.file) ||
      (typeof obj.command === "string" && obj.command) ||
      (typeof obj.pattern === "string" && obj.pattern) ||
      "";
    return hint ? `${toolName}(${String(hint)})` : toolName;
  }
  return toolName;
}

export function project(event: BackendEvent, level: VerboseLevel): Frame | null {
  switch (event.type) {
    // Final text + usage survive at every tier.
    case "message_end":
      return { type: "message_end", text: event.text, usage: event.usage };

    case "settled":
      return { type: "settled" };

    case "error":
      return { type: "error", message: event.message };

    // Tool lifecycle: summary at `on`, raw args/result at `full`, dropped at `off`.
    case "tool_start":
      if (level === "off") return null;
      if (level === "full") {
        return { type: "tool_start", toolCallId: event.toolCallId, toolName: event.toolName, args: event.args };
      }
      return { type: "tool_start", toolCallId: event.toolCallId, summary: toolSummary(event.toolName, event.args) };

    case "tool_end":
      if (level === "off") return null;
      if (level === "full") {
        return {
          type: "tool_end",
          toolCallId: event.toolCallId,
          toolName: event.toolName,
          result: event.result,
          isError: event.isError,
        };
      }
      return { type: "tool_end", toolCallId: event.toolCallId, isError: event.isError };

    // Streaming text: only in streaming contexts; kept at `on` and `full`.
    case "text_delta":
      if (level === "off") return null;
      return { type: "text_delta", delta: event.delta };

    // Reasoning/thinking: only at `full`.
    case "thinking_delta":
      if (level !== "full") return null;
      return { type: "thinking_delta", delta: event.delta };

    // Status events: shown from `on` upward.
    case "compaction":
      if (level === "off") return null;
      return { type: "compaction", phase: event.phase, reason: event.reason };

    case "auto_retry":
      if (level === "off") return null;
      return { type: "auto_retry", phase: event.phase, attempt: event.attempt };

    default: {
      const _exhaustive: never = event;
      return _exhaustive;
    }
  }
}

/**
 * Synchronous-mode trace: the projected non-terminal frames for a completed turn,
 * used to populate `data.trace` (§4.3). `off` yields an empty trace.
 */
export function buildTrace(events: BackendEvent[], level: VerboseLevel): Frame[] {
  if (level === "off") return [];
  const trace: Frame[] = [];
  for (const event of events) {
    if (event.type === "message_end" || event.type === "settled") continue;
    const frame = project(event, level);
    if (frame) trace.push(frame);
  }
  return trace;
}
