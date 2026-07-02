import { useEffect, useState } from "react";
import type { OrderRequest } from "../types";

interface Props {
  symbol: string;
  lastPrice: number;
  fractionable: boolean;
  maxNotional: number;
  heldQty: number; // shares currently held of this symbol
  heldValue: number; // market value currently held
  resetNonce: number; // bump to clear inputs after a successful order
  onReview: (req: OrderRequest, estCost: number) => void;
}

type Side = "buy" | "sell";
type SizeMode = "shares" | "dollars";
type CondType = "buy_limit" | "sell_limit" | "stop" | "trailing_stop";
type TrailUnit = "percent" | "amount";

const num = (s: string) => parseFloat(s) || 0;

// OrderPanel offers three independent, always-visible order builders, each with its own
// amount and Review button: MARKET (buy/sell now), CONDITIONAL (a single resting buy-limit /
// sell-limit / stop-loss), and OCO (protect a holding with take-profit + stop-loss). Every
// fat-finger guard is enforced here before the mandatory confirm modal; the backend re-checks.
export function OrderPanel({ symbol, lastPrice, fractionable, maxNotional, heldQty, heldValue, resetNonce, onReview }: Props) {
  // --- Market ---
  const [side, setSide] = useState<Side>("buy");
  const [mktSizeMode, setMktSizeMode] = useState<SizeMode>("dollars"); // dollars default
  const [mktAmount, setMktAmount] = useState("");
  const [mktErr, setMktErr] = useState("");

  // --- Conditional (resting) ---
  const [condType, setCondType] = useState<CondType>("buy_limit");
  const [condSizeMode, setCondSizeMode] = useState<SizeMode>("shares"); // shares default (GTC-capable)
  const [condAmount, setCondAmount] = useState("");
  const [condPrice, setCondPrice] = useState("");
  const [condExt, setCondExt] = useState(false);
  const [trailAmount, setTrailAmount] = useState("");
  const [trailUnit, setTrailUnit] = useState<TrailUnit>("percent");
  const [condErr, setCondErr] = useState("");

  // --- OCO ---
  const [ocoQty, setOcoQty] = useState("");
  const [ocoTp, setOcoTp] = useState("");
  const [ocoSl, setOcoSl] = useState("");
  const [ocoErr, setOcoErr] = useState("");

  // --- Bracket (buy entry + take-profit + stop-loss in one order) ---
  const [brkEntry, setBrkEntry] = useState<"market" | "limit">("market");
  const [brkQty, setBrkQty] = useState("");
  const [brkLimit, setBrkLimit] = useState(""); // entry price for a limit-entry bracket
  const [brkTp, setBrkTp] = useState("");
  const [brkSl, setBrkSl] = useState("");
  const [brkErr, setBrkErr] = useState("");

  // Clear inputs after a placed order (or symbol change) so it's obvious it went through.
  useEffect(() => {
    setMktAmount("");
    setCondAmount("");
    setCondPrice("");
    setCondExt(false);
    setTrailAmount("");
    setOcoQty("");
    setOcoTp("");
    setOcoSl("");
    setBrkQty("");
    setBrkLimit("");
    setBrkTp("");
    setBrkSl("");
    setMktErr("");
    setCondErr("");
    setOcoErr("");
    setBrkErr("");
  }, [resetNonce, symbol]);

  // Market dollar sizing needs a fractionable symbol (notional). Market is always regular
  // hours, so extended-hours never blocks it here.
  const dollarsAllowed = fractionable;
  const mktEffSize: SizeMode = dollarsAllowed ? mktSizeMode : "shares";

  const isLimit = condType === "buy_limit" || condType === "sell_limit";
  const isStop = condType === "stop";
  const isTrailing = condType === "trailing_stop";
  const condSide: Side = condType === "buy_limit" ? "buy" : "sell"; // trailing stop is a sell
  const cp = num(condPrice);

  // Dollar (notional) sizing is allowed for the two LIMIT types only — Alpaca rejects
  // dollar-amount STOP orders — and only when the symbol is fractionable and you're not
  // routing to extended hours (a notional order must be a regular-hours DAY order).
  const condDollarsAllowed = isLimit && fractionable && !condExt;
  const condEffSize: SizeMode = condDollarsAllowed ? condSizeMode : "shares";

  const capExceeded = (est: number) => maxNotional > 0 && est > maxNotional;
  const capMsg = (est: number) => `Order ~$${est.toFixed(0)} exceeds the $${maxNotional.toFixed(0)} safety cap.`;

  // ---------- MARKET ----------
  function submitMarket() {
    setMktErr("");
    const usingDollars = mktEffSize === "dollars";
    const req: OrderRequest = { symbol, side, type: "market", time_in_force: "day" };
    let est = 0;
    if (usingDollars) {
      const n = num(mktAmount);
      if (n <= 0) return setMktErr("Enter a dollar amount.");
      if (side === "sell" && heldValue > 0 && n > heldValue + 0.01)
        return setMktErr(`You only hold $${heldValue.toFixed(2)} of ${symbol}.`);
      req.notional = n;
      est = n;
    } else {
      const q = num(mktAmount);
      if (q <= 0) return setMktErr("Enter a share quantity.");
      if (side === "sell" && q > heldQty + 1e-9)
        return setMktErr(heldQty > 0 ? `You only hold ${heldQty} ${symbol}.` : `You don't hold any ${symbol} to sell.`);
      req.qty = q;
      est = q * lastPrice;
    }
    if (capExceeded(est)) return setMktErr(capMsg(est));
    onReview(req, est);
  }

  // ---------- CONDITIONAL (single resting buy-limit / sell-limit / stop-loss) ----------
  function submitConditional() {
    setCondErr("");

    // ---------- Trailing stop (sell-only protective exit; follows price by $ or %) ----------
    if (isTrailing) {
      const q = num(condAmount);
      if (q <= 0) return setCondErr("Enter a share quantity.");
      if (q > heldQty + 1e-9)
        return setCondErr(heldQty > 0 ? `You only hold ${heldQty} ${symbol}.` : `You don't hold any ${symbol} to protect.`);
      if (q % 1 !== 0)
        return setCondErr(`A trailing stop must be a whole number of shares (Alpaca doesn't allow fractional stops). You can protect up to ${Math.floor(heldQty)}.`);
      const t = num(trailAmount);
      if (t <= 0) return setCondErr(trailUnit === "percent" ? "Enter a trail percent." : "Enter a trail amount in dollars.");
      if (trailUnit === "percent" && t >= 100) return setCondErr("Trail percent must be below 100%.");
      const req: OrderRequest = { symbol, side: "sell", type: "trailing_stop", time_in_force: "gtc", qty: q };
      if (trailUnit === "percent") req.trail_percent = t;
      else req.trail_price = t;
      const est = q * lastPrice;
      if (capExceeded(est)) return setCondErr(capMsg(est));
      return onReview(req, est);
    }

    const usingDollars = condEffSize === "dollars";
    const req: OrderRequest = { symbol, side: condSide, type: isStop ? "stop" : "limit" };
    let est = 0;
    let fractionalShares = false;

    if (usingDollars) {
      const n = num(condAmount);
      if (n <= 0) return setCondErr("Enter a dollar amount.");
      if (condSide === "sell" && heldValue > 0 && n > heldValue + 0.01)
        return setCondErr(`You only hold $${heldValue.toFixed(2)} of ${symbol}.`);
      req.notional = n;
      est = n;
    } else {
      const q = num(condAmount);
      if (q <= 0) return setCondErr("Enter a share quantity.");
      if (condSide === "sell" && q > heldQty + 1e-9)
        return setCondErr(heldQty > 0 ? `You only hold ${heldQty} ${symbol}.` : `You don't hold any ${symbol} to sell.`);
      fractionalShares = q % 1 !== 0;
      // A resting stop-loss can't be fractional (Alpaca: fractional stops are day-only, and we
      // want it to rest until canceled). Require whole shares.
      if (isStop && fractionalShares)
        return setCondErr(
          Math.floor(heldQty) >= 1
            ? `A stop-loss must be a whole number of shares (Alpaca doesn't allow fractional stops). You can protect up to ${Math.floor(heldQty)}.`
            : `A stop-loss needs at least 1 whole share, but you hold ${heldQty} ${symbol}. Use a plain Sell to exit a fractional position.`
        );
      req.qty = q;
      est = q * cp;
    }

    if (cp <= 0) return setCondErr("Enter a price.");
    // A resting order must sit on the waiting side of the market — never one that fills now.
    if (lastPrice > 0) {
      if (condType === "buy_limit" && cp >= lastPrice)
        return setCondErr(`A buy-the-dip price must be BELOW the current $${lastPrice.toFixed(2)} — at or above it, this would buy immediately.`);
      if (condType === "sell_limit" && cp <= lastPrice)
        return setCondErr(`A take-profit price must be ABOVE the current $${lastPrice.toFixed(2)} — at or below it, this would sell immediately.`);
      if (condType === "stop" && cp >= lastPrice)
        return setCondErr(`A stop-loss price must be BELOW the current $${lastPrice.toFixed(2)} — at or above it, this would sell immediately.`);
    }

    const extOn = isLimit && condExt;
    // Dollar (notional), fractional-share, and extended-hours orders must be Day TIF; a plain
    // whole-share limit/stop can rest as GTC until canceled.
    req.time_in_force = usingDollars || fractionalShares || extOn ? "day" : "gtc";
    if (extOn) req.extended_hours = true;
    if (isStop) req.stop_price = cp;
    else req.limit_price = cp;

    if (capExceeded(est)) return setCondErr(capMsg(est));
    onReview(req, est);
  }

  // ---------- OCO ----------
  function submitOco() {
    setOcoErr("");
    if (heldQty <= 0) return setOcoErr(`You don't hold any ${symbol} to protect.`);
    const q = num(ocoQty);
    if (q <= 0) return setOcoErr("Enter how many shares to protect (or tap Sell all).");
    if (q > heldQty + 1e-9) return setOcoErr(`You only hold ${heldQty} ${symbol}.`);
    if (q % 1 !== 0)
      return setOcoErr(
        Math.floor(heldQty) >= 1
          ? `OCO must be a whole number of shares (Alpaca doesn't allow fractions for OCO). You can protect up to ${Math.floor(heldQty)}.`
          : `OCO needs at least 1 whole share, but you hold ${heldQty} ${symbol}. Use a plain Sell to exit a fractional position.`
      );
    const tp = num(ocoTp);
    const sl = num(ocoSl);
    if (tp <= 0) return setOcoErr("Enter a take-profit price.");
    if (sl <= 0) return setOcoErr("Enter a stop-loss price.");
    if (lastPrice > 0 && tp <= lastPrice)
      return setOcoErr(`Take-profit ($${tp.toFixed(2)}) must be ABOVE the current price ($${lastPrice.toFixed(2)}).`);
    if (lastPrice > 0 && sl >= lastPrice)
      return setOcoErr(`Stop-loss ($${sl.toFixed(2)}) must be BELOW the current price ($${lastPrice.toFixed(2)}).`);
    const req: OrderRequest = {
      symbol,
      side: "sell",
      type: "limit",
      order_class: "oco",
      time_in_force: "gtc",
      qty: q,
      take_profit_limit: tp,
      stop_loss_stop: sl,
    };
    onReview(req, q * (lastPrice || 0));
  }

  // ---------- BRACKET (buy entry + take-profit + stop-loss, one order) ----------
  function submitBracket() {
    setBrkErr("");
    const q = num(brkQty);
    if (q <= 0) return setBrkErr("Enter a share quantity.");
    // Bracket legs include a stop, so whole shares only (Alpaca rule).
    if (q % 1 !== 0) return setBrkErr("A bracket must be a whole number of shares (its legs include a stop).");

    const isLimitEntry = brkEntry === "limit";
    const entry = num(brkLimit);
    if (isLimitEntry) {
      if (entry <= 0) return setBrkErr("Enter your buy (entry) price.");
      if (lastPrice > 0 && entry >= lastPrice)
        return setBrkErr(`A limit entry must be BELOW the current $${lastPrice.toFixed(2)} — at or above it, use a market entry.`);
    }
    const tp = num(brkTp);
    const sl = num(brkSl);
    if (tp <= 0) return setBrkErr("Enter a take-profit price.");
    if (sl <= 0) return setBrkErr("Enter a stop-loss price.");
    // Reference: market entry uses the current price; a limit entry uses the entry price.
    const ref = isLimitEntry ? entry : (lastPrice || 0);
    if (ref > 0 && tp <= ref) return setBrkErr(`Take-profit ($${tp.toFixed(2)}) must be ABOVE your entry ($${ref.toFixed(2)}).`);
    if (ref > 0 && sl >= ref) return setBrkErr(`Stop-loss ($${sl.toFixed(2)}) must be BELOW your entry ($${ref.toFixed(2)}).`);
    if (sl >= tp) return setBrkErr("Stop-loss must be below the take-profit.");

    const est = q * (isLimitEntry ? entry : (lastPrice || 0));
    if (capExceeded(est)) return setBrkErr(capMsg(est));

    const req: OrderRequest = {
      symbol,
      side: "buy",
      type: isLimitEntry ? "limit" : "market",
      order_class: "bracket",
      // A market entry must be DAY (the protective legs are good for today); a limit entry can
      // rest as GTC so the whole bracket waits until canceled.
      time_in_force: isLimitEntry ? "gtc" : "day",
      qty: q,
      take_profit_limit: tp,
      stop_loss_stop: sl,
    };
    if (isLimitEntry) req.limit_price = entry;
    onReview(req, est);
  }

  const heldNote = (
    <div className="held-note">
      You hold <strong>{heldQty > 0 ? heldQty : 0}</strong> {symbol}
      {heldValue > 0 ? ` ($${heldValue.toFixed(2)})` : ""}
    </div>
  );

  return (
    <div className="order-panel">
      <div className="panel-title">Order — {symbol}</div>

      {/* ===================== MARKET ===================== */}
      <div className="section-label section-head">Market — buy or sell now</div>
      <div className="seg side-seg">
        <button className={`seg-btn buy ${side === "buy" ? "on" : ""}`} onClick={() => setSide("buy")}>Buy</button>
        <button className={`seg-btn sell ${side === "sell" ? "on" : ""}`} onClick={() => setSide("sell")}>Sell</button>
      </div>
      <div className="type-explain">Buys or sells <strong>right now</strong> at the best available price.</div>

      <div className="section-label">
        {dollarsAllowed ? "Amount" : "Number of shares"}
        {side === "sell" && heldQty > 0 && (
          <button
            className="sell-all"
            onClick={() => {
              setMktSizeMode("shares");
              setMktAmount(String(heldQty));
            }}
          >
            Sell all ({heldQty})
          </button>
        )}
      </div>
      {dollarsAllowed && (
        <div className="seg">
          {/* Clear the amount when switching units so a dollar figure is never reinterpreted as shares. */}
          <button className={`seg-btn ${mktEffSize === "dollars" ? "on" : ""}`} onClick={() => { setMktSizeMode("dollars"); setMktAmount(""); }}>Dollars</button>
          <button className={`seg-btn ${mktEffSize === "shares" ? "on" : ""}`} onClick={() => { setMktSizeMode("shares"); setMktAmount(""); }}>Shares</button>
        </div>
      )}
      {mktEffSize === "dollars" ? (
        <div className="input-wrap"><span className="affix-l">$</span>
          <input type="number" inputMode="decimal" value={mktAmount} placeholder="0.00" onChange={(e) => setMktAmount(e.target.value)} />
        </div>
      ) : (
        <div className="input-wrap">
          <input type="number" inputMode="decimal" value={mktAmount} placeholder="0" onChange={(e) => setMktAmount(e.target.value)} />
          <span className="affix-r">shares</span>
        </div>
      )}
      {side === "sell" && heldNote}
      {mktErr && <div className="error-box">{mktErr}</div>}
      <button className={`submit ${side}`} onClick={submitMarket}>
        Review {side === "buy" ? "Buy" : "Sell"} {symbol}
      </button>

      <div className="divider" />

      {/* ===================== CONDITIONAL ===================== */}
      <div className="section-label section-head">Conditional — waits for a price</div>
      <div className="seg" style={{ flexWrap: "wrap" }}>
        <button className={`seg-btn ${condType === "buy_limit" ? "on" : ""}`} onClick={() => setCondType("buy_limit")}>Buy limit</button>
        <button className={`seg-btn ${condType === "sell_limit" ? "on" : ""}`} onClick={() => setCondType("sell_limit")}>Sell limit</button>
        <button className={`seg-btn ${condType === "stop" ? "on" : ""}`} onClick={() => setCondType("stop")}>Stop-loss</button>
        <button className={`seg-btn ${condType === "trailing_stop" ? "on" : ""}`} onClick={() => setCondType("trailing_stop")}>Trailing stop</button>
      </div>
      <div className="type-explain">
        {condType === "buy_limit" && <>Buys if {symbol} <strong>falls</strong> to your price (the dip). Set it below the current price.</>}
        {condType === "sell_limit" && <>Sells if {symbol} <strong>rises</strong> to your price (take profit). Set it above the current price.</>}
        {condType === "stop" && <>Sells if {symbol} <strong>falls</strong> to your price (stops the bleeding). Set it below the current price.</>}
        {condType === "trailing_stop" && (
          <>Sells if {symbol} <strong>drops by your trail amount from its peak</strong>. The stop <strong>follows the price up</strong> as it rises (locking in gains) and only triggers on a pullback — no fixed price to set.</>
        )}
      </div>

      <div className="section-label">
        {condDollarsAllowed ? "Amount" : "Number of shares"}
        {condSide === "sell" && heldQty > 0 && (
          <button
            className="sell-all"
            onClick={() => {
              setCondSizeMode("shares");
              setCondAmount(String(isStop || isTrailing ? Math.floor(heldQty) : heldQty));
            }}
          >
            Sell all ({isStop || isTrailing ? Math.floor(heldQty) : heldQty})
          </button>
        )}
      </div>
      {condDollarsAllowed && (
        <div className="seg">
          {/* Clear the amount when switching units so a dollar figure is never reinterpreted as shares. */}
          <button className={`seg-btn ${condEffSize === "dollars" ? "on" : ""}`} onClick={() => { setCondSizeMode("dollars"); setCondAmount(""); }}>Dollars</button>
          <button className={`seg-btn ${condEffSize === "shares" ? "on" : ""}`} onClick={() => { setCondSizeMode("shares"); setCondAmount(""); }}>Shares</button>
        </div>
      )}
      {condEffSize === "dollars" ? (
        <div className="input-wrap"><span className="affix-l">$</span>
          <input type="number" inputMode="decimal" value={condAmount} placeholder="0.00" onChange={(e) => setCondAmount(e.target.value)} />
        </div>
      ) : (
        <div className="input-wrap">
          <input type="number" inputMode="decimal" value={condAmount} placeholder="0" onChange={(e) => setCondAmount(e.target.value)} />
          <span className="affix-r">shares</span>
        </div>
      )}
      {isStop && fractionable && (
        <div className="type-explain muted">Stop-loss is shares only — Alpaca doesn't allow dollar amounts for stop orders.</div>
      )}

      {isTrailing ? (
        <>
          <div className="section-label">Trail by</div>
          <div className="seg">
            <button className={`seg-btn ${trailUnit === "percent" ? "on" : ""}`} onClick={() => setTrailUnit("percent")}>Percent</button>
            <button className={`seg-btn ${trailUnit === "amount" ? "on" : ""}`} onClick={() => setTrailUnit("amount")}>Dollars</button>
          </div>
          {trailUnit === "percent" ? (
            <div className="input-wrap">
              <input type="number" inputMode="decimal" value={trailAmount} placeholder="e.g. 2" onChange={(e) => setTrailAmount(e.target.value)} />
              <span className="affix-r">%</span>
            </div>
          ) : (
            <div className="input-wrap"><span className="affix-l">$</span>
              <input type="number" inputMode="decimal" value={trailAmount} placeholder="e.g. 0.50" onChange={(e) => setTrailAmount(e.target.value)} />
            </div>
          )}
          {num(trailAmount) > 0 && lastPrice > 0 && (
            <div className="type-explain">
              {trailUnit === "percent"
                ? `From a peak of $${lastPrice.toFixed(2)}, it would sell around $${(lastPrice * (1 - num(trailAmount) / 100)).toFixed(2)} — and that trigger rises as ${symbol} climbs.`
                : `From a peak of $${lastPrice.toFixed(2)}, it would sell around $${(lastPrice - num(trailAmount)).toFixed(2)} — and that trigger rises as ${symbol} climbs.`}
            </div>
          )}
        </>
      ) : (
        <>
          <Field
            label={
              condType === "buy_limit" ? "Buy price (below current)"
                : condType === "sell_limit" ? "Sell price (above current)"
                : "Stop price (below current)"
            }
          >
            <Money value={condPrice} placeholder={lastPrice ? lastPrice.toFixed(2) : "0.00"} onChange={setCondPrice} />
          </Field>
          {cp > 0 && lastPrice > 0 && (
            <div className="type-explain">
              {condType === "buy_limit" && `Waits until ${symbol} drops to $${cp.toFixed(2)} (now $${lastPrice.toFixed(2)}), then buys.`}
              {condType === "sell_limit" && `Waits until ${symbol} rises to $${cp.toFixed(2)} (now $${lastPrice.toFixed(2)}), then sells.`}
              {condType === "stop" && `Waits until ${symbol} falls to $${cp.toFixed(2)} (now $${lastPrice.toFixed(2)}), then sells at market.`}
            </div>
          )}
        </>
      )}
      {isLimit && (
        <label className="ext-row">
          {/* Entering extended hours forces shares (Alpaca bans notional there) — clear the
              amount so a dollar figure isn't silently reinterpreted as a share count. */}
          <input
            type="checkbox"
            checked={condExt}
            onChange={(e) => { const v = e.target.checked; setCondExt(v); if (v) setCondAmount(""); }}
          />
          <span>Allow pre / post-market (extended hours)</span>
        </label>
      )}
      {condSide === "sell" && heldNote}
      {condErr && <div className="error-box">{condErr}</div>}
      <button className={`submit ${condSide}`} onClick={submitConditional}>
        Review {condType === "buy_limit" ? "Buy-limit" : condType === "sell_limit" ? "Sell-limit" : condType === "trailing_stop" ? "Trailing-stop" : "Stop-loss"} {symbol}
      </button>

      <div className="divider" />

      {/* ===================== OCO ===================== */}
      <div className="section-label section-head">OCO — protect a holding</div>
      <div className="type-explain">
        Protects {symbol} you already hold: sells at your take-profit (up) or stop-loss (down) — whichever
        hits first cancels the other.
      </div>
      <Field label="Take-profit price (sell when it rises to)">
        <Money value={ocoTp} placeholder="0.00" onChange={setOcoTp} />
      </Field>
      <Field label="Stop-loss price (sell when it falls to)">
        <Money value={ocoSl} placeholder="0.00" onChange={setOcoSl} />
      </Field>
      <div className="section-label">
        Number of shares
        {heldQty > 0 && (
          <button className="sell-all" onClick={() => setOcoQty(String(Math.floor(heldQty)))}>
            Protect all ({Math.floor(heldQty)})
          </button>
        )}
      </div>
      <div className="input-wrap">
        <input type="number" inputMode="decimal" value={ocoQty} placeholder="0" onChange={(e) => setOcoQty(e.target.value)} />
        <span className="affix-r">shares</span>
      </div>
      {heldNote}
      {ocoErr && <div className="error-box">{ocoErr}</div>}
      <button className="submit sell" onClick={submitOco}>
        Review OCO {symbol}
      </button>

      <div className="divider" />

      {/* ===================== BRACKET ===================== */}
      <div className="section-label section-head">Bracket — buy + auto take-profit + stop-loss</div>
      <div className="type-explain">
        Buys {symbol} and, once filled, automatically arms a take-profit (up) and a stop-loss (down) —
        whichever hits first cancels the other. One order sets up the whole trade.
      </div>
      <div className="seg">
        <button className={`seg-btn ${brkEntry === "market" ? "on" : ""}`} onClick={() => setBrkEntry("market")}>Buy now (market)</button>
        <button className={`seg-btn ${brkEntry === "limit" ? "on" : ""}`} onClick={() => setBrkEntry("limit")}>Buy at price (limit)</button>
      </div>
      <div className="type-explain muted">
        {brkEntry === "market"
          ? "Buys at the current price right now. The take-profit / stop-loss are good for today (day order)."
          : "Waits to buy at your price (below market), then the take-profit / stop-loss rest until filled or canceled."}
      </div>
      {brkEntry === "limit" && (
        <Field label="Buy price (below current)">
          <Money value={brkLimit} placeholder={lastPrice ? lastPrice.toFixed(2) : "0.00"} onChange={setBrkLimit} />
        </Field>
      )}
      <Field label="Take-profit price (sell when it rises to)">
        <Money value={brkTp} placeholder="0.00" onChange={setBrkTp} />
      </Field>
      <Field label="Stop-loss price (sell when it falls to)">
        <Money value={brkSl} placeholder="0.00" onChange={setBrkSl} />
      </Field>
      <div className="section-label">Number of shares</div>
      <div className="input-wrap">
        <input type="number" inputMode="decimal" value={brkQty} placeholder="0" onChange={(e) => setBrkQty(e.target.value)} />
        <span className="affix-r">shares</span>
      </div>
      {brkErr && <div className="error-box">{brkErr}</div>}
      <button className="submit buy" onClick={submitBracket}>
        Review Bracket {symbol}
      </button>
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
    </label>
  );
}

function Money({ value, placeholder, onChange }: { value: string; placeholder: string; onChange: (v: string) => void }) {
  return (
    <div className="input-wrap">
      <span className="affix-l">$</span>
      <input type="number" inputMode="decimal" value={value} placeholder={placeholder} onChange={(e) => onChange(e.target.value)} />
    </div>
  );
}
