import { useSyncExternalStore } from "react";
import { connectionManager } from "../lib/connection";

const subscribe = (fn: () => void) => connectionManager.on("status", fn);
const snapshot = () => connectionManager.info;

export function ConnectionBadge() {
  const info = useSyncExternalStore(subscribe, snapshot);
  return (
    <span className={`badge badge-${info.status}`} role="status">
      connection: {info.status}
      {info.status === "reconnecting" && info.nextDelayMs !== null
        ? ` (attempt ${info.attempt}, retry in ${(info.nextDelayMs / 1000).toFixed(1)}s)`
        : ""}
    </span>
  );
}
