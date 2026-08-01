# Earth-Altra — a trading terminal that runs its own experiments

A real-money US-equity trading terminal, and four automated strategies that trade
**paper accounts** beside it. The terminal is for the human. The strategies are for
finding out which ideas actually work, on live market data, without risking a cent.

> **The one hard rule:** the user's money is real; **every strategy runs on its own paper
> account with its own keys.** No strategy has a code path to the real account, and nothing
> auto-trades real money — every real order passes a confirm modal. Not financial advice.

---

## How it fits together

```
              ONE market-data connection (Alpaca SIP, sub-second)
                                  │
        ┌─────────────────────────┼─────────────────────────┐
        ▼                         ▼                         ▼
   YOUR TERMINAL             MOVERS + SCANNER          FOUR PAPER DESKS
   real money                what's in play            each on its own account
   confirm modal             ranked, scored,           ┌────────────────────┐
   on every order            journaled all day         │ RBT       days     │
                                                       │ REVERTER  minutes  │
                                                       │ BREADCRUMBS ~5 min │
                                                       │ SURGER    surges   │
                                                       └────────────────────┘
                                  │
                     every decision journaled → replayed → judged
                     against a pre-registered bar before anything changes
```

Go backend (one SIP feed, in-memory candles, WebSocket fan-out) · React front end ·
LightGBM where a model earns its place. That's the whole "how" — the rest of this page is
what it *does*.

---

## RBT — patient pairs trading

**The idea:** two stocks that normally move together drift apart; bet they snap back.

- **Scans once a day**, in the last ten minutes before the close. No intraday babysitting.
- **199 liquid names**, screened for genuine statistical relationships — not a hunch list.
- Enters only when a pair is stretched **2σ or more**; a LightGBM model ranks the
  candidates so only the **top 5** get capital.
- **Trades both directions** — long when a name is unusually cheap, short when it's
  unusually rich.
- **Five slots, equal weight** (~$20k each), so no single idea can dominate the book.

**Why you'd want it:** it's the calm one. Positions are *meant* to sit for days, so a red
afternoon means nothing. Each one has four independent ways out — the target, a
daily-close stop, an emergency stop resting at the exchange, and a hard five-session
deadline — so nothing drifts forever. A recent example: **GOOGL, held two sessions, +$709.**

## REVERTER — the twitch trader

**The idea:** within a single minute, price overshoots its own short-term average. Buy the
overshoot, sell the snap-back.

- Fires when a stock drops **1.5σ below its 15-minute mean**, exits when it returns.
- Holds for **minutes** — often under five. Hundreds of small round trips a day.
- A hard floor at 4σ and a **15:55 flatten**: it never carries risk overnight.

**Why you'd want it:** it's the fastest measuring instrument in the system. Because it
trades so much, it answers questions in *days* instead of months — and it has already
answered one. Its edge is real in the **first 30 minutes** of the session and negative
after 10:00, a pattern that has now repeated for eight straight sessions. That finding is
worth more than the P&L.

## BREADCRUMBS — the machine-learned scalper

**The idea:** in volatile names, a short-lived move is often predictable enough to take a
small, quick bite.

- **22 deliberately volatile stocks**, scored bar by bar by a **pooled LightGBM model** on
  nine scale-free features.
- A trade needs **three independent yeses**: model confidence ≥0.65, price above its
  100-bar trend, and not already stretched past 2σ from VWAP.
- Pre-committed exits: **+0.57% target, −0.71% stop**, then a 0.2% trailing stop that locks
  the target in once it's reached.
- **Retrains itself weekly** on its own fresh outcomes — no manual model babysitting.

**Why you'd want it:** it's the most self-aware desk here. It benches any symbol that stops
it out twice in one day, waits five minutes before re-entering anything, and books every
exit from the broker's actual fill price rather than what it hoped to get. It's also
running a **live A/B experiment on itself**: every position cut by the $25 rule spawns a
shadow twin that keeps trading uncut, so "does this rule help?" gets settled by evidence
on a fixed date instead of by opinion.

## MOVERS — what's actually in play, all day

**The idea:** before any strategy fires, you want to know where the action is.

- Whole-market **gainers and losers**, refreshed continuously.
- A **Risers** table scoring each name on how far it's come off the open, relative volume,
  distance from VWAP and momentum — one number for "is this real?"
- **Catalyst radar** across 39 sectors and ~683 tickers, so a move arrives with a reason.
- Click any tile for a **live chart** — even of a symbol nobody was tracking a second ago;
  the backend starts streaming it on demand.
- A **shadow recorder** journals the whole table every 15 minutes from 09:45 to 16:00 and
  keeps following every name that lit up.

**Why you'd want it:** it's the situational-awareness layer — and the honest one. It writes
down what it flagged, then tracks what actually happened next, so "the movers page is
useful" can be checked against a record instead of memory.

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
placed. Positions reconciled against the broker on every restart. And the terminal blocks
the fat-finger mistakes a novice actually makes — like a buy limit set *above* the market,
which fills instantly.

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
the operations playbook. Study write-ups: `SURGER_V2.md`, `HARVEST_STUDY.md`,
`KNIFE_STUDY.md`, `REGIME_DETECTOR_STUDY.md`, `REVERTER_FILTERS.md`. Retired systems:
`DIP_RISE_ARCHIVE.md`, `SNDK_RETIREMENT.md`, `AI_QUANT_LOG_DIGEST.md`.

---

*Personal project. Markets are hard — the point is the engineering: a system built to find
out the truth about its own ideas, and to keep real money and automated experiments
strictly apart.*
