/**
 * Shared contracts for the agent runtime orchestration layer.
 *
 * The `AgentBackend` interface is the seam described in the backend dev-design
 * (§1.3 / §7.1): the HTTP layer, verbose projection and SSE replay all sit on
 * top of this interface and never touch the pi kernel directly. Route ① (an
 * in-process `createAgentSession`) and route ② (a `./rpc-entry` subprocess) are
 * two implementations of the same contract; the deterministic test backend is a
 * third. Swapping any of them leaves the HTTP/verbose/SSE layers untouched.
 */

/** Normalized event emitted by every backend, projected from the pi event stream. */
export type BackendEvent =
  /** Assistant text delta (typewriter). Maps pi `message_update` -> `text_delta`. */
  | { type: "text_delta"; delta: string }
  /** Reasoning/thinking delta. Maps pi `message_update` -> `thinking_delta`. */
  | { type: "thinking_delta"; delta: string }
  /** Tool call started. Maps pi `tool_execution_start`. */
  | { type: "tool_start"; toolCallId: string; toolName: string; args: unknown }
  /** Tool call finished. Maps pi `tool_execution_end`. */
  | { type: "tool_end"; toolCallId: string; toolName: string; result: unknown; isError: boolean }
  /** Context compaction lifecycle. Maps pi `compaction_start` / `compaction_end`. */
  | { type: "compaction"; phase: "start" | "end"; reason: "manual" | "threshold" | "overflow" }
  /** Auto-retry lifecycle. Maps pi `auto_retry_start` / `auto_retry_end`. */
  | { type: "auto_retry"; phase: "start" | "end"; attempt: number }
  /** Final assistant message for a turn. Maps pi `message_end` / `agent_end`. */
  | { type: "message_end"; text: string; usage?: Usage }
  /** Turn settled — synchronous mode ends aggregation here. Maps pi `agent_settled`. */
  | { type: "settled" }
  /** Terminal error surfaced by the kernel. */
  | { type: "error"; message: string };

/** Token accounting, mirrors pi `Usage` (ai/src/types.ts). */
export interface Usage {
  input: number;
  output: number;
  cacheRead?: number;
  cacheWrite?: number;
  reasoning?: number;
  totalTokens: number;
  cost?: { input: number; output: number; total: number };
}

/** Point-in-time session state, mirrors pi `RpcSessionState` (rpc-types.ts:94-107). */
export interface SessionState {
  sessionId: string;
  sessionName?: string;
  model?: string;
  thinkingLevel: string;
  isStreaming: boolean;
  isCompacting: boolean;
  messageCount: number;
  pendingMessageCount: number;
}

export interface PromptOptions {
  timeoutSeconds?: number;
}

/** Unsubscribe handle returned by `subscribe`. */
export type Unsubscribe = () => void;

/**
 * A backend owns one resident agent session. `prompt` is asynchronous in pi:
 * it resolves once preflight succeeds; real output arrives through `subscribe`
 * (backend dev-design §3.2). Callers aggregate to `settled` for sync mode or
 * stream the projected events for SSE mode.
 */
export interface AgentBackend {
  readonly sessionId: string;
  subscribe(listener: (event: BackendEvent) => void): Unsubscribe;
  prompt(message: string, options?: PromptOptions): Promise<void>;
  abort(): Promise<void>;
  getState(): SessionState;
  dispose(): Promise<void>;
}

/** Factory used by the HTTP layer to lazily create per-session backends. */
export type BackendFactory = (sessionKey: string, ownerId: string) => Promise<AgentBackend>;
