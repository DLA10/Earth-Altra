# Earth-Altra — a trading terminal that runs its own experiments

A real-money US-equity trading terminal, and several automated strategies that trade
**paper accounts** beside it. The terminal is for the human. The strategies are for
finding out which ideas actually work, on live market data, without risking a cent.

> **The one hard rule:** the user's money is real; **every strategy runs on its own paper
> account with its own keys.** No strategy has a code path to the real account, and nothing
> auto-trades real money — every real order passes a confirm modal. Not financial advice.

---

## How it fits together

```
        ALPACA SIP FEED                    ALPACA TRADING + DATA REST
   trades · quotes · 1-min bars          orders · account · positions · fills
   one WebSocket for the whole app        history (1W/1M/6M/1Y) · assets · news
                │                                        │
                ▼                                        ▼
 ╔══════════════════════════════ GO BACKEND (:8080) ══════════════════════════════╗
 ║  candles.Engine      1/5/10-min OHLCV in memory · bad-tick guard · 1500 bars   ║
 ║  scanner             per-ticker stats: RVOL, VWAP, opening range, spread       ║
 ║  flow.Tracker        buy/sell pressure from the quote rule                     ║
 ║  hub.Hub             WebSocket fan-out · per-client (symbol, timeframe) subs   ║
 ║  api.Server (chi)    REST + server-side order validation                       ║
 ║  desks               RBT · RIDP · BREADCRUMBS · SURGER  (each own paper acct)  ║
 ╚════════════════════════════════════════════════════════════════════════════════╝
          │  WebSocket /ws                                  │  REST /api/*
          │  snapshot · candle · quote · trade_update        │  orders · account
          │  account · positions · orders · scan             │  history · fills
          ▼                                                  ▼  metrics · news
 ╔════════════════════════════ REACT + TYPESCRIPT (:5173) ════════════════════════╗
 ║  EXECUTION   live chart · draw-order · OrderPanel · positions · news · P&L     ║
 ║  MOVERS      market gainers/losers · Risers & Faders scoring · momentum + dip  ║
 ║  DECEPTICON  39 sector departments · catalyst radar · heatmap · mini-charts    ║
 ║  WATCHLIST   stacked live charts · opening-move ranking                        ║
 ║  HISTORY     every fill, from the broker      METRICS  realized-P&L analytics  ║
 ║  RBT · RIDP · BREADCRUMBS · SURGER   one read-only report page per desk        ║
 ╚════════════════════════════════════════════════════════════════════════════════╝
                                       │
                     every decision journaled → replayed → judged
                     against a pre-registered bar before anything changes
```

Go backend (one SIP feed, in-memory candles, WebSocket fan-out, chi REST) · React front
end · LightGBM where a model earns its place. That's the whole "how" — the rest of this
page is what it *does*.

---

## What you can actually do with it

```
 ┌─ TRADE ─────────────────────────────────────────────────────────────────────┐
 │  EXECUTION page — real money, every order confirmed by you                  │
 │                                                                             │
 │   market · limit · stop-loss · trailing stop ($ or %) · OCO · bracket       │
 │   draw-order: click a price on the live chart, get only the order types     │
 │               that make sense there — while candles are still forming       │
 │   guards:    blocks the classic disaster (a BUY limit above market fills    │
 │               instantly), blocks overselling, caps order size — checked in  │
 │               the panel, again at the confirm modal, again on the server    │
 │   read:      Bollinger + RSI · STRONG/WEAK/WAIT signal badge · your entry   │
 │               marked on the chart · news with sentiment · 1m→1Y ranges      │
 │   watch:     live equity & day P&L · buying power · cancel-all kill switch  │
 └─────────────────────────────────────────────────────────────────────────────┘
        ▲ decide what to trade                    ▲ decide what to trade
        │                                          │
 ┌─ FIND: momentum & dips ──────────┐   ┌─ FIND: sectors & catalysts ─────────┐
 │  MOVERS page                      │   │  DECEPTICON page                    │
 │                                   │   │                                     │
 │  top gainers / losers, live       │   │  39 sector departments, each opens: │
 │  ─────────────────────────────    │   │   • summary card — how the whole    │
 │  RISERS  — climbing off the open  │   │     sector is moving, its leaders   │
 │            scored on distance,    │   │   • mini-chart heatmap of every     │
 │            RVOL, vs-VWAP, trend   │   │     ticker, click to enlarge        │
 │  FADERS  — rolling over: this is  │   │   • catalyst flags — WHY it moves   │
 │            where dip entries live │   │   • high-RVOL markers               │
 │  per row: range · vs VWAP ·       │   │  catalyst radar across all 39, so   │
 │  signal · entry · exit read       │   │  a waking sector shows up early     │
 │  click any row → live chart       │   │  ~683 tickers scanned continuously  │
 └───────────────────────────────────┘   └─────────────────────────────────────┘

 ┌─ REVIEW ────────────────────┐  ┌─ WATCH ──────────┐  ┌─ EXPERIMENT ────────┐
 │  HISTORY  every fill, from  │  │  WATCHLIST       │  │  one page per paper  │
 │           the broker        │  │  stacked live    │  │  desk: positions,    │
 │  METRICS  realized P&L      │  │  charts, drag    │  │  P&L, every closed   │
 │           rebuilt from      │  │  to reorder,     │  │  trade and why it    │
 │           actual fills      │  │  opening-move    │  │  closed              │
 │           — not estimates   │  │  ranking         │  │                      │
 └─────────────────────────────┘  └──────────────────┘  └──────────────────────┘
```

**The short version:** DECEPTICON and MOVERS tell you *what* is in play and why — one by
sector and catalyst, the other by momentum and dips. The Execution page is where you act
on it, with every order type you need and a guard on each. History and Metrics tell you
honestly how it went, rebuilt from real fills rather than from what you meant to do. And
the desk pages are the laboratory running beside all of it.

## The paper desks

Four strategies trade paper accounts alongside the terminal. Each is an experiment with a
pre-registered success bar.

### RBT — Rubber-Band Trading

**The idea:** two stocks that normally move together drift apart; the band snaps back.

- **Scans once a day**, in the last ten minutes before the close. No intraday babysitting.
- **199 liquid names**, screened for genuine cointegration — not a hunch list.
- Enters only when a pair is stretched **2σ or more**; a LightGBM model ranks the
  candidates so only the **top 5** get capital.
- **Trades both directions** — long the cheap leg, short the rich one.
- **Five slots, equal weight** (~$20k each), so no single idea dominates the book.

**Why you'd want it:** it's the calm one. Positions are *meant* to sit for days, so a red
afternoon means nothing. Each has four independent ways out — the target, a daily-close
stop, an emergency stop resting at the exchange, and a hard five-session deadline — so
nothing drifts forever. A recent example: **GOOGL, held two sessions, +$709.**

→ **[Read more about RBT](strategies/RBT.md)**

### REVERTER — the twitch trader

**The idea:** within a single minute, price overshoots its own short-term average. Buy the
overshoot, sell the snap-back.

- Fires when a stock drops **1.5σ below its 15-minute mean**, exits when it returns.
- Holds for **minutes** — often under five. Hundreds of small round trips a day.
- A hard floor at 4σ and a **15:55 flatten**: it never carries risk overnight.

**Why you'd want it:** it's the fastest measuring instrument in the system. Trading that
often answers questions in *days* instead of months — and it already answered one. Its
edge is real in the **first 30 minutes** and negative after 10:00, a pattern that has now
repeated for eight straight sessions. That finding is worth more than the P&L.

→ **[Read more about REVERTER](strategies/REVERTER.md)**

### BREADCRUMBS — the machine-learned scalper

**The idea:** in volatile names, a short-lived move is often predictable enough to take a
small, quick bite. **22 volatile stocks**, scored bar by bar, three independent yeses
required before any entry (see the pipeline below).

**Why you'd want it:** the most self-aware desk here. It benches any symbol that stops it
out twice in a day, waits five minutes before re-entering anything, and books every exit
from the broker's actual fill price rather than what it hoped to get. It's also running a
**live A/B experiment on itself** — every position cut by the $25 rule spawns a shadow
twin that keeps trading uncut, so "does this rule help?" is settled by evidence on a fixed
date instead of by opinion.

→ **[Read more about BREADCRUMBS](strategies/BREADCRUMBS.md)** — including how the model is trained and what it looks at

### SURGER — continuation detection

**The idea:** a stock already climbing with unusual persistence often keeps going. Three
detectors hunt the same prey with different noses — a **CUSUM** drift-break, a **trend
purity** score, and a **spectral** read of the day's dominant wave. Each keeps its own
book and its own trailing leash (tight for short drifts, wide for day-long waves), with
`srg1_/srg2_/srg3_` order tags so the three can never be confused. Fires roughly once
every other day by design; flat weeks cost nothing.

→ **[Read more about SURGER](strategies/SURGER.md)**

---

## What ties them together

**Nothing ships on a good story.** Every idea is replayed on historical data against a
pre-registered bar before going live, and every live desk journals enough to replay its own
decisions later. Two things that discipline has already caught:

- A knife-detection program — 14 detectors and a model with a genuine 0.81 AUC — produced
  **no dollar edge at all** once measured against a fair control. Cancelled, not shipped.
- The AI agent team this project was originally built around: **retired** after 77 graded
  trades came in at −$66, with plain deterministic math beating the model's exits.

**Safety is structural, not a promise.** Separate accounts and separate keys. Orders settled
to a terminal state before anything is booked. Cancels confirmed before a replacement is
placed. Positions reconciled against the broker on every restart.

## Run it

```bash
scripts/check-keys.ps1      # verify Alpaca keys + SIP entitlement
scripts/run-backend.ps1     # Go backend → :8080
scripts/run-frontend.ps1    # Vite dev    → :5173
```

Or `START-Live-Optimus.bat` to launch both. Keys live only in a server-side environment
file and are never committed.

## Going deeper

**Per-strategy pages** — plain-language, one each:
[RBT](strategies/RBT.md) · [REVERTER](strategies/REVERTER.md) ·
[BREADCRUMBS](strategies/BREADCRUMBS.md) · [SURGER](strategies/SURGER.md)

`CLAUDE.md` is the maintained technical reference — architecture, every desk's dials and
the operations playbook. Research write-ups: `RUBBER_BAND_TRADING.md`, `SURGER_V2.md`,
`HARVEST_STUDY.md`, `KNIFE_STUDY.md`, `REGIME_DETECTOR_STUDY.md`, `REVERTER_FILTERS.md`.
Retired systems: `DIP_RISE_ARCHIVE.md`, `SNDK_RETIREMENT.md`, `AI_QUANT_LOG_DIGEST.md`.

---

*Personal project. Markets are hard — the point is the engineering: a system built to find
out the truth about its own ideas, and to keep real money and automated experiments
strictly apart.*
