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

## The terminal (this is the part that trades real money)

**Every order type you actually need, with a guard on each one.**

- **Market** buy/sell in shares or dollars · **limit** orders that wait for your price ·
  **stop-loss** · **trailing stop** in dollars or percent · **OCO** (take-profit and
  stop-loss on a held position, whichever hits first) · **bracket** (entry + target + stop
  submitted as one).
- **Draw your order on the chart.** Click a price level while candles are still forming
  and the popup offers only the order types that make sense at that price — set a resting
  limit, a stop, or a bracket by pointing at where you want it.
- **Fat-finger protection at every layer.** The classic disaster is a *buy* limit set
  *above* the market: it looks like a patient order and fills instantly. The panel blocks
  it, the confirm modal spells out the direction rule in plain language, and the backend
  re-checks before anything reaches the broker. Overselling and oversized orders are
  refused the same way.
- **Live chart with decision aids** — Bollinger bands and a time-synced RSI pane, a signal
  badge that grades the setup STRONG / WEAK / WAIT, a green line marking your entry, and
  1m/5m/10m intraday or 1W/1M/6M/1Y historical ranges. Indicators inform; they never place
  an order.
- **News beside the chart**, headlines with sentiment for whatever you're holding, plus a
  one-click "why is it moving" summary.
- **Live account header** — equity and day P&L marked to the streaming price between REST
  polls, with cost basis rebuilt from actual fills, plus a **cancel-all kill switch**.

## MOVERS — momentum and dips, ranked

The page for deciding *what* to trade. Whole-market **top gainers and losers**, then the
scored tables:

- **Risers** — names climbing off the open, scored on distance travelled, relative volume,
  position versus VWAP and trend, so you can tell a real move from a twitch.
- **Faders** — the same treatment for names rolling over, which is where dip entries live.
- Each row carries **range, vs-VWAP, a signal grade, an entry level and an exit read** —
  and clicking it opens a live chart of that symbol, even one nobody was tracking a second
  ago (the backend starts streaming it on demand).

## DECEPTICON — the sector scanner

An event-driven scan of ~683 tickers across **39 sector departments** (AI infrastructure,
semis, quantum, nuclear, biotech, defence…). Each department expands into:

- a **summary card** — how the whole sector is moving right now, and its top movers;
- a **mini-chart heatmap** of every ticker in it, click-to-enlarge into a live chart;
- **catalyst flags** on each ticker (what's driving it) and a **high-RVOL** marker for
  unusual volume;
- a **catalyst radar** across all departments, so a sector waking up is visible before it
  shows up in the price.

---

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

### SURGER — continuation detection

**The idea:** a stock already climbing with unusual persistence often keeps going. Three
detectors hunt the same prey with different noses — a **CUSUM** drift-break, a **trend
purity** score, and a **spectral** read of the day's dominant wave. Each keeps its own
book and its own trailing leash (tight for short drifts, wide for day-long waves), with
`srg1_/srg2_/srg3_` order tags so the three can never be confused. Fires roughly once
every other day by design; flat weeks cost nothing.

---

## How a model actually decides (BREADCRUMBS)

```
 1-min bars, RTH only, 22 volatile names          ← never the still-forming bar
            │
            ▼
 FEATURES  9 scale-free numbers per bar
   Z_Score      how stretched vs its own recent mean
   RSI_5 · RSI_14   short & standard momentum
   ROC_3 · ROC_10   rate of change, two horizons
   ATR_Ratio    is volatility expanding or calming?
   MACD_Hist    momentum turning
   Z_BB         position inside the Bollinger band
   Vol_Ratio    volume vs its own average
            │
            ▼
 LABEL  triple barrier, 5-min horizon           ← what "good" means, decided up front
   did +0.57% arrive BEFORE −0.71%?  →  1 / 0
            │
            ▼
 TRAIN  LightGBM classifier, pooled across all 22 names
   ~205,000 labelled bars · retrains weekly on its own fresh outcomes
            │
            ▼
 SERVE  probability per bar, 0.00 → 1.00
            │
            ▼
 THREE GATES — all must agree
   prob ≥ 0.65        model conviction
   Close > EMA-100    with the trend, not against it
   |Close − VWAP| ≤ 2σ  not already extended
            │
            ▼
 EXECUTE  fixed plan, decided before entry
   target +0.57%  →  arms a 0.2% trailing stop, floored at the target
   hard stop −0.71%      ·  $25 dollar-cut  ·  flat by 15:59
            │
            ▼
 JOURNAL  probability, signal price, entry slippage, best/worst excursion
          → replayed later to grade the model, not just the P&L
```

**RBT uses the same shape on a slower clock:** daily bars → features
`Z_5 · Z_GARCH · ATR_Ratio · BB_Width · Rel_Vol · RSI_14` → label "did the spread reach its
mean before a 1.5×ATR stop?" → LightGBM probability → the top 5 by probability get the
five slots. Training labels are generated at the *same* 2σ stretch the live scorer sees,
so the probability is calibrated on exactly the setups it will be asked to judge.

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

`CLAUDE.md` is the maintained technical reference — architecture, every desk's dials, and
the operations playbook. Study write-ups: `RUBBER_BAND_TRADING.md`, `SURGER_V2.md`,
`HARVEST_STUDY.md`, `KNIFE_STUDY.md`, `REGIME_DETECTOR_STUDY.md`, `REVERTER_FILTERS.md`.
Retired systems: `DIP_RISE_ARCHIVE.md`, `SNDK_RETIREMENT.md`, `AI_QUANT_LOG_DIGEST.md`.

---

*Personal project. Markets are hard — the point is the engineering: a system built to find
out the truth about its own ideas, and to keep real money and automated experiments
strictly apart.*
