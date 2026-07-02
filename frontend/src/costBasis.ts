import type { Activity } from "./types";

// ClosedTrade is one realized (closing) sell — partial fills of the same sell order are
// merged into a single trade — with the average-cost basis of the shares it closed.
export interface ClosedTrade {
  time: string; // ISO time of the (last) closing fill
  symbol: string;
  qtySold: number;
  avgCost: number; // average cost of the shares closed
  avgSell: number; // average price they were sold at
  pnl: number; // realized profit/loss in dollars
  perShare: number; // pnl / qtySold
  pct: number; // (avgSell - avgCost) / avgCost * 100
}

export interface Basis {
  avgEntry: number; // cost per share of the shares currently held
  cost: number; // total cost of the shares currently held
  qty: number; // reconstructed current quantity
}

// Reconstruct the cost of the shares CURRENTLY held from a symbol's fills (ascending
// by time), using average-cost accounting and resetting to flat whenever the position
// is fully closed. This fixes Alpaca's avg_entry_price, which (for same-day round-trips
// on fractional shares) blends every buy ever made instead of resetting on a full exit.
export function reconstruct(fillsAsc: Activity[]): Basis {
  let qty = 0;
  let cost = 0;
  for (const f of fillsAsc) {
    if (f.side === "buy") {
      qty += f.qty;
      cost += f.qty * f.price;
    } else {
      if (qty > 1e-9) {
        const avg = cost / qty;
        qty -= f.qty;
        cost -= f.qty * avg;
      }
    }
    if (qty < 1e-6) {
      qty = 0;
      cost = 0;
    }
  }
  return { qty, cost, avgEntry: qty > 0 ? cost / qty : 0 };
}

// realizedTrades walks all fills (chronologically, per symbol, average-cost,
// resetting on a flat position) and returns one ClosedTrade per closing sell ORDER
// (partial fills merged). This is the authoritative realized-P&L list the Metrics page
// groups by day/week/month — same accounting as reconstruct(), so totals reconcile.
export function realizedTrades(fills: Activity[]): ClosedTrade[] {
  const asc = fills.slice().sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime());
  const pos: Record<string, { qty: number; cost: number }> = {};
  // Accumulate realized pieces per sell order so partial fills become one trade.
  const orders = new Map<string, { symbol: string; time: string; qty: number; proceeds: number; costOut: number }>();

  for (const f of asc) {
    const sym = f.symbol;
    const q = f.qty;
    const p = f.price;
    if (q <= 0 || p <= 0) continue;
    if (!pos[sym]) pos[sym] = { qty: 0, cost: 0 };

    if (f.side === "buy") {
      pos[sym].qty += q;
      pos[sym].cost += q * p;
    } else {
      const avg = pos[sym].qty > 1e-9 ? pos[sym].cost / pos[sym].qty : p;
      const key = f.order_id || f.id;
      const o = orders.get(key) ?? { symbol: sym, time: f.time, qty: 0, proceeds: 0, costOut: 0 };
      o.qty += q;
      o.proceeds += q * p;
      o.costOut += q * avg;
      o.time = f.time; // last fill time of the order
      orders.set(key, o);
      pos[sym].cost -= q * avg;
      pos[sym].qty -= q;
    }
    if (pos[sym].qty < 1e-6) {
      pos[sym].qty = 0;
      pos[sym].cost = 0;
    }
  }

  const out: ClosedTrade[] = [];
  for (const o of orders.values()) {
    if (o.qty <= 0) continue;
    const pnl = o.proceeds - o.costOut;
    const avgSell = o.proceeds / o.qty;
    const avgCost = o.costOut / o.qty;
    out.push({
      time: o.time,
      symbol: o.symbol,
      qtySold: o.qty,
      avgCost,
      avgSell,
      pnl,
      perShare: pnl / o.qty,
      pct: avgCost > 0 ? ((avgSell - avgCost) / avgCost) * 100 : 0,
    });
  }
  out.sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime());
  return out;
}

// basisBySymbol reconstructs each held symbol's true current-share cost from fills,
// but only trusts it when the reconstructed quantity matches Alpaca's held quantity
// (i.e. the fills fully explain the position). Otherwise it falls back to Alpaca's
// avg_entry_price / cost_basis (e.g. when fills are older than the 90-day window).
export function basisBySymbol(
  positions: { symbol: string; qty: number; avg_entry_price: number; cost_basis: number }[],
  fills: Activity[]
): Record<string, Basis> {
  const bySym: Record<string, Activity[]> = {};
  for (const f of fills) (bySym[f.symbol] ??= []).push(f);

  const out: Record<string, Basis> = {};
  for (const p of positions) {
    const fs = (bySym[p.symbol] ?? []).slice().sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime());
    const r = reconstruct(fs);
    const matched = fs.length > 0 && Math.abs(r.qty - p.qty) < Math.max(0.01, Math.abs(p.qty) * 0.001);
    out[p.symbol] = matched && r.qty > 0 ? r : { avgEntry: p.avg_entry_price, cost: p.cost_basis, qty: p.qty };
  }
  return out;
}
