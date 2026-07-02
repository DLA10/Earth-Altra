import { useEffect, useState } from "react";
import { api } from "../api/client";
import type { Candle, ChartRange } from "../types";

// useHistoryBars fetches static historical bars when a range is selected (pass null for
// the live intraday timeframes, which stream over the WebSocket instead). Re-fetches on
// symbol/range change. A pure REST read — never touches the live candle stream.
export function useHistoryBars(symbol: string, range: ChartRange | null): Candle[] {
  const [bars, setBars] = useState<Candle[]>([]);
  useEffect(() => {
    if (!range || !symbol) {
      setBars([]);
      return;
    }
    let alive = true;
    setBars([]); // clear stale bars while the new range loads
    api
      .history(symbol, range)
      .then((r) => alive && setBars(r.bars ?? []))
      .catch(() => alive && setBars([]));
    return () => {
      alive = false;
    };
  }, [symbol, range]);
  return bars;
}

// mergeLastBar overrides the final bar's close/high/low with the live price so the most
// recent daily/hourly candle ticks live on a historical view, without re-fetching.
export function mergeLastBar(bars: Candle[], price: number): Candle[] {
  if (bars.length === 0 || !(price > 0)) return bars;
  const last = bars[bars.length - 1];
  const copy = bars.slice();
  copy[copy.length - 1] = {
    ...last,
    close: price,
    high: Math.max(last.high, price),
    low: Math.min(last.low, price),
  };
  return copy;
}
