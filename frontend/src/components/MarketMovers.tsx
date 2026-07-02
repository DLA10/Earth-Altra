import { Fragment, useEffect, useState } from "react";
import { api } from "../api/client";
import type { MarketMover, MarketMovers as Movers, MoverNews, StockNews } from "../types";

// Per-symbol dropdown news state: undefined = not loaded, "loading"/"error" = in flight.
type NewsState = StockNews | "loading" | "error" | undefined;

// MarketMovers shows the market-wide top gainers and losers (Alpaca screener). Clicking a
// stock NAME opens a dropdown with its news + a Gemini "why is it moving" summary (works
// for gainers and fallers). Fallers also get a DIP?/KNIFE badge from a cheap Alpaca-only
// sentiment read — the falling-knife filter for the dip-buy strategy. Clicking the rest of
// the row still opens the quick chart popup (onPick).
export function MarketMovers({ onPick }: { onPick?: (sym: string) => void }) {
  const [data, setData] = useState<Movers | null>(null);
  const [badges, setBadges] = useState<Record<string, MoverNews>>({});
  const [open, setOpen] = useState(true);
  const [msg, setMsg] = useState("");
  const [expanded, setExpanded] = useState<string | null>(null);
  const [news, setNews] = useState<Record<string, NewsState>>({});

  // The full gainer/loser board (cheap; refreshed often).
  useEffect(() => {
    let alive = true;
    const load = () => api.movers(50).then((m) => alive && setData(m)).catch(() => {});
    load();
    const id = window.setInterval(load, 20000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, []);

  // Alpaca-only sentiment for the badge layer (one batched call; slower cadence).
  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .moversNews(12)
        .then((mn) => {
          if (!alive) return;
          const map: Record<string, MoverNews> = {};
          for (const m of [...mn.gainers, ...mn.losers]) map[m.symbol] = m;
          setBadges(map);
        })
        .catch(() => {});
    load();
    const id = window.setInterval(load, 60000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, []);

  const flash = (m: string) => {
    setMsg(m);
    window.setTimeout(() => setMsg(""), 3000);
  };

  async function add(sym: string, where: "exec" | "watch") {
    try {
      if (where === "exec") await api.addExecSymbol(sym);
      else await api.addWatchSymbol(sym);
      flash(`${sym} → ${where === "exec" ? "Execution" : "Watchlist"}`);
    } catch (e) {
      flash(`${sym}: ${(e as Error).message}`);
    }
  }

  // Fetch headlines (instant) + poll for the background AI summary until it lands.
  function loadNews(sym: string, attempt = 0) {
    if (attempt === 0) setNews((n) => ({ ...n, [sym]: "loading" }));
    api
      .stockNews(sym)
      .then((sn) => {
        setNews((n) => ({ ...n, [sym]: sn }));
        if ((sn.summary_status === "pending" || sn.summary_status === "busy") && attempt < 10) {
          window.setTimeout(() => loadNews(sym, attempt + 1), 3000);
        }
      })
      .catch(() => setNews((n) => (attempt === 0 ? { ...n, [sym]: "error" } : n)));
  }

  function toggle(sym: string) {
    setExpanded((cur) => {
      if (cur === sym) return null;
      if (news[sym] === undefined) loadNews(sym);
      return sym;
    });
  }

  const colProps = {
    badges,
    expanded,
    news,
    onToggle: toggle,
    onAdd: add,
    onPick,
  };

  return (
    <div className="movers-panel">
      <div className="movers-panel-head">
        <button className="movers-panel-toggle" onClick={() => setOpen((o) => !o)}>
          {open ? "▾" : "▸"} Market movers — top gainers &amp; losers (whole market, live)
        </button>
        {msg && <span className="movers-msg">{msg}</span>}
      </div>
      {open && (
        <div className="movers-cols2">
          <MoverCol title="▲ Top gainers" tone="pos" rows={data?.gainers ?? []} {...colProps} />
          <MoverCol title="▼ Top losers" tone="neg" rows={data?.losers ?? []} {...colProps} />
        </div>
      )}
    </div>
  );
}

function MoverCol({
  title,
  tone,
  rows,
  badges,
  expanded,
  news,
  onToggle,
  onAdd,
  onPick,
}: {
  title: string;
  tone: "pos" | "neg";
  rows: MarketMover[];
  badges: Record<string, MoverNews>;
  expanded: string | null;
  news: Record<string, NewsState>;
  onToggle: (sym: string) => void;
  onAdd: (sym: string, where: "exec" | "watch") => void;
  onPick?: (sym: string) => void;
}) {
  return (
    <div className="mover2-col">
      <div className={`mover2-title ${tone}`}>{title}</div>
      {rows.length === 0 ? (
        <div className="empty">Loading…</div>
      ) : (
        <div className="mover2-scroll">
          {rows.map((r, i) => {
            const isOpen = expanded === r.symbol;
            return (
              <Fragment key={r.symbol}>
                <div
                  className={`mover2-row ${onPick ? "clickable" : ""}`}
                  onClick={onPick ? () => onPick(r.symbol) : undefined}
                  role={onPick ? "button" : undefined}
                  title={onPick ? "Click row for a quick chart" : undefined}
                >
                  <span className="mover2-rank">{i + 1}</span>
                  <span className="mover2-symwrap">
                    <button
                      className={`mover2-sym mono-strong ${isOpen ? "open" : ""}`}
                      onClick={(e) => {
                        e.stopPropagation();
                        onToggle(r.symbol);
                      }}
                      title="Show news"
                    >
                      <span className="sym-caret">{isOpen ? "▾" : "▸"}</span>
                      {r.symbol}
                    </button>
                    {tone === "neg" && <Badge b={badges[r.symbol]} />}
                  </span>
                  <span className="mover2-price">${r.price.toFixed(2)}</span>
                  <span className={`mover2-pct ${tone}`}>
                    {r.percent_change >= 0 ? "+" : ""}
                    {r.percent_change.toFixed(1)}%
                  </span>
                  <span className="mover2-acts">
                    <button onClick={(e) => { e.stopPropagation(); onAdd(r.symbol, "exec"); }} title="Add to Execution">+E</button>
                    <button onClick={(e) => { e.stopPropagation(); onAdd(r.symbol, "watch"); }} title="Add to Watchlist">+W</button>
                  </span>
                </div>
                {isOpen && (
                  <div className="news-dd">
                    <NewsDropdown state={news[r.symbol]} />
                  </div>
                )}
              </Fragment>
            );
          })}
        </div>
      )}
    </div>
  );
}

// Badge tags a faller: KNIFE (negative news — avoid), DIP? (no fresh bad news — candidate),
// or NEWS (has fresh non-negative news — check first).
function Badge({ b }: { b?: MoverNews }) {
  if (!b) return null;
  if (b.sentiment === "negative") {
    return (
      <span className="mover2-badge knife" title="Falling on negative news — likely keeps dropping. Avoid the knife.">
        ⚠ KNIFE
      </span>
    );
  }
  if (!b.has_catalyst) {
    return (
      <span className="mover2-badge dip" title="Falling with no fresh bad news — candidate mean-reversion dip.">
        DIP?
      </span>
    );
  }
  return (
    <span className="mover2-badge news" title="Has fresh news that isn't clearly negative — open the dropdown and check.">
      NEWS
    </span>
  );
}

function NewsDropdown({ state }: { state: NewsState }) {
  if (state === undefined || state === "loading") return <div className="news-dd-status">Loading news…</div>;
  if (state === "error") return <div className="news-dd-status">Couldn't load news.</div>;
  const sn = state;
  const headlines = sn.headlines ?? [];
  return (
    <>
      <SummaryBlock sn={sn} />
      {headlines.length > 0 ? (
        <div className="news-dd-list">
          {headlines.map((h, i) => (
            <a
              key={i}
              className="news-dd-item"
              href={h.url || undefined}
              target="_blank"
              rel="noreferrer"
            >
              <span className={`sent-dot ${dotClass(h.sentiment)}`} />
              <span className="news-dd-head">{h.headline}</span>
              <span className="news-dd-src">
                {h.source}
                {h.created_at ? ` · ${ago(h.created_at)}` : ""}
              </span>
            </a>
          ))}
        </div>
      ) : (
        sn.summary_status !== "no_news" && <div className="news-dd-status">No recent headlines.</div>
      )}
    </>
  );
}

function SummaryBlock({ sn }: { sn: StockNews }) {
  switch (sn.summary_status) {
    case "ok":
      return (
        <div className="news-dd-summary">
          <span className="news-dd-ai">AI summary</span>
          {sn.summary}
        </div>
      );
    case "pending":
    case "busy":
      return <div className="news-dd-status">⏳ Generating AI summary…</div>;
    case "no_news":
      return <div className="news-dd-status">No recent news for this stock.</div>;
    case "budget":
      return <div className="news-dd-status">AI summary paused — daily limit reached. Headlines below.</div>;
    case "error":
      return <div className="news-dd-status">AI summary unavailable (check the Gemini key). Headlines below.</div>;
    default: // "disabled" — Gemini off; just show headlines
      return null;
  }
}

function dotClass(s: string) {
  return s === "positive" ? "pos" : s === "negative" ? "neg" : "neu";
}

function ago(iso: string) {
  const t = new Date(iso).getTime();
  if (!t) return "";
  const mins = Math.max(0, Math.floor((Date.now() - t) / 60000));
  if (mins < 60) return `${mins}m`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h`;
  return `${Math.floor(hrs / 24)}d`;
}
