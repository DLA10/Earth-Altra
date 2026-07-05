# Earth-Altra — An Autonomous Artificial-Intelligence Quant Trading Desk

A trading platform with two halves. A user trades **real money** through a fast,
safety-first terminal; on top of it runs an **autonomous artificial-intelligence quant
desk** that trades a **separate simulated (paper) account entirely by itself** — finding
setups with plain-rule strategies, filtering them with a machine-learning model, sizing
and managing them with a team of language-model agents, and holding every idea to an
**evaluation framework whose only job is to kill what does not work before it can ever
risk money.**

> **The one hard rule:** the user's money is real; **everything the artificial
> intelligence does is on a paper account with its own separate keys.** There is no code
> path from any model or agent to the real account. Nothing here is financial advice.

---

## Architecture at a glance

One market-data connection feeds three independent consumers. The centerpiece is the AI
quant desk: a pipeline of increasingly selective filters, wrapped by a research plane that
teaches it, an evaluation plane that governs it, and a journal that records everything.

```
╔══════════════════════════════════════════════════════════════════════════════╗
║               ONE REAL-TIME MARKET-DATA CONNECTION  (Go core)                 ║
║        live candle engine · in memory · sub-second · one feed, fanned out      ║
╚══════════════════════════════════════════════════════════════════════════════╝
        │                          │                                │
        ▼                          ▼                                ▼
┌────────────────┐   ┌──────────────────────────────┐   ┌──────────────────────┐
│ USER TERMINAL  │   │        AI QUANT DESK          │   │   MARKET SCANNER      │
│  real money    │   │   agentic · fully autonomous  │   │   ~470 stocks         │
│  manual orders │   │        paper money            │   │   ranks live movers   │
└────────────────┘   └───────────────┬──────────────┘   └──────────────────────┘
                                     │
   ┌─ THE AI DECISION PIPELINE ──────────────────  ◆ = a language-model agent ──┐
   │  a gate can only REJECT or SHRINK a trade — none can create one            │
   │                                                                            │
   │  ◆ STRATEGIST · Opus — before the open, sets today's stance & budget       │
   │        │                                                                   │
   │        ▼                                                                   │
   │    six strategies ............. find the setups           · plain rules    │
   │        ▼                                                                   │
   │    strategy scoreboard ........ bench proven losers       · evaluation     │
   │        ▼                                                                   │
   │    machine-learning gate ...... rate each setup's odds    · six models     │
   │        ▼                                                                   │
   │  ◆ SIGNAL JUDGE · Haiku ....... veto red flags, set size  · agent          │
   │        ▼                                                                   │
   │    budget allocator ........... cap at real cash, 3 max  · code            │
   │        ▼                                                                   │
   │  ◆ EXIT MANAGER · Haiku ....... trailing stop · take profit · cut early    │
   │        │            ▲                                                      │
   │        │      ◆ SENTIMENT · local model — advises                          │
   │        ▼                                                                   │
   │    PAPER broker  (simulated account)                                       │
   │        │                                                                   │
   │        ▼    after the close                                                │
   │  ◆ REVIEWER · Opus — writes the daily report card                          │
   │  ◆ RESEARCH LOOP · Opus — proposes up to 3 changes, then STOPS for the user│
   │                                                                            │
   │  ( a seventh agent — ◆ DIP ENTRY · Haiku — feeds the same allocator from a │
   │    separate messaging dip-alert stream )                                   │
   └────────────────────────────────────────────────────────────────────────────┘
      ▲                          ▲                              │
      │ taught by                │ governed by                  │ every action logged
      │                          │                              ▼
┌───────────────────┐   ┌──────────────────────┐   ┌───────────────────────────┐
│ RESEARCH & MACHINE│   │ EVALUATION FRAMEWORK │   │   DECISION JOURNAL         │
│ LEARNING (Python) │   │ rolling scoreboard,  │   │   every signal + the       │
│ nightly retrain · │   │ automatic demotion,  │   │   outcome it WOULD have had │
│ walk-forward ·    │   │ change-point alarm,  │   │   (taken or not) →          │
│ backtester        │   │ judge calibration    │   │   a self-labelling dataset │
└───────────────────┘   └──────────────────────┘   └───────────────────────────┘

  Seven agents (◆) orchestrate the desk: a STRATEGIST sets the daily stance, a
  SIGNAL JUDGE and a DIP ENTRY agent decide entries, an EXIT MANAGER runs each
  position, a SENTIMENT model advises, a REVIEWER grades the day, and a user-gated
  RESEARCH LOOP proposes improvements. Rules and a machine-learning model feed them;
  deterministic code holds the money.
```

**The core design rule — who decides what:** *plain rules find trades · a machine-learning
model rates them · language-model agents judge and manage · deterministic code owns all the
money.* The agents can only choose a direction, veto, or tighten protection; every limit
(position size, budget, stop-losses, daily halts) is enforced in code they cannot override.

---

## The strategies (how setups are found)

Six long-only intraday strategies run at the same time, on every stock, all day. Each is
plain arithmetic — no artificial intelligence — so the exact same code runs in both the
live desk and the backtester, which is why the backtest can be trusted.

| Strategy | In one line |
|---|---|
| Opening-range breakout | Buys when a stock pushes above the high of its first fifteen minutes on strong volume. |
| Average-price reclaim | Buys when a stock falls below its volume-weighted average price for the day and then fights back above it. |
| Momentum continuation | Buys a stock already trending up that pauses briefly and then resumes. |
| Dip bounce | Buys a sharp oversold drop **only after a confirmed green recovery candle** — never a falling knife. |
| Relative strength | Buys a stock that is clearly outperforming the wider market. |
| First-hour reversal | Buys a stock that was crushed early, stopped making new lows, and turned back up. |

The universe is about ninety-six liquid, reputable technology names (semiconductors,
memory, data-center, software, space, quantum) — no penny stocks.

## The machine-learning gate (what makes it selective)

The strategies alone win only about forty-six percent of the time — close to a coin flip.
A machine-learning model turns that into an edge:

- **One model per strategy** (six gradient-boosted decision-tree classifiers). Each learns,
  from thousands of past setups, which conditions separate winners from losers, and outputs
  an **expected reward** for the setup in front of it. Below a small pre-set bar, the trade
  is rejected.
- **The brain trains, the hands score.** A Python job **retrains every night** on all
  history through that day — including the day's own fresh results — so the model walks
  forward with reality. During the day the Go engine **scores each setup in a fraction of a
  millisecond in-process** (no Python in the trading path).
- **Provably identical math.** Each night's model ships with sample inputs and the trainer's
  own answers; on load, the Go side must reproduce them to six decimal places **or the model
  is refused.** Silent drift between training and live scoring is impossible.
- **Fail-open everywhere.** A missing, stale, or unverified model means the desk simply
  trades unfiltered — it degrades to the plain-strategy baseline, it can never freeze.

## The agent team (judgment and language, never math)

Six language-model agents supervise, size, manage, and explain — every one bounded by code.

| Agent | Model | What it does |
|---|---|---|
| Strategist | Opus | Before the open, reads yesterday's results and the market trend and sets the day's stance (normal / cautious / stand-down) and budget. |
| Signal judge | Haiku | A last red-flag veto on each setup (exhausted move, hostile market, thin liquidity) and a conviction score that sets position size. |
| Dip entry agent | Haiku | Buy / do-not-buy decision for the older dip-alert pipeline. |
| Exit manager | Haiku | Manages each open position — tightens the stop, takes profit near the plan's target, or cuts early if the thesis breaks. Now knows *why* the trade was opened, so it manages a quick mean-reversion trade differently from a momentum trade. |
| Sentiment | local model | Advisory market-mood read that runs on the machine, at no cost. |
| Reviewer | Opus | After the close, writes a plain-language daily report card the next morning's Strategist reads. |

Agents are invoked with forced structured-output tool calls and cached instructions, so
their output is always valid and cheap to run.

## The evaluation framework (how bad ideas get killed)

This is the part most trading projects skip, and the part that matters most here.

- **Self-labelling journal.** *Every* signal is recorded with the outcome it *would* have
  had — target or stop-loss hit first — even the ones never traded. Every few minutes, for
  free, the system produces labelled training data.
- **Rolling scoreboard + automatic benching.** Every ten minutes each strategy's last twenty
  trading days are scored. Any strategy with proven negative expectancy, or a sudden
  performance break caught by a cumulative-sum change detector, is **automatically stopped
  from trading** — and automatically reinstated when it recovers. No user involved; it
  benched two strategies on the very first live day.
- **The agents are graded too.** Each veto by the judge is joined to what the trade actually
  did, measuring whether its vetoes really dodge losers.
- **A pre-registered promotion bar.** Before any experiment runs, the bar is fixed: an idea
  must show **better trade selection AND better real dollars, on the full history AND on a
  held-out recent slice**, or it does not ship. One change at a time; failures are recorded,
  not buried.

**What the framework actually did** (receipts, not a straight-up backtest):

| Verdict | Idea | What happened |
|---|---|---|
| ✅ Shipped | Machine-learning gate | The only idea ever to clear the full bar — better selection **and** better dollars on every window: twelve months −$718 → **+$329**, recent quarter +$871 → **+$1,512**, with a smaller worst-case drawdown. |
| 🔪 Demoted | Time-of-day filter | Looked great on six months and shipped — then a twelve-month re-test showed its edge depended on a single market regime and actually **hurt**, so it was benched to a silent observer. We kill our own darlings. |
| 🔪 Killed | Curve-fit momentum tune | Best in-sample configuration (+$596) **died out-of-sample** (−$83). Caught by the walk-forward split. |
| 🔪 Killed | Linear machine-learning gate | It **anti-selected** — the trades it rejected did better than the ones it kept. Caught by a selection metric, not by profit and loss. |
| 🔪 Killed | Three-model agreement filter | Positive dollars but **negative selection** — the money came from trading less, not trading better. The framework saw through it. |

## The guardrails (why it is safe)

- **Budget capped at real cash.** The allocator syncs to the paper account's actual equity,
  so it can never try to deploy money that is not there; a drawdown automatically shrinks
  what it will risk.
- **Hard limits in code:** a shared eight-thousand-dollar budget, at most three positions at
  once, roughly one to two thousand dollars per trade sized by conviction, a one-hundred-and-
  fifty-dollar daily loss cap that halts new entries, and a full flatten at 15:55 New York
  time (at most one winner may be held overnight).
- **Every position is protected within seconds** by a trailing stop-loss placed on the
  exchange; the exit agent can only ever *tighten* that protection, never loosen it.
- **Survives restarts.** On boot the desk re-adopts any open position, re-attaches its stop,
  and resumes managing it.

## The automation (a day runs itself)

```
  before open   Strategist sets the day's stance and budget
  9:30–15:30    strategies fire → scoreboard → gate → judge → allocator → trades
  every 10 min  scoreboard re-scores and benches/reinstates strategies
  13:30         research loop studies the day and messages a summary to the user
  15:55         flatten (one overnight winner aside)
  16:10         Reviewer writes the daily report card
  ~17:05        nightly machine-learning retrain on data including today
```

The user's whole job is to keep it running and read the messages. A daily
**research loop** (built on a state-machine agent framework) digests the day and proposes
**at most three evidence-backed changes** — and it is **strictly user-gated**: it never
applies anything itself; the user makes every change by hand.

## The user's trading terminal (brief)

The real-money side is a fast, safety-first manual terminal: sub-second live charts for any
United States stock, the full range of order types (market, limit, stop-loss, trailing,
one-cancels-the-other, and bracket), and order safety treated as a product feature —
fat-finger blocks, a mandatory plain-language confirmation step, server-side re-checks,
oversell protection, and a one-click cancel-everything switch. A separate market scanner
ranks movers across roughly four hundred and seventy stocks. This side is deliberately kept
simple and untouched by the artificial intelligence.

---

## Technology

| Layer | Choices |
|---|---|
| Backend | Go — one real-time market-data connection, an in-memory candle engine, and a web-socket fan-out to the browser |
| Frontend | React with TypeScript and the TradingView charting library |
| Machine learning | Python with the LightGBM library — walk-forward training and evaluation, plain-text datasets |
| Agents | Anthropic's Claude models (Opus and Haiku) with forced structured output, plus a local model for sentiment |
| Infrastructure | A single self-contained backend with state saved to disk — no external database or queue |

## Run it

Everything starts with one click:

```
START-Live-Optimus.bat
```

(It launches the backend and the browser interface together. Keys live only in a
server-side environment file and are never committed.)

## Deeper documentation

- **[QUANT_EXPLAINED.md](QUANT_EXPLAINED.md)** — the whole system in plain words, A to Z,
  with a one-page workflow diagram.
- **[QUANT_VISION.md](QUANT_VISION.md)** — the architecture, the phase gates, and the running
  log of what the evaluation framework promoted or killed, with numbers.
- **[RESEARCH_BACKLOG.md](RESEARCH_BACKLOG.md)** — the prioritized idea queue and every
  settled verdict.

---

*Personal project. Markets are hard; the point is the engineering — a system built to find
out the truth about its own ideas, and to keep real money and automated experiments strictly
apart.*
