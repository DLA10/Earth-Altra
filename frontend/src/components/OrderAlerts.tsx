import { useEffect, useRef, useState } from "react";
import { useWebSocket } from "../hooks/useWebSocket";
import type { WsMessage } from "../types";

// OrderAlerts shows a brief (~2s) animation whenever one of YOUR live orders fills, partially
// fills, cancels, or is rejected — anywhere in the portal, on any tab. It opens its own
// WebSocket so it receives the account's trade_update stream regardless of which page is
// mounted. Read-only / display-only: it never places or changes any order.

interface Alert {
  id: number;
  icon: string;
  title: string;
  detail: string;
  tone: "buy" | "sell" | "warn" | "bad";
}

// Map a trade_update event → a styled alert (or null to ignore noisy lifecycle events like
// "new"/"accepted", which the user already saw at the confirm step).
function toAlert(d: { event: string; side: string; symbol: string; qty: string; price: number }): Omit<Alert, "id"> | null {
  const px = d.price > 0 ? ` @ $${d.price.toFixed(2)}` : "";
  const sz = `${d.qty} ${d.symbol}`;
  switch (d.event) {
    case "fill":
      return d.side === "buy"
        ? { icon: "↑", title: "Buy filled", detail: `${sz}${px}`, tone: "buy" }
        : { icon: "↓", title: "Sell filled", detail: `${sz}${px}`, tone: "sell" };
    case "partial_fill":
      return { icon: "◐", title: `Partial ${d.side}`, detail: `${sz}${px}`, tone: d.side === "buy" ? "buy" : "sell" };
    case "canceled":
      return { icon: "✕", title: "Order canceled", detail: sz, tone: "warn" };
    case "expired":
      return { icon: "⌛", title: "Order expired", detail: sz, tone: "warn" };
    case "rejected":
      return { icon: "⚠", title: "Order rejected", detail: sz, tone: "bad" };
    default:
      return null; // new / accepted / pending_* / replaced — too noisy to animate
  }
}

export function OrderAlerts() {
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const nextId = useRef(1);

  const onMessage = (m: WsMessage) => {
    if (m.type !== "trade_update") return;
    const a = toAlert(m.data);
    if (!a) return;
    const id = nextId.current++;
    setAlerts((prev) => [...prev, { ...a, id }]);
    window.setTimeout(() => setAlerts((prev) => prev.filter((x) => x.id !== id)), 2400);
  };

  // Keep one stable handler so the WS isn't torn down on every render.
  const handlerRef = useRef(onMessage);
  handlerRef.current = onMessage;
  useWebSocket((m) => handlerRef.current(m));

  useEffect(() => () => setAlerts([]), []);

  return (
    <div className="order-alerts">
      {alerts.map((a) => (
        <div key={a.id} className={`order-alert ${a.tone}`}>
          <span className="oa-icon">{a.icon}</span>
          <div className="oa-text">
            <span className="oa-title">{a.title}</span>
            <span className="oa-detail">{a.detail}</span>
          </div>
        </div>
      ))}
    </div>
  );
}
