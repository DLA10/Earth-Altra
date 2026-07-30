# KNIFE study — can an independent detector spot falling knives in open positions?

**Date:** 2026-07-28 · **Status:** COMPLETE (one TEST window) · **Verdict: forecasting a
knife minute-by-minute is impossible with everything we tried; a simple STATE rule
(underwater + below a falling VWAP) triages which positions to kill at 3.4σ above random —
but only for dip-style positions. No value found for momentum (SURGER-style) positions,
and the design does not map to RBT's daily-bar timescale at all.**

Operator brief: "We lose to knives. Build an independent police — 10+ ideas, simple to
complex, backtest on history. Not about profit; it's detective. Kills stay manual."

---

## 1. Design (pre-registered before any results)

- **Data:** 762,424 RTH 1-min bars, 47 stocks + SPY/QQQ, 2026-06-01 → 2026-07-28
  (Alpaca SIP). Symbols = the knife-richest names from our own desk journals.
- **Synthetic positions** (16,852): F1 momentum longs (r30 ≥ +1.5% above VWAP, n=1,848)
  and F2 dip-buy longs (z15 ≤ −1.5, n=15,004), entry next-bar open, life ≤ 60 min,
  EOD-capped. Baseline exits: F1 2% peak-trail; F2 mean-revert (z≥0) / z≤−4 floor / timeout.
- **Eval minutes:** any bar ≥ entry+2 with drawdown ≥ 0.5×ATR30 → 518k labeled minutes.
- **Label** (triple barrier, 30-min horizon): price touches P−0.75×ATR first → KNIFE (41%
  base rate); touches P+0.5×ATR first → BOUNCE; ambiguous/timeout excluded from P/R.
- **Split:** FIT = sessions before 07-11 (tune everything); TEST = 07-13 → 07-28, frozen,
  one run. **Money metric:** kill at first fire (bar close) vs baseline exit, $5k slice —
  always compared against RATE-MATCHED random kills (20 reps), because the synthetic
  population is net-losing and killing *anything* looks good without a control.

## 2. The fourteen ideas

| # | idea | TEST P | TEST R | note |
|---|------|-------:|-------:|------|
| K1 | speed ≥ s×ATR/3min | 0.40 | 0.30 | = base rate |
| K2 | consecutive red run | 0.40 | 0.15 | = base rate |
| K3 | new-low cascade | 0.41 | 0.31 | = base rate |
| K4 | volume avalanche | 0.41 | 0.03 | = base rate |
| K5 | bounce-failure clock | 0.41 | 0.65 | = base rate |
| K6 | below falling VWAP, persistent | 0.41 | 0.68 | = base rate at minute level — **but see §4** |
| K7 | idiosyncratic drop vs SPY | 0.41 | 0.33 | = base rate |
| K8 | efficient fall (directional) | 0.41 | 0.14 | = base rate |
| K9 | z-escape velocity | 0.41 | 0.08 | = base rate |
| K10 | downside CUSUM | 0.40 | 0.07 | = base rate |
| K11 | logistic on 15 features | AUC **0.500** | | a literal coin |
| K12 | LightGBM, 300 trees | AUC 0.666 FIT → **0.513** TEST | | memorization collapse (REGIME-study pattern) |
| K13 | GRU on last-30-bar sequences | AUC **0.503** | | coin |
| K14 | ensemble | moot | | nothing to ensemble |

**Finding 1 — the forecast is dead.** Given a position already 0.5 ATR underwater,
whether the next 30 minutes continue down 0.75 ATR or bounce 0.5 ATR is ~random
(base 41%) under every feature, model family, and complexity level tried. 518k labeled
minutes; both families; FIT and TEST agree. Do not re-attempt minute-level knife
prediction with variations of these features.

## 3. Decisive controls (position-level $, TEST, vs 20-rep rate-matched random)

| rule | kill rate | $ net | random mean±sd | LIFT | lift/σ |
|---|---:|---:|---:|---:|---:|
| C0: kill EVERY position at arm | 100% | +1,392 | +2,865 ± 867 | **−1,473** | −1.7 |
| C0v: kill at arm IF VWAP-state true | 51% | +3,772 | +1,457 ± 973 | +2,315 | +2.4 |
| **K6: kill at first VWAP-state fire** | 73% | **+5,132** | +2,305 ± 821 | **+2,827** | **+3.4** |

**Finding 2 — state beats forecast.** Indiscriminate killing is *worse* than random
(−1.7σ): plenty of underwater positions recover. But *which* underwater positions to
kill is knowable: **≥5 of the last 8 bars below session VWAP + VWAP slope negative +
price under entry** selects kills worth +3.4σ over random. Most of the edge is already
present at the arm moment (C0v +2.4σ) — it is a state check, not a timing call.

**Finding 3 — it only works for dip-buyers.** Per family (K6): F2 dip positions
**+$6,106**, F1 momentum positions **−$973**. Per day, K6's selectivity also cuts the
bad days of indiscriminate killing roughly in half (07-21: −835 vs C0 −2,532; 07-28:
−1,899 vs −3,565) while keeping the great days (07-27: +4,582 — the reverter-massacre
session, where dip-killing was gold).

## 4. What this means per desk (the honest map)

- **REVERTER / breadcrumbs (dip-buyers): the police has a real badge.** The K6 state
  rule is the study's product. Proposed v1: log-only **Knife Police shadow** — watch open
  dip positions; when one is ≥0.5 ATR underwater AND in the VWAP state, journal + Telegram
  alert; the operator kills manually or ignores. No desk wiring. Note overlap: the
  REVERTER weekend finale (curfew × cascade halt × 3 knife filters) attacks the same
  wound at entry-time; the police is the position-time complement.
- **SURGER (momentum): no.** Evidence says kills hurt momentum positions; its per-variant
  trails + weekend exit-repair item #5 remain the right tools.
- **RBT (daily-bar pairs): wrong microscope.** A 1-min knife detector does not map onto a
  5-session mean-reversion hold — RBT's knife defense is its catastrophic stop, and the
  right experiment is the already-booked weekend duel (2.5×ATR vs flat 2%).

## 5. Caveats

Single TEST window (12 sessions, one regime — chop-with-down-days; the edge should be
re-measured in a trending month). Synthetic F2 approximates reverter but is not its exact
entry logic; a join against real logged reverter trades is the follow-up before any live
alerting. Lift per kill is thin (~$0.78/position on $5k); the value is aggregate and in
the red-day asymmetry. All thresholds FIT-frozen; TEST touched once.

*Lab: scratchpad knife_lab/ (fetch, harness, ml_finals, controls; events.pkl +
results_k1_10.json). 14 ideas, 16,852 positions, 518k labeled minutes, one afternoon.*
