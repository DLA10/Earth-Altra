import { useCallback, useEffect, useMemo, useState } from "react";
import { Chart } from "./Chart";
import { RangeToggle } from "./RangeToggle";
import { useWebSocket } from "../hooks/useWebSocket";
import { useHistoryBars, mergeLastBar } from "../hooks/useHistoryBars";
import { bollinger, rsi } from "../indicators";
import { api } from "../api/client";
import { isRange, viewToTimeframe } from "../types";
import type { Candle, ChartView, ScanState, WsMessage } from "../types";

// Merge a streamed candle into the series (replace-last / append / ignore-stale).
function upsert(arr: Candle[], c: Candle): Candle[] {
  if (arr.length === 0) return [c];
  const last = arr[arr.length - 1];
  if (c.time === last.time) {
    const copy = arr.slice();
    copy[copy.length - 1] = c;
    return copy;
  }
  if (c.time > last.time) return [...arr, c];
  return arr;
}

// LivePopupChart streams the symbol's candles over its OWN WebSocket connection (so it
// can never disturb the Execution chart's subscription) and renders the live indicator
// chart. The backend brings any symbol live on demand the moment we subscribe, so even a
// whole-market mover that wasn't being tracked starts streaming sub-second.
function LivePopupChart({ symbol }: { symbol: string }) {
  const [candles, setCandles] = useState<Candle[]>([]);
  const [last, setLast] = useState(0);
  const [view, setView] = useState<ChartView>("1m");
  const showIndicators = localStorage.getItem("lo.indicators") !== "off";
  const range = isRange(view) ? view : null;
  const tf = range ? 1 : viewToTimeframe(view);

  const handle = useCallback(
    (m: WsMessage) => {
      if (m.type === "snapshot") {
        if (m.data.symbol === symbol && m.data.timeframe === tf) setCandles(m.data.candles ?? []);
      } else if (m.type === "candle") {
        if (m.data.symbol === symbol && m.data.timeframe === tf) setCandles((prev) => upsert(prev, m.data.candle));
      } else if (m.type === "quote") {
        if (m.data.symbol === symbol) setLast(m.data.price);
      }
    },
    [symbol, tf]
  );

  const { status, send } = useWebSocket(handle);

  useEffect(() => {
    setCandles([]);
    if (status === "open") send({ action: "subscribe", symbol, timeframe: tf });
  }, [status, send, symbol, tf]);

  // Historical ranges load static REST bars; the latest bar ticks with the live price.
  const price = last || candles[candles.length - 1]?.close || 0;
  const historyBars = useHistoryBars(symbol, range);
  const displayCandles = useMemo(
    () => (range ? mergeLastBar(historyBars, price) : candles),
    [range, historyBars, price, candles]
  );
  const seriesKey = range ? `${symbol}|${range}` : `${symbol}|${tf}`;

  const bands = useMemo(() => bollinger(displayCandles), [displayCandles]);
  const rsiData = useMemo(() => rsi(displayCandles), [displayCandles]);

  return (
    <div className="cm-live">
      <RangeToggle view={view} onChange={setView} />
      <Chart
        candles={displayCandles}
        seriesKey={seriesKey}
        showIndicators={showIndicators}
        bands={bands}
        rsiData={rsiData}
        history={!!range}
      />
    </div>
  );
}

const fmtPct = (n: number) => `${n >= 0 ? "+" : ""}${n.toFixed(2)}%`;
const fmtPrice = (n: number) => (n > 0 ? `$${n.toFixed(2)}` : "—");

// ChartModal is the enlarged, focused candlestick view for a single symbol. Opened by
// clicking a heatmap tile or a chart card. Closes on ✕, backdrop click, or Esc.
export function ChartModal({
  symbol,
  company,
  state,
  onClose,
}: {
  symbol: string;
  company?: string;
  state?: ScanState;
  onClose: () => void;
}) {
  const [addState, setAddState] = useState<"idle" | "adding" | "added" | "error">("idle");
  const [addMsg, setAddMsg] = useState("");
  const [watchState, setWatchState] = useState<"idle" | "adding" | "added" | "error">("idle");

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  async function addToExecution() {
    setAddState("adding");
    try {
      await api.addExecSymbol(symbol);
      setAddState("added");
      setAddMsg(`${symbol} is ready to trade on the Execution page.`);
    } catch (e) {
      setAddState("error");
      setAddMsg((e as Error).message);
    }
  }

  async function addToWatchlist() {
    setWatchState("adding");
    try {
      await api.addWatchSymbol(symbol);
      setWatchState("added");
      setAddMsg(`${symbol} added to your Watchlist page.`);
    } catch (e) {
      setWatchState("error");
      setAddMsg((e as Error).message);
    }
  }

  return (
    <div className="chart-modal-backdrop" onClick={onClose}>
      <div className="chart-modal" onClick={(e) => e.stopPropagation()}>
        <div className="chart-modal-head">
          <div className="cm-title">
            <span className="cm-sym">{symbol}</span>
            {company && <span className="cm-company">{company}</span>}
          </div>
          {state && (
            <div className="cm-stats">
              <Stat label="Last" value={fmtPrice(state.price)} />
              <Stat label="Chg" value={fmtPct(state.chg_close_pct)} tone={state.chg_close_pct >= 0 ? "pos" : "neg"} />
              <Stat label="From open" value={fmtPct(state.chg_open_pct)} tone={state.chg_open_pct >= 0 ? "pos" : "neg"} />
              <Stat label="RVOL" value={state.rvol > 0 ? `${state.rvol.toFixed(1)}x` : "—"} />
              <Stat label="VWAP" value={fmtPrice(state.vwap)} />
              <Stat label="Day range" value={state.day_low > 0 ? `${state.day_low.toFixed(2)}–${state.day_high.toFixed(2)}` : "—"} />
            </div>
          )}
          <button
            className={`cm-add watch ${watchState === "added" ? "done" : ""}`}
            onClick={addToWatchlist}
            disabled={watchState === "adding" || watchState === "added"}
          >
            {watchState === "idle" && "+ Watchlist"}
            {watchState === "adding" && "Adding…"}
            {watchState === "added" && "✓ Watchlisted"}
            {watchState === "error" && "Retry"}
          </button>
          <button
            className={`cm-add ${addState === "added" ? "done" : ""}`}
            onClick={addToExecution}
            disabled={addState === "adding" || addState === "added"}
          >
            {addState === "idle" && "+ Execution"}
            {addState === "adding" && "Adding…"}
            {addState === "added" && "✓ Added"}
            {addState === "error" && "Retry add"}
          </button>
          <button className="cm-close" onClick={onClose} aria-label="Close">✕</button>
        </div>
        {addMsg && <div className={`cm-addmsg ${addState === "error" ? "err" : "ok"}`}>{addMsg}</div>}
        {state?.catalyst && <div className="cm-catalyst">{state.catalyst}</div>}
        <div className="chart-modal-body">
          <LivePopupChart symbol={symbol} />
        </div>
      </div>
    </div>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: "pos" | "neg" }) {
  return (
    <div className="cm-stat">
      <span className="cm-stat-label">{label}</span>
      <span className={`cm-stat-value ${tone ?? ""}`}>{value}</span>
    </div>
  );
}
