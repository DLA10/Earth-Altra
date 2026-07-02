import { useEffect, useMemo, useRef, useState } from "react";
import {
  createChart,
  ColorType,
  LineStyle,
  type IChartApi,
  type Time,
  type UTCTimestamp,
} from "lightweight-charts";
import { api } from "./api/client";
import { realizedTrades, basisBySymbol, type ClosedTrade } from "./costBasis";
import type { Activity, Position } from "./types";

type Gran = "day" | "week" | "month";

const pad = (n: number) => String(n).padStart(2, "0");
const money = (n: number) => `${n >= 0 ? "+" : "-"}$${Math.abs(n).toFixed(2)}`;
const fmtTime = (iso: string) =>
  new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

// ET calendar parts for a fill timestamp, so buckets follow the US trading day.
function etParts(iso: string) {
  const parts = new Intl.DateTimeFormat("en-CA", {
    timeZone: "America/New_York",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    weekday: "short",
  }).formatToParts(new Date(iso));
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? "";
  return { y: +get("year"), m: +get("month"), d: +get("day"), wd: get("weekday") };
}

const WD: Record<string, number> = { Mon: 0, Tue: 1, Wed: 2, Thu: 3, Fri: 4, Sat: 5, Sun: 6 };

interface Bucket {
  key: string;
  label: string;
  sortTs: number;
  trades: ClosedTrade[];
}

function bucketOf(iso: string, gran: Gran): { key: string; label: string; sortTs: number } {
  const { y, m, d, wd } = etParts(iso);
  if (gran === "month") {
    const dt = new Date(Date.UTC(y, m - 1, 1, 12));
    return { key: `${y}-${pad(m)}`, label: dt.toLocaleDateString(undefined, { month: "long", year: "numeric" }), sortTs: dt.getTime() };
  }
  if (gran === "week") {
    const monday = new Date(Date.UTC(y, m - 1, d, 12) - WD[wd] * 86400000);
    const k = monday.toISOString().slice(0, 10);
    return { key: `wk-${k}`, label: `Week of ${monday.toLocaleDateString(undefined, { month: "short", day: "numeric" })}`, sortTs: monday.getTime() };
  }
  const dt = new Date(Date.UTC(y, m - 1, d, 12));
  return { key: `${y}-${pad(m)}-${pad(d)}`, label: dt.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" }), sortTs: dt.getTime() };
}

function summarize(trades: ClosedTrade[]) {
  let net = 0, profit = 0, loss = 0, shares = 0, wins = 0, losses = 0, sumWin = 0, sumLoss = 0;
  let best = 0, worst = 0;
  for (const t of trades) {
    net += t.pnl;
    shares += t.qtySold;
    if (t.pnl > 0) { profit += t.pnl; wins++; sumWin += t.pnl; }
    else if (t.pnl < 0) { loss += t.pnl; losses++; sumLoss += t.pnl; }
    if (t.pnl > best) best = t.pnl;
    if (t.pnl < worst) worst = t.pnl;
  }
  const count = trades.length;
  return {
    net, profit, loss, shares,
    perShare: shares > 0 ? net / shares : 0,
    count, wins, losses,
    winRate: count > 0 ? (wins / count) * 100 : 0,
    avgWin: wins > 0 ? sumWin / wins : 0,
    avgLoss: losses > 0 ? sumLoss / losses : 0,
    best, worst,
  };
}

export function Metrics() {
  const [fills, setFills] = useState<Activity[]>([]);
  const [positions, setPositions] = useState<Position[]>([]);
  const [gran, setGran] = useState<Gran>("day");
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState("");

  const load = () => {
    setLoading(true);
    setErr("");
    Promise.all([api.fills(120), api.positions()])
      .then(([f, p]) => { setFills(f); setPositions(p); })
      .catch((e) => setErr((e as Error).message))
      .finally(() => setLoading(false));
  };
  useEffect(load, []);

  const trades = useMemo(() => realizedTrades(fills), [fills]);

  const buckets = useMemo<Bucket[]>(() => {
    const map = new Map<string, Bucket>();
    for (const t of trades) {
      const b = bucketOf(t.time, gran);
      const cur = map.get(b.key) ?? { key: b.key, label: b.label, sortTs: b.sortTs, trades: [] };
      cur.trades.push(t);
      map.set(b.key, cur);
    }
    // Always include the CURRENT period (today / this week / this month) so a fresh session
    // reads $0 from the open instead of silently falling back to the last day that traded.
    const now = bucketOf(new Date().toISOString(), gran);
    if (!map.has(now.key)) {
      map.set(now.key, { key: now.key, label: now.label, sortTs: now.sortTs, trades: [] });
    }
    return [...map.values()].sort((a, b) => b.sortTs - a.sortTs);
  }, [trades, gran]);

  // Default selection = most recent bucket; keep valid as granularity changes.
  useEffect(() => {
    if (buckets.length === 0) { setSelectedKey(null); return; }
    if (!selectedKey || !buckets.some((b) => b.key === selectedKey)) setSelectedKey(buckets[0].key);
  }, [buckets, selectedKey]);

  const selected = buckets.find((b) => b.key === selectedKey) ?? buckets[0];
  const sum = summarize(selected?.trades ?? []);

  // Per-symbol P&L within the selected bucket.
  const perSymbol = useMemo(() => {
    const m = new Map<string, { pnl: number; qty: number }>();
    for (const t of selected?.trades ?? []) {
      const e = m.get(t.symbol) ?? { pnl: 0, qty: 0 };
      e.pnl += t.pnl; e.qty += t.qtySold;
      m.set(t.symbol, e);
    }
    return [...m.entries()].map(([symbol, v]) => ({ symbol, ...v })).sort((a, b) => b.pnl - a.pnl);
  }, [selected]);

  // Daily cumulative realized P&L (all-time), for the equity curve.
  const curve = useMemo(() => {
    const daily = new Map<number, number>();
    for (const t of trades) {
      const { y, m, d } = etParts(t.time);
      const ts = Math.floor(Date.UTC(y, m - 1, d) / 1000);
      daily.set(ts, (daily.get(ts) ?? 0) + t.pnl);
    }
    const days = [...daily.entries()].sort((a, b) => a[0] - b[0]);
    let run = 0;
    return days.map(([ts, v]) => { run += v; return { time: ts as UTCTimestamp, value: run }; });
  }, [trades]);

  // Unrealized (open positions) — corrected cost basis × Alpaca's current price.
  const basis = useMemo(() => basisBySymbol(positions, fills), [positions, fills]);
  const openRows = positions.map((p) => {
    const entry = basis[p.symbol]?.avgEntry ?? p.avg_entry_price;
    const pl = (p.current_price - entry) * p.qty;
    return { symbol: p.symbol, qty: p.qty, entry, price: p.current_price, pl };
  });
  const unrealizedTotal = openRows.reduce((a, r) => a + r.pl, 0);

  return (
    <div className="metrics">
      <div className="metrics-head">
        <div>
          <h1 className="metrics-title">Metrics</h1>
          <p className="metrics-sub">Realized P&amp;L from your closed trades · grouped by US trading {gran}</p>
        </div>
        <div className="metrics-controls">
          <div className="seg gran-seg">
            {(["day", "week", "month"] as Gran[]).map((g) => (
              <button key={g} className={`seg-btn ${gran === g ? "on" : ""}`} onClick={() => setGran(g)}>
                {g[0].toUpperCase() + g.slice(1)}
              </button>
            ))}
          </div>
          <button className="metrics-refresh" onClick={load} disabled={loading}>{loading ? "Loading…" : "↻ Refresh"}</button>
        </div>
      </div>

      {err && <div className="error-box">{err}</div>}

      {!loading && trades.length === 0 && !err ? (
        <div className="empty">No closed trades in the last 120 days yet. Once you buy and then sell, your stats appear here.</div>
      ) : (
        <>
          {selected && (
            <>
              <div className="metrics-selected">Showing: <strong>{selected.label}</strong></div>
              <div className="metric-cards">
                <Card label="Net P&L" value={money(sum.net)} tone={sum.net >= 0 ? "pos" : "neg"} big />
                <Card label="Total profit" value={money(sum.profit)} tone="pos" />
                <Card label="Total loss" value={money(sum.loss)} tone="neg" />
                <Card label="Per-share P&L" value={money(sum.perShare)} tone={sum.perShare >= 0 ? "pos" : "neg"} />
                <Card label="Win rate" value={`${sum.winRate.toFixed(0)}%`} sub={`${sum.wins}/${sum.count} trades`} />
                <Card label="Trades" value={`${sum.count}`} sub={`${sum.shares.toFixed(2)} shares sold`} />
                <Card label="Avg win" value={money(sum.avgWin)} tone="pos" />
                <Card label="Avg loss" value={money(sum.avgLoss)} tone="neg" />
                <Card label="Best trade" value={money(sum.best)} tone="pos" />
                <Card label="Worst trade" value={money(sum.worst)} tone="neg" />
              </div>
            </>
          )}

          <div className="metrics-section-title">Cumulative realized P&L (daily)</div>
          <CumChart data={curve} />

          <div className="metrics-grid">
            <div>
              <div className="metrics-section-title">Each {gran} · tap to view</div>
              <table className="data-table">
                <thead>
                  <tr><th>{gran[0].toUpperCase() + gran.slice(1)}</th><th>Profit</th><th>Loss</th><th>Net</th><th>/share</th><th>Trades</th></tr>
                </thead>
                <tbody>
                  {buckets.map((b) => {
                    const s = summarize(b.trades);
                    return (
                      <tr key={b.key} className={`pos-row ${b.key === selectedKey ? "row-active" : ""}`} onClick={() => setSelectedKey(b.key)} role="button">
                        <td className="mono-strong">{b.label}</td>
                        <td className="pos">{money(s.profit)}</td>
                        <td className="neg">{money(s.loss)}</td>
                        <td className={s.net >= 0 ? "pos" : "neg"}>{money(s.net)}</td>
                        <td className={s.perShare >= 0 ? "pos" : "neg"}>{money(s.perShare)}</td>
                        <td>{s.count}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>

            <div>
              <div className="metrics-section-title">By symbol · {selected?.label ?? ""}</div>
              {perSymbol.length === 0 ? (
                <div className="empty">No closed trades in this {gran}.</div>
              ) : (
                <table className="data-table">
                  <thead><tr><th>Symbol</th><th>Shares</th><th>Realized P&L</th></tr></thead>
                  <tbody>
                    {perSymbol.map((r) => (
                      <tr key={r.symbol}>
                        <td className="mono-strong">{r.symbol}</td>
                        <td>{r.qty.toFixed(2)}</td>
                        <td className={r.pnl >= 0 ? "pos" : "neg"}>{money(r.pnl)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          <div className="metrics-section-title">Closed trades · {selected?.label ?? ""}</div>
          <table className="data-table">
            <thead>
              <tr><th>When</th><th>Symbol</th><th>Shares</th><th>Avg buy</th><th>Avg sell</th><th>P&L</th><th>/share</th><th>%</th></tr>
            </thead>
            <tbody>
              {(selected?.trades ?? []).slice().reverse().map((t, i) => (
                <tr key={t.time + t.symbol + i}>
                  <td>{fmtTime(t.time)}</td>
                  <td className="mono-strong">{t.symbol}</td>
                  <td>{t.qtySold.toFixed(2)}</td>
                  <td>${t.avgCost.toFixed(2)}</td>
                  <td>${t.avgSell.toFixed(2)}</td>
                  <td className={t.pnl >= 0 ? "pos" : "neg"}>{money(t.pnl)}</td>
                  <td className={t.perShare >= 0 ? "pos" : "neg"}>{money(t.perShare)}</td>
                  <td className={t.pct >= 0 ? "pos" : "neg"}>{t.pct >= 0 ? "+" : ""}{t.pct.toFixed(2)}%</td>
                </tr>
              ))}
            </tbody>
          </table>

          <div className="metrics-section-title">
            Open positions · unrealized (paper) <span className="muted small">— not locked in; moves with price</span>
          </div>
          {openRows.length === 0 ? (
            <div className="empty">No open positions.</div>
          ) : (
            <table className="data-table">
              <thead><tr><th>Symbol</th><th>Shares</th><th>Bought at</th><th>Current</th><th>Unrealized P&L</th></tr></thead>
              <tbody>
                {openRows.map((r) => (
                  <tr key={r.symbol}>
                    <td className="mono-strong">{r.symbol}</td>
                    <td>{r.qty}</td>
                    <td>${r.entry.toFixed(2)}</td>
                    <td>${r.price.toFixed(2)}</td>
                    <td className={r.pl >= 0 ? "pos" : "neg"}>{money(r.pl)}</td>
                  </tr>
                ))}
                <tr>
                  <td colSpan={4} className="muted">Total unrealized</td>
                  <td className={unrealizedTotal >= 0 ? "pos" : "neg"}>{money(unrealizedTotal)}</td>
                </tr>
              </tbody>
            </table>
          )}
        </>
      )}
    </div>
  );
}

function Card({ label, value, sub, tone, big }: { label: string; value: string; sub?: string; tone?: "pos" | "neg"; big?: boolean }) {
  return (
    <div className={`metric-card ${big ? "big" : ""}`}>
      <span className="metric-card-label">{label}</span>
      <span className={`metric-card-value ${tone ?? ""}`}>{value}</span>
      {sub && <span className="metric-card-sub">{sub}</span>}
    </div>
  );
}

function CumChart({ data }: { data: { time: UTCTimestamp; value: number }[] }) {
  const ref = useRef<HTMLDivElement>(null);
  const chartRef = useRef<IChartApi | null>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const chart = createChart(el, {
      layout: { background: { type: ColorType.Solid, color: "#0d1117" }, textColor: "#9aa4b2", fontFamily: "ui-monospace, monospace", attributionLogo: false },
      grid: { vertLines: { color: "#1b2230" }, horzLines: { color: "#1b2230" } },
      rightPriceScale: { borderColor: "#1b2230" },
      timeScale: {
        borderColor: "#1b2230",
        timeVisible: false,
        tickMarkFormatter: (t: Time) => new Date((typeof t === "number" ? t : 0) * 1000).toLocaleDateString([], { month: "short", day: "numeric" }),
      },
      autoSize: true,
    });
    const series = chart.addAreaSeries({ lineColor: "#16c784", topColor: "rgba(22,199,132,0.25)", bottomColor: "rgba(22,199,132,0.02)", lineWidth: 2 });
    series.createPriceLine({ price: 0, color: "#5b6678", lineWidth: 1, lineStyle: LineStyle.Dashed, axisLabelVisible: false, title: "" });
    series.setData(data);
    chart.timeScale().fitContent();
    chartRef.current = chart;
    return () => { chart.remove(); chartRef.current = null; };
  }, [data]);
  return <div ref={ref} className="metrics-chart" />;
}
