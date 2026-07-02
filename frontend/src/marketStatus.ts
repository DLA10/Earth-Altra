// Computes the current US-equity market phase (pre-market / open / after-hours /
// closed) and how long is left in it, from the user's clock converted to New York
// time. Holidays are not accounted for (best-effort visual indicator).

export type MarketPhase = "open" | "pre" | "after" | "closed";

export interface MarketStatus {
  phase: MarketPhase;
  label: string; // e.g. "Market open"
  tip: string; // hover text with time remaining
}

const PRE = 4 * 3600; // 04:00 ET — pre-market begins
const OPEN = 9 * 3600 + 1800; // 09:30 ET — regular session begins
const CLOSE = 16 * 3600; // 16:00 ET — regular session ends
const POST = 20 * 3600; // 20:00 ET — after-hours ends

function etNow(now: Date): { sec: number; wd: string } {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: "America/New_York",
    hour12: false,
    weekday: "short",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).formatToParts(now);
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? "0";
  const h = parseInt(get("hour"), 10) % 24;
  const m = parseInt(get("minute"), 10);
  const s = parseInt(get("second"), 10);
  return { sec: h * 3600 + m * 60 + s, wd: get("weekday") };
}

function fmtLeft(sec: number): string {
  if (sec < 0) sec = 0;
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${sec}s`;
}

// Seconds from the given ET time until the next weekday 04:00 (pre-market open),
// skipping Saturday and Sunday. Holidays are ignored.
function secsToNextPre(sec: number, wd: string): number {
  const weekend = wd === "Sat" || wd === "Sun";
  if (!weekend && sec < PRE) return PRE - sec;
  const order = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  let add = 86400 - sec + PRE; // to tomorrow 04:00
  let dayIdx = (order.indexOf(wd) + 1) % 7;
  while (dayIdx === 0 || dayIdx === 6) {
    add += 86400;
    dayIdx = (dayIdx + 1) % 7;
  }
  return add;
}

export function marketStatus(now: Date): MarketStatus {
  const { sec, wd } = etNow(now);
  const weekend = wd === "Sat" || wd === "Sun";

  if (!weekend && sec >= OPEN && sec < CLOSE) {
    return { phase: "open", label: "Market open", tip: `Regular session — closes in ${fmtLeft(CLOSE - sec)}` };
  }
  if (!weekend && sec >= PRE && sec < OPEN) {
    return { phase: "pre", label: "Pre-market", tip: `Pre-market — regular open in ${fmtLeft(OPEN - sec)}` };
  }
  if (!weekend && sec >= CLOSE && sec < POST) {
    return { phase: "after", label: "After-hours", tip: `After-hours — closes in ${fmtLeft(POST - sec)}` };
  }
  return { phase: "closed", label: "Market closed", tip: `Closed — pre-market opens in ${fmtLeft(secsToNextPre(sec, wd))}` };
}
