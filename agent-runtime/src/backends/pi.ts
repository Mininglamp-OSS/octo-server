/**
 * Route ① backend (backend dev-design §1.3, §5): in-process pi `AgentSession`.
 *
 * This is a thin adapter that (a) creates a resident session via
 * `createAgentSession` (sdk.ts:164), (b) translates pi's `AgentSessionEvent`
 * stream (agent-session.ts:136-162, plus the underlying `AgentEvent` and
 * streaming `AssistantMessageEvent`) into the normalized `BackendEvent` set, and
 * (c) maps prompt/abort/state onto the session. The pi kernel is unmodified.
 *
 * pi is an optional dependency and is loaded lazily, so the rest of the runtime
 * (and the whole test suite, which uses the deterministic backend) does not need
 * the kernel installed. Running this backend for real requires a configured model
 * provider (auth.json / models.json under the pi agent dir); without one,
 * `createAgentSession` reports no available model. pi's event objects are kept as
 * `unknown`/narrowed locally so this file type-checks whether or not pi is present.
 */
import type { AgentBackend, BackendEvent, PromptOptions, SessionState, Unsubscribe, Usage } from "../types.js";

interface PiSession {
  subscribe(listener: (event: PiEvent) => void): () => void;
  prompt(text: string, options?: unknown): Promise<void>;
  abort(): Promise<void>;
  waitForIdle?(): Promise<void>;
  dispose(): void;
  getSessionId?(): string;
}

type PiEvent = { type: string; [k: string]: any };

/** Extract concatenated assistant text from a pi AgentMessage. */
function textOf(message: any): string {
  const content = message?.content;
  if (!Array.isArray(content)) return typeof message?.content === "string" ? message.content : "";
  return content
    .filter((c: any) => c && c.type === "text" && typeof c.text === "string")
    .map((c: any) => c.text)
    .join("");
}

function usageOf(message: any): Usage | undefined {
  const u = message?.usage;
  if (!u || typeof u !== "object") return undefined;
  return {
    input: u.input ?? 0,
    output: u.output ?? 0,
    cacheRead: u.cacheRead,
    cacheWrite: u.cacheWrite,
    reasoning: u.reasoning,
    totalTokens: u.totalTokens ?? (u.input ?? 0) + (u.output ?? 0),
    cost: u.cost ? { input: u.cost.input, output: u.cost.output, total: u.cost.total } : undefined,
  };
}

/** Translate one pi event into zero or one normalized backend events. */
export function translatePiEvent(event: PiEvent): BackendEvent | null {
  switch (event.type) {
    case "agent_settled":
      return { type: "settled" };
    case "agent_end": {
      const messages: any[] = event.messages ?? [];
      const last = [...messages].reverse().find((m) => m?.role === "assistant");
      if (!last) return null;
      return { type: "message_end", text: textOf(last), usage: usageOf(last) };
    }
    case "tool_execution_start":
      return { type: "tool_start", toolCallId: event.toolCallId, toolName: event.toolName, args: event.args };
    case "tool_execution_end":
      return {
        type: "tool_end",
        toolCallId: event.toolCallId,
        toolName: event.toolName,
        result: event.result,
        isError: !!event.isError,
      };
    case "compaction_start":
      return { type: "compaction", phase: "start", reason: event.reason };
    case "compaction_end":
      return { type: "compaction", phase: "end", reason: event.reason };
    case "auto_retry_start":
      return { type: "auto_retry", phase: "start", attempt: event.attempt ?? 0 };
    case "auto_retry_end":
      return { type: "auto_retry", phase: "end", attempt: event.attempt ?? 0 };
    case "message_update": {
      const ame = event.assistantMessageEvent;
      if (!ame) return null;
      if (ame.type === "text_delta") return { type: "text_delta", delta: ame.delta ?? "" };
      if (ame.type === "thinking_delta") return { type: "thinking_delta", delta: ame.delta ?? "" };
      if (ame.type === "error") return { type: "error", message: ame.error?.errorMessage ?? "kernel error" };
      return null;
    }
    default:
      return null;
  }
}

export interface PiBackendOptions {
  cwd?: string;
  /** When true, use a non-persistent in-memory session (§5.1). */
  inMemory?: boolean;
}

export class PiAgentBackend implements AgentBackend {
  readonly sessionId: string;
  private readonly session: PiSession;
  private streaming = false;
  private messageCount = 0;

  private constructor(sessionId: string, session: PiSession) {
    this.sessionId = sessionId;
    this.session = session;
  }

  /** Create a resident in-process pi session (route ①). */
  static async create(sessionKey: string, options: PiBackendOptions = {}): Promise<PiAgentBackend> {
    const mod: any = await import("@earendil-works/pi-coding-agent");
    const createAgentSession = mod.createAgentSession;
    const SessionManager = mod.SessionManager;
    const sessionManager =
      options.inMemory && SessionManager?.inMemory ? SessionManager.inMemory(options.cwd) : undefined;
    const result = await createAgentSession({ cwd: options.cwd, sessionManager });
    const session: PiSession = result.session;
    const sessionId = session.getSessionId?.() ?? sessionKey;
    return new PiAgentBackend(sessionId, session);
  }

  subscribe(listener: (event: BackendEvent) => void): Unsubscribe {
    return this.session.subscribe((raw: PiEvent) => {
      if (raw.type === "agent_settled" || raw.type === "agent_end") this.streaming = false;
      const translated = translatePiEvent(raw);
      if (translated) listener(translated);
    });
  }

  async prompt(message: string, _options?: PromptOptions): Promise<void> {
    this.streaming = true;
    this.messageCount += 1;
    await this.session.prompt(message);
  }

  async abort(): Promise<void> {
    await this.session.abort();
    this.streaming = false;
  }

  getState(): SessionState {
    return {
      sessionId: this.sessionId,
      thinkingLevel: "medium",
      isStreaming: this.streaming,
      isCompacting: false,
      messageCount: this.messageCount,
      pendingMessageCount: 0,
    };
  }

  async dispose(): Promise<void> {
    try {
      await this.session.waitForIdle?.();
    } catch {
      // best-effort drain
    }
    this.session.dispose();
  }
}
