import { useCallback, useEffect, useMemo, useState } from "react";
import { Chart } from "./Chart";
import { NewsPanel } from "./NewsPanel";
import { StrategyBadge } from "./StrategyBadge";
import { useWebSocket } from "../hooks/useWebSocket";
import { bollinger, rsi, evaluate } from "../indicators";
import { useHistoryBars, mergeLastBar } from "../hooks/useHistoryBars";
import { isRange, viewToTimeframe } from "../types";
import type { Candle, ChartView, WsMessage } from "../types";

interface Props {
  symbol: string;
  company?: string;
  sector?: string;
  view: ChartView;
  onSendToExecution?: (symbol: string) => void;
  onRemove?: (symbol: string) => void;
  onHandleDragStart?: () => void;
}

// Merge a streamed candle into a series array.
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

// LiveChart is a self-contained, full-size, sub-second candlestick chart for one
// symbol: it opens its own WebSocket, subscribes to the symbol's candles, and renders
// the same indicator-rich Chart used on the Execution page (Bollinger bands + a live
// BUY/SELL/WAIT signal badge).
export function LiveChart({ symbol, company, sector, view, onSendToExecution, onRemove, onHandleDragStart }: Props) {
  const [candles, setCandles] = useState<Candle[]>([]);
  const [last, setLast] = useState(0);
  const [showIndicators, setShowIndicators] = useState<boolean>(
    () => localStorage.getItem("lo.indicators") !== "off"
  );
  // A historical range shows static REST bars; intraday keeps the live 1m stream warm.
  const range = isRange(view) ? view : null;
  const timeframe = range ? 1 : viewToTimeframe(view);

  const handle = useCallback(
    (m: WsMessage) => {
      if (m.type === "snapshot") {
        if (m.data.symbol === symbol && m.data.timeframe === timeframe) setCandles(m.data.candles ?? []);
      } else if (m.type === "candle") {
        if (m.data.symbol === symbol && m.data.timeframe === timeframe) {
          setCandles((prev) => upsert(prev, m.data.candle));
        }
      } else if (m.type === "quote") {
        if (m.data.symbol === symbol) setLast(m.data.price);
      }
    },
    [symbol, timeframe]
  );

  const { status, send } = useWebSocket(handle);

  useEffect(() => {
    setCandles([]);
    if (status === "open") send({ action: "subscribe", symbol, timeframe });
  }, [status, send, symbol, timeframe]);

  const price = last || candles[candles.length - 1]?.close || 0;
  const ref = candles[0]?.close ?? 0;
  const chgPct = ref > 0 && price > 0 ? ((price - ref) / ref) * 100 : 0;

  // Historical ranges load static REST bars (latest bar ticks with the live price);
  // intraday uses the live WS series. Header price/% always reflect the live stream.
  const historyBars = useHistoryBars(symbol, range);
  const displayCandles = useMemo(
    () => (range ? mergeLastBar(historyBars, price) : candles),
    [range, historyBars, price, candles]
  );
  const seriesKey = range ? `${symbol}|${range}` : `${symbol}|${timeframe}`;

  const bands = useMemo(() => bollinger(displayCandles), [displayCandles]);
  const rsiData = useMemo(() => rsi(displayCandles), [displayCandles]);
  const signal = useMemo(() => evaluate(displayCandles, bands, rsiData), [displayCandles, bands, rsiData]);

  function toggleIndicators() {
    setShowIndicators((on) => {
      const next = !on;
      localStorage.setItem("lo.indicators", next ? "on" : "off");
      return next;
    });
  }

  return (
    <div className="lc">
      <div className="lc-head">
        <div className="lc-id">
          {/* Drag handle — the watchlist page reorders charts by dragging this grip. */}
          {onHandleDragStart && (
            <span
              className="lc-grip"
              title="Drag to reorder"
              draggable
              onDragStart={onHandleDragStart}
            >
              ⠿
            </span>
          )}
          <span className="lc-sym">{symbol}</span>
          {company && company !== symbol && <span className="lc-company">{company}</span>}
          {sector && <span className="lc-sector">{sector}</span>}
          <span className="lc-price">{price > 0 ? `$${price.toFixed(2)}` : "—"}</span>
          <span className={`lc-chg ${chgPct >= 0 ? "pos" : "neg"}`}>
            {price > 0 && ref > 0 ? `${chgPct >= 0 ? "+" : ""}${chgPct.toFixed(2)}%` : ""}
          </span>
        </div>
        <div className="lc-actions">
          {showIndicators && <StrategyBadge result={signal} />}
          <button
            className={`ind-toggle ${showIndicators ? "on" : ""}`}
            onClick={toggleIndicators}
            title="Show/hide Bollinger Bands"
          >
            Indicators
          </button>
          {onSendToExecution && (
            <button className="lc-send" onClick={() => onSendToExecution(symbol)} title="Send to Execution">
              → Execution
            </button>
          )}
          {onRemove && (
            <button className="lc-remove" onClick={() => onRemove(symbol)} title="Remove from watchlist">✕</button>
          )}
        </div>
      </div>
      <div className="lc-chart">
        <Chart candles={displayCandles} seriesKey={seriesKey} showIndicators={showIndicators} bands={bands} rsiData={rsiData} history={!!range} />
      </div>
      <NewsPanel symbol={symbol} />
    </div>
  );
}
