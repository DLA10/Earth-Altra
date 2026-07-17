import { useEffect, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import type { WsMessage } from "./types";

interface SndkPosition {
  symbol: string;
  direction: string;
  qty: number;
  entry_price: number;
  opened_at: string;
  target_price: number;
  stop_loss: number;
  age_minutes: number;
}

interface SndkTrade {
  symbol: string;
  direction: string;
  qty: number;
  entry_price: number;
  exit_price: number;
  pnl: number;
  reason: string;
  opened_at: string;
  closed_at: string;
}

interface SndkReport {
  live: boolean;
  realized_pnl: number;
  unrealized_pnl: number;
  total_trades: number;
  win_rate: number;
  open_count: number;
  max_slots: number;
  cash: number;
  equity: number;
  positions: SndkPosition[] | null;
  trades: SndkTrade[] | null;
}

export function Sndk() {
  const [data, setData] = useState<{ enabled: boolean; report?: SndkReport } | null>(null);
  const [err, setErr] = useState("");
  // Live prices for positions updated via WebSockets (subsecond)
  const [livePrices, setLivePrices] = useState<Record<string, number>>({});
  const symbolsRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .sndk()
        .then((r) => {
          if (!alive) return;
          setData(r);
          if (r.report && r.report.positions) {
            symbolsRef.current = new Set(r.report.positions.map((p: SndkPosition) => p.symbol));
          }
          setErr("");
        })
        .catch((e) => alive && setErr(String(e)));

    load();
    const id = window.setInterval(load, 3000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, []);

  // Listen to live quotes and update current prices subsecond
  useWebSocket((m: WsMessage) => {
    if (m.type === "quote" && symbolsRef.current.has(m.data.symbol)) {
      setLivePrices((prev) => ({ ...prev, [m.data.symbol]: m.data.price }));
    }
  });

  if (!data) {
    return (
      <div className="quant-page">
        <h2>SNDK — Intraday 1-Minute Scalping Desk</h2>
        <p className="muted">{err ? `Error: ${err}` : "Loading state..."}</p>
      </div>
    );
  }

  if (!data.enabled) {
    return (
      <div className="quant-page">
        <h2>SNDK — Intraday 1-Minute Scalping Desk</h2>
        <p className="muted">
          SNDK paper-trading is not enabled. Set <code>PAPER_RBT_KEY</code> and{" "}
          <code>PAPER_RBT_SECRET</code> in your <code>backend/.env</code> file.
        </p>
      </div>
    );
  }

  const rep = data.report;
  if (!rep) {
    return (
      <div className="quant-page">
        <h2>SNDK — Intraday 1-Minute Scalping Desk</h2>
        <p className="muted">Waiting for report data...</p>
      </div>
    );
  }

  const money = (v: number) => `${v < 0 ? "−" : "+"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");

  const getMarkPrice = (p: SndkPosition) => livePrices[p.symbol] || p.entry_price;
  const getUnrealizedPnL = (p: SndkPosition) => {
    const currentPrice = getMarkPrice(p);
    return p.direction === "Long"
      ? (currentPrice - p.entry_price) * p.qty
      : (p.entry_price - currentPrice) * p.qty;
  };

  const openPositions = rep.positions || [];
  const closedTrades = rep.trades || [];
  const liveUnrealizedPnL = openPositions.reduce((acc, p) => acc + getUnrealizedPnL(p), 0);
  const liveEquity = rep.cash + openPositions.reduce((acc, p) => acc + (getMarkPrice(p) * p.qty), 0);

  return (
    <div className="quant-page animate-fade-in">
      <div className="quant-head">
        <h2>
          SNDK — Intraday 1-Minute Scalping Desk{" "}
          <span className="muted" style={{ fontSize: "0.55em", marginLeft: 8 }}>
            LightGBM Classifier + 1-Minute Z-Score + Time Filters
          </span>
        </h2>
        <span className={`mode-badge ${rep.live ? "live" : "shadow"}`}>
          {rep.live ? "LIVE (paper)" : "SHADOW"}
        </span>
      </div>

      {/* KPI Cards */}
      <div className="quant-cards">
        <Card
          label="Open P&L (live)"
          value={money(liveUnrealizedPnL)}
          tone={cls(liveUnrealizedPnL)}
        />
        <Card
          label="Realized P&L"
          value={money(rep.realized_pnl)}
          tone={cls(rep.realized_pnl)}
        />
        <Card
          label="Total Balance (Equity)"
          value={`$${liveEquity.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`}
        />
        <Card
          label="Win Rate"
          value={rep.win_rate > 0 ? `${rep.win_rate.toFixed(1)}%` : "0%"}
        />
        <Card
          label="Closed Trades"
          value={String(rep.total_trades)}
        />
        <Card
          label="Active Trades / Slots"
          value={`${rep.open_count} / ${rep.max_slots}`}
        />
      </div>

      {/* Strategy Description Callout */}
      <div className="attr-verdict" style={{ marginBottom: 20 }}>
        💡 <b>How SNDK works:</b> A high-frequency model tuned specifically for <b>SNDK</b>. 
        Features include MACD Histogram, Bollinger Band Z-score, EMA Z-score, RSI, and Volume. 
        A <b>LightGBM Classifier</b> scores each 1-minute close. Entries trigger on &ge; 65% probability of hitting a +$8.00 target first. 
        Stop loss is fixed at -$8.00, with a strict 5-minute timeout. Skips entries during lunch-hour (11:30 AM to 1:30 PM ET).
      </div>

      {/* Active Positions */}
      <div className="panel">
        <div className="panel-title">Active Positions ({openPositions.length})</div>
        {openPositions.length === 0 ? (
          <p className="muted" style={{ padding: 12 }}>
            No active positions. The pipeline runs scanning every 1 minute during market hours to execute micro-scalps.
          </p>
        ) : (
          <table className="q-table">
            <thead>
              <tr>
                <th>Symbol</th>
                <th>Direction</th>
                <th>Qty</th>
                <th>Entry Price</th>
                <th>Current Price</th>
                <th>P&amp;L (live)</th>
                <th>Target (+$8.00)</th>
                <th>Stop Loss (-$8.00)</th>
                <th>Duration</th>
              </tr>
            </thead>
            <tbody>
              {openPositions.map((p) => {
                const uPnL = getUnrealizedPnL(p);
                // Calculate time elapsed
                const minutes = Math.floor((new Date().getTime() - new Date(p.opened_at).getTime()) / 60000);
                return (
                  <tr key={p.symbol}>
                    <td>
                      <b className="mono-strong">{p.symbol}</b>
                    </td>
                    <td>
                      <span className="direction-badge long">
                        🟢 Long
                      </span>
                    </td>
                    <td>{p.qty.toLocaleString()}</td>
                    <td>${p.entry_price.toFixed(2)}</td>
                    <td>${getMarkPrice(p).toFixed(2)}</td>
                    <td className={cls(uPnL)}>
                      <b>{money(uPnL)}</b>
                    </td>
                    <td>${p.target_price.toFixed(2)}</td>
                    <td>${p.stop_loss.toFixed(2)}</td>
                    <td>{minutes}m / 5m</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Closed Trades */}
      <div className="panel">
        <div className="panel-title">Closed Trade History</div>
        {closedTrades.length === 0 ? (
          <p className="muted" style={{ padding: 12 }}>No closed trades recorded yet.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr>
                <th>Time (Close)</th>
                <th>Symbol</th>
                <th>Direction</th>
                <th>Qty</th>
                <th>Entry Price</th>
                <th>Exit Price</th>
                <th>PnL</th>
                <th>Exit Reason</th>
              </tr>
            </thead>
            <tbody>
              {[...closedTrades].reverse().map((t, idx) => {
                const dateStr = new Date(t.closed_at).toLocaleTimeString() + " " + new Date(t.closed_at).toLocaleDateString();
                return (
                  <tr key={idx}>
                    <td>{dateStr}</td>
                    <td>
                      <b className="mono-strong">{t.symbol}</b>
                    </td>
                    <td>
                      <span className="direction-badge long">
                        🟢 Long
                      </span>
                    </td>
                    <td>{t.qty.toLocaleString()}</td>
                    <td>${t.entry_price.toFixed(2)}</td>
                    <td>${t.exit_price.toFixed(2)}</td>
                    <td className={cls(t.pnl)}>
                      <b>{money(t.pnl)}</b>
                    </td>
                    <td>
                      <span className={`reason-badge ${t.reason}`}>
                        {t.reason === "target"
                          ? "🎯 Target Hit (+$8.00)"
                          : t.reason === "stop_loss"
                          ? "🛑 Stop Loss Hit (-$8.00)"
                          : t.reason === "catastrophic_stop"
                          ? "🛡️ Protective Stop hit"
                          : "🕒 Time Out (5m)"}
                      </span>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function Card({ label, value, tone = "" }: { label: string; value: string; tone?: string }) {
  return (
    <div className="q-card">
      <div className="q-card-label">{label}</div>
      <div className={`q-card-value ${tone}`}>{value}</div>
    </div>
  );
}
