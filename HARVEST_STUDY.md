# HARVEST study — many-small-profits detector program (2026-07-21)

**Verdict: NEGATIVE — no deployment.** Sixteen ideas, four iterations, ML/DL, and combos
produced a family that earned 62–63% WR on the July dev window and then failed both
walk-backward checks (June: 49.7%, May: 46–48%). The edge was July-specific. The
pre-registered Mar–Apr final window was never burned (nothing passed the May bar).

## Program design

Goal (operator's brief): the breadcrumbs philosophy — not one large win but many tiny,
repeatable captures that accumulate. Fixed harness so ideas compete on DETECTION only:
entry next-bar-open 10:00–15:30 ET on completed bars · exit +0.40% TP / −0.40% SL
(both-touch = SL, conservative) / 30-bar time exit / EOD 15:55 · one position per
symbol, 30-min cooldown, $1.5k slices · costs at 2bp and 5bp per side.

**The bracket math that governs everything:** at ±0.4% symmetric, breakeven WR is
**55% @2bp** and **62.5% @5bp**. A small-profit scalper pays a quarter of its target in
costs at 5bp/side. This is the quantified version of what bled the live breadcrumbs desk.

Data: 97 sessions of 1-min SIP bars, 534 names (Mar 1 – Jul 20, the SURGER pickles).
Dev = Jul 6–20 · walk-backward checks = Jun 10–Jul 3 and May 1–Jun 9 · Mar–Apr reserved.

## What was tried (all results @2bp unless noted; dev window)

Raw simple ideas (batch 1) — ALL below 50% WR, huge trade counts (noise harvesting):
BB snapback 47.9% · VWAP stretch 46.8% · red-run exhaustion 48.6% · breadcrumbs-twin
momentum 47.7% · idio-dip 49.8% (best) · VR-gated stretch 48.3% · cusum-stab 46.5% ·
flow exhaustion 45.6%.

Iterated structure that emerged (real WITHIN July):
- **Noise-fit gate** sd1(60) ∈ [0.18%, 0.24%]/min: +5.6 WR pts on idio (49.8→55.4%).
  Found by independent diagnostic quartiles, but did NOT tolerate widening — fragility
  that foreshadowed the regime failure.
- **Stabilization (3 bars no-new-low) beats confirmation** (strong green bar HURTS —
  buying the proven bounce is buying it spent): 57.1%.
- **Afternoon >> morning**: same signal 53.5% before 11:30, 60.8% midday, 66.7% after
  14:00. Morning dips trend; afternoon dips revert.
- **Deeper stretch hurts** (−1.5% catches knives; −1.2% optimal).
- **Relative beats absolute** (the central finding): absolute-stretch entries (z15,
  zvwap, BB) never worked under any gate; the idiosyncratic version of the same idea
  (stock down ≥1.2% vs 534-name median flat) carried all the edge. Confirmed again by
  ideas 9–11: Fourier purity ≤1 (47.1%), Hurst ≤0.45 (49.5%), OU half-life (48.9%) all
  failed as absolute-stretch entry gates.

Dev finalists (Jul 6–20): pm2 = idio ≤−1.2% + market flat + fit + no-new-low ≥11:30 →
**61.9% WR, +$119 @2bp, 16.5 t/d, 9/11 green** · +VR≤1.0 → 63.4%, +$94, only config
green @5bp (+$3) · +OU hl≤20 → 61.8%, 11/11 green days · union with cusum-capitulation
leg (65.1% standalone on 43 trades) → 63.1%.

ML/DL (trained Jun 10–Jul 3, tested dev; shape-only scale-free features, no
minute-of-day/symbol identity): LGBM meta-label 58.7% (worse than hand rule; inverse
threshold response = non-transfer); per-episode dedup fix 55.2% (still worse); pooled
LGBM 46–49%; CNN knife-vs-bounce 50.0%; AE knife-veto 59.0% (vetoed good trades).
**Four attempts, zero beat the hand rule** — same result as the SURGER program's ML/DL.

## The kill (walk-backward, real simulator)

| Window | pm2 | +VR | +OU | union |
|---|---|---|---|---|
| Jul 6–20 (dev) | 61.9% / +$119 | 63.4% / +$94 | 61.8% / +$78 | 63.1% / +$103 |
| Jun 10–Jul 3 | **49.7% / −$181** | **43.1% / −$230** | 49.3% / −$144 | 53.1% / −$92 |
| May 1–Jun 9 | **46.4% / −$529** | 47.5% / −$282 | 46.7% / −$348 | 48.2% / −$453 |

The VR filter — best-looking on dev — was the WORST in June: fitted noise, not quality.
Contrast SURGER's C2 at the same checkpoint: 68.7% on June, 53% on Mar–Apr. The pipeline
distinguishes real edges from mirages; this one was a mirage.

## What survives

1. The bracket-cost math (55%/62.5% breakeven walls) — permanent context for any future
   small-profit idea, including breadcrumbs dial decisions.
2. "Relative beats absolute" at the 1-min scale — cross-sectional context is the only
   stretch definition that ever showed life.
3. The July regime hosted BOTH momentum (SURGER) and reversion (HARVEST-dev) edges —
   July was a high-dispersion tape. A future revival needs a REGIME GATE that predicts
   when idio-reversion pays (candidate features: prior-day cross-sectional dispersion,
   market-median intraday vol, index chop). That requires more months of data than the
   4.5 on hand and is parked as future work.
4. Negative result cost $0 (vs breadcrumbs' live −$1,216 lesson of the same shape).

## Addendum — universe check (operator hypothesis, 2026-07-21)

Hypothesis: the edge lives in habitually-volatile names; the full-534 universe diluted
it. Causal test: classify each name by its PRIOR session's 1-min return stdev, bucket
trades per window, and re-run the champion restricted to volatile names.

Result: **real in July, not a rescue.** Restricting to prior-day σ ≥ 0.15–0.20%/min
lifted July dev to 64.8–65.4% WR (+$129/+$101 @2bp) — but made June WORSE (46.9–49.0%
vs 49.7% full) and left May red (46%). The vol ladder rises in July (53.8→65.9% across
bands), INVERTS in June (b4 0.20–0.30%: best in July, worst in June), flat-red in May.
Universe selection AMPLIFIES the regime's sign; it cannot flip it. Hierarchy measured:
regime >> time-of-day > universe.

Per-name nuance worth keeping: best hosts were liquid mid-vol semis (WDC 76.5%, MU
69.6%, AMKR 63.2% — n=17–23 each); worst were lottery-ticket names (IONQ 35.5%, QBTS
36.1%) whose dips are real repricings, not noise. Do NOT build a universe from this
table (survivorship on tiny n) — but "volatile-yet-institutional beats ultra-spec" is a
prior for any revival. Script: scratchpad `harvest_universe.py`.

Artifacts: harness + all runs in session scratchpad (`harvest_lab.py`, `harvest_iter1-4`,
`harvest_ml*.py`, `harvest_dl.py`, `harvest_combos.py`, `harvest_unseen.py`,
`harvest_universe.py`, `harvest_results.json`). No production code was created or
modified.
