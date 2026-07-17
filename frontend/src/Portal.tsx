import { useEffect, useState } from "react";
import ExecutionEngine from "./App";
import { Decepticon } from "./Decepticon";
import { TradeHistory } from "./TradeHistory";
import { Watchlist } from "./Watchlist";
import { Metrics } from "./Metrics";
import { Quant } from "./Quant";
import { DipRise } from "./DipRise";
import { Ridp } from "./Ridp";
import { Rbt } from "./Rbt";
import { Movers } from "./Movers";
import { Sndk } from "./Sndk";
import { OrderAlerts } from "./components/OrderAlerts";
import { SymbolSearch } from "./components/SymbolSearch";
import { api } from "./api/client";

type View = "execution" | "watchlist" | "decepticon" | "history" | "metrics" | "paper-claude" | "diprise" | "ridp" | "rbt" | "sndk" | "movers";

// Portal is the single app shell. It switches between the live execution engine and
// the DECEPTICON scanner without leaving the app — same backend, same session, same
// Alpaca connection. Each view mounts only while selected, so DECEPTICON's scan
// stream isn't running (and consuming resources) while you're trading.
export default function Portal() {
  const [view, setView] = useState<View>("execution");
  const [decepticonEnabled, setDecepticonEnabled] = useState(false);

  useEffect(() => {
    api
      .config()
      .then((c) => setDecepticonEnabled(c.decepticon_enabled))
      .catch(() => {});
  }, []);

  return (
    <div className="portal">
      <nav className="portal-nav">
        <span className="portal-brand">◆ Earth-Altra</span>
        <div className="portal-tabs">
          <button
            className={view === "execution" ? "on" : ""}
            onClick={() => setView("execution")}
          >
            <i className="ti ti-bolt" aria-hidden="true" /> Execution
          </button>
          <button className={view === "movers" ? "on" : ""} onClick={() => setView("movers")}>
            <i className="ti ti-flame" aria-hidden="true" /> Movers
          </button>
          <button
            className={view === "watchlist" ? "on" : ""}
            onClick={() => {
              // Clicking Watchlist always returns to the top of the page — handy after a
              // mover-row click scrolls you down to a stock's chart.
              setView("watchlist");
              document.querySelector(".portal-body")?.scrollTo({ top: 0, behavior: "smooth" });
            }}
          >
            <i className="ti ti-eye" aria-hidden="true" /> Watchlist
          </button>
          {decepticonEnabled && (
            <button
              className={view === "decepticon" ? "on" : ""}
              onClick={() => setView("decepticon")}
            >
              <i className="ti ti-radar" aria-hidden="true" /> DECEPTICON
            </button>
          )}
          <button className={view === "history" ? "on" : ""} onClick={() => setView("history")}>
            <i className="ti ti-history" aria-hidden="true" /> History
          </button>
          <button className={view === "metrics" ? "on" : ""} onClick={() => setView("metrics")}>
            <i className="ti ti-chart-bar" aria-hidden="true" /> Metrics
          </button>
          <button className={view === "paper-claude" ? "on" : ""} onClick={() => setView("paper-claude")}>
            <i className="ti ti-robot" aria-hidden="true" /> Paper · Claude
          </button>
          <button className={view === "diprise" ? "on" : ""} onClick={() => setView("diprise")}>
            <i className="ti ti-wave-sine" aria-hidden="true" /> Dip+Rise
          </button>
          <button className={view === "ridp" ? "on" : ""} onClick={() => setView("ridp")}>
            <i className="ti ti-mountain" aria-hidden="true" /> RIDP
          </button>
          <button className={view === "rbt" ? "on" : ""} onClick={() => setView("rbt")}>
            <i className="ti ti-activity" aria-hidden="true" /> Paper · RBT
          </button>
          <button className={view === "sndk" ? "on" : ""} onClick={() => setView("sndk")}>
            <i className="ti ti-bolt" aria-hidden="true" /> Paper · SNDK
          </button>
        </div>
        <SymbolSearch />
      </nav>
      <div className="portal-body">
        {view === "execution" && <ExecutionEngine />}
        {view === "movers" && <Movers />}
        {view === "watchlist" && <Watchlist />}
        {view === "decepticon" && <Decepticon />}
        {view === "history" && <TradeHistory />}
        {view === "metrics" && <Metrics />}
        {view === "paper-claude" && <Quant />}
        {view === "diprise" && <DipRise />}
        {view === "ridp" && <Ridp />}
        {view === "rbt" && <Rbt />}
        {view === "sndk" && <Sndk />}
      </div>
      {/* Portal-wide order-fill animations — show on any tab when a live order fills/cancels. */}
      <OrderAlerts />
    </div>
  );
}
