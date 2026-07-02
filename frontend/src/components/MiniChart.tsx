import { useEffect, useRef, useState } from "react";
import {
  createChart,
  ColorType,
  type IChartApi,
  type UTCTimestamp,
} from "lightweight-charts";
import { api } from "../api/client";

// MiniChart fetches a symbol's 1-minute session bars on mount and renders candlesticks
// with a VWAP overlay. When Alpaca has no data for the symbol (delisted / merged /
// renamed / off-SIP), it shows a clear placeholder instead of an empty chart box.
// variant="large" fills its container (used by the enlarged ChartModal).
export function MiniChart({
  symbol,
  variant = "mini",
}: {
  symbol: string;
  variant?: "mini" | "large";
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<"loading" | "ready" | "empty">("loading");

  useEffect(() => {
    let chart: IChartApi | null = null;
    let disposed = false;
    setStatus("loading");

    api
      .decepticonBars(symbol)
      .then(({ bars, vwap }) => {
        if (disposed) return;
        if (!bars || bars.length === 0) {
          setStatus("empty");
          return;
        }
        setStatus("ready");
        // ref is rendered for loading/ready states; guard anyway.
        if (!ref.current) return;
        chart = createChart(ref.current, {
          layout: {
            background: { type: ColorType.Solid, color: "#0b0f16" },
            textColor: "#6e7787",
            fontSize: variant === "large" ? 11 : 10,
            attributionLogo: false,
          },
          grid: {
            vertLines: { color: "#141a24" },
            horzLines: { color: "#141a24" },
          },
          rightPriceScale: { borderColor: "#141a24" },
          timeScale: { borderColor: "#141a24", timeVisible: true, secondsVisible: false },
          autoSize: true,
          handleScale: variant === "large",
          handleScroll: variant === "large",
        });
        const cs = chart.addCandlestickSeries({
          upColor: "#16c784",
          downColor: "#ea3943",
          borderUpColor: "#16c784",
          borderDownColor: "#ea3943",
          wickUpColor: "#16c784",
          wickDownColor: "#ea3943",
        });
        cs.setData(
          bars.map((b) => ({
            time: b.time as UTCTimestamp,
            open: b.open,
            high: b.high,
            low: b.low,
            close: b.close,
          }))
        );
        if (vwap && vwap.length > 0) {
          const vs = chart.addLineSeries({
            color: "#e0a13a",
            lineWidth: 1,
            priceLineVisible: false,
            lastValueVisible: false,
            crosshairMarkerVisible: false,
          });
          vs.setData(vwap.map((p) => ({ time: p.time as UTCTimestamp, value: p.value })));
        }
        chart.timeScale().fitContent();
      })
      .catch(() => {
        if (!disposed) setStatus("empty");
      });

    return () => {
      disposed = true;
      chart?.remove();
    };
  }, [symbol, variant]);

  const cls = variant === "large" ? "large-chart" : "mini-chart";

  if (status === "empty") {
    return (
      <div className={`${cls} chart-no-data`}>
        <span className="nd-title">No recent data</span>
        <span className="nd-sub">likely delisted, merged, or renamed since the watchlist date</span>
      </div>
    );
  }
  return <div ref={ref} className={cls} />;
}
