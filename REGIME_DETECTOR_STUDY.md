# Regime Detector Study — 8 pipelines, 136 sessions (2026-07-22 overnight)

**Verdict: ONE detector passed the pre-registered bar — D3 "morning probe", the
zero-parameter one. Every statistical, ML, DL, HMM, and Hawkes pipeline failed
out-of-sample.** D3 is recommended for a SHADOW slot only; nothing gates a live desk
until it proves itself forward.

## Design (fixed before any detector ran)

**Question:** by 10:30 ET, using only the first hour, predict whether the AFTERNOON
favors momentum (follow-through) or dip-reversion (snap-back).

**Truth labels from our own strategies** — two fixed micro-probes replayed per afternoon:
- **R-probe** (reversion): idio dips ≤ −1.0% vs flat market median, 3-bar stabilization,
  entries 12:00–15:00, ±0.4% bracket, 30-min max hold, $1.5k slices.
- **M-probe** (momentum): top-5 movers ranked 10:30→12:00, entered 12:01, 1.5%→0.5%
  trail (the validated C2 exit), flat 15:55.
- label = TREND if M-probe P&L > R-probe P&L; decisive if |Δ| ≥ $20.

**Data:** 136 sessions, Jan 2 – Jul 21, 534-name universe, 1-min SIP bars.
**Split (pre-registered):** FIT = Jan–Apr (82 sessions), TEST = May–Jul 21 (54, untouched
by all fitting). **Pass bar (pre-registered):** TEST accuracy ≥ 55% on decisive days AND
detector-gated P&L > best always-one-probe baseline.

## The eight pipelines

| # | Pipeline | Steps |
|---|---|---|
| D1 | ORB-HOLD | opening-range break/hold rates across universe → threshold (fit on FIT) |
| D2 | VR-DISPERSION | market variance-ratio + dispersion + breadth + ORB, z-scored sum → threshold |
| D3 | MORNING-PROBE | run BOTH probes on the morning (10:05–11:25); bet the morning's winner repeats in the afternoon. Zero fitted parameters |
| D4a/b | ML | logistic regression / LightGBM over 14 morning features |
| D5 | HMM | 2-state Gaussian HMM over daily feature vectors, causal forward filtering (no future peeking) |
| D6 | HAWKES | self-excitation of market shock events (per-minute counts of names moving >2σ); overdispersion + autocorrelation as branching-ratio proxy → threshold |
| D7 | GRU | tiny recurrent net on the morning's minute tensor (market return + shock counts) |
| D8 | ENSEMBLE | majority vote of D1/D2/D3/D6 |

Baselines: always-R, always-M, B0 = yesterday's winner, ORACLE = per-day best.

## Scorecard (TEST = May–Jul 21, 54 sessions, 47 decisive)

| Detector | TEST acc | Gated P&L | vs best single (+$513) | Verdict |
|---|---|---|---|---|
| **D3 morning-probe** | **72.3%** | **+$1,106** | **+$593, 32% oracle capture** | **PASS** |
| B0 yesterday (baseline) | 61.7% | +$445 | −$68 | baseline |
| D7 GRU | 48.9% | −$550 | fail | DL now 0-for-8 on this codebase |
| D2 VR-disp | 44.7% | −$750 | fail | |
| D5 HMM | 44.7% | −$825 | degenerate (predicted CHOP every day) | fail |
| D1 ORB-hold | 42.6% | −$471 | fail (worked Jan–Feb, died after) | |
| D6 Hawkes-proxy | 38.3% | −$1,123 | fail | |
| D4a logistic | 36.2% | −$1,089 | fail | |
| D4b LGBM | 36.2% | −$1,266 | **FIT was 94%** — textbook memorization | fail |

Per-month gated P&L (winner vs baselines):

| Month | D3 | always-R | always-M | oracle |
|---|---|---|---|---|
| Jan | −8 | −268 | +23 | +422 |
| Feb | +34 | −270 | −14 | +499 |
| Mar | −24 | +565 | −705 | +760 |
| Apr | +28 | −197 | −26 | +501 |
| **May** | **+644** | +429 | +348 | +1155 |
| **Jun** | **+456** | −524 | +258 | +945 |
| **Jul** | **+6** | −730 | −93 | +250 |

## Honest caveats

1. **D3's edge is concentrated in May–Jul** (FIT accuracy was only 49%). Since it has no
   parameters, that isn't overfitting — it means *intraday regime persistence itself is a
   regime* that switched on around May. Corroboration: the yesterday's-winner baseline
   also strengthened in TEST (62%). Both persistence signals are currently ON; they can
   switch off. This is exactly why D3 gets a shadow week, not authority.
2. Probe dollars are miniature ($1.5k slices); the value is the 72% directional signal,
   which would gate real desks (REVERTER afternoons, trail on/off, SURGER expectations)
   at much larger dollar leverage.
3. Eight detectors were compared → residual selection risk even on an untouched TEST
   window. Forward shadow performance is the final judge.
4. Oracle capture is 32% — a better detector likely exists; none of the seven fancy ones
   here is it.

## The meta-lesson (fourth confirmation this week)

Hand-crafted/empirical simplicity keeps beating sophistication on this data: SURGER's
hand rule beat its ML; HARVEST's relative-dip beat Fourier/Hurst/OU; the dumb-game
curfew beat every per-position tripwire; and now a zero-parameter "run both strategies
for 80 minutes and bet the winner repeats" beat logistic, LightGBM, GRU, HMM, and a
Hawkes proxy. The market data we have rewards measurement, not modeling.

## Proposed next step (operator decision — nothing deployed)

Shadow-publish D3 daily: compute both morning probes at ~11:30 ET (pure-Go replay of the
two probe rules over the engine's morning bars, or a 1-min Python cron), journal
`{day, prediction, actual_label}` alongside the RIDP Guardian logs, and review the live
hit rate at the weekly decision. If it holds ≥60% for 2 weeks, wire it as a gate for
REVERTER afternoons (curfew-override: afternoons allowed only on predicted-CHOP days)
and as the trail on/off switch.

Artifacts: `regime_fetch.py`, `regime_panel.py` (features+labels+probes),
`regime_detectors.py`, `regime_days.pkl` (136-session panel), `regime_results.json` —
session scratchpad. No production code touched.
