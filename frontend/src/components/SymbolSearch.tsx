import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import type { Asset } from "../types";

// SymbolSearch is a global search box (in the top nav) to look up any tradable US
// stock by ticker or company name and add it to Execution or the Watchlist.
export function SymbolSearch() {
  const [q, setQ] = useState("");
  const [results, setResults] = useState<Asset[]>([]);
  const [open, setOpen] = useState(false);
  const [msg, setMsg] = useState("");
  const boxRef = useRef<HTMLDivElement>(null);

  // Debounced search (hits our cached backend list — no Alpaca call per keystroke).
  useEffect(() => {
    const query = q.trim();
    if (!query) {
      setResults([]);
      return;
    }
    const id = window.setTimeout(() => {
      api
        .searchAssets(query, 20)
        .then((r) => {
          setResults(r);
          setOpen(true);
        })
        .catch(() => setResults([]));
    }, 200);
    return () => window.clearTimeout(id);
  }, [q]);

  // Close the dropdown on any outside click.
  useEffect(() => {
    const onDoc = (e: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, []);

  const flash = (m: string) => {
    setMsg(m);
    window.setTimeout(() => setMsg(""), 3500);
  };

  async function add(sym: string, where: "exec" | "watch") {
    try {
      if (where === "exec") await api.addExecSymbol(sym);
      else await api.addWatchSymbol(sym);
      flash(`${sym} added to ${where === "exec" ? "Execution" : "Watchlist"}.`);
      setOpen(false);
      setQ("");
      setResults([]);
    } catch (e) {
      flash(`${sym}: ${(e as Error).message}`);
    }
  }

  return (
    <div className="symsearch" ref={boxRef}>
      <input
        className="symsearch-input"
        value={q}
        placeholder="Search any stock — ticker or name…"
        onChange={(e) => setQ(e.target.value)}
        onFocus={() => results.length > 0 && setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === "Escape") setOpen(false);
        }}
      />
      {open && results.length > 0 && (
        <div className="symsearch-drop">
          {results.map((a) => (
            <div key={a.symbol} className="symsearch-row">
              <div className="symsearch-id">
                <span className="symsearch-sym">{a.symbol}</span>
                <span className="symsearch-name">{a.name}</span>
              </div>
              <div className="symsearch-acts">
                <button onClick={() => add(a.symbol, "exec")} title="Add to Execution">+ Exec</button>
                <button onClick={() => add(a.symbol, "watch")} title="Add to Watchlist">+ Watch</button>
              </div>
            </div>
          ))}
        </div>
      )}
      {msg && <div className="symsearch-msg">{msg}</div>}
    </div>
  );
}
