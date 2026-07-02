import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import { LazyMount } from "./components/LazyMount";
import { MiniChart } from "./components/MiniChart";
import { ChartModal } from "./components/ChartModal";
import { MarketMovers } from "./components/MarketMovers";
import type { Department, ScanState, WsMessage } from "./types";

const RVOL_HOT = 2.0; // relative-volume threshold for "high RVOL" / catalyst-active

// moveColor maps a % move to a heatmap tile / chip color (dark theme).
function moveColor(pct: number): { bg: string; fg: string } {
  if (!isFinite(pct) || Math.abs(pct) < 0.05) return { bg: "#161b24", fg: "#8b94a3" };
  const m = Math.min(Math.abs(pct), 4) / 4; // 0..1 intensity, saturates at ±4%
  const a = (0.14 + m * 0.5).toFixed(2);
  return pct > 0
    ? { bg: `rgba(22,199,132,${a})`, fg: "#bdf5d8" }
    : { bg: `rgba(234,57,67,${a})`, fg: "#ffcdd2" };
}

const fmtPct = (n: number) => `${n >= 0 ? "+" : ""}${n.toFixed(2)}%`;
const fmtPrice = (n: number) => (n > 0 ? `$${n.toFixed(2)}` : "—");

interface Agg {
  sectorMove: number;
  up: number;
  down: number;
  highRvol: number;
  catalystFlags: number;
  live: number;
}
function aggregate(states: ScanState[]): Agg {
  const withBars = states.filter((s) => s.has_bars);
  const sectorMove =
    withBars.length > 0 ? withBars.reduce((a, s) => a + s.chg_close_pct, 0) / withBars.length : 0;
  return {
    sectorMove,
    up: withBars.filter((s) => s.chg_close_pct > 0).length,
    down: withBars.filter((s) => s.chg_close_pct < 0).length,
    highRvol: states.filter((s) => s.rvol >= RVOL_HOT).length,
    catalystFlags: states.filter((s) => s.rvol >= RVOL_HOT && Math.abs(s.chg_close_pct) >= 1).length,
    live: withBars.length,
  };
}

function DepartmentSection({
  dept,
  scan,
  expanded,
  onToggle,
  onFocus,
}: {
  dept: Department;
  scan: Record<string, ScanState>;
  expanded: boolean;
  onToggle: () => void;
  onFocus: (symbol: string, company: string) => void;
}) {
  const states = dept.tickers.map((t) => scan[t.symbol]).filter(Boolean) as ScanState[];
  const agg = aggregate(states);
  const metaOf = useMemo(() => {
    const m: Record<string, { company: string; catalyst: string }> = {};
    for (const t of dept.tickers) m[t.symbol] = { company: t.company, catalyst: t.catalyst };
    return m;
  }, [dept]);
  const companyOf = (sym: string) => metaOf[sym]?.company ?? "";

  const active = states.filter((s) => s.has_bars);
  const inactive = states.filter((s) => !s.has_bars).map((s) => s.symbol);

  const movers = [...active]
    .sort((a, b) => Math.abs(b.chg_close_pct) - Math.abs(a.chg_close_pct))
    .slice(0, 5);
  const radar = [...active].filter((s) => s.rvol >= RVOL_HOT).sort((a, b) => b.rvol - a.rvol).slice(0, 5);
  // Active names sorted by move, then inactive (no-data) tiles last.
  const heat = [
    ...[...active].sort((a, b) => b.chg_close_pct - a.chg_close_pct),
    ...states.filter((s) => !s.has_bars),
  ];

  return (
    <section className="dept">
      <button className="dept-head" onClick={onToggle}>
        <span className="dept-head-left">
          <span className="dept-icon">
            <i className={`ti ti-${dept.icon}`} aria-hidden="true" />
          </span>
          <span>
            <span className="dept-name">{dept.name}</span>
            <span className="dept-sub">
              DECEPTICON · sector scan · {dept.tickers.length} names
            </span>
          </span>
        </span>
        <span className="dept-head-right">
          <span className={`sector-move ${agg.sectorMove >= 0 ? "pos" : "neg"}`}>
            {fmtPct(agg.sectorMove)}
          </span>
          <span className="dept-caret">{expanded ? "▾" : "▸"}</span>
        </span>
      </button>

      {expanded && (
        <div className="dept-body">
          {/* Summary cards */}
          <div className="scan-cards">
            <Card label="Sector move" value={fmtPct(agg.sectorMove)} tone={agg.sectorMove >= 0 ? "pos" : "neg"} />
            <Card label="Breadth (up / down)" value={`${agg.up} / ${agg.down}`} />
            <Card label="High RVOL" value={`${agg.highRvol}`} />
            <Card label="Catalyst flags" value={`${agg.catalystFlags}`} tone={agg.catalystFlags > 0 ? "warn" : undefined} />
          </div>

          {/* Movers + catalyst radar */}
          <div className="scan-panels">
            <div className="scan-panel">
              <div className="scan-panel-title up">▲ Top movers</div>
              {movers.length === 0 ? (
                <div className="scan-empty">Waiting for data…</div>
              ) : (
                movers.map((s) => {
                  const c = moveColor(s.chg_close_pct);
                  return (
                    <div key={s.symbol} className="mover-row">
                      <span>
                        <span className="mono-strong">{s.symbol}</span>{" "}
                        <span className="muted">{fmtPrice(s.price)}</span>
                      </span>
                      <span className="chip" style={{ background: c.bg, color: c.fg }}>
                        {fmtPct(s.chg_close_pct)}
                      </span>
                    </div>
                  );
                })
              )}
            </div>

            <div className="scan-panel">
              <div className="scan-panel-title warn">⚡ Catalyst radar</div>
              {radar.length === 0 ? (
                <div className="scan-empty">No high-RVOL names right now.</div>
              ) : (
                radar.map((s) => (
                  <div key={s.symbol} className="radar-row">
                    <div className="radar-top">
                      <span className="mono-strong">{s.symbol}</span>
                      <span className="muted small">RVOL {s.rvol.toFixed(1)}x</span>
                    </div>
                    <div className="radar-cat">{metaOf[s.symbol]?.catalyst}</div>
                  </div>
                ))
              )}
            </div>
          </div>

          {/* Heatmap */}
          <div className="scan-panel">
            <div className="scan-panel-title">Sector heatmap <span className="muted small">· click a tile to enlarge its chart</span></div>
            <div className="heatmap">
              {heat.map((s) => {
                if (!s.has_bars) {
                  return (
                    <button
                      key={s.symbol}
                      className="heat-tile heat-dead"
                      onClick={() => onFocus(s.symbol, companyOf(s.symbol))}
                      title="No recent data (delisted / merged / renamed)"
                    >
                      <p className="heat-sym">{s.symbol}</p>
                      <p className="heat-pct">no data</p>
                    </button>
                  );
                }
                const c = moveColor(s.chg_close_pct);
                return (
                  <button
                    key={s.symbol}
                    className="heat-tile"
                    style={{ background: c.bg }}
                    onClick={() => onFocus(s.symbol, companyOf(s.symbol))}
                  >
                    <p className="heat-sym" style={{ color: c.fg }}>{s.symbol}</p>
                    <p className="heat-pct" style={{ color: c.fg }}>{fmtPct(s.chg_close_pct)}</p>
                  </button>
                );
              })}
            </div>
            <div className="heat-legend">
              <span><span className="legend-sw" style={{ background: "rgba(22,199,132,0.6)" }} />strong up</span>
              <span><span className="legend-sw" style={{ background: "#161b24" }} />flat</span>
              <span><span className="legend-sw" style={{ background: "rgba(234,57,67,0.6)" }} />down</span>
              <span><span className="legend-sw heat-dead-sw" />no data</span>
            </div>
            {inactive.length > 0 && (
              <div className="inactive-note">
                <strong>{inactive.length} inactive</strong> (no recent data — likely delisted, merged, or
                renamed; consider pruning from the watchlist): {inactive.join(", ")}
              </div>
            )}
          </div>

          {/* Per-stock candlestick charts (lazy-mounted on scroll) */}
          <div className="dept-charts">
            {dept.tickers.map((t) => {
              const s = scan[t.symbol];
              const dead = s ? !s.has_bars : false;
              return (
                <div
                  key={t.symbol}
                  className={`chart-card ${dead ? "dead" : ""}`}
                  onClick={() => onFocus(t.symbol, t.company)}
                  title="Click to enlarge"
                >
                  <div className="chart-card-head">
                    <span>
                      <span className="mono-strong">{t.symbol}</span>{" "}
                      <span className="muted small">{t.company}</span>
                    </span>
                    {s && !dead && (
                      <span className="chart-card-stats">
                        <span className="muted">{fmtPrice(s.price)}</span>
                        <span className={s.chg_close_pct >= 0 ? "pos" : "neg"}>{fmtPct(s.chg_close_pct)}</span>
                        <span className="muted small">RVOL {s.rvol > 0 ? `${s.rvol.toFixed(1)}x` : "—"}</span>
                      </span>
                    )}
                    {dead && <span className="dead-badge">no data</span>}
                    <i className="ti ti-arrows-maximize expand-icon" aria-hidden="true" />
                  </div>
                  <div className="chart-card-cat">{t.catalyst}</div>
                  <LazyMount minHeight={190}>
                    <MiniChart symbol={t.symbol} />
                  </LazyMount>
                </div>
              );
            })}
          </div>
        </div>
      )}
    </section>
  );
}

function Card({ label, value, tone }: { label: string; value: string; tone?: "pos" | "neg" | "warn" }) {
  return (
    <div className="scan-card">
      <p className="scan-card-label">{label}</p>
      <p className={`scan-card-value ${tone ?? ""}`}>{value}</p>
    </div>
  );
}

export function Decepticon() {
  const [departments, setDepartments] = useState<Department[]>([]);
  const [scan, setScan] = useState<Record<string, ScanState>>({});
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [sipDegraded, setSipDegraded] = useState(false);
  const [symbolCount, setSymbolCount] = useState(0);
  const [loaded, setLoaded] = useState(false);
  const [focused, setFocused] = useState<{ symbol: string; company: string } | null>(null);

  const onMessage = useRef((m: WsMessage) => {
    if (m.type === "scan") {
      const map: Record<string, ScanState> = {};
      for (const s of m.data) map[s.symbol] = s;
      setScan(map);
    }
  });

  const { status, send } = useWebSocket((m) => onMessage.current(m));

  // Subscribe to the throttled scan channel whenever the socket is open.
  useEffect(() => {
    if (status === "open") send({ action: "scan_subscribe" });
  }, [status, send]);

  // Load watchlist + an initial scan snapshot.
  useEffect(() => {
    api
      .decepticonWatchlist()
      .then((wl) => {
        setDepartments(wl.departments);
        setSipDegraded(wl.sip_degraded);
        setSymbolCount(wl.symbol_count);
        setLoaded(true);
      })
      .catch(() => setLoaded(true));
    api
      .decepticonScan()
      .then((states) => {
        const map: Record<string, ScanState> = {};
        for (const s of states) map[s.symbol] = s;
        setScan(map);
      })
      .catch(() => {});
  }, []);

  if (!loaded) return <div className="boot">Loading DECEPTICON…</div>;
  if (departments.length === 0)
    return <div className="boot">DECEPTICON watchlist unavailable.</div>;

  return (
    <div className="decepticon">
      <div className="dec-header">
        <div>
          <h1 className="dec-title">◆ DECEPTICON</h1>
          <p className="dec-sub">
            Event-driven scanner · {departments.length} departments · {symbolCount} tickers
          </p>
        </div>
        <div className="dec-status">
          <span className={`conn-dot ${status}`} />
          <span className="conn-label">{status === "open" ? "live" : status}</span>
        </div>
      </div>

      {sipDegraded && (
        <div className="sip-degraded">
          ⚠ Data feed is not SIP — volume metrics (RVOL, true % move) are degraded across
          names. Set <code>ALPACA_DATA_FEED=sip</code> (Algo Trader Plus) for reliable signals.
        </div>
      )}

      <MarketMovers onPick={(sym) => setFocused({ symbol: sym, company: "" })} />

      {departments.map((d) => (
        <DepartmentSection
          key={d.slug}
          dept={d}
          scan={scan}
          expanded={!!expanded[d.slug]}
          onToggle={() => setExpanded((e) => ({ ...e, [d.slug]: !e[d.slug] }))}
          onFocus={(symbol, company) => setFocused({ symbol, company })}
        />
      ))}

      {focused && (
        <ChartModal
          symbol={focused.symbol}
          company={focused.company}
          state={scan[focused.symbol]}
          onClose={() => setFocused(null)}
        />
      )}
    </div>
  );
}
