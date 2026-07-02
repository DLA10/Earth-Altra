import { Fragment, useState } from "react";
import type { Activity, Order, Position, Quote } from "../types";
import type { Basis } from "../costBasis";

interface Props {
  positions: Position[];
  orders: Order[];
  quotes: Record<string, Quote>;
  fills: Activity[]; // buy/sell ledger (last 90 days) for the expand view
  basis: Record<string, Basis>; // corrected cost basis of currently-held shares
  onCancel: (id: string) => void;
}

const fmtTime = (iso: string) =>
  new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

export function Positions({ positions, orders, quotes, fills, basis, onCancel }: Props) {
  const [open, setOpen] = useState<Set<string>>(new Set());

  function toggle(sym: string) {
    setOpen((prev) => {
      const n = new Set(prev);
      n.has(sym) ? n.delete(sym) : n.add(sym);
      return n;
    });
  }

  return (
    <div className="positions">
      <div className="panel-title">Your positions <span className="muted small">· current holdings only · tap a row for its buy/sell history</span></div>
      {positions.length === 0 ? (
        <div className="empty">You don't hold any stocks yet.</div>
      ) : (
        <table className="data-table">
          <thead>
            <tr>
              <th>Stock</th>
              <th>Shares</th>
              <th>Bought at (avg)</th>
              <th>$ Spent</th>
              <th>Current</th>
              <th>Profit / Loss</th>
            </tr>
          </thead>
          <tbody>
            {positions.map((p) => {
              const cur = quotes[p.symbol]?.price ?? p.current_price;
              const entry = basis[p.symbol]?.avgEntry ?? p.avg_entry_price;
              const spent = basis[p.symbol]?.cost ?? p.cost_basis;
              const pl = (cur - entry) * p.qty;
              const plPct = entry > 0 ? ((cur - entry) / entry) * 100 : 0;
              const isOpen = open.has(p.symbol);
              const ledger = fills.filter((f) => f.symbol === p.symbol);
              return (
                <Fragment key={p.symbol}>
                  <tr className="pos-row" onClick={() => toggle(p.symbol)} role="button">
                    <td className="mono-strong">
                      <span className="pos-caret">{isOpen ? "▾" : "▸"}</span> {p.symbol}
                    </td>
                    <td>{p.qty}</td>
                    <td>${entry.toFixed(2)}</td>
                    <td>${spent.toFixed(2)}</td>
                    <td>${cur.toFixed(2)}</td>
                    <td className={pl >= 0 ? "pos" : "neg"}>
                      {pl >= 0 ? "+" : "-"}${Math.abs(pl).toFixed(2)} ({plPct >= 0 ? "+" : ""}{plPct.toFixed(2)}%)
                    </td>
                  </tr>
                  {isOpen && (
                    <tr key={p.symbol + "-lots"} className="pos-lots">
                      <td colSpan={6}>
                        {ledger.length === 0 ? (
                          <div className="muted small">No fills in the last 90 days (older history isn't shown).</div>
                        ) : (
                          <>
                            <div className="muted small lots-cap">
                              Buys &amp; sells (last 90 days) — these net to your current {p.qty} {p.symbol}.
                            </div>
                            <table className="data-table lots-table">
                              <thead>
                                <tr>
                                  <th>When</th>
                                  <th>Action</th>
                                  <th>Shares</th>
                                  <th>Price</th>
                                  <th>Amount</th>
                                </tr>
                              </thead>
                              <tbody>
                                {ledger.map((b) => (
                                  <tr key={b.id}>
                                    <td>{fmtTime(b.time)}</td>
                                    <td className={b.side === "buy" ? "pos" : "neg"}>{b.side.toUpperCase()}</td>
                                    <td>{b.qty}</td>
                                    <td>${b.price.toFixed(2)}</td>
                                    <td>${(b.qty * b.price).toFixed(2)}</td>
                                  </tr>
                                ))}
                              </tbody>
                            </table>
                          </>
                        )}
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </table>
      )}

      <div className="panel-title" style={{ marginTop: 16 }}>
        Open orders {orders.length > 0 && <span className="muted small">· resting on Alpaca · tap ✕ to cancel</span>}
      </div>
      {orders.length === 0 ? (
        <div className="empty">No open orders.</div>
      ) : (
        <table className="data-table">
          <thead>
            <tr>
              <th>Stock</th>
              <th>Side</th>
              <th>Type</th>
              <th>Qty / $</th>
              <th>Limit</th>
              <th>Status</th>
              <th>Cancel</th>
            </tr>
          </thead>
          <tbody>
            {orders.map((o) => (
              <tr key={o.id}>
                <td className="mono-strong">{o.symbol}</td>
                <td className={o.side === "buy" ? "pos" : "neg"}>{o.side}</td>
                <td>{o.type.replace("_", " ")}</td>
                <td>{o.notional ? `$${o.notional}` : o.qty}</td>
                <td>{o.limit_price || o.stop_price || "—"}</td>
                <td>{o.status}</td>
                <td>
                  <button className="mini-cancel" onClick={() => onCancel(o.id)} title="Cancel this order on Alpaca">✕</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
