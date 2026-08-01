# RBT: Rubber-Band Trading

**In one line:** two stocks that normally move together drift apart; RBT bets the band
snaps back.

Paper account · scans once a day · holds for days · trades long **and** short.

---

## The idea

Some pairs of stocks genuinely belong together: same business, same customers, same
shocks. Their prices wander, but the *gap* between them tends to return to normal. When
that gap stretches unusually wide, one side is temporarily too cheap and the other too
expensive. RBT buys the cheap one, sells the rich one, and waits for the gap to close.

It's called rubber-band trading because that's the whole thesis: stretch, then snap back.

## How it decides

```
  Once a day, 15:50 ET (10 minutes before the close)
            │
            ▼
  199 liquid stocks  →  which ones are statistically tied together?
            │              (a cointegration test, refreshed nightly)
            ▼
  For each tied pair: how stretched is the gap right now?
            │
            ▼
  Only consider gaps stretched 2σ or more          ← "unusually wide"
            │
            ▼
  A LightGBM model scores each candidate:
  "given how this setup looks, how often did the gap actually close?"
            │
            ▼
  Take the top 5 by that score. Fill the 5 slots. Done for the day.
```

## The rules

| | |
|---|---|
| **Universe** | 199 liquid names, screened for real statistical relationships |
| **When it trades** | once daily, 15:50–16:00 ET. Nothing intraday. |
| **Entry** | gap stretched ≥ 2σ, ranked by model probability |
| **Direction** | long the cheap leg, short the rich one (needs margin enabled) |
| **Size** | 5 slots, equal weight, about $20k each, capped by account equity |
| **Exit 1, target** | the gap returns to its average → take the profit |
| **Exit 2, stop** | moves 1.5×ATR against us on a daily close → controlled loss |
| **Exit 3, emergency** | a 2.5×ATR stop resting at the exchange, live around the clock |
| **Exit 4, the clock** | still open after 5 sessions → close it, the bet expired |

Exits 1, 2 and 4 are checked once a day at the same 15:50 window. Exit 3 is the only one
that can fire while nobody's watching.

## Why it's built this way

**Once a day, on purpose.** The thesis is about daily closing prices, so that's what it
trades on. Watching it intraday would tempt it into decisions its research never tested.

**Four ways out, three of them boring.** Most positions leave by target or by the clock.
The emergency stop exists for the day a stock gaps on news. That is the one that actually
hurts, and it's the only exit that doesn't wait for the daily check.

**Five equal slots.** No conviction sizing, no doubling down. One idea can't sink the book.

## Honest status

Trading live on paper. Its best result so far: **GOOGL held two sessions, +$709**, a
textbook stretch-and-snap. Its worst was a bug, not a bad idea: a position whose
protective stop was mis-sized after a partial fill, which cost −$910 and produced the
order-lifecycle rules every desk now follows.

**Open question for review:** the emergency stop is currently 2.5×ATR. On calm megacaps
that's roughly 2%; on jumpy names it's 5–6%. Whether a flat 2% would be better is a
scheduled test. The thing to measure isn't just total P&L, but how often a tighter stop
would fire on a position that recovered by the close anyway.

## Deeper

[`RUBBER_BAND_TRADING.md`](../RUBBER_BAND_TRADING.md) covers the maths, the cointegration
screen, and sector-by-sector results.
