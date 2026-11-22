import type { ClientMessage, ServerMessage } from "@argus/protocol";
import { DEFAULT_BACKOFF } from "./backoff";
import { Emitter } from "./emitter";
import { ArgusSocket, type ArgusSocketOptions, type SocketStatus } from "./ws";

export interface ConnectionInfo {
  status: SocketStatus;
  /** Reconnect attempts since the last healthy connection (0 while open). */
  attempt: number;
  /** Delay before the next attempt while reconnecting, else null. */
  nextDelayMs: number | null;
}

const IDLE: ConnectionInfo = { status: "idle", attempt: 0, nextDelayMs: null };

interface ConnectionEvents extends Record<string, unknown> {
  status: ConnectionInfo;
  message: ServerMessage;
}

/** Owns the console's WebSocket: reconnects with backoff and republishes status and messages. */
export class ConnectionManager extends Emitter<ConnectionEvents> {
  private sock: ArgusSocket | null = null;
  private epoch = 0;
  private current: ConnectionInfo = IDLE;

  get info(): ConnectionInfo {
    return this.current;
  }

  private publish(next: ConnectionInfo): void {
    this.current = next;
    this.emit("status", next);
  }

  /**
   * Open the connection, replacing any existing one.
   * @param wsUrl WebSocket URL, or a function returning a fresh one for each (re)connect
   * @param scenario scenario pack to start
   * @param streams stream ids to subscribe to
   * @param options extra socket options (tests inject a fake WebSocket and timers)
   */
  connect(
    url: ArgusSocketOptions["url"],
    scenario: string,
    streamIds: string[],
    options: Partial<ArgusSocketOptions> = {},
  ): void {
    this.disconnect();
    const epoch = ++this.epoch;
    const s = new ArgusSocket({
      ...options,
      url,
      scenario,
      streamIds,
      // Retries at most 5 times with exponential backoff, then reports "closed".
      backoff: DEFAULT_BACKOFF,
    });
    this.sock = s;
    s.on("status", (status) => {
      if (epoch !== this.epoch) return;
      const { attempt, nextDelayMs } = this.current;
      this.publish(
        status === "open"
          ? { status, attempt: 0, nextDelayMs: null }
          : { status, attempt, nextDelayMs: status === "reconnecting" ? nextDelayMs : null },
      );
    });
    s.on("reconnecting", (r) => {
      if (epoch !== this.epoch) return;
      this.publish({ status: "reconnecting", attempt: r.attempt, nextDelayMs: r.delayMs });
    });
    s.on("message", (m) => {
      if (epoch === this.epoch) this.emit("message", m);
    });
    s.connect();
  }

  /** Close the connection and stop reconnecting. No-op when nothing is open. */
  disconnect(): void {
    const s = this.sock;
    if (!s) return;
    this.epoch++;
    this.sock = null;
    s.close();
    this.publish({ status: "closed", attempt: 0, nextDelayMs: null });
  }

  /** Returns false when offline or the message is invalid. */
  send(msg: ClientMessage | Record<string, unknown>): boolean {
    return this.sock?.send(msg) ?? false;
  }
}

/** The app-wide connection, used by `useSession` and `ConnectionBadge`. */
export const connectionManager = new ConnectionManager();
