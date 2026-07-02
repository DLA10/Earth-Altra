import { useCallback, useEffect, useState } from "react";
import { api } from "./api/client";
import type { Activity } from "./types";

const fmtTime = (iso: string) =>
  new Date(iso).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });

// TradeHistory shows the account's fill log, pulled live from Alpaca's authoritative
// activity API (no local storage — Alpaca persists it).
export function TradeHistory() {
  const [fills, setFills] = useState<Activity[]>([]);
  const [days, setDays] = useState(30);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState("");

  const load = useCallback(() => {
    setLoading(true);
    setErr("");
    api
      .activities(days, 100)
      .then((rows) => setFills(rows))
      .catch((e) => setErr((e as Error).message))
      .finally(() => setLoading(false));
  }, [days]);

  useEffect(() => {
    load();
  }, [load]);

  const totalValue = fills.reduce((a, f) => a + f.value, 0);

  return (
    <div className="history">
      <div className="history-head">
        <div>
          <h1 className="history-title">Trade History</h1>
          <p className="history-sub">
            Fills from Alpaca's account activity log · {fills.length} executions
          </p>
        </div>
        <div className="history-controls">
          <select value={days} onChange={(e) => setDays(Number(e.target.value))}>
            <option value={1}>Last 1 day</option>
            <option value={7}>Last 7 days</option>
            <option value={30}>Last 30 days</option>
            <option value={90}>Last 90 days</option>
          </select>
          <button onClick={load} disabled={loading}>
            {loading ? "Loading…" : "↻ Refresh"}
          </button>
        </div>
      </div>

      {err && <div className="error-box">{err}</div>}

      {!loading && fills.length === 0 && !err && (
        <div className="empty">No fills in this window. Trades you execute will appear here.</div>
      )}

      {fills.length > 0 && (
        <table className="data-table history-table">
          <thead>
            <tr>
              <th>Time</th>
              <th>Symbol</th>
              <th>Side</th>
              <th>Qty</th>
              <th>Price</th>
              <th>Value</th>
              <th>Type</th>
            </tr>
          </thead>
          <tbody>
            {fills.map((f) => (
              <tr key={f.id}>
                <td>{fmtTime(f.time)}</td>
                <td className="mono-strong">{f.symbol}</td>
                <td className={f.side === "buy" ? "pos" : "neg"}>{f.side.toUpperCase()}</td>
                <td>{f.qty}</td>
                <td>${f.price.toFixed(2)}</td>
                <td>${f.value.toFixed(2)}</td>
                <td className="muted">{f.type.replace("_", " ")}</td>
              </tr>
            ))}
          </tbody>
          <tfoot>
            <tr>
              <td colSpan={5} className="muted">Total notional executed</td>
              <td>${totalValue.toFixed(2)}</td>
              <td></td>
            </tr>
          </tfoot>
        </table>
      )}
    </div>
  );
}
