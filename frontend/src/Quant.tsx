import { useEffect, useState } from "react";
import { api } from "./api/client";
import type { QuantReport, Scoreboard, SourceStat } from "./types";

// Quant renders the dip-driven AI team on the Claude paper account: the deterministic detector →
// Agent 2 (entry, Opus) → shared-budget allocator → Agent 3 (exit, Haiku), plus Agent 4
// (sentiment, local) and the daily review. All P&L is realized-only.
export function Quant() {
  const [rep, setRep] = useState<QuantReport | null>(null);
  const [enabled, setEnabled] = useState(true);
  const [err, setErr] = useState("");
  const [scoreboard, setScoreboard] = useState<Scoreboard | null>(null);

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

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .evals()
        .then((sb) => {
          if (!alive) return;
          setScoreboard(sb.enabled === false ? null : sb);
        })
        .catch(() => {
          /* scoreboard is best-effort; keep last known value on error */
        });
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

      {/* Budget vs real account equity (the allocator is capped at real cash) */}
      <div className="attr-verdict" style={{ marginBottom: 12 }}>
        Allocator budget <b>${a.budget.toFixed(0)}</b>
        {a.account_equity > 0 ? (
          <> · capped at real paper-account equity <b>${a.account_equity.toFixed(0)}</b>
            {a.configured_max > a.account_equity && <span className="neg"> (target ${a.configured_max.toFixed(0)} trimmed to fit the account)</span>}
          </>
        ) : (
          <span className="muted"> · account equity not yet synced</span>
        )}
        {" · "}deployed <b>${a.deployed.toFixed(0)}</b>
      </div>

      {/* Team P&L by pipeline — which half of the desk is actually earning (rolling window,
          forward from when source-tagging began). */}
      {rep.dip_score && (
        <div className="panel">
          <div className="panel-title">Team P&L by pipeline (rolling {rep.dip_score.window_days}d)</div>
          <table className="q-table">
            <thead>
              <tr><th>Pipeline</th><th>Trades</th><th>Win rate</th><th>Realized P&L</th><th>Avg / trade</th></tr>
            </thead>
            <tbody>
              <PipelineRow label="Dip pipeline · Agent 2" st={rep.dip_score.dip} money={money} cls={cls} />
              <PipelineRow label="Signal engine · 6 strategies + ML gate" st={rep.dip_score.signal} money={money} cls={cls} />
              {rep.dip_score.rehydrated.trades > 0 && (
                <PipelineRow label="Rehydrated (post-restart, origin unknown)" st={rep.dip_score.rehydrated} money={money} cls={cls} faint />
              )}
            </tbody>
          </table>
          <div className="attr-verdict muted">
            The dip watcher (Telegram alerts) itself places no trades — only the dip pipeline (Agent 2) does.
          </div>
        </div>
      )}

      {/* The team — models are the ACTUAL configured ones (from the backend), so this can't
          drift out of sync with what's really running. */}
      <div className="quant-agents">
        {(rep.agents ?? []).map((ag) => (
          <AgentChip key={ag.name} name={ag.name} model={ag.model} role={ag.role} live={ag.live} />
        ))}
      </div>

      {/* Strategy scoreboard (rolling 20d) */}
      <div className="panel">
        <div className="panel-title">Strategy scoreboard (rolling 20d)</div>
        {!scoreboard || !(scoreboard.strategies ?? []).length ? (
          <p className="muted">collecting data…</p>
        ) : (
          <>
            <table className="q-table">
              <thead>
                <tr><th>Strategy</th><th>Signals</th><th>Outcomes</th><th>Mean R</th><th>Traded</th><th>Status</th></tr>
              </thead>
              <tbody>
                {(scoreboard.strategies ?? []).map((row) => (
                  <tr key={row.strategy}>
                    <td className="mono-strong">{row.strategy}</td>
                    <td>{row.signals}</td>
                    <td>{row.outcomes}</td>
                    <td className={cls(row.mean_r)}>{row.mean_r.toFixed(2)}</td>
                    <td>{row.traded}</td>
                    <td className={row.demoted ? "neg" : "pos"}>
                      {row.demoted ? `DEMOTED (${row.reason})` : "active"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <div className="attr-verdict">
              {scoreboard.judge.joined < 10 ? (
                "Judge calibration: collecting data…"
              ) : (
                <>
                  Judge: {scoreboard.judge.decisions} decisions ({scoreboard.judge.approved} approved /{" "}
                  {scoreboard.judge.vetoed} vetoed) · veto value{" "}
                  <b className={cls(scoreboard.judge.veto_value_r)}>{scoreboard.judge.veto_value_r.toFixed(2)}R</b> ·
                  Brier {scoreboard.judge.brier.toFixed(3)}
                </>
              )}
            </div>
          </>
        )}
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

      {/* Dip-agent scorecard: is Agent 2 picking real bounces or catching knives? */}
      {rep.dip_score && (
        <div className="panel">
          <div className="panel-title">Dip agent scorecard — real bounces vs falling knives (rolling {rep.dip_score.window_days}d)</div>
          <table className="q-table">
            <thead>
              <tr><th>Entry pipeline</th><th>Trades</th><th>Win rate</th><th>Total P&L</th><th>Avg P&L</th></tr>
            </thead>
            <tbody>
              <tr>
                <td className="mono-strong">Dip (Agent 2)</td>
                <td>{rep.dip_score.dip.trades}</td>
                <td>{(rep.dip_score.dip.win_rate * 100).toFixed(0)}%</td>
                <td className={cls(rep.dip_score.dip.total_pnl)}>{money(rep.dip_score.dip.total_pnl)}</td>
                <td className={cls(rep.dip_score.dip.avg_pnl)}>{money(rep.dip_score.dip.avg_pnl)}</td>
              </tr>
              <tr>
                <td className="mono-strong">Signal engine (for comparison)</td>
                <td>{rep.dip_score.signal.trades}</td>
                <td>{(rep.dip_score.signal.win_rate * 100).toFixed(0)}%</td>
                <td className={cls(rep.dip_score.signal.total_pnl)}>{money(rep.dip_score.signal.total_pnl)}</td>
                <td className={cls(rep.dip_score.signal.avg_pnl)}>{money(rep.dip_score.signal.avg_pnl)}</td>
              </tr>
            </tbody>
          </table>
          <div className="attr-verdict">
            Agent 2 decisions: <b>{rep.dip_score.approved}</b> approved / <b>{rep.dip_score.rejected}</b> rejected
            {rep.dip_score.approved > 0 && <> · avg conviction <b>{rep.dip_score.avg_confidence.toFixed(2)}</b></>}
            {rep.dip_score.dip.trades > 0 && (
              <> · knife rate <b className={rep.dip_score.knife_rate > 0.6 ? "neg" : "pos"}>{(rep.dip_score.knife_rate * 100).toFixed(0)}%</b> (dip trades that lost)</>
            )}
            <div className={rep.dip_score.dip.total_pnl < 0 && rep.dip_score.dip.trades >= 5 ? "neg" : ""} style={{ marginTop: 4 }}>
              {rep.dip_score.verdict}
            </div>
          </div>
        </div>
      )}

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

function PipelineRow({
  label, st, money, cls, faint,
}: {
  label: string;
  st: SourceStat;
  money: (v: number) => string;
  cls: (v: number) => string;
  faint?: boolean;
}) {
  return (
    <tr className={faint ? "muted" : ""}>
      <td className="mono-strong">{label}</td>
      <td>{st.trades}</td>
      <td>{st.trades > 0 ? `${(st.win_rate * 100).toFixed(0)}%` : "—"}</td>
      <td className={cls(st.total_pnl)}>{st.trades > 0 ? money(st.total_pnl) : "—"}</td>
      <td className={cls(st.avg_pnl)}>{st.trades > 0 ? money(st.avg_pnl) : "—"}</td>
    </tr>
  );
}

function AgentChip({ name, model, role, live }: { name: string; model?: string; role: string; live?: boolean }) {
  return (
    <div className="agent-chip">
      <div className="agent-name">
        {name}
        {live === false && <span className="agent-off" title="not currently running"> · off</span>}
      </div>
      {model && <div className="agent-model">{model}</div>}
      <div className="agent-role">{role}</div>
    </div>
  );
}
