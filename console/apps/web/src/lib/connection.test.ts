import { describe, expect, it } from "vitest";
import { ConnectionManager, type ConnectionInfo } from "./connection";
import type { WebSocketLike } from "./ws";

class FakeWS implements WebSocketLike {
  static instances: FakeWS[] = [];
  readyState = 0;
  onopen: WebSocketLike["onopen"] = null;
  onmessage: WebSocketLike["onmessage"] = null;
  onclose: WebSocketLike["onclose"] = null;
  onerror: WebSocketLike["onerror"] = null;
  sent: string[] = [];
  constructor(public url: string) {
    FakeWS.instances.push(this);
  }
  send(d: string) {
    this.sent.push(d);
  }
  close() {
    this.readyState = 3;
  }
  open() {
    this.readyState = 1;
    this.onopen?.({});
  }
  drop() {
    this.readyState = 3;
    this.onclose?.({});
  }
}

function mk() {
  FakeWS.instances = [];
  const timers: Array<() => void> = [];
  const m = new ConnectionManager();
  const seen: ConnectionInfo[] = [];
  m.on("status", (i) => seen.push(i));
  const connect = () =>
    m.connect("ws://x", "s", ["cam1"], {
      WebSocketImpl: FakeWS,
      rand: () => 0,
      setTimeoutImpl: (fn) => timers.push(fn),
      clearTimeoutImpl: () => {},
    });
  return { m, seen, timers, connect };
}

describe("ConnectionManager", () => {
  it("publishes status transitions and retry info", () => {
    const { m, seen, timers, connect } = mk();
    connect();
    expect(m.info.status).toBe("connecting");
    FakeWS.instances[0]!.open();
    expect(m.info).toEqual({ status: "open", attempt: 0, nextDelayMs: null });
    FakeWS.instances[0]!.drop();
    expect(m.info).toEqual({ status: "reconnecting", attempt: 1, nextDelayMs: 500 });
    timers.shift()!();
    FakeWS.instances[1]!.drop();
    expect(m.info).toMatchObject({ status: "reconnecting", attempt: 2, nextDelayMs: 1000 });
    expect(seen.length).toBeGreaterThan(3);
  });

  it("disconnect stops reconnecting and ignores the old socket", () => {
    const { m, timers, connect } = mk();
    connect();
    FakeWS.instances[0]!.open();
    m.disconnect();
    expect(m.info.status).toBe("closed");
    FakeWS.instances[0]!.drop();
    expect(timers).toHaveLength(0);
    expect(m.send({ type: "control.barge_in" })).toBe(false);
  });

  it("replaces an existing connection", () => {
    const { m, connect } = mk();
    connect();
    connect();
    expect(FakeWS.instances).toHaveLength(2);
    FakeWS.instances[1]!.open();
    expect(m.send({ type: "control.barge_in" })).toBe(true);
  });
});
