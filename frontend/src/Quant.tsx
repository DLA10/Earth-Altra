import { useEffect, useState } from "react";
import { api } from "./api/client";
import type { QuantReport } from "./types";

// Quant renders the dip-driven AI team on the Claude paper account: the deterministic detector →
// Agent 2 (entry, Opus) → shared-budget allocator → Agent 3 (exit, Haiku), plus Agent 4
// (sentiment, local) and the daily review. All P&L is realized-only.
export function Quant() {
  const [rep, setRep] = useState<QuantReport | null>(null);
  const [enabled, setEnabled] = useState(true);
  const [err, setErr] = useState("");

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .quant()
        .then((r) => {
          if (!alive) return;
          setEnabled(r.enabled);
          setRep(r.report ?? null);
          setErr("");
        })
        .catch((e) => alive && setErr(String(e)));
    load();
    const id = window.setInterval(load, 5000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, []);

  if (!enabled) {
    return (
      <div className="quant-page">
        <h2>Paper Trade · Claude — AI Quant Team</h2>
        <p className="muted">
          The quant pipeline is not enabled. Set <code>PAPER_CLAUDE_KEY/SECRET</code> and{" "}
          <code>ANTHROPIC_API_KEY</code>, then run the pre-market session ("Run Instruction.md") to
          create today's universe.
        </p>
      </div>
    );
  }
  if (!rep) {
    return (
      <div className="quant-page">
        <h2>Paper Trade · Claude — AI Quant Team</h2>
        <p className="muted">{err || "Loading…"}</p>
      </div>
    );
  }

  const s = rep.state;
  const a = rep.alloc;
  const att = rep.attribution;
  // Go marshals nil slices/maps as JSON null, so guard these before reading .length / mapping.
  const positions = s.positions ?? [];
  const trades = s.trades ?? [];
  const byReason = att.by_reason ?? {};
  const money = (v: number) => `${v >= 0 ? "+" : "−"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");

  return (
    <div className="quant-page">
      <div className="quant-head">
        <h2>Paper Trade · Claude — AI Quant Team</h2>
        <span className={`mode-badge ${rep.live ? "live" : "shadow"}`}>{rep.live ? "LIVE (paper)" : "SHADOW"}</span>
        {rep.posture && <span className="posture-badge">{rep.posture}</span>}
        <span className="muted small">universe: {rep.universe_size} symbols</span>
      </div>

      {/* Headline cards (realized-only) */}
      <div className="quant-cards">
        <Card label="Realized P&L" value={money(s.realized_pnl)} cls={cls(s.realized_pnl)} />
        <Card label="Unrealized (open)" value={money(s.unrealized_pnl)} cls={cls(s.unrealized_pnl)} />
        <Card label="Win rate" value={`${s.win_rate.toFixed(0)}%`} />
        <Card label="Closed trades" value={String(s.total_trades)} />
        <Card label="Open / Max" value={`${a.open_count} / ${a.max_concurrent}`} />
        <Card label="Budget free" value={`$${a.free.toFixed(0)} / $${a.budget.toFixed(0)}`} />
      </div>

      {/* The team */}
      <div className="quant-agents">
        <AgentChip name="Detector" role="dip + bounce (deterministic)" />
        <AgentChip name="Agent 2 · Entry" role="Opus — buy/no-buy" />
        <AgentChip name="Allocator" role={`shared budget · ${a.open_count}/${a.max_concurrent} slots`} />
        <AgentChip name="Agent 3 · Exit" role="Haiku — trailing stop + verbs" />
        <AgentChip name="Agent 4 · Sentiment" role="local gemma2:2b" />
      </div>

      {/* Agent-3 value attribution */}
      <div className="panel">
        <div className="panel-title">Agent 3 — does it add value? (realized P&L by exit)</div>
        {s.total_trades === 0 ? (
          <p className="muted">No closed trades yet.</p>
        ) : (
          <>
            <table className="q-table">
              <thead>
                <tr><th>Exit reason</th><th>Trades</th><th>Total P&L</th><th>Avg P&L</th></tr>
              </thead>
              <tbody>
                {Object.entries(byReason).map(([reason, st]) => (
                  <tr key={reason}>
                    <td>{reason}</td>
                    <td>{st.count}</td>
                    <td className={cls(st.total_pnl)}>{money(st.total_pnl)}</td>
                    <td className={cls(st.avg_pnl)}>{money(st.avg_pnl)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <div className="attr-verdict">
              Discretionary exits (AI_Exit / Take_Profit): <b className={cls(att.discretionary_avg_pnl)}>{money(att.discretionary_avg_pnl)}</b> avg ({att.discretionary_count}) ·
              {" "}Stop exits: <b className={cls(att.stop_avg_pnl)}>{money(att.stop_avg_pnl)}</b> avg ({att.stop_count}) →{" "}
              <b className={att.agent3_adds_value ? "pos" : "neg"}>
                {att.discretionary_count === 0 ? "no discretionary exits yet" : att.agent3_adds_value ? "Agent 3 is adding value" : "stop-only would've done better"}
              </b>
            </div>
          </>
        )}
      </div>

      {/* Open positions */}
      <div className="panel">
        <div className="panel-title">Open positions</div>
        {positions.length === 0 ? (
          <p className="muted">Flat.</p>
        ) : (
          <table className="q-table">
            <thead><tr><th>Symbol</th><th>Qty</th><th>Entry</th><th>Mark</th><th>Unrealized</th></tr></thead>
            <tbody>
              {positions.map((p) => (
                <tr key={p.symbol}>
                  <td className="mono-strong">{p.symbol}</td>
                  <td>{p.qty}</td>
                  <td>${p.entry_price.toFixed(2)}</td>
                  <td>${p.mark_price.toFixed(2)}</td>
                  <td className={cls(p.unrealized_pnl)}>{money(p.unrealized_pnl)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Closed trades */}
      <div className="panel">
        <div className="panel-title">Closed trades (realized)</div>
        {trades.length === 0 ? (
          <p className="muted">None yet.</p>
        ) : (
          <table className="q-table">
            <thead><tr><th>Symbol</th><th>Qty</th><th>Entry</th><th>Exit</th><th>P&L</th><th>Reason</th></tr></thead>
            <tbody>
              {[...trades].reverse().map((t, i) => (
                <tr key={i}>
                  <td className="mono-strong">{t.symbol}</td>
                  <td>{t.qty}</td>
                  <td>${t.entry_price.toFixed(2)}</td>
                  <td>${t.exit_price.toFixed(2)}</td>
                  <td className={cls(t.pnl)}>{money(t.pnl)}</td>
                  <td>{t.exit_reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Latest daily review */}
      {rep.review && (
        <div className="panel">
          <div className="panel-title">
            Daily review — {rep.review.date} · consistency {rep.review.consistency_score}/10
          </div>
          <p>{rep.review.summary}</p>
          {rep.review.what_worked?.length > 0 && (
            <div className="rv-block"><b>What worked</b><ul>{rep.review.what_worked.map((x, i) => <li key={i}>{x}</li>)}</ul></div>
          )}
          {rep.review.what_didnt?.length > 0 && (
            <div className="rv-block"><b>What didn't</b><ul>{rep.review.what_didnt.map((x, i) => <li key={i}>{x}</li>)}</ul></div>
          )}
          {rep.review.suggested_changes?.length > 0 && (
            <div className="rv-block"><b>Suggested changes (need your approval)</b><ul>{rep.review.suggested_changes.map((x, i) => <li key={i}>{x}</li>)}</ul></div>
          )}
        </div>
      )}
    </div>
  );
}

function Card({ label, value, cls }: { label: string; value: string; cls?: string }) {
  return (
    <div className="q-card">
      <div className="q-card-label">{label}</div>
      <div className={`q-card-value ${cls ?? ""}`}>{value}</div>
    </div>
  );
}

function AgentChip({ name, role }: { name: string; role: string }) {
  return (
    <div className="agent-chip">
      <div className="agent-name">{name}</div>
      <div className="agent-role">{role}</div>
    </div>
  );
}
