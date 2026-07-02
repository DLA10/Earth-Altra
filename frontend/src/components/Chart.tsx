import { useEffect, useRef } from "react";
import {
  createChart,
  ColorType,
  CrosshairMode,
  LineStyle,
  type IChartApi,
  type IPriceLine,
  type ISeriesApi,
  type Time,
  type UTCTimestamp,
} from "lightweight-charts";
import type { Candle } from "../types";
import type { Bollinger, Point } from "../indicators";

interface Props {
  candles: Candle[];
  seriesKey: string; // `${symbol}|${timeframe}` — changing it snaps to the latest bars
  entryPrice?: number; // draws a green "you bought here" line when > 0
  showIndicators?: boolean; // overlay the Bollinger bands on the price chart
  showRsiPane?: boolean; // render a synced RSI sub-pane below the chart
  bands?: Bollinger; // precomputed Bollinger bands (so the badge + chart share one calc)
  rsiData?: Point[]; // precomputed Wilder RSI series
  history?: boolean; // a static historical range (1W/1M/6M/1Y): fit the whole span into view
  drawMode?: boolean; // when true, clicking the chart picks a price (visual order placement)
  onPriceSelect?: (price: number) => void; // called with the clicked price in draw mode
  draftPrice?: number | null; // a pending visual-order level to draw on the chart
  draftColor?: string; // color of the draft line (varies by chosen order type)
  draftLabel?: string; // axis label for the draft line (e.g. "take profit")
  orderLines?: OrderLine[]; // resting open orders to draw on the chart (TP/SL/limits/stops)
}

// OrderLine is one resting order drawn on the chart.
export interface OrderLine {
  id: string;
  price: number;
  color: string;
  label: string;
}

function asMs(time: Time): number {
  return (typeof time === "number" ? time : 0) * 1000;
}
const localTime = (time: Time) => new Date(asMs(time)).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const localDate = (time: Time) => new Date(asMs(time)).toLocaleDateString([], { month: "short", day: "numeric" });
const localDateTime = (time: Time) =>
  new Date(asMs(time)).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

// DEFAULT_BAR_SPACING gives a readable, non-compressed default zoom (instead of cramming
// the whole session in). The user's own zoom is a chart-level setting we never reset, so
// it persists across symbol switches and timeframe toggles.
const DEFAULT_BAR_SPACING = 9;

const toLine = (pts: Point[] | undefined) =>
  (pts ?? []).map((p) => ({ time: p.time as UTCTimestamp, value: p.value }));

// Chart renders a candlestick chart with a volume sub-pane. If entryPrice is given, a
// green horizontal line marks the price you bought at. With showIndicators it overlays
// the Bollinger bands; with showRsiPane it adds a time-synced RSI pane underneath.
export function Chart({ candles, seriesKey, entryPrice, showIndicators, showRsiPane, bands, rsiData, history, drawMode, onPriceSelect, draftPrice, draftColor, draftLabel, orderLines }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const rsiContainerRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<"Candlestick"> | null>(null);
  const volumeRef = useRef<ISeriesApi<"Histogram"> | null>(null);
  const upperRef = useRef<ISeriesApi<"Line"> | null>(null);
  const lowerRef = useRef<ISeriesApi<"Line"> | null>(null);
  const basisRef = useRef<ISeriesApi<"Line"> | null>(null);
  const rsiChartRef = useRef<IChartApi | null>(null);
  const rsiSeriesRef = useRef<ISeriesApi<"Line"> | null>(null);
  const entryLineRef = useRef<IPriceLine | null>(null);
  const draftLineRef = useRef<IPriceLine | null>(null);
  const orderLineRefs = useRef<IPriceLine[]>([]);
  const onPriceSelectRef = useRef(onPriceSelect);
  onPriceSelectRef.current = onPriceSelect;
  const lastKeyRef = useRef<string>("");

  // ---- Main price chart (created once) ----
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const chart = createChart(el, {
      layout: {
        background: { type: ColorType.Solid, color: "#0d1117" },
        textColor: "#9aa4b2",
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
        attributionLogo: false,
      },
      grid: { vertLines: { color: "#1b2230" }, horzLines: { color: "#1b2230" } },
      crosshair: {
        mode: CrosshairMode.Normal,
        vertLine: { color: "#8b95a7", width: 1, style: LineStyle.Dashed, labelVisible: true, labelBackgroundColor: "#2b3344" },
        horzLine: { color: "#8b95a7", width: 1, style: LineStyle.Dashed, labelVisible: true, labelBackgroundColor: "#2b3344" },
      },
      // Leave room at the bottom for the volume histogram.
      rightPriceScale: { borderColor: "#1b2230", scaleMargins: { top: 0.1, bottom: 0.26 }, minimumWidth: 64 },
      localization: { timeFormatter: localDateTime },
      timeScale: {
        borderColor: "#1b2230",
        timeVisible: true,
        secondsVisible: false,
        rightOffset: 6,
        barSpacing: DEFAULT_BAR_SPACING,
        tickMarkFormatter: (t: Time, type: number) => (type < 3 ? localDate(t) : localTime(t)),
      },
      // Full navigation: drag the chart to pan through time, drag the right price axis
      // (the numbers) to move the view up/down, mouse-wheel / pinch to zoom.
      handleScroll: { mouseWheel: true, pressedMouseMove: true, horzTouchDrag: true, vertTouchDrag: true },
      handleScale: {
        axisPressedMouseMove: { time: true, price: true },
        mouseWheel: true,
        pinch: true,
      },
      autoSize: true,
    });
    const series = chart.addCandlestickSeries({
      upColor: "#16c784",
      downColor: "#ea3943",
      borderUpColor: "#16c784",
      borderDownColor: "#ea3943",
      wickUpColor: "#16c784",
      wickDownColor: "#ea3943",
    });
    const volume = chart.addHistogramSeries({
      priceScaleId: "vol",
      priceFormat: { type: "volume" },
      priceLineVisible: false,
      lastValueVisible: false,
    });
    chart.priceScale("vol").applyOptions({ scaleMargins: { top: 0.82, bottom: 0 } });

    // Bollinger band overlays (red upper guardrail, green lower guardrail, gray middle).
    // Created up-front but only fed data when showIndicators is on.
    const bandOpts = { lineWidth: 1 as const, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false };
    const upper = chart.addLineSeries({ ...bandOpts, color: "#ef5350", title: "BB upper" });
    const lower = chart.addLineSeries({ ...bandOpts, color: "#26a69a", title: "BB lower" });
    const basis = chart.addLineSeries({ ...bandOpts, color: "#7d8694", lineStyle: LineStyle.Dotted, title: "BB mid" });

    chartRef.current = chart;
    seriesRef.current = series;
    volumeRef.current = volume;
    upperRef.current = upper;
    lowerRef.current = lower;
    basisRef.current = basis;
    return () => {
      chart.remove();
      chartRef.current = null;
      seriesRef.current = null;
      volumeRef.current = null;
      upperRef.current = lowerRef.current = basisRef.current = null;
      entryLineRef.current = null;
      draftLineRef.current = null;
      orderLineRefs.current = [];
      lastKeyRef.current = "";
    };
  }, []);

  // ---- RSI sub-pane: a second chart, time-synced with the price chart ----
  useEffect(() => {
    const main = chartRef.current;
    const el = rsiContainerRef.current;
    if (!showRsiPane || !main || !el) return;

    const rsiChart = createChart(el, {
      layout: {
        background: { type: ColorType.Solid, color: "#0d1117" },
        textColor: "#9aa4b2",
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
        attributionLogo: false,
      },
      grid: { vertLines: { color: "#1b2230" }, horzLines: { color: "#1b2230" } },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: "#1b2230", scaleMargins: { top: 0.15, bottom: 0.15 }, minimumWidth: 64 },
      timeScale: { borderColor: "#1b2230", visible: false },
      handleScroll: { mouseWheel: true, pressedMouseMove: true, horzTouchDrag: true, vertTouchDrag: true },
      handleScale: { axisPressedMouseMove: { time: true, price: false }, mouseWheel: true, pinch: true },
      autoSize: true,
    });
    const rsiSeries = rsiChart.addLineSeries({
      color: "#c08cff",
      lineWidth: 2,
      priceLineVisible: false,
      lastValueVisible: true,
    });
    // Overbought / oversold guides.
    rsiSeries.createPriceLine({ price: 70, color: "#6e1f27", lineWidth: 1, lineStyle: LineStyle.Dashed, axisLabelVisible: true, title: "70" });
    rsiSeries.createPriceLine({ price: 30, color: "#1f6e4a", lineWidth: 1, lineStyle: LineStyle.Dashed, axisLabelVisible: true, title: "30" });
    rsiSeries.createPriceLine({ price: 50, color: "#2a3344", lineWidth: 1, lineStyle: LineStyle.Dotted, axisLabelVisible: false, title: "" });
    rsiSeries.setData(toLine(rsiData));

    rsiChartRef.current = rsiChart;
    rsiSeriesRef.current = rsiSeries;

    // Keep both panes panned/zoomed together via their (shared) logical range.
    let syncing = false;
    const mainTs = main.timeScale();
    const rsiTs = rsiChart.timeScale();
    const fromMain = (r: ReturnType<typeof mainTs.getVisibleLogicalRange>) => {
      if (!r || syncing) return;
      syncing = true;
      rsiTs.setVisibleLogicalRange(r);
      syncing = false;
    };
    const fromRsi = (r: ReturnType<typeof rsiTs.getVisibleLogicalRange>) => {
      if (!r || syncing) return;
      syncing = true;
      mainTs.setVisibleLogicalRange(r);
      syncing = false;
    };
    mainTs.subscribeVisibleLogicalRangeChange(fromMain);
    rsiTs.subscribeVisibleLogicalRangeChange(fromRsi);
    const initial = mainTs.getVisibleLogicalRange();
    if (initial) rsiTs.setVisibleLogicalRange(initial);

    return () => {
      mainTs.unsubscribeVisibleLogicalRangeChange(fromMain);
      rsiChart.remove();
      rsiChartRef.current = null;
      rsiSeriesRef.current = null;
    };
  }, [showRsiPane]); // eslint-disable-line react-hooks/exhaustive-deps

  // Data: replace the full dataset (robust to symbol switches + snapshots). The view
  // (zoom/scroll) is preserved on live updates; on a symbol/timeframe change we only
  // snap to the latest bars — never re-fit — so the user's chosen zoom sticks.
  useEffect(() => {
    const series = seriesRef.current;
    const vol = volumeRef.current;
    if (!series || !vol) return;
    const sorted = [...candles].sort((a, b) => a.time - b.time);
    const bars: { time: UTCTimestamp; open: number; high: number; low: number; close: number }[] = [];
    const vols: { time: UTCTimestamp; value: number; color: string }[] = [];
    let prev = -1;
    for (const c of sorted) {
      const t = c.time as UTCTimestamp;
      const up = c.close >= c.open;
      const barRow = { time: t, open: c.open, high: c.high, low: c.low, close: c.close };
      const volRow = { time: t, value: c.volume, color: up ? "rgba(22,199,132,0.45)" : "rgba(234,57,67,0.45)" };
      if (c.time === prev) {
        bars[bars.length - 1] = barRow;
        vols[vols.length - 1] = volRow;
      } else {
        bars.push(barRow);
        vols.push(volRow);
        prev = c.time;
      }
    }
    series.setData(bars);
    vol.setData(vols);
    if (seriesKey !== lastKeyRef.current) {
      // New view: a historical range fits the whole span into view; a live intraday view
      // jumps to the most recent bars at the user's current zoom.
      //
      // Re-enable autoScale on the price (right) axis on every symbol/timeframe switch.
      // Dragging the price axis during a long session turns autoScale OFF (it's a sticky
      // per-scale flag), which leaves the price scale pinned to the OLD symbol's range — so
      // the new symbol's candles render off-screen and only the volume bars (on their own
      // 'vol' scale) show. Re-fitting the right scale snaps the candles back into view.
      chartRef.current?.priceScale("right").applyOptions({ autoScale: true });
      if (history) chartRef.current?.timeScale().fitContent();
      else chartRef.current?.timeScale().scrollToRealTime();
      lastKeyRef.current = seriesKey;
    }
  }, [candles, seriesKey, history]);

  // Bollinger band data — fed when indicators are on, cleared otherwise.
  useEffect(() => {
    if (!upperRef.current || !lowerRef.current || !basisRef.current) return;
    if (showIndicators && bands) {
      upperRef.current.setData(toLine(bands.upper));
      lowerRef.current.setData(toLine(bands.lower));
      basisRef.current.setData(toLine(bands.basis));
    } else {
      upperRef.current.setData([]);
      lowerRef.current.setData([]);
      basisRef.current.setData([]);
    }
  }, [showIndicators, bands]);

  // RSI line data (only when the pane exists).
  useEffect(() => {
    rsiSeriesRef.current?.setData(toLine(rsiData));
  }, [rsiData]);

  // Green "bought here" line.
  useEffect(() => {
    const series = seriesRef.current;
    if (!series) return;
    if (entryLineRef.current) {
      series.removePriceLine(entryLineRef.current);
      entryLineRef.current = null;
    }
    if (entryPrice && entryPrice > 0) {
      entryLineRef.current = series.createPriceLine({
        price: entryPrice,
        // Yellow (not green): keeps the "bought here" line distinct from the green/red
        // candles, so it stays readable once price crosses above the entry.
        color: "#ffd23f",
        lineWidth: 2,
        lineStyle: LineStyle.Solid,
        axisLabelVisible: true,
        title: "bought",
      });
    }
  }, [entryPrice, seriesKey]);

  // ---- Visual order placement: click the chart to pick a price level ----
  useEffect(() => {
    const chart = chartRef.current;
    const series = seriesRef.current;
    const el = containerRef.current;
    if (!chart || !series) return;
    if (!drawMode) {
      if (el) el.style.cursor = "";
      return;
    }
    if (el) el.style.cursor = "crosshair";
    const handler = (param: { point?: { x: number; y: number } }) => {
      if (!param.point) return;
      const price = series.coordinateToPrice(param.point.y);
      if (price != null && price > 0 && onPriceSelectRef.current) {
        onPriceSelectRef.current(Math.round(price * 100) / 100);
      }
    };
    chart.subscribeClick(handler);
    return () => {
      chart.unsubscribeClick(handler);
      if (el) el.style.cursor = "";
    };
  }, [drawMode]);

  // Draft line for the pending visual order (cyan, dashed — distinct from entry/candles).
  useEffect(() => {
    const series = seriesRef.current;
    if (!series) return;
    if (draftLineRef.current) {
      series.removePriceLine(draftLineRef.current);
      draftLineRef.current = null;
    }
    if (draftPrice && draftPrice > 0) {
      draftLineRef.current = series.createPriceLine({
        price: draftPrice,
        color: draftColor ?? "#3fc7ff",
        lineWidth: 2,
        lineStyle: LineStyle.Dashed,
        axisLabelVisible: true,
        title: draftLabel ?? "order",
      });
    }
  }, [draftPrice, draftColor, draftLabel, seriesKey]);

  // Resting open orders, drawn as solid colored lines (green TP, red SL, etc.) so you can
  // SEE your live orders sitting on the chart, not just in a list.
  useEffect(() => {
    const series = seriesRef.current;
    if (!series) return;
    for (const pl of orderLineRefs.current) series.removePriceLine(pl);
    orderLineRefs.current = [];
    for (const o of orderLines ?? []) {
      if (!(o.price > 0)) continue;
      orderLineRefs.current.push(
        series.createPriceLine({
          price: o.price,
          color: o.color,
          lineWidth: 1,
          lineStyle: LineStyle.Dotted,
          axisLabelVisible: true,
          title: o.label,
        })
      );
    }
  }, [orderLines, seriesKey]);

  return (
    <div className="chart-stack">
      <div ref={containerRef} className="chart" />
      {showRsiPane && (
        <div className="rsi-pane">
          <div ref={rsiContainerRef} className="rsi-chart" />
        </div>
      )}
    </div>
  );
}
