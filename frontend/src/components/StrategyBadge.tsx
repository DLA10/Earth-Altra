import type { SignalResult } from "../indicators";

// StrategyBadge renders the live Bollinger+RSI verdict for the latest bar:
// BUY / SELL / WAIT, dimmed for a "weak" (one trigger) vs. ringed for a "strong"
// (band AND RSI agree) signal, with a plain-language reason.
export function StrategyBadge({ result }: { result: SignalResult }) {
  const tone = result.signal.toLowerCase(); // buy | sell | wait
  const strength = result.strength ?? ""; // strong | weak | ""
  const label =
    result.signal === "WAIT"
      ? "WAIT"
      : `${result.strength === "strong" ? "STRONG " : ""}${result.signal}`;
  return (
    <div className={`signal-badge ${tone} ${strength}`} title={result.reason}>
      <span className="sig-label">{label}</span>
      <span className="sig-reason">{result.reason}</span>
    </div>
  );
}
