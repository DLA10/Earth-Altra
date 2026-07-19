import { useEffect, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import type { WsMessage } from "./types";

interface BcPosition {
  symbol: string;
  qty: number;
  entry_price: number;
  opened_at: string;
  target_price: number;
  stop_loss: number;
  peak: number;
  armed: boolean;
  adopted: boolean;
  prob: number;
}

interface BcTrade {
  symbol: string;
  qty: number;
  entry_price: number;
  exit_price: number;
  pnl: number;
  reason: string;
  opened_at: string;
  closed_at: string;
}

interface BcReport {
  live: boolean;
  budget: number;
  deployed: number;
  budget_free: number;
  notional: number;
  max_slots: number;
  open_count: number;
  universe_size: number;
  universe: string[];
  model_trained: string;
  cash: number;
  equity: number;
  buying_power: number;
  account_day_pnl: number;
  realized_pnl: number;
  unrealized_pnl: number;
  total_trades: number;
  win_rate: number;
  exit: { tp_pct: number; sl_pct: number; trail_pct: number; lock: boolean };
  positions: BcPosition[] | null;
  trades: BcTrade[] | null;
}

export function Breadcrumbs() {
  const [data, setData] = useState<{ enabled: boolean; report?: BcReport } | null>(null);
  const [err, setErr] = useState("");
  const [livePrices, setLivePrices] = useState<Record<string, number>>({});
  const symbolsRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .breadcrumbs()
        .then((r) => {
          if (!alive) return;
          setData(r);
          if (r.report && r.report.positions) {
            symbolsRef.current = new Set(r.report.positions.map((p: BcPosition) => p.symbol));
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

  // Sub-second position marking off the live quote stream (between 3s REST polls).
  useWebSocket((m: WsMessage) => {
    if (m.type === "quote" && symbolsRef.current.has(m.data.symbol)) {
      setLivePrices((prev) => ({ ...prev, [m.data.symbol]: m.data.price }));
    }
  });

  const title = "Breadcrumbs — Generalized Volatility Scalper";

  if (!data) {
    return (
      <div className="quant-page">
        <h2>{title}</h2>
        <p className="muted">{err ? `Error: ${err}` : "Loading state..."}</p>
      </div>
    );
  }
  if (!data.enabled) {
    return (
      <div className="quant-page">
        <h2>{title}</h2>
        <p className="muted">
          Breadcrumbs paper desk is not enabled. Set <code>PAPER_BREADCRUMBS_KEY</code> and{" "}
          <code>PAPER_BREADCRUMBS_SECRET</code> in <code>backend/.env</code> and restart.
        </p>
      </div>
    );
  }
  const rep = data.report;
  if (!rep) {
    return (
      <div className="quant-page">
        <h2>{title}</h2>
        <p className="muted">Waiting for report data...</p>
      </div>
    );
  }

  const money = (v: number) => `${v < 0 ? "−" : "+"}$${Math.abs(v).toFixed(2)}`;
  const usd = (v: number) => `$${v.toLocaleString(undefined, { minimumFractionDigits: 0, maximumFractionDigits: 0 })}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");

  const markPrice = (p: BcPosition) => livePrices[p.symbol] || p.entry_price;
  const uPnL = (p: BcPosition) => (markPrice(p) - p.entry_price) * p.qty;

  const positions = rep.positions || [];
  const trades = rep.trades || [];
  const liveUnrealized = positions.reduce((a, p) => a + uPnL(p), 0);
  const liveDeployed = positions.reduce((a, p) => a + markPrice(p) * p.qty, 0);
  const pctUsed = rep.budget > 0 ? Math.min(100, (rep.deployed / rep.budget) * 100) : 0;

  const reasonBadge = (reason: string) => {
    switch (reason) {
      case "target": return "🎯 Target";
      case "trail": return "📉 Trailing stop";
      case "stop_loss": return "🛑 Hard stop";
      case "catastrophic_stop": return "🛡️ Exchange stop";
      case "eod": return "🕔 EOD flat";
      case "reconcile_vanished": return "👻 Reconcile (ghost)";
      case "safety_exit": return "⚠️ Safety exit";
      default: return reason;
    }
  };

  return (
    <div className="quant-page animate-fade-in">
      <div className="quant-head">
        <h2>
          {title}{" "}
          <span className="muted" style={{ fontSize: "0.55em", marginLeft: 8 }}>
            Pooled LightGBM · 22 volatile names · 0.2% trail + profit-lock
          </span>
        </h2>
        <span className={`mode-badge ${rep.live ? "live" : "shadow"}`}>
          {rep.live ? "LIVE (paper)" : "SHADOW"}
        </span>
      </div>

      {/* KPI cards */}
      <div className="quant-cards">
        <Card label="Open P&L (live)" value={money(liveUnrealized)} tone={cls(liveUnrealized)} />
        <Card label="Day P&L (Alpaca)" value={money(rep.account_day_pnl)} tone={cls(rep.account_day_pnl)} />
        <Card label="Realized P&L" value={money(rep.realized_pnl)} tone={cls(rep.realized_pnl)} />
        <Card label="Equity" value={`$${rep.equity.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`} />
        <Card label="Win Rate" value={rep.win_rate > 0 ? `${rep.win_rate.toFixed(1)}%` : "0%"} />
        <Card label="Closed Trades" value={String(rep.total_trades)} />
        <Card label="Active / Slots" value={`${rep.open_count} / ${rep.max_slots}`} />
      </div>

      {/* Budget tracker — the scale guardrail, front and center */}
      <div className="panel" style={{ marginBottom: 16 }}>
        <div className="panel-title">Budget tracker (hard cap)</div>
        <div style={{ padding: "12px 14px" }}>
          <div style={{ display: "flex", justifyContent: "space-between", marginBottom: 6, fontSize: 13 }}>
            <span>Deployed <b>{usd(rep.deployed)}</b> of <b>{usd(rep.budget)}</b> budget</span>
            <span className="muted">Free {usd(rep.budget_free)} · Buying power {usd(rep.buying_power)}</span>
          </div>
          <div style={{ height: 14, background: "var(--panel-alt, #1a1f2b)", borderRadius: 7, overflow: "hidden" }}>
            <div
              style={{
                width: `${pctUsed}%`,
                height: "100%",
                background: pctUsed > 90 ? "#e5484d" : pctUsed > 70 ? "#f5a524" : "#30a46c",
                transition: "width 0.4s ease",
              }}
            />
          </div>
          <div className="muted" style={{ marginTop: 6, fontSize: 12 }}>
            {pctUsed.toFixed(1)}% of budget deployed · ${rep.notional.toLocaleString()}/trade ·
            live-marked exposure {usd(liveDeployed)} · every position reconciled against the broker every 30s
          </div>
        </div>
      </div>

      {/* How it works */}
      <div className="attr-verdict" style={{ marginBottom: 20 }}>
        💡 <b>How Breadcrumbs works:</b> the validated SNDK pipeline generalized to a{" "}
        <b>{rep.universe_size}-name high-volatility basket</b>. One pooled <b>LightGBM</b> (retrained
        monthly{rep.model_trained ? `, currently through ${rep.model_trained}` : ""}) scores each 1-minute
        close on 9 scale-free features. Entry when <b>prob ≥ 0.65</b>, price is above its EMA-100 trend,
        and within 2σ of VWAP. Exit rides a <b>{(rep.exit.trail_pct * 100).toFixed(1)}% trailing stop</b>
        {rep.exit.lock ? " with a profit-lock floored at" : " (no lock) targeting"} the{" "}
        <b>+{(rep.exit.tp_pct * 100).toFixed(2)}%</b> target, hard stop <b>−{(rep.exit.sl_pct * 100).toFixed(2)}%</b>,
        flat by the close. Multiple concurrent positions, capped by the budget above and reconciled against
        the account so nothing is ever left untracked.
      </div>

      {/* Active positions */}
      <div className="panel">
        <div className="panel-title">Active Positions ({positions.length})</div>
        {positions.length === 0 ? (
          <p className="muted" style={{ padding: 12 }}>
            No active positions. Scanning the {rep.universe_size}-name basket every minute during market hours.
          </p>
        ) : (
          <table className="q-table">
            <thead>
              <tr>
                <th>Symbol</th>
                <th>Qty</th>
                <th>Entry</th>
                <th>Current</th>
                <th>P&amp;L (live)</th>
                <th>Target</th>
                <th>Stop</th>
                <th>State</th>
                <th>Duration</th>
              </tr>
            </thead>
            <tbody>
              {positions.map((p) => {
                const u = uPnL(p);
                const minutes = Math.floor((Date.now() - new Date(p.opened_at).getTime()) / 60000);
                return (
                  <tr key={p.symbol}>
                    <td><b className="mono-strong">{p.symbol}</b></td>
                    <td>{p.qty.toLocaleString()}</td>
                    <td>${p.entry_price.toFixed(2)}</td>
                    <td>${markPrice(p).toFixed(2)}</td>
                    <td className={cls(u)}><b>{money(u)}</b></td>
                    <td>${p.target_price.toFixed(2)}</td>
                    <td>${p.stop_loss.toFixed(2)}</td>
                    <td>
                      {p.armed ? <span className="reason-badge target">🔒 trailing</span> : <span className="muted">waiting</span>}
                      {p.adopted ? <span className="reason-badge" style={{ marginLeft: 4 }}>adopted</span> : null}
                    </td>
                    <td>{minutes}m</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Closed trades */}
      <div className="panel">
        <div className="panel-title">Closed Trade History</div>
        {trades.length === 0 ? (
          <p className="muted" style={{ padding: 12 }}>No closed trades recorded yet.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr>
                <th>Time (Close)</th>
                <th>Symbol</th>
                <th>Qty</th>
                <th>Entry</th>
                <th>Exit</th>
                <th>P&amp;L</th>
                <th>Exit Reason</th>
              </tr>
            </thead>
            <tbody>
              {[...trades].reverse().map((t, idx) => (
                <tr key={idx}>
                  <td>{new Date(t.closed_at).toLocaleTimeString()} {new Date(t.closed_at).toLocaleDateString()}</td>
                  <td><b className="mono-strong">{t.symbol}</b></td>
                  <td>{t.qty.toLocaleString()}</td>
                  <td>${t.entry_price.toFixed(2)}</td>
                  <td>${t.exit_price.toFixed(2)}</td>
                  <td className={cls(t.pnl)}><b>{money(t.pnl)}</b></td>
                  <td><span className={`reason-badge ${t.reason}`}>{reasonBadge(t.reason)}</span></td>
                </tr>
              ))}
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
