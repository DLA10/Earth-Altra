# The AI Quant Desk, explained A→Z

*A plain-language tour of the autonomous paper-trading system inside Earth-Altra — written
as if explaining it to someone seeing it for the first time. Technical names appear in
(parentheses) so you can find them in the code.*

---

## 1. What this is, in one paragraph

A fully autonomous intraday trading desk that runs on a **paper account** (fake money,
real market). Six simple pattern-spotters watch ~96 quality tech stocks all day and
propose trades; a stack of filters — a machine-learned profitability model, an
automatic strategy scoreboard, deterministic risk rails, and an AI judge — throws away
the weak ideas; the survivors get bought with strict stop-losses and are force-sold
before the close. Every night the system retrains its ML model on the day's fresh
outcomes, an AI reviewer writes a report card, and a research agent studies the logs and
texts the operator on Telegram. Humans set policy; the machine executes it.

**The one hard rule:** this entire system lives on a separate paper-trading account with
its own API keys. It has no code path to the real-money account that the Execution page
uses. Nothing described here can spend a real dollar.

---

## 1b. The whole workflow on one page

Everything below, top to bottom. `[Go]` = deterministic code, `[ML]` = trained model,
`[AI]` = LLM agent; `═►` order/money flow, `─►` data flow, `⟳` scheduled loop.

```
════════════════════════════════════════════════════════════════════════════
   EARTH-ALTRA · AI QUANT DESK — COMPLETE WORKFLOW        ‹PAPER ACCOUNT ONLY›
════════════════════════════════════════════════════════════════════════════
 LEGEND   [Go] deterministic code   [ML] trained model   [AI] LLM agent
          ═► order/money flow   ─► data flow   ⟳ scheduled loop   ✓ pass  ✗ reject


━━━ 1 · MARKET DATA IN ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ [Go] ━━

     Alpaca SIP WebSocket  ── one connection, every trade & quote, live
                 │
                 ▼
     Candle engine ── folds ticks into 1-minute bars in memory
     ~96 curated tech stocks  +  SPY/QQQ/SMH (market context, never traded)
                 │
                 │  additive listener — adds ZERO latency to the
                 ▼  real-money Execution page (untouched, separate)


━━━ 2 · DETECTION — 6 scouts, all at once, every stock, every bar ━━━━ [Go] ━━
        pure math · no AI · the SAME code runs in the backtester

     orb_breakout    break of the first-15-min box on volume
     vwap_reclaim    flush below VWAP, then a green close back above
     momentum_cont   new high after a shallow pullback in an uptrend
     dip_bounce      deep oversold drop + a confirmed green bounce
     rel_strength    new highs while QQQ (the market) is flat/red
     fh_reversal     morning dump stabilises & turns  (weakest → watched hardest)

     each fires →  SIGNAL { entry · stop · target (ATR-scaled) · feature snapshot }
     limits: 1 per (strategy,stock)/30min · max 2/day · LONG ONLY
                 │
        ┌────────┴─────────────────────────────┐
        ▼                                       ▼
  (every signal, traded or not)          (this signal continues ↓)
        │
━━━ 3 · THE JOURNAL ━━━━ [Go] ━━━━━━━━━━━━━━━━━━━━━┓
   writes EVERY signal + its counterfactual        ┃  feeds ⟳
   (would target or stop hit first?)               ┃  the ML retrain,
   → a self-labelling training set, grows daily ───┺──→ scoreboard & research
                                                          (see §7)

━━━ 4 · THE ENTRY GAUNTLET — survive ALL, in this exact order ━━━━━━━━━━━━━━━━
        any gate can REJECT (→ logged, dropped) · NONE can create a trade

   SIGNAL
     │
     ▼  [Go] (1) TOD gate ............ SHADOW-ONLY now — logs a verdict,
     │                                 blocks nothing (QUANT_TOD_GATE=false)
     ▼  [Go] (2) Scoreboard demotion . strategy losing over 20d? .......... ✗
     │                                 (automatic · self-reinstating)
     ▼  [ML] (3) clf gate ★ .......... the strategy's own model scores
     │                                 expected R;  EV < +0.03R? ........... ✗
     │                                 ← the promoted edge (6 models)
     ▼  [Go] (4) Session guard ....... fresh entry after 15:30 ET? ........ ✗
     │
     ▼  [Go] (5) Posture ............. Strategist says stand_down? ........ ✗
     │
     ▼  [Go] (6) Daily loss cap ...... day ≈ −$150? → halt for the day .... ✗
     │
     ▼  [Go] (7) Allocator free? ..... no slot / no cash / already held? .. ✗
     │                                 (cheap check BEFORE any LLM call)
     ▼  [AI] (8) Entry judge (Haiku) . reads the tape like a trader:
     │            • DEFAULT = APPROVE — vetoes only on a nameable red flag
     │            • veto: exhaustion / hostile tape / thin / too late .... ✗
     │            • else APPROVE + conviction 0–1  (drives size)
     │            • judge errors out → skip this one trade (fail-closed) . ✗
     ▼  [Go] (9) Cautious posture? ... conviction < 0.65? ................ ✗
     │
     ▼  ✓ survived every gate → BUY


━━━ 5 · SIZING & FUNDING — the budget allocator ━━━━━━━━━━━━━━ [Go, not AI] ━━

     shared budget $8,000 · max 3 open · 1 per symbol · recycles on close
     conviction ≥0.7 → full slice (~$2,000)   ·   else → half (~$1,000)
     contention (many buys, few slots) → rank by quality, fund best first
                 │
                 ▼  Fund(sym,$) commits the capital


━━━ 6 · POSITION LIFECYCLE & EXITS ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

   [Go] ═► MARKET BUY on the PAPER account
            │
   [Go] ═► Trailing stop 1.5% placed on the exchange IMMEDIATELY
            │        (sub-second safety · follows price up · never loosens)
            ▼
   [AI]    Agent 3 exit loop — every 5–12 s while the position is open
            │   sees: price · P&L% · minutes held · current stop ·
            │         dist to VWAP · RSI · RVOL · market · last 10 bars
            │   picks: hold · tighten_stop(up-only) · take_profit · exit_now
            │   [Go] guardrails: never widens a stop · always keeps one live
            ▼
   [Go] ═► 15:55 ET — FLATTEN everything
            │   exception: at most ONE winner ≤ $2,000 may ride overnight
            ▼
        position closes → P&L realised → capital recycled to allocator (§5)
                        → OUTCOME written back to the JOURNAL (§3) ⟲


━━━ 7 · THE BRAINS THAT SUPERVISE & EVOLVE  (times ET) ━━━━━━━━━━━━━━━━━━━━━━━

   08:50–09:25  [AI] Strategist (Opus) ─ scoreboard + last review + trend
                     → today's posture & budget   (code clamps everything)
   every 10 min [Go] Eval scoreboard ─ rolling 20d mean-R + CUSUM alarm
                     → auto-bench / reinstate strategies · judge calibration
   13:30        [AI] Research loop (Opus·LangGraph) ─ studies the day →
                     ≤3 evidence-cited proposals → your TELEGRAM
                     (NEVER auto-applied — you apply changes by hand)
   16:10        [AI] Reviewer (Opus) ─ plain-English daily report card
   ~17:05       [ML] Nightly retrain (train_live.py) ─ 6 models on data
                     incl. today's journal → parity-checked vs Python →
                     hot-reloaded   (boot catch-up if the models are stale)

   what actually gets better over time:
      [ML] the 6 gate models — retrained nightly ........ genuinely LEARN
      [Go] the scoreboard — benches the weak ............ ADAPTS
      the LLM agents' INPUTS (scoreboard, reviews) ...... get SHARPER
      the LLM brains themselves ......................... FROZEN, don't self-train


━━━ 8 · WHERE THE ORDERS GO — verified paper-only ━━━━━━━━━━━━━━━━━━━━━━━━━━━━

   Quant desk broker → key PK…(paper) → https://paper-api.alpaca.markets
        paper endpoint + paper keys .......... HTTP 200  ✓  (acct PA-REDACTED)
        live  endpoint + paper keys .......... HTTP 401  ✗  (Alpaca rejects)

   Execution page (YOUR real money) → key AK…(live) → https://api.alpaca.markets

        └── SEPARATE keys · SEPARATE endpoint · NO code path between them ──┘


━━━ KILL SWITCHES  (backend/.env) ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

   QUANT_SIGNALS_LIVE=false → journal only, place no paper orders
   QUANT_CLF_GATE=false     → ML gate off (desk trades ungated)
   QUANT_RETRAIN=false      → stop the nightly retrain (gate goes stale → fails open)
   QUANT_TOD_GATE=true      → re-enable the (currently demoted) time-of-day gate
   QUANT_DAILY_LOSS_CAP=150   ·   QUANT_OVERNIGHT_CAP=0
════════════════════════════════════════════════════════════════════════════
```

> **Honesty footnote the diagram makes visual:** the backtested +$1,512 came from
> §2 → §3 → §5 with plain bracket exits only. It did **not** include the §4 judge
> (gate 8) or the §6 Agent 3 / trailing stop — those are live-only discretionary layers
> that can help or hurt, which is exactly what the live paper run now measures.

---

## 2. The data plumbing (how prices get in)

- One WebSocket connection to Alpaca's SIP feed streams **every trade and quote** in
  real time; the backend folds them into 1-minute candles in memory.
- The quant desk is an **additive listener** on that same stream — it reads the bars the
  terminal already receives. It adds zero latency to the human's real-money path.
- The stock list (the "universe", `QUANT_UNIVERSE.json`) is ~96 hand-curated, liquid,
  reputable names — semiconductors, memory, datacenter, software, quantum, space — plus
  SPY/QQQ/SMH streamed as *market context* (never traded). No penny stocks: signals
  additionally require price $5–$1,000 and ≥1M average daily volume.

---

## 3. The six scouts (the strategies, `internal/signals/strategies.go`)

All six run **simultaneously, on every stock, all day**. They are not options the system
picks between — they're independent scouts, and whichever spots its pattern speaks up.
Each signal comes with a complete pre-computed plan: entry price, stop-loss below,
profit target above (scaled to the stock's typical daily wiggle, its ATR), and a feature
snapshot of the conditions at that moment.

| Strategy | In plain words |
|---|---|
| **Opening-range breakout** (`orb_breakout`) | The first 15 minutes set a high/low "box". The stock punches above the box on heavy volume → join the momentum. |
| **VWAP reclaim** (`vwap_reclaim`) | VWAP is the day's volume-weighted average price — the session's "fair price". The stock washes out below it, then fights back above → buyers took control back. |
| **Momentum continuation** (`momentum_cont`) | A stock already up strongly takes a small breather without breaking down, then resumes → join the trend on the pause. |
| **Dip bounce** (`dip_bounce`) | A hard, oversold drop (well below VWAP, weak RSI) prints a confirmed green recovery candle → buy the bounce, never the falling knife. |
| **Relative strength** (`rel_strength`) | The stock is clearly stronger than the market itself (vs QQQ) → back the strongest horse. |
| **First-hour reversal** (`fh_reversal`) | A stock crushed ≥1 ATR in the first hour stops making new lows for 10+ minutes, then lifts → catch the turn. *On a short leash: it failed standalone backtests, so the gates supervise it hardest.* |

Anti-spam limits: one signal per (strategy, stock) per 30 minutes, max two per day.
Long-only — the desk never shorts.

## 4. The notebook (the self-growing training set)

**Every** signal is journaled to `backend/data/signals/<date>.jsonl` — including the
thousands the desk never trades. For each one, the engine tracks the *counterfactual*:
if this had been taken, would the bracket have hit the target or the stop first? (Ties
break pessimistically — stop first.) The result is a labeled training example produced
for free, every few minutes, forever. This notebook is the fuel for everything below:
the ML gate learns from it, the scoreboard referees with it, and the research agent
reads it.

---

## 5. The gauntlet (what a signal must survive to become a trade)

A fired signal (`quant/signaltrader.go`) passes these checks **in order** — any failure
is logged with its reason and the signal dies:

1. **Time-of-day gate — shadow only.** A learned "avoid bad half-hours" filter that was
   demoted after failing a 12-month re-test (§9). It still writes its opinion in the
   journal but blocks nothing (re-enable: `QUANT_TOD_GATE=true`).
2. **Scoreboard demotion (automatic).** Strategies with a proven losing streak are
   refused (§8). No human involvement.
3. **The ML gate (the profitability model, §6).** Expected reward below +0.03R →
   rejected. The single most valuable filter the system has.
4. **Session guard.** Nothing new after 15:30 ET — too little runway before the close.
5. **Posture.** The pre-market Strategist (§7) can declare `stand_down` (no entries all
   day) or `cautious` (only high-conviction entries).
6. **Daily loss cap.** Roughly −$150 realized on the day → no more entries today.
7. **Allocator.** Shared budget $8,000, max 3 open positions, one per symbol,
   per-position cap (default $2,000, Strategist can trim). No funding → skip.
8. **The AI judge** (`signaljudge.go`, Claude Haiku). A last human-style look for red
   flags a formula misses: exhausted move, hostile tape, thin liquidity, bad bracket
   geometry. Its default is *approve* — it exists to veto rarely and to set conviction,
   which scales position size (low conviction = half slice). If the judge is
   unreachable, the desk falls back to a conservative default rather than freezing.

## 6. The ML gate, in depth (`quant/clfgate.go` + `ml/train_live.py`)

**The model.** LightGBM — gradient-boosted decision trees. Plain words: a team of 150
small decision trees built one after another, each new tree correcting the mistakes of
the previous ones. The standard tool for learning from a few thousand rows of tabular
numbers; trains in seconds on a laptop, no GPU.

**One model per strategy.** A breakout and a dip-bounce win under different conditions,
so each of the six strategies gets its own classifier trained on its own history.

**What it learns.** Input: the signal's feature snapshot (~8–11 numbers per strategy —
things like relative volume, minute of the session, how far the move stretched in ATRs,
whether the market is green, distance from VWAP). Label: did this signal's bracket win
(hit target before stop)? Raw base rate: only ~46% of setups win — near coin-flip,
which is exactly why filtering matters.

**How the score is used.** The model outputs a win probability *p*. That converts into
an expected reward using the signal's own bracket:
`EV = p × (reward:risk of this trade) − (1 − p) × 1` — "risking $1 here, what do I make
on average?" Trades proceed only when **EV ≥ +0.03R** (at least +3 cents per risked
dollar). The margin was fixed before any experiment ran and is never tuned per-run.

**No hyperparameter tuning — on purpose.** One modest configuration was fixed up front
and never swept. Tuning knobs until the backtest looks good is the classic way to fit
noise instead of pattern. Three model *families* were compared as pre-declared
candidates (regressor / classifier / ranker); the classifier won on the honest test.

**The honest test it passed.** Walk-forward evaluation: every day's signals are scored
by a model trained **only on earlier days** — zero peeking. Trained on 17,511 setups
from 12 months of 1-minute data (Jul 2025 → Jul 2026, ~12.9M bars). It is the only
mechanism ever to pass the desk's full promotion bar (§9): better-picked trades AND
better dollars on all three test windows —

| Window | Without gate | With clf gate |
|---|---|---|
| Full 12 months | −$718 | **+$329** |
| Jan–Jul 2026 | −$219 | **+$302** |
| Apr–Jul 2026 (holdout) | +$871 | **+$1,512** (smaller drawdown) |

**Nightly retrain (the walk-forward continues live).** Each weekday ~17:05 ET the
backend runs `ml/train_live.py`: static dataset + every resolved outcome from the live
journal → six fresh models. So Tuesday trades on a model that learned from Monday. If
the machine was off, a boot catch-up retrains immediately.

**The parity check (trust, verified).** Python trains; Go scores in-process in
microseconds (via the `leaves` library — no Python in the trade path). Every night's
export includes sample inputs with the trainer's own answers; at load, Go must
reproduce each within 0.000001 **or the model is refused**. Silent train/serve drift
is mechanically impossible.

**Fail-open everywhere.** Missing models, stale models (>7 days), failed parity, a
strategy with <150 training rows → those signals pass through *ungated*, i.e. the desk
degrades to its pre-gate baseline. The gate can only filter the desk, never brick it.
Kill switch: `QUANT_CLF_GATE=false`.

---

## 7. The AI agent team (LLMs propose, Go disposes)

Every agent is advisory or bounded — deterministic code clamps everything they output,
and every call is logged to `backend/data/decisions/`.

| Agent | Model | When | Job |
|---|---|---|---|
| **Strategist** | Opus | 08:50–09:25 ET (+ boot catch-up) | Reads the scoreboard + latest review + market trend; sets today's posture (normal/cautious/stand_down) and allocation. Code clamps budget ≤$8k, per-position ≤$2.5k, ≤3 slots; API failure → pure-rules fallback. |
| **Entry judge** | Haiku | Per surviving signal | Red-flag veto + conviction sizing (§5, step 8). |
| **Exit manager** ("Agent 3") | Haiku | While a position is open | May tighten stops — ratchet up only; a deterministic 1.5% trailing floor exists regardless. |
| **Sentiment** ("Agent 4") | local Ollama | Background | Advisory color; never blocks anything. |
| **Reviewer** | Opus | 16:10 ET | Daily report card from the full day's logs → `backend/data/reviews/` (tomorrow's Strategist reads it). |
| **Research loop** | Opus via LangGraph | 13:30 ET weekdays | Digests journals/scoreboard/decisions; proposes at most 3 evidence-cited changes — default output is *zero proposals*; report lands on the operator's **Telegram**. Proposals are **never auto-applied**: a human changes knobs, always. |

## 8. The referee (evals scoreboard, `internal/evals/`)

Recomputed every 10 minutes from the journal's rolling 20 trading days:

- Per strategy: signals, resolved outcomes, mean R (average risk-adjusted result), how
  many actually traded.
- **Automatic demotion**: ≥30 outcomes with negative mean R, or a CUSUM changepoint
  alarm (a statistical tripwire for a *sudden* performance break, with decay so an old
  alarm expires). Demoted strategies keep journaling but cannot trade; they return
  automatically when the rolling record recovers. This is not a human process — it
  benched two strategies on the very first live day.
- **Judge calibration**: joins each judge decision to its signal's eventual outcome
  (via `signal_id`) — measuring whether vetoes actually dodge losers (veto value in R,
  Brier score). The ML gate's live accept/reject verdicts are logged the same way, so
  its real-world spread is measurable after a few weeks.
- Visible on the Paper · Claude page ("Strategy scoreboard"), served at `/api/evals`.

## 9. The research discipline (why anything gets believed)

Every idea must state its economic *why*, run through the same harness, and clear a
**pre-registered bar** decided before the experiment: better trade selection
(accepted-vs-rejected counterfactual R) AND better dollars, on the full window AND the
holdout. One change ships at a time. Failures are recorded, not buried — the receipts
live in `RESEARCH_BACKLOG.md`. Killed so far: a curve-fit momentum config (great
in-sample, negative out-of-sample), a ridge-regression gate (anti-selected), passive
limit entries (adverse selection), chase execution (worse than both alternatives), an
ensemble filter (positive dollars but *negative* selection — caught by the framework),
recency weighting, first-hour-reversal standalone, "avoid the first 65 minutes".

**The time-of-day story (a lesson in honesty).** A learned "avoid bad half-hours"
filter passed the original 6-month validation and shipped. The 12-month re-test showed
it *hurt* across a regime change — its buckets could never forget a stale market mood.
An exponential-forgetting fix was designed, pre-registered, and tested: it repaired part
of the long window but destroyed the short-window value (too few outcomes per bucket at
any forgetting speed). Neither variant beat no-gate, so the pre-agreed fallback fired:
the gate now runs shadow-only. Its replacement — the clf gate — is what passed.

---

## 10. Life of a position (`quant/manager.go`)

Entry is a paper market order. Immediately protected by a stop-loss; a deterministic
trailing floor (1.5%) follows the price up and never loosens. The AI exit manager may
tighten (never widen) the stop. At 15:55 ET everything flattens — except at most one
profitable position worth ≤$2,000 may be held overnight (`QUANT_OVERNIGHT_CAP`). If the
backend restarts mid-day, it **rehydrates**: re-adopts surviving broker positions,
re-attaches or replaces their stops, re-funds the allocator, and resumes managing.

## 11. A full trading day, on the clock (ET)

| Time | What happens (all automatic) |
|---|---|
| boot | Models load (parity-verified); positions rehydrate; stale-day catch-ups run |
| 08:50–09:25 | Strategist sets posture + allocation for the day |
| 09:30–15:30 | Scouts fire → gauntlet filters → survivors trade; scoreboard refreshes every 10 min; every event journaled |
| 13:30 | Research loop analyzes the session → Telegram report to the operator |
| 15:55 | Flatten (overnight exception aside) |
| 16:10 | Reviewer writes the daily report card |
| ~17:05 | Nightly ML retrain on data including today; gate hot-reloads |

The operator's entire job: keep the backend running, read the Telegram/report cards,
and manually apply a research proposal if (rarely) one appears and convinces them.

## 12. Kill switches & knobs (`backend/.env`)

| Switch | Default | Effect |
|---|---|---|
| `QUANT_SIGNALS_LIVE` | `true` | `false` = signals journal only, no paper orders |
| `QUANT_CLF_GATE` | `true` | `false` = ML gate off (desk trades ungated) |
| `QUANT_RETRAIN` | `true` | `false` = no nightly retrain (gate goes stale → fails open in 7 days) |
| `QUANT_TOD_GATE` | `false` | `true` = re-enforce the demoted time-of-day gate |
| `QUANT_DAILY_LOSS_CAP` | `150` | Daily stop-entering threshold (USD) |
| `QUANT_OVERNIGHT_CAP` | `0` | Max value of the single overnight winner (USD) |
| `QUANT_STRATEGIST` / `RESEARCH_LOOP` | `true` | The morning agent / the 13:30 pulse |

## 13. Where everything lives

| Path | Contents |
|---|---|
| `backend/internal/signals/` | Strategies, live engine, journal, backtester |
| `backend/internal/quant/` | Trader, ML gate, judge, manager, strategist, reviewer |
| `backend/internal/evals/` | Scoreboard + CUSUM + demotion |
| `backend/internal/risk/` | Deterministic risk rails |
| `ml/train_live.py` / `ml/train_gate.py` | Nightly production trainer / research harness |
| `ml/research_loop.py` | LangGraph research agent (Telegram) |
| `backend/data/signals/` · `decisions/` · `reviews/` · `evals/` · `models/` | The journals, agent logs, report cards, scoreboard, trained models (all local, gitignored) |
| `RESEARCH_BACKLOG.md` | The idea ledger — every verdict with receipts |

## 14. Honest limitations

- Backtest fills are idealized (close + slippage allowance); real fills differ. That's
  precisely what the paper account measures next.
- 17.5k training rows is respectable, not big; the edge (+0.03R bar) is thin by design.
  Expect losing days — the system's promise is *discipline*, not clairvoyance.
- The whole edge is regime-dependent in principle; the scoreboard, CUSUM, daily loss
  cap, and nightly retrain exist because markets change and models rot.
- It only knows prices and volumes. It cannot read news (the judge partially covers
  this); earnings surprises will occasionally hit a position — capped by stops and the
  3-position/$8k limits.
- Paper only. Nothing here is investment advice, and nothing here touches real money.
