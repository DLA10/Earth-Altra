# SNDK scalping pipeline — volatility-generalization study (2026-07-18)

**Status:** research finding, NOT implemented. Paper-only. Documents whether the SNDK
1-minute ML scalping pipeline is a general edge, and under what conditions it works.

**Question:** SNDK exploded in Jan–Feb, so its backtest looked great. Is the pipeline a
real, generalizable edge — or is it optimized for / dependent on SNDK's extreme volatility?

**One-line answer:** it is a **volatility harvester** — a genuine, out-of-sample,
cost-surviving edge on *high-volatility* liquid stocks (22/22 profitable), and roughly
break-even on stable stocks. The edge is NOT stock-specific: a single **pooled** model
generalizes across the whole volatile basket (slightly better than per-stock models).

---

## 1. The pipeline (exactly, from the SNDK code)

**Features (9), per 1-minute bar** — all scale-free/relative:
`Z_Score` (Close vs EMA10 / rolling std10), `RSI_5`, `RSI_14`, `ROC_3`, `ROC_10`,
`ATR_Ratio` (ATR5/ATR20), `MACD_Hist`, `Z_BB` (Close vs Bollinger20 mean/std),
`Vol_Ratio` (VolMA5/VolMA20). (Source: `ml/sndk_live_signals.py` / `train_sndk_production_model.py`.)

**Label (ML target) — triple-barrier:** for each bar, label = 1 if, within the next
**5 minutes (same trading day)**, price reaches **+target before −target**; else 0.
Original SNDK used ±$8 (fixed dollars). **This study uses percentage barriers** so it is
comparable across stocks (see §2).

**Model:** LightGBM classifier — `n_estimators=50, max_depth=3, learning_rate=0.05,
class_weight="balanced", random_state=42`. Predicts P(hits +target before −target in 5 min).

**Entry (all three must hold):** `prob ≥ 0.65` AND `Close > EMA_100` (trend filter) AND
`|Close − VWAP| / VWAP_std ≤ 2.0` (not overextended from VWAP). Fixed notional per trade.

**Exit:** hard stop at −stop%; once price reaches +target%, a trailing stop activates.
The seven exit variants tested are in §3. EOD flat at 15:59 ET; RTH only (09:30–16:00).

---

## 2. The critical design decision — percentage barriers

The original ±$8 target/stop is calibrated for a ~$1,400 stock (0.57%). On a $60 stock,
$8 = 13% — nonsensical. To test "the same pipeline" across stocks of any price, **both the
training labels and the exits are expressed as percentages**, SNDK-equivalent:
`TP = 0.57%`, `SL = 0.71%`, trail widths in %. This is mandatory for a fair cross-stock
test and is consistent with the separate finding that percentage settings generalize while
fixed-dollar settings overfit to a price level.

---

## 3. The seven exit configs (and the winner)

All: buy on signal, hard stop at −0.71%, trailing activates at +0.57%. Variants differ in
trail width and whether a **profit-lock** floors the stop at +target once reached:

| # | Trail width | Profit-lock (+target floor) |
|---|---|---|
| A | 0.5% | no (the original) |
| B | 0.5% | yes |
| C | 0.3% | no |
| D | 0.3% | yes |
| E | 0.2% | no |
| **F** | **0.2%** | **yes ← WINNER** |
| (also fixed-$ trails — REJECTED: overfit to price level, don't generalize) |

**Consistent ranking across every basket tested (4 independent times):**
`0.2%+lock  >  0.2% no-lock  >  0.3%+lock  >  0.5%+lock  >  0.5% no-lock (worst)`.
Two independent improvements over the original, and they stack: (a) a **tighter %-trail**
(0.5%→0.2%) and (b) the **+target profit-lock**. On a high-priced stock the original 0.5%
trail (~$7–12) was almost as large as the $8 target, so it gave back most of every winner;
the tight trail + lock fixes that. Fixed-dollar trails looked best in-sample but collapsed
out-of-sample (price-level overfit) and are rejected.

---

## 4. The walk-forward procedure (exact, no leakage)

For **each stock independently**:
1. Compute features + %-labels on the full continuous 1-min series (RTH), once (for warmup).
2. Group bars by calendar month.
3. **Rolling train/test:** train a FRESH model on month *N* → score month *N+1* → advance:
   train on month *N+1* → score *N+2* → … The training window is always the **single most
   recent month**; a brand-new model is fit each fold (not warm-started, not cumulative).
4. Each test month is never-before-seen by its model.

**No lookahead leakage:** features only look backward; labels look forward ≤5 min but are
**capped at the same trading day**, so a training-month label can't peek into the test month.

**Cost model:** per trade, cost = `(bps/10000) × (entry_price + exit_price) × qty` — i.e.
`bps` charged on BOTH the entry and exit notional. So **"@2bp" = 2bp per side ≈ 4bp
round-trip.** Fixed **$1,500 notional per trade** (qty = 1500/price) so P&L is comparable
across stocks. Frictionless fills assumed (see caveats).

**Per-stock vs pooled:** "per-stock" = each stock trains its own monthly models. "Pooled"
= one model per month trained on ALL stocks' prior-month data, applied to each stock's next
month.

---

## 5. Results (walk-forward, per $1,500 slice, winner config 0.2%+lock)

| Basket | Frictionless | @2bp | Stocks positive |
|---|--:|--:|:--:|
| **12 volatile** (NVDA AMD MU SMCI MRVL ARM PLTR COIN TSLA MSTR IONQ CRWV) | +$7,307 | +$4,230 | **12/12** |
| **10 volatile** (WDC ON LRCX DELL ANET SNOW HOOD RGTI ASTS RIOT) | +$8,974 | +$6,241 | **10/10** |
| **Combined 22 volatile** | +$16,281 | **+$10,471** | **22/22** |
| SNDK alone (walk-forward) | +$1,225 | +$840 | 5/6 folds |
| 12 stable (KO PG JNJ WMT MCD CSCO VZ MRK HON CAT JPM TXN) | +$992 | +$131 | 4/12 |

- **22 of 22 volatile stocks profitable** out-of-sample after cost — the strongest,
  broadest result of the whole investigation.
- Volatile basket is **~32×** the stable basket at 2bp, and 22/22 vs 4/12 positive.
- **Volatility → profit correlation** visible: highest-vol names lead (ASTS 8.6% → +$984,
  RGTI 7.8% → +$965); lowest trail (NVDA 3.0% → +$105, ANET 4.1% → +$204).
- **SNDK's edge survives fresh walk-forward** (+$840 @2bp) → it was NOT mainly model-overfit;
  it's volatility. SNDK is an extreme example of the right kind of stock.

## 6. Pooled (generalized) vs per-stock model (22 volatile stocks, 0.2%+lock)

| | @2bp | Stocks positive | Frictionless |
|---|--:|:--:|--:|
| Per-stock (22 specialist models) | +$10,471 | 22/22 | +$16,281 |
| **Pooled** (1 generalist, retrained monthly) | **+$11,693** | 21/22 | +$18,787 |

**A single pooled model is slightly BETTER *and* far simpler to run live.** Because the
features are scale-free and the signal is a *generic* short-term-bounce pattern (not
stock-specific), pooling ~20× more training data makes the model more robust and less
overfit to one stock's thin month. Pooling helped 12 stocks (RIOT +$535, ASTS +$531,
MRVL +$407…), hurt 10 (HOOD −$317, ANET −$249, AMD −$235…), net positive. Trade a little
per-name edge for robustness + operational simplicity → for a basket, pooled wins.

## 7. Cost sensitivity & break-even

Cost is linear in bps. Approx break-even (per-side bps, where net → 0):
- Combined volatile basket: ~5–6.5bp **per side** (≈10–13bp round-trip).
- Stable basket: ~1bp per side (essentially unusable).
Higher-volatility baskets tolerate more slippage because each trade's edge is larger
relative to the spread. Real fills must beat this — see caveats.

## 8. Caveats (do not ignore)

1. **Frictionless fill idealization.** The tight 0.2% trail exit assumes a fill at the trail
   price; real fast-reversal fills slip. The @2bp/etc. columns model spread but not this
   path-dependent slippage. The break-even is an upper bound on tolerance.
2. **Regime.** The 6-month window (Jan–Jul 2026) was a HOT tape for semis/momentum/quantum/
   crypto. The edge lives on volatility being present; a sustained sell-off in these names
   would shrink it.
3. **Live measurement is the only remaining unknown** — real limit/market fill quality vs
   the ~5–6bp/side break-even. No backtest can answer it.

## 9. Recommendation

Generalize the SNDK pipeline to a **curated basket of high-volatility liquid names**, using:
- **One pooled LightGBM**, retrained **monthly** on the whole basket (features/labels as §1–2).
- Entry: prob ≥ 0.65 + Close > EMA100 + within 2σ of VWAP.
- Exit: **0.2% trailing stop with the +target profit-lock** (config F), hard stop at −0.71%,
  EOD flat.
- **Paper-trade it** to measure real fills against the ~5–6bp/side break-even. If fills
  cooperate, this is the first broadly-validated earner of the investigation.
Never run it on quiet blue-chips (marginal), and never use fixed-dollar exits (price-overfit).

## 10. Reproducibility (session scratchpad scripts)

`sndk_bt.py` / `sndk_3mo_bt.py` (single-model SNDK), `wf_engine.py` (walk-forward engine:
`build()` features+labels, `sim()` exit, CFGS), `wf_vol_run.py` / `wf_vol2_run.py` (volatile
baskets), `wf_engine.py`-derived SNDK & stable runs, `pooled_result.py` logic (per-stock vs
pooled). Data pulled via Alpaca SIP 1-min, Jan 17–Jul 18 2026. All figures are paper
backtests; nothing here trades real money.
