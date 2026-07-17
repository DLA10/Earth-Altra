import { useEffect, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import type { RidpReport, RidpPosition, RidpTrade, WsMessage } from "./types";

// RIDP — the two-strategy deterministic paper desk (RIDER + DIPPER), the operator's own
// validated patterns. Open-position P&L is marked to the live quote stream client-side
// (sub-second), between 3s REST refreshes of the full report. No AI on the trade path.
export function Ridp() {
  const [rep, setRep] = useState<RidpReport | null>(null);
  const [err, setErr] = useState("");
  // live prices per symbol from the portal-wide quote broadcast (sub-second ticks)
  const [live, setLive] = useState<Record<string, number>>({});
  const symbolsRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .ridp()
        .then((r) => {
          if (!alive) return;
          setRep(r);
          symbolsRef.current = new Set([
            ...(r.open ?? []).map((p) => p.symbol),
            ...(r.reverter_open ?? []).map((p) => p.symbol),
          ]);
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
        <h2>RIDP — Rider &amp; Dipper</h2>
        <p className="muted">{err ? `Error: ${err}` : "Loading…"}</p>
      </div>
    );
  }
  if (!rep.enabled) {
    return (
      <div className="quant-page">
        <h2>RIDP — Rider &amp; Dipper</h2>
        <p className="muted">
          RIDP is not enabled — set <code>PAPER_RIDP_KEY/SECRET</code> in <code>backend/.env</code> (strict one paper account per desk).
        </p>
      </div>
    );
  }

  const money = (v: number) => `${v < 0 ? "−" : "+"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");
  const mark = (p: RidpPosition) => live[p.symbol] ?? p.last;
  const unreal = (p: RidpPosition) => (mark(p) - p.entry) * p.qty;
  // ALL open positions — rider/dipper AND reverter (the report splits them into two
  // lists for display, but the headline cards must cover the whole desk).
  const allOpen = [...(rep.open ?? []), ...(rep.reverter_open ?? [])];
  const totalUnreal = allOpen.reduce((a, p) => a + unreal(p), 0);
  const openFor = (s: string) => allOpen.filter((p) => p.strategy === s);
  const unrealFor = (s: string) => openFor(s).reduce((a, p) => a + unreal(p), 0);
  // Day P&L straight from Alpaca (equity vs prior close — includes EVERYTHING on the
  // account, tracked or not), marked live between 3s polls the same way the Execution
  // page does it: snapshot + how far live quotes have moved since the snapshot.
  const liveDrift = allOpen.reduce((a, p) => a + ((live[p.symbol] ?? p.last) - p.last) * p.qty, 0);
  const accountDayPnl = rep.account_day_pnl + liveDrift;
  const totalRealized = rep.rider.realized_pnl + rep.dipper.realized_pnl + rep.reverter.realized_pnl;
  const todayRealized = rep.rider.today_pnl + rep.dipper.today_pnl + rep.reverter.today_pnl;
  const totalTrades = rep.rider.trades + rep.dipper.trades + rep.reverter.trades;
  const totalWins = rep.rider.wins + rep.dipper.wins + rep.reverter.wins;

  return (
    <div className="quant-page">
      <h2>
        RIDP — Rider &amp; Dipper{" "}
        <span className="muted" style={{ fontSize: "0.6em" }}>
          two deterministic strategies · paper account · no AI on the trade path
        </span>
      </h2>

      <div className="quant-cards">
        <Card
          label="Day P&L (Alpaca · live)"
          value={rep.account_last_equity > 0 ? money(accountDayPnl) : "—"}
          tone={rep.account_last_equity > 0 ? cls(accountDayPnl) : ""}
        />
        <Card
          label="Open P&L (live)"
          value={allOpen.length === 0 ? "flat" : money(totalUnreal)}
          tone={allOpen.length === 0 ? "" : cls(totalUnreal)}
        />
        <Card label="Realized today (desk ledger)" value={money(todayRealized)} tone={cls(todayRealized)} />
        <Card label="Realized total" value={money(totalRealized)} tone={cls(totalRealized)} />
        <Card
          label="Win rate"
          value={totalTrades > 0 ? `${((totalWins / totalTrades) * 100).toFixed(0)}%` : "—"}
        />
        <Card label="Deployed / Equity" value={`$${rep.deployed.toFixed(0)} / $${rep.account_equity.toFixed(0)}`} />
        <Card label="Mode" value={rep.live ? "LIVE (paper)" : "SHADOW"} tone={rep.live ? "pos" : ""} />
      </div>

      {/* Broker-truth cross-check: shares Alpaca says we hold that NO strategy tracks.
          These are leaks (crossed entry/exit orders) — auto-flattened during the session. */}
      {rep.books_bad && (
        <div className="panel" style={{ borderColor: "#e5484d" }}>
          <div className="panel-title neg">⚠ Desk books unreadable (state.json corrupt)</div>
          <p className="muted">Ghost auto-flatten is standing down for safety. Fix or remove backend/data/ridp/state.json and restart.</p>
        </div>
      )}
      {(rep.ghosts ?? []).length > 0 && (
        <div className="panel" style={{ borderColor: "#e5a13d" }}>
          <div className="panel-title">⚠ On the account but NOT tracked ({(rep.ghosts ?? []).length})</div>
          <p className="muted" style={{ fontSize: "0.85em" }}>
            Alpaca holds these shares but no strategy claims them (leaked by a crossed entry/exit).
            They are <b>unmanaged and unprotected</b>; the desk auto-sells them during market hours.
          </p>
          <table className="q-table">
            <thead><tr><th>Symbol</th><th>Untracked qty</th><th>Last</th><th>Approx value</th></tr></thead>
            <tbody>
              {(rep.ghosts ?? []).map((g) => (
                <tr key={g.symbol}>
                  <td><b>{g.symbol}</b></td>
                  <td>{g.qty}</td>
                  <td>{g.last > 0 ? `$${g.last.toFixed(2)}` : "—"}</td>
                  <td>{g.last > 0 ? `$${(g.qty * g.last).toFixed(0)}` : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* ALL open positions — one table, every strategy; P&L ticks sub-second via quotes */}
      <div className="panel">
        <div className="panel-title">
          Open positions — all strategies ({allOpen.length})
          {allOpen.length > 0 && (rep.ghosts ?? []).length === 0 && (
            <span className="pos" style={{ fontSize: "0.8em", marginLeft: 8 }}>books ↔ account ✓</span>
          )}
        </div>
        {allOpen.length === 0 ? (
          <p className="muted">
            Nothing open. RIDER hunts 09:45–14:30 ET (ranked, uncapped); DIPPER buys turned dips at the open; REVERTER buys −1.5σ dips 09:45–15:45.
          </p>
        ) : (
          <table className="q-table">
            <thead>
              <tr>
                <th>Strategy</th><th>Symbol</th><th>Qty</th><th>Entry</th><th>Now</th>
                <th>P&amp;L (live)</th><th>Exit level</th><th>Held</th>
              </tr>
            </thead>
            <tbody>
              {[...allOpen]
                .sort((a, b) => new Date(a.opened_at).getTime() - new Date(b.opened_at).getTime())
                .map((p) => {
                  const u = unreal(p);
                  return (
                    <tr key={p.symbol}>
                      <td>{stratLabel(p.strategy)}</td>
                      <td><b>{p.symbol}</b></td>
                      <td>{p.qty}</td>
                      <td>${p.entry.toFixed(2)}</td>
                      <td>${mark(p).toFixed(2)}</td>
                      <td className={cls(u)}><b>{money(u)}</b></td>
                      <td>${p.trail_level.toFixed(2)}{p.tightened ? " 🔒" : ""}</td>
                      <td>{p.strategy === "dipper" ? `${p.sessions}d` : since(p.opened_at)}</td>
                    </tr>
                  );
                })}
            </tbody>
          </table>
        )}
      </div>

      {/* Per-strategy panels: LIVE open rows + realized stats, so a panel is never
          "stale zeros" while its strategy is holding positions. */}
      <div className="quant-columns">
        <StratPanel name="🏇 RIDER (intraday momentum)" st={rep.rider}
          open={openFor("rider")} openUnreal={unrealFor("rider")}
          note="From 09:45 ET (strict gates till 10:00) · ranked by gain×volume · uncapped seats · re-board above prior peak (max 3/day) → trail 3.5%→2% → flat 15:55" />
        <StratPanel name="⛰ DIPPER (multi-day dip turn)" st={rep.dipper}
          open={openFor("dipper")} openUnreal={unrealFor("dipper")}
          note="2+ red days or −4%/5d → buy the turn (prior-high close or +1.5% close on volume) → 2×ATR hard stop → trail 2.5×ATR" />
        <StratPanel name="🔄 REVERTER (intraday mean reversion)" st={rep.reverter}
          open={openFor("reverter")} openUnreal={unrealFor("reverter")}
          note="Top-55 high-amplitude names · buy −1.5σ below the 15-min mean · exit at the mean · −4σ floor · flat 15:55 · budget-capped, no trade cap" />
      </div>

      {/* DIPPER radar: what it's stalking */}
      <div className="panel">
        <div className="panel-title">DIPPER radar</div>
        <p>
          <b>Triggered (buy candidates at next open):</b>{" "}
          {rep.dipper_triggered?.length ? rep.dipper_triggered.join(", ") : <span className="muted">none</span>}
        </p>
        <p>
          <b>In setup (falling, watching for the turn):</b>{" "}
          {rep.dipper_setups?.length ? rep.dipper_setups.join(", ") : <span className="muted">none</span>}
        </p>
      </div>

      <div className="panel">
        <div className="panel-title">Closed trades (latest {Math.min(50, (rep.closed ?? []).length)})</div>
        {(rep.closed ?? []).length === 0 ? (
          <p className="muted">No closed trades yet.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr><th>Closed</th><th>Strategy</th><th>Symbol</th><th>Entry</th><th>Exit</th><th>P&amp;L</th><th>Reason</th></tr>
            </thead>
            <tbody>
              {(rep.closed ?? []).map((t: RidpTrade, i: number) => (
                <tr key={i}>
                  <td>{new Date(t.closed_at).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}</td>
                  <td>{t.strategy === "rider" ? "🏇" : t.strategy === "reverter" ? "🔄" : "⛰"} {t.strategy}</td>
                  <td><b>{t.symbol}</b></td>
                  <td>${t.entry.toFixed(2)}</td>
                  <td>${t.exit.toFixed(2)}</td>
                  <td className={cls(t.pnl)}><b>{money(t.pnl)}</b></td>
                  <td className="muted">{t.reason}</td>
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

function Card({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="q-card">
      <div className="q-card-label">{label}</div>
      <div className={`q-card-value ${tone ?? ""}`}>{value}</div>
    </div>
  );
}

function stratLabel(s: string): string {
  return s === "rider" ? "🏇 RIDER" : s === "reverter" ? "🔄 REVERTER" : "⛰ DIPPER";
}

function StratPanel({ name, st, note, open, openUnreal }: {
  name: string; st: RidpReport["rider"]; note: string;
  open: RidpPosition[]; openUnreal: number;
}) {
  const money = (v: number) => `${v < 0 ? "−" : "+"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");
  return (
    <div className="panel">
      <div className="panel-title">{name}</div>
      <p className="muted" style={{ fontSize: "0.85em" }}>{note}</p>
      <table className="q-table">
        <tbody>
          <tr>
            <td>Open now</td>
            <td>
              {open.length === 0 ? <span className="muted">none</span> : (
                <>{open.length} ({open.map((p) => p.symbol).join(", ")})</>
              )}
            </td>
          </tr>
          <tr>
            <td>Open P&amp;L (live)</td>
            <td className={cls(openUnreal)}>{open.length > 0 ? money(openUnreal) : "—"}</td>
          </tr>
          <tr><td>Closed trades</td><td>{st.trades}</td></tr>
          <tr><td>Win rate</td><td>{st.trades > 0 ? `${(st.win_rate * 100).toFixed(0)}%` : "—"}</td></tr>
          <tr><td>Realized P&amp;L</td><td className={cls(st.realized_pnl)}>{money(st.realized_pnl)}</td></tr>
          <tr><td>Avg / trade</td><td className={cls(st.avg_pnl)}>{money(st.avg_pnl)}</td></tr>
          <tr><td>Realized today</td><td className={cls(st.today_pnl)}>{money(st.today_pnl)}</td></tr>
        </tbody>
      </table>
    </div>
  );
}

function since(iso: string): string {
  const mins = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 60000));
  if (mins < 60) return `${mins}m`;
  return `${Math.floor(mins / 60)}h${mins % 60}m`;
}
