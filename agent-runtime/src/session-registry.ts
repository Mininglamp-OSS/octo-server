/**
 * Resident session registry (backend dev-design §5.2). pi has no server-side
 * session pool; the orchestration layer maps `sessionKey -> { backend, ownerId,
 * ring, ... }`. `POST /v1/agent` reuses by key or creates on demand. Cross-session
 * reads are owner-scoped here (the pre-SQLite form of the §5.5 queryService ACL:
 * default-deny, only the owning token can see a session).
 */
import type { RuntimeConfig } from "./config.js";
import { SseBuffer } from "./sse.js";
import type { AgentBackend, BackendEvent, BackendFactory, SessionState, Usage } from "./types.js";

/** Fan-out entry: a raw backend event plus the id it was assigned by the ring (or null if dropped). */
export interface RingNotification {
  id: number | null;
  event: BackendEvent;
}

/**
 * A resident session. Exactly one connection drives a turn (calls `prompt` and
 * appends to the ring); reconnecting observers replay from the ring and tail live
 * appends via `listeners` without re-appending, so event-ids stay single-sourced.
 */
export class SessionRecord {
  readonly sessionKey: string;
  readonly ownerId: string;
  readonly backend: AgentBackend;
  readonly ring: SseBuffer;
  readonly createdAt: number;
  /** True while a turn is actively producing events. */
  streaming = false;
  /** Last settled turn result, used to deliver `done` to reconnects after settle. */
  lastResult: { text: string; usage?: Usage } | undefined;
  private readonly listeners = new Set<(n: RingNotification) => void>();

  constructor(sessionKey: string, ownerId: string, backend: AgentBackend, ring: SseBuffer) {
    this.sessionKey = sessionKey;
    this.ownerId = ownerId;
    this.backend = backend;
    this.ring = ring;
    this.createdAt = Date.now();
  }

  addListener(fn: (n: RingNotification) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  notify(n: RingNotification): void {
    for (const fn of [...this.listeners]) fn(n);
  }
}

export class SessionRegistry {
  private readonly sessions = new Map<string, SessionRecord>();
  private readonly factory: BackendFactory;
  private readonly config: RuntimeConfig;
  /** Serialize create-by-key so two concurrent requests don't double-create (§7.3). */
  private readonly pending = new Map<string, Promise<SessionRecord>>();

  constructor(config: RuntimeConfig, factory: BackendFactory) {
    this.config = config;
    this.factory = factory;
  }

  /** Default resident session key for a token/owner when none is supplied (§3.1.1). */
  defaultKeyFor(ownerId: string): string {
    return `default:${ownerId}`;
  }

  /** Get or create the session for `sessionKey`, owned by `ownerId`. */
  async getOrCreate(sessionKey: string, ownerId: string): Promise<SessionRecord> {
    const existing = this.sessions.get(sessionKey);
    if (existing) return existing;

    const inFlight = this.pending.get(sessionKey);
    if (inFlight) return inFlight;

    const promise = (async () => {
      const backend = await this.factory(sessionKey, ownerId);
      const record = new SessionRecord(sessionKey, ownerId, backend, new SseBuffer(this.config.sseBufferSize));
      this.sessions.set(sessionKey, record);
      return record;
    })();
    this.pending.set(sessionKey, promise);
    try {
      return await promise;
    } finally {
      this.pending.delete(sessionKey);
    }
  }

  /** Owner-scoped lookup (default-deny): only the owning token sees the session. */
  get(sessionKey: string, ownerId: string): SessionRecord | undefined {
    const record = this.sessions.get(sessionKey);
    if (!record || record.ownerId !== ownerId) return undefined;
    return record;
  }

  /** List sessions visible to `ownerId` (§3.1 GET /v1/sessions filter). */
  list(ownerId: string): Array<{ sessionKey: string; createdAt: number; state: SessionState }> {
    const out: Array<{ sessionKey: string; createdAt: number; state: SessionState }> = [];
    for (const record of this.sessions.values()) {
      if (record.ownerId !== ownerId) continue;
      out.push({ sessionKey: record.sessionKey, createdAt: record.createdAt, state: record.backend.getState() });
    }
    return out.sort((a, b) => a.createdAt - b.createdAt);
  }

  async disposeAll(): Promise<void> {
    const all = [...this.sessions.values()];
    this.sessions.clear();
    await Promise.allSettled(all.map((r) => r.backend.dispose()));
  }
}
