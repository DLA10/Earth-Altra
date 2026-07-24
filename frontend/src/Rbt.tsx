import { useEffect, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import type { WsMessage } from "./types";

interface RbtPosition {
  symbol: string;
  direction: string;
  qty: number;
  entry_price: number;
  opened_at: string;
  target_price: number;
  stop_loss: number;
  age: number;
  last_px?: number; // backend mark (engine, else broker) — fallback when no quote streams
}

interface RbtTrade {
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

interface RbtReport {
  live: boolean;
  realized_pnl: number;
  unrealized_pnl: number;
  total_trades: number;
  win_rate: number;
  open_count: number;
  max_slots: number;
  cash: number;
  equity: number;
  positions: RbtPosition[] | null;
  trades: RbtTrade[] | null;
}

export function Rbt() {
  const [data, setData] = useState<{ enabled: boolean; report?: RbtReport } | null>(null);
  const [err, setErr] = useState("");
  // Live prices for positions updated via WebSockets (subsecond)
  const [livePrices, setLivePrices] = useState<Record<string, number>>({});
  const symbolsRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .rbt()
        .then((r) => {
          if (!alive) return;
          setData(r);
          if (r.report && r.report.positions) {
            const positions = r.report.positions as RbtPosition[];
            symbolsRef.current = new Set(positions.map((p) => p.symbol));
            // Seed marks from the backend for symbols with no live quote yet (adopted
            // or off-hours names would otherwise show a fake $0 P&L until a tick lands).
            setLivePrices((prev) => {
              const next = { ...prev };
              for (const p of positions) {
                if (next[p.symbol] === undefined && p.last_px && p.last_px > 0) {
                  next[p.symbol] = p.last_px;
                }
              }
              return next;
            });
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
        <h2>RBT — Rubber Band Trading Desk</h2>
        <p className="muted">{err ? `Error: ${err}` : "Loading state..."}</p>
      </div>
    );
  }

  if (!data.enabled) {
    return (
      <div className="quant-page">
        <h2>RBT — Rubber Band Trading Desk</h2>
        <p className="muted">
          RBT paper-trading is not enabled. Set <code>PAPER_RBT_KEY</code> and{" "}
          <code>PAPER_RBT_SECRET</code> in your <code>backend/.env</code> file.
        </p>
      </div>
    );
  }

  const rep = data.report;
  if (!rep) {
    return (
      <div className="quant-page">
        <h2>RBT — Rubber Band Trading Desk</h2>
        <p className="muted">Waiting for report data...</p>
      </div>
    );
  }

  const money = (v: number) => `${v < 0 ? "−" : "+"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");

  const getMarkPrice = (p: RbtPosition) => livePrices[p.symbol] || p.entry_price;
  const getUnrealizedPnL = (p: RbtPosition) => {
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
          RBT — Rubber Band Trading Desk{" "}
          <span className="muted" style={{ fontSize: "0.55em", marginLeft: 8 }}>
            Co-integration + GARCH + LightGBM Mean Reversion
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
          value={rep.win_rate > 0 ? `${rep.win_rate.toFixed(0)}%` : "0%"}
        />
        <Card
          label="Closed Trades"
          value={String(rep.total_trades)}
        />
        <Card
          label="Slots Allocated"
          value={`${rep.open_count} / ${rep.max_slots}`}
        />
      </div>

      {/* Strategy Description Callout */}
      <div className="attr-verdict" style={{ marginBottom: 20 }}>
        💡 <b>How RBT works:</b> <b>Engle-Granger co-integration</b> groups stocks into co-moving clusters.
        A <b>GARCH(1,1)</b> volatility model adjusts position entry thresholds (Z-spreads) dynamically.
        A <b>LightGBM Classifier</b> filters signals, selecting entries with &ge; 65% probability.
        Exits are governed by group mean-reversion targets, a 1.5&times; ATR stop loss, or a strict 5-day hold age.
      </div>

      {/* Active Positions */}
      <div className="panel">
        <div className="panel-title">Active Positions ({openPositions.length})</div>
        {openPositions.length === 0 ? (
          <p className="muted" style={{ padding: 12 }}>
            No active positions. The pipeline runs scanning daily at 15:50 ET to execute close-reversion trades.
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
                <th>Target (Mean)</th>
                <th>Stop Loss (1.5x ATR)</th>
                <th>Age</th>
              </tr>
            </thead>
            <tbody>
              {openPositions.map((p) => {
                const uPnL = getUnrealizedPnL(p);
                return (
                  <tr key={p.symbol}>
                    <td>
                      <b className="mono-strong">{p.symbol}</b>
                    </td>
                    <td>
                      <span className={`direction-badge ${p.direction.toLowerCase()}`}>
                        {p.direction === "Long" ? "🟢 Long" : "🔴 Short"}
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
                    <td>{p.age}d</td>
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
                <th>Date</th>
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
                const dateStr = new Date(t.closed_at).toLocaleDateString();
                return (
                  <tr key={idx}>
                    <td>{dateStr}</td>
                    <td>
                      <b className="mono-strong">{t.symbol}</b>
                    </td>
                    <td>
                      <span className={`direction-badge ${t.direction.toLowerCase()}`}>
                        {t.direction === "Long" ? "🟢 Long" : "🔴 Short"}
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
                          ? "🎯 Target Reverted"
                          : t.reason === "stop_loss"
                          ? "🛑 Stop Loss Hit"
                          : "🕒 Time Out (5d)"}
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
