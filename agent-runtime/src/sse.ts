/**
 * Per-session SSE event-id, bounded ring buffer and reconnect replay (backend
 * dev-design §3.6). Entirely an orchestration-layer concern — pi's event stream
 * (session.subscribe) has no native event-id or replay.
 *
 * Contract:
 *  - Each surviving frame is assigned a session-scoped monotonic integer id from 1.
 *    Events dropped by verbose projection do NOT consume an id, so the client sees
 *    a gapless sequence at a fixed stream level.
 *  - The ring keeps the last `capacity` (raw event + assigned id) pairs. On
 *    reconnect the raw events are re-projected at the reconnect verbose level
 *    (which may differ from the first connection).
 *  - `Last-Event-ID: n` in the window -> resend (n, tail]. Slid out of the window
 *    -> a single `{type:"resync_required", from:<earliest available id>}` frame so
 *    the client reconciles via GET /v1/sessions/{key} instead of silently losing
 *    events.
 */
import type { VerboseLevel } from "./config.js";
import { project, type Frame } from "./verbose.js";
import type { BackendEvent } from "./types.js";

interface RingEntry {
  id: number;
  event: BackendEvent;
}

export interface EmittedFrame {
  id: number;
  frame: Frame;
}

export class SseBuffer {
  private readonly capacity: number;
  private entries: RingEntry[] = [];
  private nextId = 1;

  constructor(capacity: number) {
    this.capacity = Math.max(1, capacity);
  }

  /** Highest id assigned so far (0 when nothing emitted yet). */
  get maxId(): number {
    return this.nextId - 1;
  }

  /** Smallest id still retained in the ring (0 when empty). */
  get earliestId(): number {
    return this.entries.length > 0 ? this.entries[0]!.id : 0;
  }

  /**
   * Project `event` at `level`; if it survives, assign the next id, buffer the raw
   * event, and return the frame to write. Returns null for dropped events (no id
   * consumed). Buffering the raw event lets a later reconnect re-project it.
   */
  append(event: BackendEvent, level: VerboseLevel): EmittedFrame | null {
    const frame = project(event, level);
    if (!frame) return null;
    const id = this.nextId++;
    this.entries.push({ id, event });
    if (this.entries.length > this.capacity) {
      this.entries.shift(); // evict oldest; advances earliestId
    }
    return { id, frame };
  }

  /**
   * Compute what to send to a reconnecting client that last saw `lastEventId`,
   * re-projected at `level`.
   *  - `{ resync: frame }`   when the next expected id has slid out of the window.
   *  - `{ frames: [...] }`   the replayable tail (may be empty if already current).
   */
  replay(lastEventId: number, level: VerboseLevel): { resync?: Frame; frames?: EmittedFrame[] } {
    if (lastEventId >= this.maxId) {
      return { frames: [] }; // client already current; nothing was missed
    }
    // The next event the client needs is lastEventId + 1. If that id is no longer
    // retained (evicted, or buffer recycled), we cannot replay it losslessly.
    if (this.earliestId === 0 || lastEventId + 1 < this.earliestId) {
      const from = this.earliestId === 0 ? this.nextId : this.earliestId;
      return { resync: { type: "resync_required", from } };
    }
    const frames: EmittedFrame[] = [];
    for (const entry of this.entries) {
      if (entry.id <= lastEventId) continue;
      const frame = project(entry.event, level);
      if (frame) frames.push({ id: entry.id, frame });
    }
    return { frames };
  }

  /** Drop all buffered frames (called on TTL recycle after a settled turn). */
  clear(): void {
    this.entries = [];
  }
}

/** Serialize an emitted frame as an SSE record: `id:` line + `data:` line. */
export function serializeSse(id: number | null, frame: Frame): string {
  const lines: string[] = [];
  if (id !== null) lines.push(`id: ${id}`);
  lines.push(`data: ${JSON.stringify(frame)}`);
  return lines.join("\n") + "\n\n";
}
