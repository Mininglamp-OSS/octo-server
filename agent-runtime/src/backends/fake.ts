/**
 * Deterministic in-memory backend. Emits a pi-shaped event stream without any
 * model provider, so the HTTP contract, auth, verbose projection and SSE replay
 * are all exercisable end-to-end in tests and local smoke runs. This is the third
 * implementation of the `AgentBackend` seam (§1.3) alongside route ① (pi) / ② (rpc).
 *
 * The scripted turn mirrors a real one: a tool call, some streamed text, a bit of
 * thinking, then a final message with usage and `settled`.
 */
import type { AgentBackend, BackendEvent, PromptOptions, SessionState, Unsubscribe, Usage } from "../types.js";

const USAGE: Usage = {
  input: 42,
  output: 17,
  cacheRead: 0,
  cacheWrite: 0,
  totalTokens: 59,
  cost: { input: 0.00012, output: 0.00021, total: 0.00033 },
};

export class FakeAgentBackend implements AgentBackend {
  readonly sessionId: string;
  private listeners = new Set<(event: BackendEvent) => void>();
  private streaming = false;
  private messageCount = 0;
  private aborted = false;

  constructor(sessionId: string) {
    this.sessionId = sessionId;
  }

  subscribe(listener: (event: BackendEvent) => void): Unsubscribe {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  private emit(event: BackendEvent): void {
    for (const listener of [...this.listeners]) listener(event);
  }

  async prompt(message: string, _options?: PromptOptions): Promise<void> {
    // Resolves at preflight; events follow asynchronously (pi `prompt` semantics, §3.2).
    this.aborted = false;
    this.streaming = true;
    this.messageCount += 1;
    queueMicrotask(() => this.runTurn(message));
  }

  private runTurn(message: string): void {
    if (this.aborted) return;
    this.emit({ type: "thinking_delta", delta: "considering the request" });
    this.emit({
      type: "tool_start",
      toolCallId: "call_1",
      toolName: "read_file",
      args: { path: "/etc/hostname" },
    });
    this.emit({
      type: "tool_end",
      toolCallId: "call_1",
      toolName: "read_file",
      result: "octo-runtime",
      isError: false,
    });
    const reply = `echo: ${message}`;
    for (const chunk of reply.match(/.{1,5}/g) ?? [reply]) {
      if (this.aborted) return;
      this.emit({ type: "text_delta", delta: chunk });
    }
    this.streaming = false;
    this.emit({ type: "message_end", text: reply, usage: USAGE });
    this.emit({ type: "settled" });
  }

  async abort(): Promise<void> {
    this.aborted = true;
    this.streaming = false;
    this.emit({ type: "settled" });
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
    this.listeners.clear();
  }
}
