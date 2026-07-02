import type { OrderRequest, OrderType } from "../types";

function typeLabel(t: OrderType): string {
  return (
    { market: "MARKET", limit: "LIMIT", stop: "STOP", stop_limit: "STOP-LIMIT", trailing_stop: "TRAILING STOP" }[t] ??
    t.toUpperCase()
  );
}

function Row({ label, value, cls, strong }: { label: string; value: string; cls?: string; strong?: boolean }) {
  return (
    <tr>
      <td>{label}</td>
      <td className={cls}>{strong ? <strong>{value}</strong> : value}</td>
    </tr>
  );
}

interface Props {
  req: OrderRequest;
  estCost: number;
  mode: "LIVE" | "PAPER";
  currentPrice: number; // live price, to flag marketable limits
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

// ConfirmModal is mandatory before every order. In LIVE mode it is styled as a
// hard warning so real-money orders are never one-click.
export function ConfirmModal({ req, estCost, mode, currentPrice, busy, onConfirm, onCancel }: Props) {
  const isLive = mode === "LIVE";

  // A limit at/above market (buy) or at/below market (sell) executes immediately at
  // the current price rather than waiting — surface this loudly so it's never a
  // surprise (the "$1,500 buy limit filled instantly" trap).
  const lp = req.limit_price ?? 0;
  const marketable =
    req.type === "limit" &&
    req.order_class !== "oco" &&
    lp > 0 &&
    currentPrice > 0 &&
    (req.side === "buy" ? lp >= currentPrice : lp <= currentPrice);

  // A stop on the wrong side of the market triggers immediately instead of waiting.
  const sp = req.stop_price ?? 0;
  const stopImmediate =
    req.type === "stop" &&
    sp > 0 &&
    currentPrice > 0 &&
    (req.side === "sell" ? sp >= currentPrice : sp <= currentPrice);
  return (
    <div className="modal-backdrop" onClick={onCancel}>
      <div className={`modal ${isLive ? "live" : ""}`} onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          {isLive ? "⚠ CONFIRM LIVE ORDER (REAL MONEY)" : "Confirm order (paper)"}
        </div>

        {marketable && (
          <div className="modal-warn">
            <strong>This limit fills immediately.</strong> {req.symbol} is at $
            {currentPrice.toFixed(2)} now, so this {req.side.toUpperCase()} limit of $
            {lp.toFixed(2)} executes right away at about ${currentPrice.toFixed(2)} — it does{" "}
            <strong>not</strong> wait for ${lp.toFixed(2)}.{" "}
            {req.side === "buy"
              ? "A buy limit only waits if it's set BELOW the current price."
              : "A sell limit only waits if it's set ABOVE the current price."}
          </div>
        )}

        {stopImmediate && (
          <div className="modal-warn">
            <strong>This stop triggers immediately.</strong> {req.symbol} is at $
            {currentPrice.toFixed(2)} now, and your stop of ${sp.toFixed(2)} is already on the wrong side, so it
            fires right away.{" "}
            {req.side === "sell"
              ? "A stop-loss only waits if it's set BELOW the current price."
              : "A buy stop only waits if it's set ABOVE the current price."}
          </div>
        )}

        <table className="confirm-table">
          <tbody>
            <Row label="Symbol" value={req.symbol} />
            <Row label="Side" value={req.side.toUpperCase()} cls={req.side === "buy" ? "pos" : "neg"} />
            {req.order_class && req.order_class !== "simple" && (
              <Row label="Strategy" value={req.order_class.toUpperCase()} />
            )}
            <Row label="Type" value={typeLabel(req.type)} />
            {req.limit_price != null && <Row label="Limit price" value={`$${req.limit_price.toFixed(2)}`} />}
            {req.stop_price != null && <Row label="Stop price" value={`$${req.stop_price.toFixed(2)}`} />}
            {req.trail_price != null && <Row label="Trail" value={`$${req.trail_price.toFixed(2)}`} />}
            {req.trail_percent != null && <Row label="Trail" value={`${req.trail_percent}%`} />}
            {req.take_profit_limit != null && <Row label="Take-profit" value={`$${req.take_profit_limit.toFixed(2)}`} />}
            {req.stop_loss_stop != null && <Row label="Stop-loss" value={`$${req.stop_loss_stop.toFixed(2)}`} />}
            <Row
              label="Size"
              value={req.notional != null ? `$${req.notional.toFixed(2)} (dollars)` : `${req.qty} shares`}
            />
            <Row label="Est. value" value={`$${estCost.toFixed(2)}`} strong />
            <Row label="Time in force" value={(req.time_in_force ?? "day").toUpperCase()} />
            {req.extended_hours && <Row label="Extended hours" value="Yes (pre / post-market)" />}
          </tbody>
        </table>

        <div className="modal-actions">
          <button className="btn-ghost" onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button className={`btn-confirm ${req.side}`} onClick={onConfirm} disabled={busy}>
            {busy ? "Placing…" : isLive ? `Place LIVE ${req.side.toUpperCase()}` : `Place ${req.side.toUpperCase()}`}
          </button>
        </div>
      </div>
    </div>
  );
}
