import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import { LazyMount } from "./components/LazyMount";
import { LiveChart } from "./components/LiveChart";
import { RangeToggle } from "./components/RangeToggle";
import { applyOrder, loadOrder, move, saveOrder } from "./order";
import type { ChartView, IntervalRank, Mover, SymbolMeta, WsMessage } from "./types";

const ORDER_KEY = "lo.watchOrder";

export function Watchlist() {
  const [symbols, setSymbols] = useState<string[]>([]);
  const [meta, setMeta] = useState<Record<string, SymbolMeta>>({});
  const [view, setView] = useState<ChartView>("1m");
  const [analysis, setAnalysis] = useState<IntervalRank[]>([]);
  const [activeMark, setActiveMark] = useState(5);
  const [toast, setToast] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [order, setOrder] = useState<string[]>([]);
  const dragFrom = useRef<number | null>(null);

  const flash = (m: string) => {
    setToast(m);
    setTimeout(() => setToast(""), 4000);
  };

  const loadAnalysis = useCallback(() => {
    api.openingAnalysis().then(setAnalysis).catch(() => setAnalysis([]));
  }, []);

  useEffect(() => {
    api
      .watchlistSymbols()
      .then((r) => setSymbols(r.symbols))
      .catch(() => {})
      .finally(() => setLoaded(true));
  }, []);

  // Keep the chart display order in sync (custom order persisted; new ones appended).
  useEffect(() => {
    setOrder(applyOrder(symbols, loadOrder(ORDER_KEY)));
  }, [symbols]);

  useEffect(() => {
    loadAnalysis();
  }, [symbols, loadAnalysis]);

  // Resolve company name + sector for chart headings.
  useEffect(() => {
    const missing = symbols.filter((s) => !meta[s]);
    if (missing.length === 0) return;
    api.symbolMeta(missing).then((m) => setMeta((prev) => ({ ...prev, ...m }))).catch(() => {});
  }, [symbols, meta]);

  useEffect(() => {
    const id = window.setInterval(loadAnalysis, 30000);
    return () => window.clearInterval(id);
  }, [loadAnalysis]);

  // Keep the symbol list in sync if it changes elsewhere (e.g. added from DECEPTICON).
  const onMsg = useRef((m: WsMessage) => {
    if (m.type === "watch_symbols") setSymbols(m.data);
  });
  useWebSocket((m) => onMsg.current(m));

  const scrollToChart = (sym: string) => {
    document.getElementById(`wlchart-${sym}`)?.scrollIntoView({ behavior: "smooth", block: "start" });
  };

  function onDrop(toIndex: number) {
    const from = dragFrom.current;
    dragFrom.current = null;
    if (from === null || from === toIndex) return;
    const next = move(order, from, toIndex);
    setOrder(next);
    saveOrder(ORDER_KEY, next);
  }

  async function sendToExecution(sym: string) {
    try {
      await api.addExecSymbol(sym);
      flash(`${sym} sent to Execution — ready to trade.`);
    } catch (e) {
      flash(`Failed: ${(e as Error).message}`);
    }
  }
  async function addToWatch(sym: string) {
    try {
      const r = await api.addWatchSymbol(sym);
      setSymbols(r.symbols);
      flash(`${sym} added to watchlist.`);
    } catch (e) {
      flash(`Failed: ${(e as Error).message}`);
    }
  }
  async function removeFromWatch(sym: string) {
    try {
      const r = await api.removeWatchSymbol(sym);
      setSymbols(r.symbols);
    } catch (e) {
      flash(`Failed: ${(e as Error).message}`);
    }
  }

  const current = analysis.find((a) => a.minutes === activeMark);

  return (
    <div className="watchpage">
      {/* Opening-movers ranking */}
      <section className="movers">
        <div className="movers-head">
          <div>
            <h2 className="movers-title">Opening movers</h2>
            <p className="movers-sub">Your watchlist only · ranked by % move from today's 9:30 AM ET open · click a ticker to jump to its chart</p>
          </div>
          <button className="movers-refresh" onClick={loadAnalysis}>↻ Refresh</button>
        </div>
        <div className="mark-tabs">
          {[5, 15, 30, 45, 60].map((m) => {
            const a = analysis.find((x) => x.minutes === m);
            return (
              <button
                key={m}
                className={`mark-tab ${activeMark === m ? "on" : ""} ${a && !a.elapsed ? "pending" : ""}`}
                onClick={() => setActiveMark(m)}
              >
                +{m} min{a && !a.elapsed ? " ·  pending" : ""}
              </button>
            );
          })}
        </div>

        {!current || !current.elapsed ? (
          <div className="movers-empty">
            No +{activeMark} min ranking right now. It appears once the regular session (9:30 AM ET) has run
            {" "}{activeMark} minutes, and clears after the 8 PM ET after-hours close until the next day's open.
          </div>
        ) : (
          <div className="movers-cols">
            <MoverCol title="▲ Top gainers" tone="pos" rows={current.rising} onExec={sendToExecution} onWatch={addToWatch} onPick={scrollToChart} watched={symbols} />
            <MoverCol title="▼ Top fallers" tone="neg" rows={current.falling} onExec={sendToExecution} onWatch={addToWatch} onPick={scrollToChart} watched={symbols} />
          </div>
        )}
      </section>

      {/* Stacked full-size live charts */}
      <section className="charts-head">
        <h2 className="movers-title">Watchlist · {symbols.length} stocks <span className="muted small">· drag the ⠿ handle to reorder</span></h2>
        <RangeToggle view={view} onChange={setView} />
      </section>

      {loaded && symbols.length === 0 && (
        <div className="movers-empty">No stocks yet. Add some from DECEPTICON or the movers list above.</div>
      )}

      <div className="watch-charts">
        {order.map((sym, i) => (
          <div
            key={sym}
            id={`wlchart-${sym}`}
            className="wl-chart-wrap"
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              e.preventDefault();
              onDrop(i);
            }}
          >
            <LazyMount minHeight={520}>
              <LiveChart
                symbol={sym}
                company={meta[sym]?.name}
                sector={meta[sym]?.sector}
                view={view}
                onSendToExecution={sendToExecution}
                onRemove={removeFromWatch}
                onHandleDragStart={() => (dragFrom.current = i)}
              />
            </LazyMount>
          </div>
        ))}
      </div>

      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}

function MoverCol({
  title,
  tone,
  rows,
  onExec,
  onWatch,
  onPick,
  watched,
}: {
  title: string;
  tone: "pos" | "neg";
  rows: Mover[];
  onExec: (s: string) => void;
  onWatch: (s: string) => void;
  onPick: (s: string) => void;
  watched: string[];
}) {
  const set = new Set(watched);
  return (
    <div className="mover-col">
      <div className={`mover-col-title ${tone}`}>{title}</div>
      {rows.length === 0 ? (
        <div className="movers-empty small">None.</div>
      ) : (
        rows.map((m) => (
          <div key={m.symbol} className="mover-line">
            <button className="mover-sym" title="Jump to chart" onClick={() => onPick(m.symbol)}>{m.symbol}</button>
            <span className="muted">${m.price.toFixed(2)}</span>
            <span className={tone}>{m.pct >= 0 ? "+" : ""}{m.pct.toFixed(2)}%</span>
            <span className="mover-btns">
              <button className="mini-act" title="Send to Execution" onClick={() => onExec(m.symbol)}>→ Exec</button>
              {!set.has(m.symbol) && (
                <button className="mini-act" title="Add to watchlist" onClick={() => onWatch(m.symbol)}>+ Watch</button>
              )}
            </span>
          </div>
        ))
      )}
    </div>
  );
}
