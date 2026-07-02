import { useEffect, useState } from "react";
import type { Account } from "../types";
import { marketStatus, type MarketStatus } from "../marketStatus";

interface Props {
  mode: "LIVE" | "PAPER";
  feed: string;
  status: "connecting" | "open" | "closed";
  account: Account | null;
  liveEquity: number; // equity marked to live prices (glides between REST polls)
  dayPL: number; // total unrealized profit/loss in dollars across open positions
  sipEntitled: boolean | null;
  onKillSwitch: () => void;
}

export function Header({ mode, feed, status, account, liveEquity, dayPL, sipEntitled, onKillSwitch }: Props) {
  // Prefer the live-marked equity; fall back to the REST value before positions load.
  const equity = liveEquity > 0 ? liveEquity : account?.equity ?? 0;

  return (
    <header className="header">
      <div className="brand">
        <span className="logo">◆ OPTIMUS</span>
        <MarketBadge />
        {mode === "PAPER" && <span className="mode-badge paper">PAPER</span>}
        <span className="feed-badge" title="Market data feed">
          {feed.toUpperCase()}
          {sipEntitled === false && <span className="sip-warn"> · SIP NOT ENTITLED</span>}
        </span>
      </div>

      <div className="acct-stats">
        <Stat label="Equity" value={`$${equity.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`} />
        <Stat
          label="Day P/L (open positions)"
          value={`${dayPL >= 0 ? "+" : "-"}$${Math.abs(dayPL).toFixed(2)}`}
          tone={dayPL >= 0 ? "pos" : "neg"}
        />
        <Stat
          label="Buying power"
          value={`$${(account?.buying_power ?? 0).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`}
        />
      </div>

      <div className="header-right">
        <span className={`conn-dot ${status}`} title={`Live data connection: ${status}`} />
        <button
          className="kill-switch"
          onClick={onKillSwitch}
          title="Cancels every open / resting order on Alpaca (limit, stop, OCO). It does NOT sell shares you already own."
        >
          ⛔ Cancel all orders
        </button>
      </div>
    </header>
  );
}

// MarketBadge shows the current US-market phase and, on hover, the time left in it.
// It self-updates each second so the countdown stays live without re-rendering the
// rest of the header.
function MarketBadge() {
  const [st, setSt] = useState<MarketStatus>(() => marketStatus(new Date()));
  useEffect(() => {
    const id = window.setInterval(() => setSt(marketStatus(new Date())), 1000);
    return () => window.clearInterval(id);
  }, []);
  return (
    <span className={`market-badge ${st.phase}`} title={st.tip}>
      {st.label}
    </span>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: "pos" | "neg" }) {
  return (
    <div className="stat">
      <span className="stat-label">{label}</span>
      <span className={`stat-value ${tone ?? ""}`}>{value}</span>
    </div>
  );
}
