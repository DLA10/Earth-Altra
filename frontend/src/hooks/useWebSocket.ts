import { useCallback, useEffect, useRef, useState } from "react";
import type { WsMessage } from "../types";

type Status = "connecting" | "open" | "closed";

interface UseWebSocket {
  status: Status;
  send: (obj: unknown) => void;
  last: WsMessage | null;
}

// useWebSocket maintains a single resilient WS connection with auto-reconnect.
// Consumers read `last` (the most recent message) and react via effects/handlers.
export function useWebSocket(onMessage: (m: WsMessage) => void): UseWebSocket {
  const [status, setStatus] = useState<Status>("connecting");
  const [last, setLast] = useState<WsMessage | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const reconnectRef = useRef<number | null>(null);
  // Once the consumer unmounts, no further reconnect may be scheduled. Without this
  // flag, cleanup's close() fires onclose AFTER cleanup ran, which schedules a fresh
  // reconnect nothing cancels — every unmounted chart/popup leaked a zombie socket
  // that reconnected forever and kept receiving all quote broadcasts.
  const disposedRef = useRef(false);
  const onMessageRef = useRef(onMessage);
  onMessageRef.current = onMessage;

  const connect = useCallback(() => {
    if (disposedRef.current) return;
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(`${proto}://${location.host}/ws`);
    wsRef.current = ws;
    setStatus("connecting");

    ws.onopen = () => setStatus("open");
    ws.onclose = () => {
      if (disposedRef.current) return;
      setStatus("closed");
      // Reconnect after a short delay.
      reconnectRef.current = window.setTimeout(connect, 1500);
    };
    ws.onerror = () => ws.close();
    ws.onmessage = (ev) => {
      try {
        const msg = JSON.parse(ev.data) as WsMessage;
        setLast(msg);
        onMessageRef.current(msg);
      } catch {
        /* ignore malformed frames */
      }
    };
  }, []);

  useEffect(() => {
    disposedRef.current = false;
    connect();
    return () => {
      disposedRef.current = true;
      if (reconnectRef.current) window.clearTimeout(reconnectRef.current);
      wsRef.current?.close();
    };
  }, [connect]);

  const send = useCallback((obj: unknown) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }, []);

  return { status, send, last };
}
