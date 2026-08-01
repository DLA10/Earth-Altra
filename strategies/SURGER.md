# SURGER — continuation detection

**In one line:** a stock already climbing with unusual persistence often keeps climbing —
three different detectors try to spot which climbs are real.

Paper account · 534-name universe · fires roughly once every other day · flat every night.

---

## The idea

Most of the other desks here are betting on things coming *back* — a gap closing, a price
returning to its average. SURGER is the opposite: it bets on things carrying *on*.

The difficulty is that "it's going up" describes both a genuine trend and a random walk
that happens to have gone up. So rather than one definition of "real", SURGER runs three
detectors with three different ideas of what conviction looks like, each keeping its own
books, and lets the results argue it out over time.

## The three detectors

```
   Same 534 stocks, same 1-minute bars, three different questions
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
   C2 · CUSUM            C1 · PURITY           SPECTRAL
   "is there a tide      "is the climb         "is the day one big
    under the chop?"      clean?"               slow wave?"

   Adds up every          Measures how          Reads the day's shape
   small move. Noise      one-sided the         as waves. One long
   cancels out; a real    climbing is —         wave = a trend day.
   push accumulates       shallow pullbacks,    Many short ripples =
   past a threshold.      mostly green bars.    chop, stay out.

   Fires on steady        Fires on grinding     Fires on big, smooth,
   persistent drift       conviction moves      day-long moves
        │                     │                     │
        ▼                     ▼                     ▼
   trail 1.5% → 0.5%     trail 2.5% → 1.0%     trail 3.5% → 2.0%
   (tightens once up 1.5%)  (once up 2.5%)      (once up 3.5%)
```

Each detector has **its own book, its own five slots, and its own order tag**
(`srg1_`, `srg2_`, `srg3_`), so their results can never be confused with each other even
though they share one account.

## The rules

| | |
|---|---|
| **Universe** | 534 names |
| **Entry window** | 10:00–15:30 ET (the main detectors realistically fire from ~11:30) |
| **Size** | $5,000 per position, 5 slots **per detector** |
| **Exclusivity** | only enters a symbol the account holds zero of |
| **Exit** | a trailing stop that starts wide and tightens once the trade has paid for itself |
| **End of day** | everything flat at 15:55 |
| **Bars** | completed minutes only — never the bar still forming |

## Why the leashes differ

This is the most interesting design decision in the desk, and it was settled by evidence
rather than taste.

A trailing stop is a leash. Too tight and normal wobble knocks you out of a good move; too
loose and you hand back the profit. **The right width depends on how noisy the move you're
riding is** — and the three detectors ride very different animals.

The proof came from replaying one real trade three ways. A stock that trended all day:

| leash used | what happened |
|---|---|
| C2-style (tight) | stopped out early afternoon, **+$115** |
| C1-style (medium) | stopped out a bit later, **+$98** |
| SPECTRAL-style (wide) | **rode it to the close, +$275** |

Same trade, same day. The wide leash wasn't sloppy — it was correctly sized for a day-long
wave. Equally, that width would be reckless on the short drifts C2 hunts. So each detector
keeps the leash that fits its prey.

## Honest status

Live on paper since deployment, with a **pre-registered review date** and pre-agreed rules
for what would justify promoting or benching each detector — written down *before* the
results came in, precisely so a hot streak or a bad week can't drive the decision.

Current picture: two detectors are net positive, one is negative but hasn't come close to
its bench criteria. SPECTRAL has the best record on the fewest trades, which is exactly the
situation where it's easiest to fool yourself — three wins is a hot hand, not proof.

Recent addition: every exit now records its **slippage** — the difference between where the
stop was meant to trigger and what the fill actually was. Paper profits mean nothing if the
real fills are worse than assumed, and this is the number the promote decision hangs on.

⚠️ This desk has **no automated tests**. It's also now the sole occupant of its paper
account. Add tests before changing it.

## Deeper

[`SURGER_V2.md`](../SURGER_V2.md) — the validation across four backtest windows, the exit
study, and the per-detector numbers.
