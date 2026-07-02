import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import { applyOrder, loadOrder, move, saveOrder } from "../order";
import type { Candle, Quote, SymbolMeta } from "../types";

interface Props {
  symbols: string[];
  selected: string;
  onSelect: (s: string) => void;
  quotes: Record<string, Quote>;
  refPrices: Record<string, number>;
  lastCandles: Record<string, Candle | undefined>;
  addedSymbols?: string[];
  onRemove?: (s: string) => void;
  onRemoveBoth?: (s: string) => void;
}

const ORDER_KEY = "lo.execOrder";

export function Watchlist({ symbols, selected, onSelect, quotes, refPrices, lastCandles, addedSymbols, onRemove, onRemoveBoth }: Props) {
  const added = new Set(addedSymbols ?? []);
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [meta, setMeta] = useState<Record<string, SymbolMeta>>({});
  const [order, setOrder] = useState<string[]>(() => applyOrder(symbols, loadOrder(ORDER_KEY)));
  const dragFrom = useRef<number | null>(null);

  // Keep the display order in sync as symbols change, and PERSIST it so new symbols
  // always append at the end and existing rows never shuffle. (The backend returns added
  // symbols alphabetically; without persisting, every add re-sorted the whole list.)
  useEffect(() => {
    const next = applyOrder(symbols, loadOrder(ORDER_KEY));
    setOrder(next);
    saveOrder(ORDER_KEY, next);
  }, [symbols]);

  // Resolve company name + sector for every symbol (incl. ones added at runtime).
  useEffect(() => {
    const missing = symbols.filter((s) => !meta[s]);
    if (missing.length === 0) return;
    api.symbolMeta(missing).then((m) => setMeta((prev) => ({ ...prev, ...m }))).catch(() => {});
  }, [symbols, meta]);

  // Auto-sort the panel by opening surge (% from the 9:30 ET open) — once at the +15 min mark,
  // again at +30 min, highest surge first. After that you can still drag rows to your own order
  // (it persists). Each mark applies at most once per ET day; manual drags are never undone
  // except by the next day's +15/+30 auto-sort.
  useEffect(() => {
    let alive = true;
    const AUTO_KEY = "lo.execAutoSort";
    const todayET = () => new Date().toLocaleDateString("en-CA", { timeZone: "America/New_York" });
    const getApplied = (): number[] => {
      try {
        const r = JSON.parse(localStorage.getItem(AUTO_KEY) || "{}");
        return r.day === todayET() ? r.applied ?? [] : [];
      } catch {
        return [];
      }
    };
    const run = () => {
      api
        .openingAnalysis(15, "execution")
        .then((ranks) => {
          if (!alive) return;
          const applied = getApplied();
          let changed = false;
          for (const mark of [15, 30]) {
            if (applied.includes(mark)) continue;
            const rank = ranks.find((r) => r.minutes === mark);
            if (!rank || !rank.elapsed) continue;
            // Don't consume the mark until there's actual ranking data, or a momentary empty
            // result would skip the sort for the rest of the day.
            if (rank.rising.length === 0 && rank.falling.length === 0) continue;
            const pct: Record<string, number> = {};
            for (const m of [...rank.rising, ...rank.falling]) pct[m.symbol] = m.pct;
            setOrder((prev) => {
              const sorted = [...prev].sort((a, b) => (pct[b] ?? -Infinity) - (pct[a] ?? -Infinity));
              saveOrder(ORDER_KEY, sorted);
              return sorted;
            });
            applied.push(mark);
            changed = true;
          }
          if (changed) localStorage.setItem(AUTO_KEY, JSON.stringify({ day: todayET(), applied }));
        })
        .catch(() => {});
    };
    run();
    const id = window.setInterval(run, 30000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, []);

  // Close the ⋯ menu on any outside click.
  useEffect(() => {
    if (!menuFor) return;
    const close = () => setMenuFor(null);
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [menuFor]);

  const hasActions = Boolean(onRemove || onRemoveBoth);

  function onDrop(toIndex: number) {
    const from = dragFrom.current;
    dragFrom.current = null;
    if (from === null || from === toIndex) return;
    const next = move(order, from, toIndex);
    setOrder(next);
    saveOrder(ORDER_KEY, next);
  }

  return (
    <div className="watchlist">
      <div className="panel-title">Prime List</div>
      {order.map((sym, i) => {
        const q = quotes[sym];
        const price = q?.price ?? lastCandles[sym]?.close ?? 0;
        const ref = refPrices[sym] ?? 0;
        const chg = ref > 0 && price > 0 ? ((price - ref) / ref) * 100 : 0;
        const up = chg >= 0;
        const isAdded = added.has(sym);
        const m = meta[sym];
        return (
          <div
            key={sym}
            className={`wl-row ${selected === sym ? "active" : ""}`}
            onClick={() => {
              setMenuFor(null);
              onSelect(sym);
            }}
            role="button"
            draggable
            onDragStart={() => (dragFrom.current = i)}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              e.preventDefault();
              onDrop(i);
            }}
          >
            <span className="wl-grip" title="Drag to reorder">⠿</span>
            <div className="wl-sym">
              <span className="wl-ticker">
                {sym}
                {isAdded && <span className="wl-added" title="Added from DECEPTICON">+</span>}
              </span>
              <span className="wl-name">{m?.name || ""}</span>
            </div>
            <div className="wl-px">
              <span className="wl-price">{price > 0 ? price.toFixed(2) : ""}</span>
              <span className={`wl-chg ${up ? "pos" : "neg"}`}>
                {price > 0 && ref > 0 ? `${up ? "+" : ""}${chg.toFixed(2)}%` : ""}
              </span>
            </div>

            {hasActions && (
              <button
                className="wl-kebab"
                title="Options"
                onClick={(e) => {
                  e.stopPropagation();
                  setMenuFor((cur) => (cur === sym ? null : sym));
                }}
              >
                ⋯
              </button>
            )}

            {menuFor === sym && (
              <div className="wl-menu" onClick={(e) => e.stopPropagation()}>
                {onRemove && (
                  <button
                    onClick={() => {
                      setMenuFor(null);
                      onRemove(sym);
                    }}
                  >
                    ✕ Remove from Execution (keep in Watchlist)
                  </button>
                )}
                {onRemoveBoth && (
                  <button
                    className="danger"
                    onClick={() => {
                      setMenuFor(null);
                      onRemoveBoth(sym);
                    }}
                  >
                    ✕ Remove from Execution + Watchlist
                  </button>
                )}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
