import { useEffect, useState } from "react";
import { api } from "./api/client";
import type { SurgerReport } from "./types";

// Surger — the SURGER v2 lab: three intraday continuation detectors (C2 cusum / C1 purity
// / SPECTRAL), each with its own book, trading live paper with srg1_/srg2_/srg3_ order
// attribution so the books can never mix. Validated over 4 backtest windows — see
// SURGER_V2.md. Promoted to its own page 2026-07-31 when the AI quant desk it used to be
// nested under (Dip+Rise) was retired; the desk itself is unchanged.
export function Surger() {
  const [rep, setRep] = useState<SurgerReport | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .surger()
        .then((r) => {
          if (!alive) return;
          setRep(r);
          setErr("");
        })
        .catch((e) => alive && setErr(String(e)));
    load();
    const id = window.setInterval(load, 3000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, []);

  const money = (v: number) => `${v >= 0 ? "+" : "−"}$${Math.abs(v).toFixed(2)}`;
  const cls = (v: number) => (v > 0 ? "pos" : v < 0 ? "neg" : "");

  if (err) return <div className="quant-page"><p className="neg">Error: {err}</p></div>;
  if (!rep) return <div className="quant-page"><p className="muted">Loading…</p></div>;
  if (!rep.enabled || !rep.variants)
    return (
      <div className="quant-page">
        <h2>SURGER v2 lab</h2>
        <p className="muted">
          Desk is off — set PAPER_DIP_KEY / PAPER_DIP_SECRET in backend/.env and restart.
        </p>
      </div>
    );

  // Go marshals nil slices as null — every per-variant list needs a guard
  const allOpen = rep.variants.flatMap((v) => (v.open ?? []).map((p) => ({ ...p, vname: v.name })));
  const allTrades = rep.variants
    .flatMap((v) => (v.trades ?? []).map((t) => ({ ...t, vname: v.name })))
    .sort((a, b) => (a.closed_at < b.closed_at ? 1 : -1))
    .slice(0, 25);

  return (
    <div className="quant-page">
      <div className="quant-head">
        <h2>
          SURGER v2 lab{" "}
          <span className="muted" style={{ fontSize: "0.65em" }}>
            3 continuation detectors · srg*_ orders · ${rep.notional?.toFixed(0)}/slice ·{" "}
            {rep.slots} slots each · {rep.universe} symbols
          </span>
        </h2>
        <span className={`mode-badge ${rep.live ? "live" : "shadow"}`}>
          {rep.live ? "LIVE" : "SHADOW"}
        </span>
      </div>

      <div className="quant-cards">
        {rep.variants.map((v) => (
          <Card
            key={v.name}
            label={`${v.name} — today / all-time`}
            value={`${money(v.realized_today)} / ${money(v.realized_pnl)}`}
            tone={cls(v.realized_today !== 0 ? v.realized_today : v.realized_pnl)}
          />
        ))}
        {rep.variants.map((v) => (
          <Card
            key={v.name + "_wr"}
            label={`${v.name} — WR · trades · open`}
            value={`${v.total_trades > 0 ? v.win_rate.toFixed(0) + "%" : "—"} · ${v.total_trades} · ${(v.open ?? []).length}`}
          />
        ))}
      </div>

      {allOpen.length > 0 && (
        <table className="quant-table" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>Detector</th><th>Symbol</th><th>Qty</th><th>Entry</th><th>Stop</th>
              <th>Peak</th><th>Slip bps</th><th>Opened</th>
            </tr>
          </thead>
          <tbody>
            {allOpen.map((p) => (
              <tr key={p.vname + p.symbol}>
                <td>{p.vname}</td>
                <td><b>{p.symbol}</b></td>
                <td>{p.qty.toFixed(0)}</td>
                <td>${p.entry_price.toFixed(2)}</td>
                <td>${p.stop_px.toFixed(2)}</td>
                <td>${p.peak.toFixed(2)}</td>
                <td>{p.entry_slip_bps.toFixed(1)}</td>
                <td>{fmtTime(p.opened_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {allTrades.length > 0 && (
        <table className="quant-table" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>Detector</th><th>Symbol</th><th>Qty</th><th>Entry→Exit</th><th>P&L</th>
              <th>Reason</th><th>Held</th><th>Closed</th>
            </tr>
          </thead>
          <tbody>
            {allTrades.map((t, i) => (
              <tr key={i}>
                <td>{t.vname}</td>
                <td><b>{t.symbol}</b></td>
                <td>{t.qty.toFixed(0)}</td>
                <td>${t.entry_price.toFixed(2)}→${t.exit_price.toFixed(2)}</td>
                <td className={cls(t.pnl)}>{money(t.pnl)}</td>
                <td>{t.reason}</td>
                <td>{heldFor(t.opened_at, t.closed_at)}</td>
                <td>{fmtTime(t.closed_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {allOpen.length === 0 && allTrades.length === 0 && (
        <p className="muted">
          No SURGER trades yet — the detectors wait for a clean, freshly-breaking surge
          (flat weeks are normal and cost nothing; see SURGER_V2.md for the validation).
        </p>
      )}
    </div>
  );
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime())
    ? iso
    : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// heldFor renders how long a closed trade was held, e.g. "14m" or "1h05m".
function heldFor(entryIso: string, exitIso: string): string {
  const a = new Date(entryIso).getTime();
  const b = new Date(exitIso).getTime();
  if (isNaN(a) || isNaN(b) || b <= a) return "—";
  const mins = Math.round((b - a) / 60000);
  if (mins < 60) return `${mins}m`;
  return `${Math.floor(mins / 60)}h${String(mins % 60).padStart(2, "0")}m`;
}

function Card({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="q-card">
      <div className="q-card-label">{label}</div>
      <div className={`q-card-value ${tone ?? ""}`}>{value}</div>
    </div>
  );
}
