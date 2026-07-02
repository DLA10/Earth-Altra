import { useEffect, useMemo, useRef, useState } from "react";
import type { OrderRequest } from "../types";

// Order types available from a drawn line, chosen by whether the line is above or below the
// market. Each maps to a server-side Alpaca order (limit/stop) that triggers on its own.
export type DrawType = "buy_stop" | "buy_limit" | "sell_limit" | "sell_stop";

// Each order type gets its own line color + axis label, so a drawn level reads at a glance.
// Colors are chosen to stand apart from the candles: the candles use bright green (#16c784)
// and bright red (#ea3943), so take-profit / stop-loss use DARK green / wine red instead, and
// the buy levels use cyan / blue (which don't clash with the candle colors at all).
export const DRAW_MARKS: Record<DrawType, { color: string; label: string }> = {
  buy_limit: { color: "#3fc7ff", label: "buy dip" }, // cyan
  buy_stop: { color: "#5b8cff", label: "buy stop" }, // blue
  sell_limit: { color: "#1f8a4c", label: "take profit" }, // dark green
  sell_stop: { color: "#a01f2e", label: "stop loss" }, // wine red
};

interface Props {
  symbol: string;
  price: number; // the price level the user drew on the chart
  lastPrice: number; // current market price
  heldQty: number; // shares currently held of this symbol
  heldValue: number; // market value currently held
  fractionable: boolean; // whether dollar (notional) sizing is allowed for this symbol
  maxNotional: number;
  onReview: (req: OrderRequest, est: number) => void; // routes to the existing confirm modal
  onTypeChange?: (t: DrawType) => void; // report the selected type so the chart can recolor the line
  onClose: () => void;
}

const num = (s: string) => parseFloat(s) || 0;

// ChartOrderPopup turns a price drawn on the chart into a real order. It only SETS the price;
// the order itself is a normal Alpaca limit/stop that Alpaca triggers server-side. Every order
// still routes through the mandatory confirm modal (onReview) — nothing is placed from here.
export function ChartOrderPopup({ symbol, price, lastPrice, heldQty, heldValue, fractionable, maxNotional, onReview, onTypeChange, onClose }: Props) {
  const above = lastPrice > 0 && price > lastPrice;
  const below = lastPrice > 0 && price < lastPrice;

  // Contextual options: a level above the market can be a breakout BUY (buy-stop) or a
  // take-profit SELL (sell-limit); a level below can be a dip BUY (buy-limit) or a stop-loss
  // SELL (sell-stop). Selling options only appear when you actually hold the stock.
  const options = useMemo(() => {
    const out: { type: DrawType; label: string; explain: string }[] = [];
    if (above) {
      out.push({ type: "buy_stop", label: "Buy if it breaks up", explain: `Buys when ${symbol} rises to $${price.toFixed(2)} (a breakout buy).` });
      if (heldQty > 0) out.push({ type: "sell_limit", label: "Take profit (sell)", explain: `Sells when ${symbol} rises to $${price.toFixed(2)} (locks in a gain).` });
    } else if (below) {
      out.push({ type: "buy_limit", label: "Buy the dip", explain: `Buys when ${symbol} falls to $${price.toFixed(2)} (buy the dip).` });
      if (heldQty > 0) out.push({ type: "sell_stop", label: "Stop-loss (sell)", explain: `Sells when ${symbol} falls to $${price.toFixed(2)} (stops the bleeding).` });
    }
    return out;
  }, [above, below, heldQty, price, symbol]);

  const [type, setType] = useState<DrawType | null>(options[0]?.type ?? null);
  const [qty, setQty] = useState("");
  const [sizeMode, setSizeMode] = useState<"shares" | "dollars">("shares");
  const [err, setErr] = useState("");

  const sel = options.find((o) => o.type === type) ?? options[0];

  // Dollar (notional) sizing is allowed only for the two LIMIT draw-types (Alpaca rejects
  // dollar-amount stop orders) and only on a fractionable symbol.
  const isLimitType = sel?.type === "buy_limit" || sel?.type === "sell_limit";
  const dollarsAllowed = isLimitType && fractionable;
  const effSize = dollarsAllowed ? sizeMode : "shares";

  // Report the selected type up so the chart line recolors to match (TP green, SL red, etc.).
  const onTypeChangeRef = useRef(onTypeChange);
  onTypeChangeRef.current = onTypeChange;
  useEffect(() => {
    if (sel) onTypeChangeRef.current?.(sel.type);
  }, [sel]);

  // Whenever the effective order type changes (a button click, OR the line crossing the
  // market so the options flip), clear the amount and reset to shares — so a dollar figure is
  // never silently reinterpreted as a share count when the new type is a shares-only stop.
  useEffect(() => {
    setQty("");
    setSizeMode("shares");
  }, [sel?.type]);

  function submit() {
    setErr("");
    if (!sel) return setErr("Draw the line above or below the current price.");
    const isSell = sel.type === "sell_limit" || sel.type === "sell_stop";
    const isStop = sel.type === "buy_stop" || sel.type === "sell_stop";
    const req: OrderRequest = { symbol, side: isSell ? "sell" : "buy", type: isStop ? "stop" : "limit" };
    let est = 0;

    if (effSize === "dollars") {
      // Dollar-amount (notional) limit — must be a regular-hours DAY order (Alpaca rule).
      const n = num(qty);
      if (n <= 0) return setErr("Enter a dollar amount.");
      if (isSell && heldValue > 0 && n > heldValue + 0.01) return setErr(`You only hold $${heldValue.toFixed(2)} of ${symbol}.`);
      req.notional = n;
      req.time_in_force = "day";
      est = n;
    } else {
      const q = num(qty);
      if (q <= 0) return setErr("Enter a share quantity.");
      if (isSell && q > heldQty + 1e-9)
        return setErr(heldQty > 0 ? `You only hold ${heldQty} ${symbol}.` : `You don't hold any ${symbol} to sell.`);
      // Stops can't be fractional (Alpaca: fractional stops are day-only and we want it to rest).
      if (isStop && q % 1 !== 0) return setErr(`This order must be a whole number of shares (no fractions for stop orders).`);
      req.qty = q;
      req.time_in_force = q % 1 !== 0 ? "day" : "gtc"; // fractional → day; whole → rest as GTC
      est = q * price;
    }
    if (isStop) req.stop_price = price;
    else req.limit_price = price;

    if (maxNotional > 0 && est > maxNotional) return setErr(`Order ~$${est.toFixed(0)} exceeds the $${maxNotional.toFixed(0)} safety cap.`);
    onReview(req, est);
  }

  return (
    <div className="draw-popup">
      <div className="draw-popup-head">
        <span>Order at <strong>${price.toFixed(2)}</strong></span>
        <span className="muted small">{above ? "above" : below ? "below" : "at"} market (${lastPrice.toFixed(2)})</span>
        <button className="draw-popup-x" onClick={onClose} title="Cancel">✕</button>
      </div>

      {options.length === 0 ? (
        <div className="type-explain">Draw the line clearly above or below the current price to place an order.</div>
      ) : (
        <>
          <div className="seg" style={{ flexWrap: "wrap" }}>
            {options.map((o) => (
              <button key={o.type} className={`seg-btn ${sel?.type === o.type ? "on" : ""}`} onClick={() => setType(o.type)}>
                {o.label}
              </button>
            ))}
          </div>
          {sel && <div className="type-explain">{sel.explain}</div>}

          <div className="section-label">
            {effSize === "dollars" ? "Amount" : "Number of shares"}
            {(sel?.type === "sell_limit" || sel?.type === "sell_stop") && heldQty > 0 && (
              <button
                className="sell-all"
                onClick={() => {
                  setSizeMode("shares");
                  setQty(String(sel.type === "sell_stop" ? Math.floor(heldQty) : heldQty));
                }}
              >
                Sell all ({sel.type === "sell_stop" ? Math.floor(heldQty) : heldQty})
              </button>
            )}
          </div>
          {dollarsAllowed && (
            <div className="seg">
              {/* Clear the amount when switching units so a dollar figure is never reinterpreted as shares. */}
              <button className={`seg-btn ${effSize === "shares" ? "on" : ""}`} onClick={() => { setSizeMode("shares"); setQty(""); }}>Shares</button>
              <button className={`seg-btn ${effSize === "dollars" ? "on" : ""}`} onClick={() => { setSizeMode("dollars"); setQty(""); }}>Dollars</button>
            </div>
          )}
          {effSize === "dollars" ? (
            <div className="input-wrap"><span className="affix-l">$</span>
              <input type="number" inputMode="decimal" value={qty} placeholder="0.00" onChange={(e) => setQty(e.target.value)} />
            </div>
          ) : (
            <div className="input-wrap">
              <input type="number" inputMode="decimal" value={qty} placeholder="0" onChange={(e) => setQty(e.target.value)} />
              <span className="affix-r">shares</span>
            </div>
          )}

          {err && <div className="error-box">{err}</div>}
          <button className={`submit ${sel?.type.startsWith("sell") ? "sell" : "buy"}`} onClick={submit}>
            Review {sel?.label} {symbol}
          </button>
        </>
      )}
    </div>
  );
}
