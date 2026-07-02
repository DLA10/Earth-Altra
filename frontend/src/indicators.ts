import type { Candle } from "./types";

// Indicator math for the Bollinger + RSI "Combo" strategy. Computed natively from the
// candle series (Lightweight Charts doesn't run Pine Script). Bands use population
// standard deviation (matching TradingView's ta.stdev); RSI uses Wilder's smoothing
// (matching ta.rsi), the conventional definition for the 30/70 thresholds.

export interface Point {
  time: number;
  value: number;
}

export interface Bollinger {
  basis: Point[];
  upper: Point[];
  lower: Point[];
}

export type Signal = "BUY" | "SELL" | "WAIT";
export type Strength = "strong" | "weak" | null;

export interface SignalResult {
  signal: Signal;
  strength: Strength;
  reason: string;
  rsi: number | null;
  close: number;
  upper: number | null;
  lower: number | null;
}

// grade applies the AND/strength logic for one bar:
//   STRONG  = both the band and the RSI agree (high conviction)
//   WEAK    = only one of them is triggered
//   WAIT    = neither
export function grade(
  close: number,
  upper: number | null,
  lower: number | null,
  r: number | null
): { signal: Signal; strength: Strength } {
  const buyBand = lower != null && close <= lower;
  const buyRsi = r != null && r <= 30;
  const sellBand = upper != null && close >= upper;
  const sellRsi = r != null && r >= 70;

  const buyLvl = buyBand && buyRsi ? 2 : buyBand || buyRsi ? 1 : 0;
  const sellLvl = sellBand && sellRsi ? 2 : sellBand || sellRsi ? 1 : 0;

  if (buyLvl > sellLvl) return { signal: "BUY", strength: buyLvl === 2 ? "strong" : "weak" };
  if (sellLvl > buyLvl) return { signal: "SELL", strength: sellLvl === 2 ? "strong" : "weak" };
  return { signal: "WAIT", strength: null }; // neither, or a rare conflict
}

// bollinger computes SMA(length) ± mult·stdev(length).
export function bollinger(candles: Candle[], length = 20, mult = 2): Bollinger {
  const basis: Point[] = [];
  const upper: Point[] = [];
  const lower: Point[] = [];
  for (let i = length - 1; i < candles.length; i++) {
    let sum = 0;
    for (let j = i - length + 1; j <= i; j++) sum += candles[j].close;
    const mean = sum / length;
    let variance = 0;
    for (let j = i - length + 1; j <= i; j++) {
      const d = candles[j].close - mean;
      variance += d * d;
    }
    const std = Math.sqrt(variance / length); // population stdev
    const t = candles[i].time;
    basis.push({ time: t, value: mean });
    upper.push({ time: t, value: mean + mult * std });
    lower.push({ time: t, value: mean - mult * std });
  }
  return { basis, upper, lower };
}

// rsi computes Wilder's RSI(length).
export function rsi(candles: Candle[], length = 14): Point[] {
  const out: Point[] = [];
  if (candles.length <= length) return out;

  let gainSum = 0;
  let lossSum = 0;
  for (let i = 1; i <= length; i++) {
    const delta = candles[i].close - candles[i - 1].close;
    if (delta >= 0) gainSum += delta;
    else lossSum += -delta;
  }
  let avgGain = gainSum / length;
  let avgLoss = lossSum / length;

  const push = (i: number) => {
    const rs = avgLoss === 0 ? Infinity : avgGain / avgLoss;
    const v = avgLoss === 0 ? 100 : 100 - 100 / (1 + rs);
    out.push({ time: candles[i].time, value: v });
  };
  push(length);

  for (let i = length + 1; i < candles.length; i++) {
    const delta = candles[i].close - candles[i - 1].close;
    const gain = delta > 0 ? delta : 0;
    const loss = delta < 0 ? -delta : 0;
    avgGain = (avgGain * (length - 1) + gain) / length;
    avgLoss = (avgLoss * (length - 1) + loss) / length;
    push(i);
  }
  return out;
}

// evaluate returns the graded strategy signal for the latest bar (STRONG = band and
// RSI agree; WEAK = only one; WAIT = neither), with a human-readable reason.
export function evaluate(candles: Candle[], bb: Bollinger, rsiArr: Point[]): SignalResult {
  const empty: SignalResult = { signal: "WAIT", strength: null, reason: "warming up…", rsi: null, close: 0, upper: null, lower: null };
  if (candles.length === 0) return empty;
  const close = candles[candles.length - 1].close;
  const upper = bb.upper.length ? bb.upper[bb.upper.length - 1].value : null;
  const lower = bb.lower.length ? bb.lower[bb.lower.length - 1].value : null;
  const r = rsiArr.length ? rsiArr[rsiArr.length - 1].value : null;

  const { signal, strength } = grade(close, upper, lower, r);
  const reasons: string[] = [];
  if (signal === "BUY") {
    if (lower != null && close <= lower) reasons.push("price at/below lower band");
    if (r != null && r <= 30) reasons.push(`RSI ${r.toFixed(0)} ≤ 30 (oversold)`);
  } else if (signal === "SELL") {
    if (upper != null && close >= upper) reasons.push("price at/above upper band");
    if (r != null && r >= 70) reasons.push(`RSI ${r.toFixed(0)} ≥ 70 (overbought)`);
  } else {
    reasons.push(r != null ? `RSI ${r.toFixed(0)}, inside bands` : "inside bands");
  }

  return { signal, strength, reason: reasons.join(" · "), rsi: r, close, upper, lower };
}
