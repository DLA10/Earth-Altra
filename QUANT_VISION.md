# QUANT_VISION.md — The AI Agentic Quant Desk

> Direction + architecture + phased roadmap for turning the current dip-bot into a real
> multi-strategy, ML-assisted, agent-orchestrated intraday trading system. Paper-only for
> the AI (real money stays 100% manual through the Execution page). This document is the
> contract for what gets built; each phase gets approved before it starts.

---

## 0. What we're fixing (from the retro)

| Problem today | Root cause | The fix |
|---|---|---|
| Gemini paper engine returned nothing | 2-green-candle "momentum" has no edge | **Removed** (done) |
| Claude quant barely trades / no results | Single trigger (dip+bounce) on a hand-picked ~10–20 name universe → a handful of signals/day, most passed | Multi-strategy signal engine over the **whole scanner universe (~470 names)** |
| Agents biased toward RSI/Bollinger | That's most of what their snapshot contains | Strategy-specific feature snapshots: order flow, relative strength, opening structure, quote imbalance, ML probability — RSI/BB become two features among thirty |
| Everything hangs off the dip watcher | Detection and strategy were fused | Detection → **Signal Bus** (typed setups from N independent strategies); dip-bounce becomes just one publisher |
| No learning loop | Decisions logged but never scored systematically | **Eval framework**: nightly scoring, strategy scoreboard, shadow A/B, evidence-gated promotion |

**North star (unchanged): consistency over peak profit.** Target ≈ **$50–100/day** of
realized paper P&L with a hard daily loss cap, zero overnight risk, and boring
repeatability.

### Honest target math (read this before judging results)

$75/day is **not** achievable sustainably on the current $3k paper budget (that's 2.5%/day
— hedge funds dream of 2.5%/month). The manual benchmark ($70/day) is earned on the live
account's full buying power. To make the comparison fair and the target physically
possible:

- **Reset the quant paper account to $25–30k cash** (also mirrors the PDT minimum a real
  intraday account needs). At $25k, $75/day = 0.3%/day.
- Working shape of the goal: **3–6 trades/day × $15–25 average expectancy**, i.e. ~55–60%
  hit rate with ≥1.2:1 reward:risk on ~$4–6k positions capturing 0.3–0.5% moves.
- Judge the system on **20-day rolling expectancy and max drawdown**, never on any single
  day.

---

## 1. Architecture: three planes

Keeps the proven house rule — **"Python brain, Go hands"** — and the existing safety
doctrine: *models propose, deterministic Go disposes; the AI never touches the live keys.*

```
┌────────────────────────── GO — DATA & EXECUTION PLANE (exists, extend) ─────────────────────────┐
│ SIP stream → candle engine + scanner(~470 syms) + flow tracker                                   │
│      │                                                                                           │
│  ┌── signals/ (NEW) ──────────────┐     ┌─ risk/ (NEW, pure Go) ─┐    ┌─ broker (exists) ─┐      │
│  │ orb_breakout   vwap_reclaim    │ →   │ daily loss cap, R/trade │ →  │ paper orders,     │      │
│  │ momentum_cont  mean_reversion  │     │ exposure + corr caps,   │    │ stop floor,       │      │
│  │ dip_bounce (moved here)  ...   │     │ no-overnight, halts     │    │ manager loop      │      │
│  └───────────────┬────────────────┘     └────────────────────────┘    └───────────────────┘      │
│                  │ every signal + feature snapshot + outcome → data/signals/*.jsonl (DATASET)    │
└──────────────────┼───────────────────────────────────────────────────────────────────────────────┘
                   │ localhost HTTP (hard timeout, deterministic fallback)
┌──────────────────▼─────────── PYTHON — RESEARCH & ML PLANE (new, offline-first) ─────────────────┐
│ feature store (parquet) → labels (triple-barrier) → walk-forward training →                      │
│ calibrated p(win) gate served by a FastAPI sidecar → nightly retrain + eval report               │
└──────────────────┬───────────────────────────────────────────────────────────────────────────────┘
                   │ scores enrich snapshots; never place orders
┌──────────────────▼──────────────── AGENT PLANE (LLM, judgment only) ─────────────────────────────┐
│ Strategist (pre-mkt, Opus)  ·  Entry Judge (ambiguous cases only)  ·  Exit Manager (exists)      │
│ Evaluator/Reviewer (post-mkt)  ·  all decisions JSONL-logged and eval-scored                     │
└───────────────────────────────────────────────────────────────────────────────────────────────────┘
```

**Division of labor — the core design decision:**
- **Numbers → models.** LLMs do not predict prices. Probability estimates come from
  calibrated ML on logged outcomes.
- **Judgment → LLMs.** Regime reads, catalyst interpretation ("is this dip justified
  selling?"), strategy arbitration, post-market review, improvement proposals.
- **Money → deterministic Go.** Sizing, budget, caps, stops, halts. No exceptions.

---

## 2. The Signal Bus (Phase 1 — the keystone)

A new `backend/internal/signals` package. Each strategy is a small deterministic detector
implementing one interface, scanning the **full scanner universe** (all ~470 streamed
names + intraday movers), publishing typed events:

```go
type Signal struct {
    Strategy   string            // "orb_breakout" | "vwap_reclaim" | ...
    Symbol     string
    Side       string            // long-only in v1
    Time       time.Time
    Features   map[string]float64 // the full snapshot at trigger time (becomes training data)
    Suggested  struct{ Entry, Stop, Target float64 } // ATR-scaled defaults
    Quality    float64            // deterministic pre-score (RVOL, spread, liquidity)
}
```

**v1 strategy set** (all long-only, intraday, well-understood — deliberately no exotic
models until the plumbing proves itself):

1. **Opening-range breakout (ORB)** — break of the 15-min opening high on RVOL ≥ 1.5 and
   positive order-flow, market (SPY/QQQ) not risk-off. Classic momentum.
2. **VWAP reclaim** — mean reversion: stock flushes below VWAP, then reclaims it on
   rising volume. (Generalizes today's dip-bounce.)
3. **Momentum continuation** — new session high after a shallow (<0.4×ATR) pullback in a
   strong uptrend, sector/market confirming.
4. **Dip-bounce** — the existing detector, ported onto the bus unchanged (its Telegram
   alert stays).
5. **Relative-strength divergence** — stock green and above VWAP while its DECEPTICON
   sector is flat/red (cross-sectional edge; cheap "graph" signal without a GNN).

**Universal filters** (before any signal publishes): price $5–800, spread < 15 bps,
avg volume > 1M, not halted, not inside the first 5 min, none after 15:30 ET.

**The crucial trick: every signal is logged with its features AND its counterfactual
outcome** (what a bracket at Suggested.Entry/Stop/Target would have returned), whether it
was traded or not. From day one, the running system **generates its own labeled training
dataset** — a few weeks of full-universe logging yields thousands of labeled setups. That
dataset is what makes Phase 2 real instead of hand-wavy.

**Execution in Phase 1:** the existing Allocator/Manager/Broker pipeline consumes the
top-ranked signals directly (deterministic quality score), ATR-scaled bracket exits,
trailing-stop floor kept. LLM entry judgment becomes **optional** per strategy — clear
rule-based signals trade without burning an LLM call.

### Risk Officer (`backend/internal/risk`, pure Go — non-negotiable rails)

- **Daily loss cap**: realized day P&L ≤ −$150 → flatten everything, halt entries until
  tomorrow. (The single most important consistency device.)
- Per-trade risk ≤ $40 (entry-to-stop distance × size); position ≤ $6k; max 4 concurrent;
  max 2 per sector; no overnight (existing 15:55 flatten, now bug-fixed past 16:00);
  stand-down posture blocks entries (exists).
- One kill switch flattens the paper account and stops all strategies.

---

## 3. The ML layer (Phase 2 — Python sidecar)

**Data honesty first:** Alpaca SIP provides trades + **top-of-book NBBO quotes only — no
L2 depth**, so true limit-order-book modeling (queue dynamics, DeepLOB-style) is off the
table with current data. What IS available and genuinely predictive intraday:

- **Microstructure-lite features**: quote imbalance (bid/ask size ratio), microprice vs
  mid, effective spread, trade-sign runs, tick-rule flow (the flow tracker already does
  this), volume clock features.
- **Session-structure features**: distance to VWAP/OR-high/PM-high in ATRs, time-of-day,
  RVOL curve position (scanner already computes the volume profiles).
- **Cross-sectional features**: sector breadth, symbol-vs-sector return spread, SPY/QQQ
  regime. (This captures most of what a spatial-temporal graph net would learn, for ~1%
  of the complexity — if these features carry signal, a graph model becomes a justified
  *upgrade*, not a starting point.)

**Model plan — simple first, by design:**

1. **Baseline: gradient boosting (LightGBM) p(win) classifier** per strategy family,
   trained on the logged signals with **triple-barrier labels** (hit target / hit stop /
   time-out), **walk-forward validation only** (no shuffled CV — leakage lies), and
   **probability calibration** (isotonic). Output: `p(win)` + expected R.
2. **The gate**: signals trade only when `p(win) × R − (1−p(win)) ≥ threshold`. Threshold
   set by the eval loop, not by hand.
3. **Sizing**: fractional-Kelly-capped (¼ Kelly, never above the risk officer's caps).
4. **Only after the baseline demonstrably adds lift** (shadow comparison, §5): experiment
   with sequence models (small temporal CNN/transformer on 1-min bars) and cross-sectional
   graph models. If the fancy model can't beat calibrated LightGBM out-of-sample, it
   doesn't ship — that's the eval framework doing its job, and writing that finding up
   honestly is *itself* strong portfolio material.

**Serving**: FastAPI on `localhost:8090`, `POST /score` (features → p(win), <10ms), Go
calls it with a **250ms timeout and a deterministic fallback** (quality-score-only mode)
so the trading path never depends on Python being up. Nightly `retrain.py` job re-fits on
the growing dataset and writes a model version + eval card. Native Windows venv — no
WSL2, no Docker, per the house preference.

**Options / LOB modeling**: explicitly deferred. Options add a different risk surface
(assignment, gamma, spreads) and Alpaca's options data is thin; revisit only after the
equity engine holds its target for a full month. Deferred ≠ forgotten — it's Phase 5.

---

## 4. The agent team (Phase 3 — judgment, not math)

| Agent | Model | When | Job | Hard boundary |
|---|---|---|---|---|
| **Strategist** | Opus | pre-market (scheduled) | Regime read, macro calendar, catalyst scan → writes `daily_config.json`: posture, which strategies are ON, risk budget, priority sectors, per-strategy notes. Replaces the manual Instruction.md run. | Config only — can never place orders |
| **Entry Judge** | Haiku/Sonnet | intraday, **ambiguous signals only** (news-driven, mixed evidence, `p(win)` near threshold) | Read the catalyst + snapshot, veto or approve with reason. Clear-cut signals bypass it entirely (cheaper, faster, less LLM bias). | Binary veto on one signal; can't initiate |
| **Exit Manager** | Haiku | per open position (exists) | Keep: hold / tighten / take-profit / exit-now over the deterministic floor. Snapshot gains flow + ML features. | Ratchet-up-only stops (exists) |
| **Evaluator** | Opus | post-market | Scores the day (§5), writes the review, proposes ≤3 evidence-cited changes. | Proposals require human approval |

Orchestration stays the current pattern — forced tool calls against the raw Anthropic API
from Go — because it is simpler, faster, and more auditable than a framework on the hot
path. **LangGraph enters where it actually fits**: the *offline* research/eval loop in
Python (Strategist research → Evaluator scoring → proposal generation is a natural graph
with retries and human-approval interrupts). That gives the portfolio story a real
LangGraph artifact without putting a framework between market data and money.

---

## 5. Evals & self-evolution (the portfolio centerpiece)

This is what elevates the project from "trading bot" to "agent engineering showcase" —
and it's also the only honest way to know whether anything works.

1. **Strategy scoreboard** (`data/evals/scoreboard.json`, rendered on the Quant page):
   rolling 20-day expectancy, hit rate, avg R, max DD, signal count — per strategy.
   Strategies below floor (e.g. expectancy < 0 over 20d with ≥30 signals) are
   **auto-demoted to shadow**; the Strategist can't re-enable them without human approval.
2. **Counterfactual baselines** (extends the existing Agent-3 attribution): every traded
   signal is compared against (a) pure bracket with no agent exits, (b) no ML gate,
   (c) random-N sample of untraded signals. Measures where each layer adds or destroys
   value — *"stop-only would've done better"* becomes a measured fact, not a vibe.
3. **LLM decision evals**: calibration of Entry-Judge confidence vs realized outcomes
   (Brier score), veto value (P&L of vetoed signals — did the vetoes save money?),
   Exit-Manager value-add (exists, kept). Bad calibration → prompt iteration, with
   before/after eval diffs.
4. **Shadow A/B promotion**: any change (new model version, new strategy, prompt edit,
   threshold move) runs in **shadow** (decisions logged, no orders) alongside the
   incumbent. Promotion requires beating the incumbent over ≥10 trading days. One change
   at a time. This is the "self-evolving" mechanism — evolution by measured evidence with
   a human approval gate, not by an LLM rewriting its own rules unsupervised.
5. **Daily report** (exists, extended): posture vs realized outcome, per-strategy digest,
   eval deltas, open proposals — one JSON + one human-readable summary per day.

---

## 6. Roadmap

**Operator amendments (2026-07-02):** universe = a curated ~100-name list
(`QUANT_UNIVERSE.json`: memory/semis, datacenter, software, space, quantum, DC power — no
penny stocks) instead of the full 470-name scanner set; paper account funded at **$8k**
(target restated: ~$50/day on ~$4k deployed); overnight holds allowed but capped at
**$2,000** — a single profitable position only (`QUANT_OVERNIGHT_CAP`, default 0);
**backtest before any paper execution**.

**Phase-1 validation results (2026-07-02, 3-month SIP walk-forward, 2026-03-31 →
2026-07-01, 96 symbols, ~3.7M minute bars):** parameter tuning on Apr–May with June held
out showed the in-sample momentum winner was curve-fit (died out-of-sample) and that a
60-min hold cap destroys the edge — both rejected by evidence. Per-strategy regime test:
**vwap_reclaim** and **momentum_cont** were positive in BOTH the Apr–May and June
regimes; dip_bounce / orb_breakout / rel_strength were Apr–May-only → shadow.
The promoted pair as a portfolio under the risk rails: **+$1,240 over 64 trading days
(+$19.4/day), +$342 in adverse June (+$16.3/day), max drawdown $279, daily loss cap
verified.** `signals.PromotedStrategies()` encodes this; the ML training dataset
(4,415 signal rows with features + counterfactual outcomes) was bootstrapped from the
same replay (`-dataset` flag) — Phase 2 does not need to wait for shadow logs.

| Phase | Deliverable | Exit criterion (gate to next phase) |
|---|---|---|
| **0 — Cleanup** ✅ | Bugs fixed, Gemini engine removed, docs true | done |
| **1 — Signal engine** ✅ validated | `signals/` + `risk/` packages; 5 strategies over the curated universe; signal+outcome logging; `cmd/backtest` with IS/OOS sweep, cooldown-faithful sim, ML dataset export; promoted set = vwap_reclaim + momentum_cont | done — see validation results above; next: wire promoted-pair paper execution |
| **2 — ML gate** | Python sidecar; LightGBM p(win) per strategy; walk-forward + calibration; gate in shadow, then live-paper | Gated selection beats ungated by ≥20% expectancy over 10 days |
| **2a — v1 linear gate** ❌ rejected by evals (2026-07-02) | Go ridge expected-R gate, retrained daily walk-forward inside the backtester (`-mlgate`); selectivity metric = counterfactual avg R of accepted vs rejected signals | **Failed its exit criterion and does not ship**: rejected signals out-performed accepted ones in EVERY strategy (e.g. vwap_reclaim +0.227R rejected vs +0.100R accepted) — the feature→outcome relationship is real but non-linear, so the linear fit misorients. P&L impact ≈ neutral (portfolio effects mask it), which is exactly why the per-signal selectivity metric, not P&L, is the promotion yardstick. Phase 2 proper (nonlinear/LightGBM) inherits the same harness and must show accepted-R > rejected-R walk-forward before gating any order |
| **2b — LightGBM gate + 6-month re-validation** ⚠ mixed (2026-07-02) | Python plane (`ml/train_gate.py`, LightGBM walk-forward, daily retrain, reg + clf variants); `-mlpred` replays its predictions through the Go simulator; 6-month dataset (9,014 signals, 2026-01-02 → 2026-07-01); deterministic QQQ-vs-20d-MA regime brake (`-regime`) | **LightGBM-clf PASSED the selectivity bar** (accepted +0.038R vs rejected +0.000R overall; +0.031 spread in held-out June — needed the 6-month dataset; the 3-month one was data-starved). **But nothing passed the portfolio bar**: the 6-month window exposed the Apr–Jun "+$19/day pair" as regime-carried (Jan–Mar lost it all back: pair = +$90/6mo). Best full-window configs: all-5+regime +$201 (DD $898), pair+regime +$188 (DD $726) — robust-ish but ~$1.6/day, far from target. Gate selectivity did not convert to portfolio dollars under slot competition (and had zero spread on the pair). **No configuration promoted to paper execution.** Verdict: 1-min-bar features are too weak to carry the edge; next iteration = live-only microstructure features (spread, quote imbalance, flow) collected by the shadow engine + regime-conditional strategy routing, judged by the same bars |
| **3 — Agent team + evals** | Strategist automation, scoped Entry Judge, LangGraph research loop, full eval suite, shadow A/B promotion pipeline | System runs a full week hands-off; daily report + scoreboard self-maintaining |
| **4 — Consistency campaign** | Tuning cycle driven purely by the eval loop | **≥ $50/day average over 20 consecutive trading days, max daily loss ≤ $150** |
| **5 — Frontier experiments** | Sequence/graph models vs LightGBM bake-off; options flow data; short side | Each admitted only by beating the incumbent in shadow |

**Phase 1 is next** and is pure Go on existing infrastructure (scanner + engine + broker
already do the heavy lifting) — say the word and it gets built.

---

## 7. Unchanged non-negotiables

- The **live account is never touched by any agent** — real money flows only through the
  Execution page's confirm-modal path. The AI trades the paper account, full stop.
- The Go hot path (SIP → engine → hub → Execution page) keeps its sub-second latency; the
  brain is consulted off the critical path with timeouts and fallbacks.
- Every automated decision is logged, attributable, and eval-scored. If it isn't
  measured, it doesn't get to evolve.
