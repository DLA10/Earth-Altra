# Generalized volatility-scalper desk — implementation spec (2026-07-19)

**Status:** implementation plan. NOT built yet. Paper-only, own Alpaca paper account
(`PAPER_SNDK_*`). Turns the validated finding in `SNDK_VOLATILITY_PIPELINE_STUDY.md` into
concrete code changes to the existing SNDK desk (`backend/internal/sndk/sndk.go` + the two
`ml/` scripts). No change touches the live real-money Execution path.

> Read `SNDK_VOLATILITY_PIPELINE_STUDY.md` first — it is the *why*. This file is the *what to change*.

---

## 0. The target state (validated recipe, frozen)

- **Universe:** curated basket of high-volatility liquid names (the validated 22:
  NVDA AMD MU SMCI MRVL ARM PLTR COIN TSLA MSTR IONQ CRWV WDC ON LRCX DELL ANET SNOW HOOD
  RGTI ASTS RIOT). Never quiet blue-chips.
- **Features (9):** unchanged from today — `Z_Score, RSI_5, RSI_14, ROC_3, ROC_10,
  ATR_Ratio, MACD_Hist, Z_BB, Vol_Ratio`. Already scale-free.
- **Model:** ONE **pooled** LightGBM, retrained **monthly (rolling 1-month)** across the
  whole basket. (Holdout-proven: pooled ≥ per-stock; rolling-1mo ≈ expanding, so use the
  simplest/most-adaptive.) Same hyperparams: `n_estimators=50, max_depth=3, lr=0.05,
  class_weight=balanced`.
- **Label:** **percentage** triple-barrier — +0.57% before −0.71% within 5 min, same day.
  (NOT fixed ±$8.)
- **Entry gate (all three):** `prob ≥ 0.65` AND `Close > EMA100` AND `|Close−VWAP|/VWAP_std ≤ 2.0`.
- **Exit (config F — the winner):** hard stop **−0.71%**; at **+0.57%** the trail arms; then a
  **0.2% trailing stop** whose floor is **locked at the +0.57% target**; EOD flat 15:59 ET.
- **Sizing:** fixed **notional per slice** (default $1,500 → qty = floor(notional/price)).
- **Costs to beat:** ~5.5 bp/side break-even — the ONLY thing live paper measures that a
  backtest can't.

---

## 1. Current state vs target — the gap table

| Component | Current (`sndk.go` + scripts) | Target | Severity |
|---|---|---|---|
| Universe | **SNDK only**, hardcoded everywhere | 22-name basket, config-driven | large |
| Model | single **static all-data** model, SNDK-only | **pooled, monthly rolling** retrain | large |
| Labels | **fixed ±$8** (`train_sndk…py` L65-66) | **percentage** ±0.57%/0.71% | large |
| **Exit** | **fixed ±$8 TP/SL + 5-min timeout** (`manageStrategyExits`) — **NO trailing, NO lock** | **0.2% trail + target-lock**, −0.71% hard stop | **correctness** |
| 5-min time exit | present (L217-223) | **remove** — not in validated sim | correctness |
| Lunch skip 11:30–13:30 | present (L133) | remove (not in validated sim) — or keep as explicit overlay | medium |
| Sizing | `qty = 2` shares hardcoded (L399) | notional-based, per-price | large |
| Concurrency | 1 global slot | N-slot cap (recommend 3) across basket | medium |
| Entry window | 9:31–15:50 | keep (late entries EOD-flatten anyway) | low |
| Retrain wiring | none (manual `train_sndk…py`) | nightly/monthly goroutine (like `runNightlyRetrain`) | medium |
| Log/comment bug | L90 says "no PAPER_RBT keys" | fix to `PAPER_SNDK` | trivial |

**The single most important change is the exit.** The desk currently validated to make money
with a **0.2% trail + lock**, but the live code has **no trail** — it takes a flat +$8 and
stops at −$8 with a 5-minute cutoff. Shipping the universe/model without fixing the exit
would run an **unvalidated strategy**.

---

## 2. Changes by file

### 2.1 `ml/train_sndk_production_model.py` → generalized pooled trainer
Rename/replace with `ml/train_vol_pooled.py` (keep the old one for SNDK reference). Changes:
- Accept a **list of symbols** + a data source (Alpaca SIP 1-min or cached pkl per symbol),
  RTH-filtered.
- Compute the 9 features **per symbol** (identical formulas — copy from the current script).
- **Percentage triple-barrier label** (replace L64-89):
  ```
  TP = 0.0057; SL = 0.0071; HORIZON = 5
  up = close*(1+TP); dn = close*(1-SL)
  for k in 1..HORIZON (same trading day):
      if low[i+k]  <= dn: label=0; break
      if high[i+k] >= up: label=1; break
  ```
- **Pool** all symbols' bars for the training month, fit ONE LightGBM, save
  `ml/vol_pooled_model.bin` (+ a `vol_pooled_meta.json`: trained_through, symbols, row count —
  mirror `clf_meta.json`).
- **Monthly rolling:** train on the most recent calendar month only. (Nightly job just
  refits on the trailing ~1 month; no expanding window — proven equivalent.)
- Reuse the frozen scratchpad engine as the reference: `wf_engine.py` `build()` (features
  +labels) and the pooled loop in `pooled_window.py` (scheme **W1**).

### 2.2 `ml/sndk_live_signals.py` → `ml/vol_live_signals.py` (multi-symbol scoring)
- Take `--symbol` (or score a batch) and `--recent-bars`. Feature block is **unchanged**
  (already correct). Two edits:
  - Load `vol_pooled_model.bin` instead of `sndk_lgbm_model.bin`.
  - Everything else (prob≥0.65 + EMA100 + VWAP≤2σ gate, JSON out) stays.
- The percentage barriers do NOT live here — scoring only emits the signal; the **exit lives
  in Go** (§2.3), so this script needs no barrier logic.

### 2.3 `backend/internal/sndk/sndk.go` → the real work
Generalize from one symbol to a basket and **replace the exit with the trail+lock machine.**

**(a) Symbol set.** Replace hardcoded `"SNDK"` with a configured `[]string universe`. `Start`
loops `ensureLive(sym)` for each. `runEntryScan` loops the basket (skip symbols already held /
when slots full). `open *Position` → `open map[string]*Position`; add `maxSlots` (default 3).

**(b) Sizing.** Replace `qty = 2` (L399) with notional:
`qty = math.Floor(notional / price)` where `notional` = `SNDK_NOTIONAL` (default 1500); reject
if qty < 1 or buying power short.

**(c) Percentage targets (replace L437-438):**
```go
tpPrice := price * (1 + tpPct)   // tpPct = 0.0057
slPrice := price * (1 - slPct)   // slPct = 0.0071
```
The exchange-side catastrophic `StopSell` stays at `slPrice` (now percentage-derived).

**(d) EXIT STATE MACHINE — replace `manageStrategyExits` (L189-232) entirely.** Per open
position, track `peak` and `armed` on the `Position` struct. Per tick (price = last trade):
```
// hard stop (always)
if price <= pos.StopLoss { exit("stop_loss"); return }

// arm the trail at target
if !pos.Armed && price >= pos.TargetPrice {
    pos.Armed = true
    pos.Peak  = max(price, pos.TargetPrice)
}

// trailing + lock (only once armed)
if pos.Armed {
    pos.Peak = max(pos.Peak, price)
    trail := pos.Peak * (1 - trailPct)      // trailPct = 0.002
    if lockAtTarget && trail < pos.TargetPrice { trail = pos.TargetPrice }  // profit-lock
    if price <= trail { exit("trail"); return }
}

// EOD flat
if mins >= 15*60+59 { exit("eod"); return }
```
**Delete the 5-minute time exit (L217-223) and the lunch skip (L133)** — neither is in the
validated sim. (If you want a max-hold safety, make it a *long* backstop, e.g. 60 min, and
document it as an overlay — but default is trail/stop/EOD only.)

> Fidelity note: the backtest fills stop/trail exactly at the level (frictionless). Live
> uses a **market sell on breach** — realistic and slightly worse; that gap is exactly the
> ~5.5 bp/side we're paper-measuring. Keep the existing fill-price recording (`awaitFill`)
> so P&L is the ACTUAL fill, not the trigger.

**(e) Keep all the hard-won safety plumbing as-is:** `monitorCatastrophicStops`,
confirm-cancel-before-sell (L249-270), re-protect-on-failed-exit (L275-291), `awaitFill`
real-price recording, ghost-position `safety_exit`, atomic `saveState`. These fixed real
ghost-share incidents — do not regress them. They just need to be keyed per-symbol in the
`map`.

**(f) coid prefix stays `sndk_`** so the shared-account rehydrate guard keeps ignoring these
(CLAUDE.md §13.6.7). This desk MUST stay on its **own** `PAPER_SNDK_*` account.

### 2.4 `backend/cmd/server/main.go` — wiring
- Read the new env (below); build the desk with the universe + dials.
- Add a **retrain goroutine** modeled on `runNightlyRetrain` (clf gate): weekday ~17:05 ET +
  boot catch-up → run `ml/train_vol_pooled.py` → the desk hot-reloads `vol_pooled_model.bin`
  next scan (the script is re-exec'd per scan, so it picks up the new file automatically).
- Only arm when `PAPER_SNDK_*` keys are set (already the `broker.Enabled()` gate).

### 2.5 `frontend/src/Sndk.tsx` — report (minor)
`Report()` already returns positions/trades; extend for **multiple** open positions
(map→slice) and show per-symbol. Add `max_slots` (now 3) and a per-symbol P&L column. No
new endpoint — `/api/sndk` (or wherever it's mounted) just returns the richer map.

### 2.6 `.env.example` / `config` — new dials
| Key | Default | Meaning |
|---|---|---|
| `PAPER_SNDK_KEY` / `_SECRET` | — | desk's own paper account (empty = OFF) |
| `SNDK_LIVE` | `false` | `true` = place paper orders; `false` = shadow journal only |
| `SNDK_UNIVERSE` | the 22 names | comma-sep basket |
| `SNDK_NOTIONAL` | `1500` | USD per slice |
| `SNDK_MAX_SLOTS` | `3` | concurrent positions cap |
| `SNDK_TP_PCT` | `0.0057` | target % (arms trail) |
| `SNDK_SL_PCT` | `0.0071` | hard stop % |
| `SNDK_TRAIL_PCT` | `0.002` | trailing width % |
| `SNDK_LOCK` | `true` | floor the trail at target (profit-lock) |
| `SNDK_MODEL` | `vol_pooled_model.bin` | model file |
| `SNDK_RETRAIN` | `true` | nightly monthly-rolling pooled retrain |

All dials so the validated config is the default and any knob can be swept without a rebuild.

---

## 3. Rollout plan (paper-only, staged)

1. **Build + shadow.** `SNDK_LIVE=false`. Desk scans the basket, scores, logs *would-be*
   entries/exits with the trail+lock machine to `data/sndk/` — **places nothing**. Confirm
   signal counts and simulated exits look sane vs the backtest for a few sessions.
2. **Go live-paper, one desk isolated.** `SNDK_LIVE=true`. Its OWN account. Compare **real
   fills** against the ~5.5 bp/side break-even — this is the whole point. Watch: slippage on
   the tight 0.2% trail (the frictionless idealization), and whether the 3-slot cap starves
   entries vs the per-stock backtest (expected, acceptable).
3. **Decide.** Success bar (consistent with the quant-team bar): net-positive after real
   fills over a measurement month, with the trail exits not bleeding vs the sim. If fills
   eat the edge → the strategy is spread-bound; document and bench.

**One change per live test** (memory: test discipline) — ship the exit+universe+model
together only in *shadow*; when going live, isolate this desk so its P&L is unarguable.

---

## 4. Known divergences from the backtest (accept & watch, don't "fix" silently)

- **Market-sell on breach vs frictionless fill** → real slippage (the measurement target).
- **3-slot cap vs independent per-stock** → fewer trades than the sim's per-stock totals.
- **Live scoring re-execs Python per scan** (existing pattern) → fine, but if latency bites,
  batch-score the basket in one call.
- **Model recency**: nightly monthly-rolling means the live model is always ≤1 month stale —
  which is what was validated. Don't switch to a static all-data model (that's the current,
  unvalidated setup).

## 5. Open questions deferred (NOT in this pass)

- **Volatility-breathing trail** (`trail = k × ATR%`, one global k) — a principled alt to
  flat 0.2%, untested; run on the frozen holdout before adopting. Flat 0.2%+lock is the
  default until then.
- **Per-stock notional weighting** by volatility — leave equal-notional for now (simpler,
  and per-stock tuning is the overfitting direction).
- **Basket size** — start at the validated 22; prune consistent losers (SMCI, PLTR were the
  soft names on the holdout) only with fresh out-of-sample evidence.

## 6. Reproducibility / provenance
Validated in `SNDK_VOLATILITY_PIPELINE_STUDY.md` + the frozen-holdout and training-window
runs (scratchpad `final_holdout.py`, `pooled_window.py`, `wf_engine.py`). All paper
backtests; nothing here trades real money.
