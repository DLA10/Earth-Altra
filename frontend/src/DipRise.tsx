import { useEffect, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import type { DipRiseReport, DipRiseEvent, WsMessage } from "./types";

// DipRise renders the Dip+Rise desk: Agent 2 dip entries + the rise watcher, trading
// their own paper account. Open-position P&L is marked to the live quote stream
// (sub-second) between 3s REST refreshes; the timeline below shows the full story of
// every dip — detection → Agent 2 verdict → rise arm → trigger/expiry → funding → outcome.
export function DipRise() {
  const [rep, setRep] = useState<DipRiseReport | null>(null);
  const [err, setErr] = useState("");
  const [live, setLive] = useState<Record<string, number>>({});
  const [timelineAll, setTimelineAll] = useState(false); // false = hide "skip" chatter
  const symbolsRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .diprise()
        .then((r) => {
          if (!alive) return;
          setRep(r);
          symbolsRef.current = new Set((r.state?.positions ?? []).map((p) => p.symbol));
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

  useWebSocket((m: WsMessage) => {
    if (m.type === "quote" && symbolsRef.current.has(m.data.symbol)) {
      setLive((prev) => ({ ...prev, [m.data.symbol]: m.data.price }));
    }
  });

  if (!rep) {
    return (
      <div className="quant-page">
        <h2>Dip + Rise — Agent 2 &amp; Rise Watcher</h2>
        <p className="muted">{err ? `Error: ${err}` : "Loading…"}</p>
      </div>
    );
  }
  if (!rep.enabled) {
    return (
      <div className="quant-page">
        <h2>Dip + Rise — Agent 2 &amp; Rise Watcher</h2>
        <p className="muted">
          The Dip+Rise desk is not enabled — set <code>PAPER_DIP_KEY/SECRET</code> in{" "}
          <code>backend/.env</code> and restart. (It watches and journals either way; keys only
          gate order placement.)
        </p>
      </div>
    );
  }

  const s = rep.state;
  const a = rep.alloc;
  const positions = s.positions ?? [];
  const trades = s.trades ?? [];
  const money = (v: number) => `${v >= 0 ? "+" : "−"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");
  const mark = (sym: string, fallback: number) => live[sym] ?? fallback;
  const liveUnreal = positions.reduce(
    (sum, p) => sum + (mark(p.symbol, p.mark_price) - p.entry_price) * p.qty,
    0
  );

  return (
    <div className="quant-page">
      <div className="quant-head">
        <h2>
          Dip + Rise{" "}
          <span className="muted" style={{ fontSize: "0.6em" }}>
            Agent 2 judges dips · rise watcher buys confirmed bounces · own paper account
          </span>
        </h2>
        <span className={`mode-badge ${rep.live ? "live" : "shadow"}`}>
          dip: {rep.live ? "LIVE (paper)" : "SHADOW"}
        </span>
        <span className={`mode-badge ${rep.rise_live ? "live" : "shadow"}`}>
          rise: {rep.rise_live ? "LIVE (paper)" : "SHADOW"}
        </span>
      </div>

      <div className="quant-cards">
        <Card
          label="Open P&L (live)"
          value={positions.length === 0 ? "flat" : money(liveUnreal)}
          tone={positions.length === 0 ? "" : cls(liveUnreal)}
        />
        <Card label="Realized P&L" value={money(s.realized_pnl)} tone={cls(s.realized_pnl)} />
        <Card label="Win rate" value={s.total_trades > 0 ? `${s.win_rate.toFixed(0)}%` : "—"} />
        <Card label="Closed trades" value={String(s.total_trades)} />
        <Card label="Open / Max" value={`${a.open_count} / ${a.max_concurrent}`} />
        <Card label="Budget free" value={`$${a.free.toFixed(0)} / $${a.budget.toFixed(0)}`} />
      </div>

      <div className="attr-verdict" style={{ marginBottom: 12 }}>
        How this desk works: the Telegram dip watcher spots a dip+bounce → <b>Agent 2</b> says
        buy/no-buy → declined dips are <b>armed for 10 minutes</b> → a green 1-min close +0.10%
        above the dip price (dip low intact, volume holding) confirms the rise →{" "}
        <b>deterministic entry</b> (stop at the dip low, target +2R, 1.5% trail, 40-min max hold).
        {a.account_equity > 0 && (
          <> Account equity <b>${a.account_equity.toFixed(0)}</b>.</>
        )}
      </div>

      {/* Armed dips: what the rise watcher is stalking right now */}
      <div className="panel">
        <div className="panel-title">⏱ Armed — waiting for a confirmed rise ({rep.armed.length})</div>
        {rep.armed.length === 0 ? (
          <p className="muted">
            Nothing armed. Dips get armed here the moment Agent 2 declines them (10-minute window).
          </p>
        ) : (
          <table className="q-table">
            <thead>
              <tr>
                <th>Symbol</th><th>Dip price</th><th>Dip low (kill level)</th>
                <th>Confirms at</th><th>Now</th><th>Expires in</th><th>Agent 2 conf</th>
              </tr>
            </thead>
            <tbody>
              {rep.armed.map((x) => (
                <tr key={x.symbol}>
                  <td className="mono-strong">{x.symbol}</td>
                  <td>${x.dip_price.toFixed(2)}</td>
                  <td>${x.dip_low.toFixed(2)}</td>
                  <td>${x.confirm_level.toFixed(2)}</td>
                  <td>{live[x.symbol] ? `$${live[x.symbol].toFixed(2)}` : "—"}</td>
                  <td>{Math.floor(x.expires_in_sec / 60)}m{x.expires_in_sec % 60}s</td>
                  <td>{x.agent2_conf.toFixed(2)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Open positions — P&L ticks sub-second via the quote stream */}
      <div className="panel">
        <div className="panel-title">Open positions ({positions.length})</div>
        {positions.length === 0 ? (
          <p className="muted">Flat.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr><th>Symbol</th><th>Qty</th><th>Entry</th><th>Now</th><th>P&amp;L (live)</th></tr>
            </thead>
            <tbody>
              {positions.map((p) => {
                const px = mark(p.symbol, p.mark_price);
                const u = (px - p.entry_price) * p.qty;
                return (
                  <tr key={p.symbol}>
                    <td className="mono-strong">{p.symbol}</td>
                    <td>{p.qty}</td>
                    <td>${p.entry_price.toFixed(2)}</td>
                    <td>${px.toFixed(2)}</td>
                    <td className={cls(u)}><b>{money(u)}</b></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Scorecard: is Agent 2 catching bounces or knives; is the rise rule earning */}
      {rep.dip_score && (
        <div className="panel">
          <div className="panel-title">Scorecard (rolling {rep.dip_score.window_days}d)</div>
          <table className="q-table">
            <thead>
              <tr><th>Pipeline</th><th>Trades</th><th>Win rate</th><th>Total P&L</th><th>Avg / trade</th></tr>
            </thead>
            <tbody>
              <ScoreRow label="Dip entries (Agent 2 approved)" st={rep.dip_score.dip} money={money} cls={cls} />
              <ScoreRow label="Rise entries (confirmed bounces)" st={rep.dip_score.rise} money={money} cls={cls} />
            </tbody>
          </table>
          <div className="attr-verdict">
            Agent 2: <b>{rep.dip_score.approved}</b> approved / <b>{rep.dip_score.rejected}</b> rejected
            {rep.dip_score.approved > 0 && <> · avg conviction <b>{rep.dip_score.avg_confidence.toFixed(2)}</b></>}
            {rep.dip_score.dip.trades > 0 && (
              <> · knife rate{" "}
                <b className={rep.dip_score.knife_rate > 0.6 ? "neg" : "pos"}>
                  {(rep.dip_score.knife_rate * 100).toFixed(0)}%
                </b>
              </>
            )}
            <div style={{ marginTop: 4 }}>{rep.dip_score.verdict}</div>
          </div>
        </div>
      )}

      {/* Closed trades on this desk's account */}
      <div className="panel">
        <div className="panel-title">Closed trades (this account)</div>
        {trades.length === 0 ? (
          <p className="muted">None yet.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr><th>Closed</th><th>Held</th><th>Symbol</th><th>Qty</th><th>Entry</th><th>Exit</th><th>P&amp;L</th><th>Reason</th></tr>
            </thead>
            <tbody>
              {[...trades].reverse().map((t, i) => (
                <tr key={i}>
                  <td>{fmtTime(t.exit_time)}</td>
                  <td className="muted">{heldFor(t.entry_time, t.exit_time)}</td>
                  <td className="mono-strong">{t.symbol}</td>
                  <td>{t.qty}</td>
                  <td>${t.entry_price.toFixed(2)}</td>
                  <td>${t.exit_price.toFixed(2)}</td>
                  <td className={cls(t.pnl)}><b>{money(t.pnl)}</b></td>
                  <td className="muted">{t.exit_reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* The story: every dip, verdict, arm, trigger, expiry, funding, and outcome.
          Skip-chatter is hidden by default so the trades don't drown in it. */}
      <div className="panel">
        <div className="panel-title">
          Timeline — the last 2 days, newest first (
          {(rep.events ?? []).filter((ev) => timelineAll || ev.event !== "skip").length}
          {!timelineAll && ` of ${(rep.events ?? []).length}`})
          <button
            onClick={() => setTimelineAll(!timelineAll)}
            style={{ marginLeft: 10, fontSize: "0.8em", cursor: "pointer" }}
          >
            {timelineAll ? "hide skips" : `show all (${(rep.events ?? []).length})`}
          </button>
        </div>
        {(rep.events ?? []).length === 0 ? (
          <p className="muted">No dip/rise activity journaled yet.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr><th>Time</th><th>Who</th><th>Event</th><th>Symbol</th><th>What happened</th></tr>
            </thead>
            <tbody>
              {(rep.events ?? [])
                .filter((ev) => timelineAll || ev.event !== "skip")
                .map((ev, i) => (
                  <tr key={i} className={ev.event === "skip" ? "muted" : ""}>
                    <td>{fmtTime(ev.time)}</td>
                    <td>{agentLabel(ev.agent)}</td>
                    <td>{ev.event}</td>
                    <td className="mono-strong">{ev.symbol}</td>
                    <td style={{ whiteSpace: "pre-wrap" }}>{ev.note}</td>
                  </tr>
                ))}
            </tbody>
          </table>
        )}
      </div>
      {err && <p className="neg">Error: {err}</p>}
    </div>
  );
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime())
    ? iso
    : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// heldFor renders how long a closed trade was held, e.g. "14m" or "1h05m".
function heldFor(entryIso: string, exitIso: string): string {
  const a = new Date(entryIso).getTime();
  const b = new Date(exitIso).getTime();
  if (isNaN(a) || isNaN(b) || b <= a) return "—";
  const mins = Math.round((b - a) / 60000);
  if (mins < 60) return `${mins}m`;
  return `${Math.floor(mins / 60)}h${String(mins % 60).padStart(2, "0")}m`;
}

function agentLabel(agent: DipRiseEvent["agent"]): string {
  switch (agent) {
    case "pipeline": return "🔻 dip watcher";
    case "agent2_entry": return "🤖 Agent 2";
    case "rise_watch": return "📈 rise watcher";
    case "allocator": return "💰 allocator";
    default: return agent;
  }
}

function Card({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="q-card">
      <div className="q-card-label">{label}</div>
      <div className={`q-card-value ${tone ?? ""}`}>{value}</div>
    </div>
  );
}

function ScoreRow({
  label, st, money, cls,
}: {
  label: string;
  st: { trades: number; win_rate: number; total_pnl: number; avg_pnl: number };
  money: (v: number) => string;
  cls: (v: number) => string;
}) {
  return (
    <tr>
      <td className="mono-strong">{label}</td>
      <td>{st.trades}</td>
      <td>{st.trades > 0 ? `${(st.win_rate * 100).toFixed(0)}%` : "—"}</td>
      <td className={cls(st.total_pnl)}>{st.trades > 0 ? money(st.total_pnl) : "—"}</td>
      <td className={cls(st.avg_pnl)}>{st.trades > 0 ? money(st.avg_pnl) : "—"}</td>
    </tr>
  );
}
