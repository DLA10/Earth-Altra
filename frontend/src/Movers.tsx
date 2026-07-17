import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import { ChartModal } from "./components/ChartModal";
import type { MarketMovers, Position, ScanState, WsMessage } from "./types";

// Morning Movers — a decision-support radar for the operator's own intraday method:
//   at ~10:30 ET take the day's violent first-hour movers, split them into FADERS (big fallers,
//   reversion-to-the-mean candidates) and RISERS (still-climbing momentum candidates), and show
//   the indicators + suggested exit levels for each. It reads the DECEPTICON scanner's existing
//   live data (% from open, RVOL, VWAP, day range, spread) and PLACES NO ORDERS — every trade is
//   the operator's manual click through the Execution page. Its whole job is to do the scanning
//   so the operator's judgement (the actual edge) is spent on the few names that matter.

const MOVE_MIN = 1.5; // % from open to count as a "violent" first-hour mover
const clamp01 = (x: number) => Math.max(0, Math.min(1, x));
const pct = (n: number) => `${n >= 0 ? "+" : ""}${n.toFixed(2)}%`;
const money = (n: number) => (n > 0 ? `$${n.toFixed(2)}` : "—");

// vwapGap: % the price sits above (+) or below (−) VWAP.
const vwapGap = (s: ScanState) => (s.vwap > 0 ? (s.price - s.vwap) / s.vwap * 100 : 0);
// rangePos: 0 = at the day's low, 1 = at the day's high.
const rangePos = (s: ScanState) =>
  s.day_high > s.day_low ? (s.price - s.day_low) / (s.day_high - s.day_low) : 0.5;

// Signal (0–100): a TRANSPARENT composite of the factors the analysis found predictive —
// relative volume, distance from VWAP (in the setup's favour), position in the day's range, and
// move size. Higher = a cleaner-looking setup. It is a heuristic, NOT a black-box model.
function riserSignal(s: ScanState) {
  return Math.round(100 * (0.35 * clamp01(s.rvol / 4) + 0.25 * clamp01(vwapGap(s) / 2) +
    0.20 * clamp01(rangePos(s)) + 0.20 * clamp01(Math.abs(s.chg_open_pct) / 4)));
}
function faderSignal(s: ScanState) {
  return Math.round(100 * (0.35 * clamp01(s.rvol / 4) + 0.25 * clamp01(-vwapGap(s) / 2) +
    0.20 * clamp01(1 - rangePos(s)) + 0.20 * clamp01(Math.abs(s.chg_open_pct) / 4)));
}

export function Movers() {
  const [scan, setScan] = useState<Record<string, ScanState>>({});
  const [own, setOwn] = useState<Set<string>>(new Set());
  const [positions, setPositions] = useState<Position[]>([]);
  const [mkt, setMkt] = useState<MarketMovers | null>(null);
  const [focused, setFocused] = useState<{ symbol: string; company: string } | null>(null);
  const [enabled, setEnabled] = useState(true);
  const onMsg = useRef<(m: WsMessage) => void>(() => {});

  // pin the operator's own names (execution + watchlist)
  useEffect(() => {
    Promise.all([api.config().catch(() => null), api.watchlistSymbols().catch(() => ({ symbols: [] }))])
      .then(([c, w]) => {
        setEnabled(c ? c.decepticon_enabled : true);
        const set = new Set<string>();
        c?.symbols?.forEach((s) => set.add(s));
        (w.symbols ?? []).forEach((s) => set.add(s));
        setOwn(set);
      });
  }, []);

  // seed scan + poll positions & market movers
  useEffect(() => {
    api.decepticonScan().then((arr) => {
      const m: Record<string, ScanState> = {};
      arr.forEach((s) => (m[s.symbol] = s));
      setScan(m);
    }).catch(() => {});
    const load = () => {
      api.positions().then(setPositions).catch(() => {});
      api.movers(30).then(setMkt).catch(() => {});
    };
    load();
    const id = window.setInterval(load, 5000);
    return () => window.clearInterval(id);
  }, []);

  onMsg.current = (m: WsMessage) => {
    if (m.type === "scan") {
      const map: Record<string, ScanState> = {};
      (m.data as ScanState[]).forEach((s) => (map[s.symbol] = s));
      setScan(map);
    }
  };
  const { status, send } = useWebSocket((m) => onMsg.current(m));
  useEffect(() => {
    if (status === "open") send({ action: "scan_subscribe" });
  }, [status, send]);

  const { risers, faders } = useMemo(() => {
    const live = Object.values(scan).filter((s) => s.has_bars && s.price > 0);
    const rise = live.filter((s) => s.chg_open_pct >= MOVE_MIN && vwapGap(s) > -0.1);
    const fall = live.filter((s) => s.chg_open_pct <= -MOVE_MIN);
    const rank = (arr: ScanState[], sig: (s: ScanState) => number) =>
      arr.map((s) => ({ s, sig: sig(s) }))
        .sort((a, b) => (own.has(b.s.symbol) ? 1 : 0) - (own.has(a.s.symbol) ? 1 : 0) || b.sig - a.sig)
        .slice(0, 12);
    return { risers: rank(rise, riserSignal), faders: rank(fall, faderSignal) };
  }, [scan, own]);

  if (!enabled) {
    return (
      <div className="quant-page">
        <h2>Morning Movers</h2>
        <p className="muted">The scanner is disabled (<code>DECEPTICON_ENABLED=false</code>). Movers needs it on to read the universe.</p>
      </div>
    );
  }

  const sigColor = (v: number) => (v >= 70 ? "pos" : v >= 45 ? "" : "neg");

  return (
    <div className="quant-page">
      <h2>
        Morning Movers{" "}
        <span className="muted" style={{ fontSize: "0.55em" }}>
          the day's violent first-hour movers · you judge, you trade · no auto-orders
        </span>
      </h2>

      <div className="attr-verdict" style={{ marginBottom: 16 }}>
        💡 <b>How to use:</b> after the first hour, scan the two lists below. <b>Risers</b> are momentum
        setups — buy if it's still climbing above a rising VWAP, ride it, exit on a VWAP break or at the
        close. <b>Faders</b> are reversion setups — buy the stretched-down ones, take profit back at VWAP
        (the mean), hard-stop below the day's low. <b>Do NOT use a tight trailing stop</b> — that was the
        one thing testing proved kills these trades. Your ⭐ names are pinned on top.
      </div>

      <div className="quant-columns">
        <MoverPanel title="📈 Risers — ride the momentum" rows={risers} own={own} kind="rise"
          onFocus={(sym, co) => setFocused({ symbol: sym, company: co })} sigColor={sigColor} />
        <MoverPanel title="📉 Faders — buy the dip, sell at the mean" rows={faders} own={own} kind="fade"
          onFocus={(sym, co) => setFocused({ symbol: sym, company: co })} sigColor={sigColor} />
      </div>

      {/* Exit-assist for your open positions */}
      <div className="panel">
        <div className="panel-title">Your open positions — exit assist</div>
        {positions.length === 0 ? (
          <p className="muted">No open positions. Exit signals appear here once you're in a trade.</p>
        ) : (
          <table className="q-table">
            <thead>
              <tr><th>Symbol</th><th>Qty</th><th>Entry</th><th>Now</th><th>P&amp;L</th><th>vs VWAP</th><th>Exit read</th></tr>
            </thead>
            <tbody>
              {positions.map((p) => {
                const s = scan[p.symbol];
                const gap = s ? vwapGap(s) : NaN;
                let read = "not in radar universe — watch your chart";
                if (s) {
                  if (gap >= 0.1) read = "above VWAP — trend intact (ride), take profit into strength";
                  else if (gap <= -0.1) read = "below VWAP — momentum broken, consider exiting";
                  else read = "at VWAP — decision point";
                }
                return (
                  <tr key={p.symbol} onClick={() => setFocused({ symbol: p.symbol, company: "" })} style={{ cursor: "pointer" }}>
                    <td><b>{p.symbol}</b></td>
                    <td>{p.qty}</td>
                    <td>{money(p.avg_entry_price)}</td>
                    <td>{money(p.current_price)}</td>
                    <td className={p.unrealized_pl >= 0 ? "pos" : "neg"}>{pct(p.unrealized_plpc * 100)}</td>
                    <td className={isFinite(gap) ? (gap >= 0 ? "pos" : "neg") : ""}>{isFinite(gap) ? pct(gap) : "—"}</td>
                    <td className="muted">{read}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        <p className="muted" style={{ fontSize: "0.8em", marginTop: 6 }}>
          Reminder: hold winners toward the close (testing showed that beats a tight trail), and flatten by 15:55 ET.
        </p>
      </div>

      {/* Whole-market movers outside the scanned universe */}
      {mkt && (
        <div className="panel">
          <div className="panel-title">Market-wide movers (outside the radar universe — click ➕ to add)</div>
          <div className="quant-columns">
            <MktStrip title="Top gainers" rows={mkt.gainers.filter((g) => !scan[g.symbol]).slice(0, 8)}
              onAdd={(sym) => api.addWatchSymbol(sym).catch(() => {})} onFocus={(sym) => setFocused({ symbol: sym, company: "" })} />
            <MktStrip title="Top losers" rows={mkt.losers.filter((g) => !scan[g.symbol]).slice(0, 8)}
              onAdd={(sym) => api.addWatchSymbol(sym).catch(() => {})} onFocus={(sym) => setFocused({ symbol: sym, company: "" })} />
          </div>
        </div>
      )}

      {focused && <ChartModal symbol={focused.symbol} company={focused.company} onClose={() => setFocused(null)} />}
    </div>
  );
}

function MoverPanel({ title, rows, own, kind, onFocus, sigColor }: {
  title: string; rows: { s: ScanState; sig: number }[]; own: Set<string>; kind: "rise" | "fade";
  onFocus: (sym: string, co: string) => void; sigColor: (v: number) => string;
}) {
  return (
    <div className="panel">
      <div className="panel-title">{title} ({rows.length})</div>
      {rows.length === 0 ? (
        <p className="muted">No {kind === "rise" ? "risers" : "faders"} beyond ±{MOVE_MIN}% yet — quiet first hour, or pre-open.</p>
      ) : (
        <table className="q-table">
          <thead>
            <tr>
              <th>Symbol</th><th>%open</th><th>RVOL</th><th>VWAP</th><th>Range</th><th>Signal</th>
              <th>{kind === "fade" ? "Target / Stop" : "Trend line"}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(({ s, sig }) => {
              const gap = vwapGap(s);
              const rp = rangePos(s);
              const range = s.day_high - s.day_low;
              const stop = kind === "fade" ? Math.min(s.day_low, s.price - 0.5 * range) : 0;
              return (
                <tr key={s.symbol} onClick={() => onFocus(s.symbol, "")} style={{ cursor: "pointer" }}>
                  <td>{own.has(s.symbol) ? "⭐ " : ""}<b>{s.symbol}</b></td>
                  <td className={s.chg_open_pct >= 0 ? "pos" : "neg"}>{pct(s.chg_open_pct)}</td>
                  <td className={s.rvol >= 2 ? "pos" : ""}>{s.rvol.toFixed(1)}×</td>
                  <td className={gap >= 0 ? "pos" : "neg"}>{pct(gap)}</td>
                  <td>{(rp * 100).toFixed(0)}%</td>
                  <td className={sigColor(sig)}><b>{sig}</b></td>
                  <td className="muted">
                    {kind === "fade"
                      ? <>🎯 {money(s.vwap)} · 🛑 {money(stop)}</>
                      : <>ride &gt; {money(s.vwap)}</>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function MktStrip({ title, rows, onAdd, onFocus }: {
  title: string; rows: { symbol: string; percent_change: number; price: number }[];
  onAdd: (sym: string) => void; onFocus: (sym: string) => void;
}) {
  return (
    <div>
      <div className="muted" style={{ marginBottom: 4 }}>{title}</div>
      <table className="q-table">
        <tbody>
          {rows.length === 0 ? <tr><td className="muted">—</td></tr> : rows.map((r) => (
            <tr key={r.symbol}>
              <td onClick={() => onFocus(r.symbol)} style={{ cursor: "pointer" }}><b>{r.symbol}</b></td>
              <td className={r.percent_change >= 0 ? "pos" : "neg"}>{pct(r.percent_change)}</td>
              <td>{money(r.price)}</td>
              <td><button className="mini-btn" onClick={() => onAdd(r.symbol)}>➕</button></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
