import { useEffect, useState } from "react";
import { api } from "../api/client";
import type { NewsItem, Pressure } from "../types";

function ago(iso: string): string {
  const s = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return new Date(iso).toLocaleDateString([], { month: "short", day: "numeric" });
}

// NewsPanel shows a buy/sell pressure meter and recent headlines (with a sentiment tag)
// for one symbol. Pressure polls fast (flow changes intraday); news polls slowly.
export function NewsPanel({ symbol }: { symbol: string }) {
  const [news, setNews] = useState<NewsItem[]>([]);
  const [flow, setFlow] = useState<Pressure | null>(null);
  const [newsOpen, setNewsOpen] = useState(false);

  useEffect(() => {
    if (!symbol) return;
    let alive = true;
    const loadNews = () => api.news([symbol], 10).then((n) => alive && setNews(n)).catch(() => {});
    const loadFlow = () => api.pressure(symbol).then((p) => alive && setFlow(p)).catch(() => {});
    setNews([]);
    setFlow(null);
    loadNews();
    loadFlow();
    const nId = window.setInterval(loadNews, 60000);
    const pId = window.setInterval(loadFlow, 1000); // free (reads our backend, not Alpaca)
    return () => {
      alive = false;
      window.clearInterval(nId);
      window.clearInterval(pId);
    };
  }, [symbol]);

  return (
    <div className="news-panel">
      <div className="panel-title">Buy / sell pressure — {symbol}</div>
      <PressureBar flow={flow} />

      {/* News, as a separate collapsible dropdown below the pressure bar. */}
      <div className="news-drop">
        <button className="news-toggle" onClick={() => setNewsOpen((o) => !o)} aria-expanded={newsOpen}>
          <span>📰 News{news.length ? ` · ${news.length}` : ""}</span>
          <span className="news-caret">{newsOpen ? "▾" : "▸"}</span>
        </button>
        {newsOpen &&
          (news.length === 0 ? (
            <div className="empty">No recent headlines for {symbol}.</div>
          ) : (
            <ul className="news-list">
              {news.map((n) => (
                <li key={n.id} className="news-item">
                  <a href={n.url || "#"} target="_blank" rel="noreferrer" className="news-link">
                    <span className={`news-dot ${n.sentiment}`} title={`sentiment: ${n.sentiment}`} />
                    <span className="news-headline">{n.headline}</span>
                  </a>
                  <span className="news-meta">{n.author || "Benzinga"} · {ago(n.created_at)}</span>
                </li>
              ))}
            </ul>
          ))}
      </div>
    </div>
  );
}

function PressureBar({ flow }: { flow: Pressure | null }) {
  if (!flow) {
    return <div className="flow-empty">Buy/sell pressure — waiting for trades…</div>;
  }
  const win = flow.window_min || 5;
  const rollTotal = flow.roll_buy_vol + flow.roll_sell_vol;
  return (
    <div className="flow-wrap">
      <FlowRow label={`Last ${win} min`} pct={rollTotal > 0 ? flow.roll_buy_pct : null} live />
    </div>
  );
}

function FlowRow({ label, pct, live }: { label: string; pct: number | null; live?: boolean }) {
  if (pct === null) {
    return (
      <div className="flow-row">
        <span className="flow-rowlabel">{label}{live ? " ●" : ""}</span>
        <span className="flow-quiet">no trades</span>
      </div>
    );
  }
  const buy = Math.round(pct);
  return (
    <div
      className="flow-row"
      title={`${label}: buyer-initiated ${buy}% vs seller-initiated ${100 - buy}% of classified volume (estimated from trades vs bid/ask).`}
    >
      <div className="flow-head">
        <span className="flow-rowlabel">{label}{live ? " ●" : ""}</span>
        <span className="flow-nums"><span className="pos">▲{buy}%</span> / <span className="neg">{100 - buy}%▼</span></span>
      </div>
      <div className="flow-bar">
        <div className="flow-buy" style={{ width: `${buy}%` }} />
        <div className="flow-sell" style={{ width: `${100 - buy}%` }} />
      </div>
    </div>
  );
}
