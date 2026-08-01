# Architecture

How Earth-Altra is built, in plain terms. If you only want to know what it *does*, the
[README](README.md) covers that.

---

## The one-sentence version

One connection to the market feeds everything; a Go program keeps the prices in memory and
pushes them to the browser as they happen; a React app draws them; and orders travel a
completely separate, slower, more careful path.

---

## Why two languages

**Go handles anything that must be fast.** Market data arrives continuously, thousands of
trades a second across hundreds of symbols. Go handles that kind of load without the pauses
that would show up as a chart stuttering, and it's happy running dozens of things at once
(each strategy, the price engine, the scanner) without them tripping over each other.

**React handles anything you look at.** Charts, tables, order panels. It never talks to the
broker directly and it never holds a secret.

**Python handles the models.** Training happens offline, on a schedule, in the language
where the ML libraries live. The trained model is written to a file; the Go side just reads
scores from it. Nothing in the trade path waits on Python.

## One feed, many consumers

The expensive, fragile thing is the market data connection. So there's exactly **one**, and
everything inside the program reads from it:

```
   Alpaca SIP feed  ──►  price engine  ──┬──►  your charts
   (trades, quotes,                      ├──►  the scanner
    1-minute bars)                       ├──►  buy/sell pressure tracker
                                         └──►  each trading strategy
```

Adding a strategy doesn't add a connection. If the feed drops, it reconnects and refills the
gap once, for everybody.

**Subscribe to anything, any time.** You can open a chart for a symbol nobody was watching.
The browser asks for it, the server notices it isn't tracked, fetches today's history,
subscribes to it on the live feed, and starts streaming, usually before the chart finishes
drawing. Symbols are only ever *added* this way, never removed mid-session, because
removing one could disturb something that's mid-trade.

## What the backend is made of

| Piece | What it does |
|---|---|
| **Price engine** | Keeps 1-, 5- and 10-minute candles in memory for every tracked symbol. Folds each trade into the candle still forming, and throws out obviously bad prices (a zero, or a wild spike that no real trade would make). |
| **Hub** | The broadcaster. Each browser tab says which symbol and timeframe it wants; the hub sends only that. Updates are throttled to a few per second, fast enough to look live, slow enough not to flood the browser. |
| **Scanner** | Per-stock statistics the whole app leans on: relative volume, VWAP, how far it's moved from the open, the day's range, the spread. This is what the Movers scores and the DECEPTICON page are built from. |
| **Pressure tracker** | Estimates how much volume was buyer-driven versus seller-driven, by comparing each trade to the quote at that moment. |
| **REST API** | Everything that isn't a live price: placing orders, account and positions, historical bars, your fill history, news, the desk reports. |
| **The desks** | Four independent strategies. Each has its own account, its own money, its own journal, and its own file of saved state. |

## Two paths, on purpose

This is the most important design decision in the whole system.

```
  PRICES  ──  fast path  ──  WebSocket  ──  read-only, hundreds of messages a minute
  ORDERS  ──  slow path  ──  REST       ──  one at a time, validated three times
```

Prices stream over a WebSocket because they must be instant and nothing bad happens if one
is missed, another arrives in a moment. **Orders never travel that way.** They go over
ordinary web requests, one at a time, each with an explicit reply. A dropped connection can
never half-place an order, and no amount of price traffic can delay one.

## How an order actually travels

1. **You fill in the panel.** It checks the obvious things immediately, enough shares to
   sell, a stop on the correct side of the price, size within your cap.
2. **The confirm modal appears.** Always. It restates what will happen in plain language,
   and warns loudly if the price you picked will fill instantly rather than wait.
3. **The server checks everything again**, because a browser can be wrong or bypassed. Same
   rules, enforced where they can't be edited.
4. **Only then does it reach the broker**, and the result comes back as a live fill
   notification.

Three layers, deliberately repetitive. The rule that matters: **nothing places a real order
without a human pressing a button.**

## How the strategies stay isolated

Each desk runs on **its own paper account with its own keys**. Not a sub-account, a
genuinely separate one. If a strategy has a bug and sells everything it can see, the only
thing it can see is its own money.

Within an account, every order carries a **tag** identifying which strategy placed it, so
books can never be confused even where something is shared. And a desk with no keys
configured simply doesn't start.

They also all follow the same hard-won rules about orders, learned from real incidents:

- **Wait for the final answer.** An order that's half-filled isn't finished. Book only what
  actually filled, once the broker says it's done.
- **Confirm a cancel before replacing it.** Cancels aren't instant. While the old order
  lives it still holds the shares, so an immediate replacement gets rejected.
- **Never invent an exit.** Record the price the broker actually got, not the price you
  hoped for when you decided to sell.
- **Reconcile on every restart.** Compare what the strategy thinks it holds against what
  the account really holds, and fix the difference before doing anything else.

## What the browser does

A single-page app with a tab per view. **Each tab only runs while you're looking at it**,
the sector scanner isn't consuming anything while you're on the Execution page.

Charts are canvas-based for speed. Live updates modify the last candle in place rather than
redrawing, and your zoom is preserved as prices come in, because nothing is more annoying
than a chart that jumps while you're reading it.

**Where money is concerned, the browser is never the source of truth.** Your P&L is
recalculated from your actual fills rather than trusting the broker's blended average
cost, which can mislead after you've added to a position.

## Where things are stored

There is no database. State is small and file-based:

- **In memory:** the candles. Rebuilt from history on startup in a few seconds.
- **On disk:** each strategy's open positions and trade log, your symbol lists, the trained
  models, and every decision journal. Plain JSON and line-per-entry files you can open in a
  text editor.
- **Never in the repository:** anything containing real account activity, and the secrets
  file. Those stay on the machine.

The journals are the point, not a side effect. Every strategy writes down what it saw and
why it acted, so a decision can be replayed months later and judged on evidence rather than
memory.

## Running and checking it

```bash
scripts/check-keys.ps1      # are the keys valid, and is real-time data entitled?
scripts/run-backend.ps1     # Go server → :8080
scripts/run-frontend.ps1    # browser app → :5173
```

Before any change is considered done: the Go code must build, pass vet, and pass its tests;
the front end must type-check and build. Then the important one, **open the Execution page
and confirm it still streams and still places orders correctly**, because that's the part
with real money behind it.

## Further reading

[`CLAUDE.md`](CLAUDE.md) is the full technical reference: every module, every configuration
value, the daily schedule, and the operations playbook for when something misbehaves.
Per-strategy pages live in [`strategies/`](strategies/).
