# Regime Detector Study — 8 pipelines, 136 sessions (2026-07-22, incl. leak correction)

**Verdict (leak-free): D3 "morning probe" survives — 71.1% TEST direction accuracy —
and ships as a SHADOW publisher only. Its probe-dollar edge over always-momentum is ~$0,
so its value case rests entirely on the direction signal (gating REVERTER-style desks).
Every statistical/ML/DL/HMM/Hawkes pipeline failed out-of-sample, worse after the fix.**

## ⚠ Correction record (read first)

The first published run (same night) contained a look-ahead bug: the MORNING R-probe's
management loop stopped at 11:00 and marked still-open positions at the 15:55 close —
afternoon prices leaking into a morning signal. Found while porting the probe to Go,
before anything shipped. All four probe legs were recomputed leak-free (v2: entries in
[t0,t1), management through t1+maxhold) for all 136 sessions and every detector was
re-scored. Numbers below are the honest ones. Effect: D3 accuracy 72.3% → **71.1%**
(the direction signal was real), but its gated P&L edge over the best baseline fell
from +$593 to **+$9** (the leak had flattered probe magnitudes on both legs).

## Design (fixed before any detector ran)

By 10:30 ET, using only the first hour, predict whether the AFTERNOON favors momentum
(TREND) or dip-reversion (CHOP). Truth labels from two fixed micro-probes replayed per
afternoon (leak-free v2): **R-probe** — idio dips ≤ −1.0% vs flat market median, 3-bar
stabilization, entries 12:00–15:00, ±0.4% bracket, 30-min max hold; **M-probe** — top-5
movers 10:30→12:00 entered 12:01, 1.5%→0.5% trail, flat 15:55. label = TREND iff
M > R; decisive iff |Δ| ≥ $20. Data: 136 sessions Jan 2 – Jul 21, 534 names, 1-min SIP.
**Split:** FIT Jan–Apr (82), TEST May–Jul (54, untouched). **Pass bar:** TEST accuracy
≥ 55% on decisive days AND gated P&L > best single-probe baseline.

## The eight pipelines

| # | Pipeline | Core |
|---|---|---|
| D1 | ORB-HOLD | opening-range break/hold rates → threshold |
| D2 | VR-DISPERSION | market VR + dispersion + breadth composite → threshold |
| D3 | MORNING-PROBE | run both probes 10:05–11:25; bet the morning's winner repeats. Zero parameters |
| D4a/b | ML | logistic / LightGBM on 14 morning features |
| D5 | HMM | 2-state Gaussian HMM, causal forward filtering |
| D6 | HAWKES | shock-event self-excitation proxy (overdispersion + autocorr) |
| D7 | GRU | tiny recurrent net on the morning minute-tensor |
| D8 | ENSEMBLE | majority of D1/D2/D3/D6 |

## Leak-free scorecard (TEST = May–Jul, 54 sessions, 45 decisive)

Baselines: always-R −$1,043 · always-M +$513 · yesterday's-winner 53.3% / +$144 ·
oracle +$1,980.

| Detector | TEST acc | Gated P&L | Verdict |
|---|---|---|---|
| **D3 morning-probe** | **71.1%** | **+$522** | **PASS (letter); probe-$ edge ≈ 0 — ships as SHADOW, direction-signal value only** |
| D1 ORB-hold | 44.4% | −$465 | fail |
| D2 VR-disp | 44.4% | −$960 | fail |
| D4a logistic | 42.2% | −$971 | fail |
| D4b LGBM | 44.4% | −$732 | fail (FIT 95.6% — memorization) |
| D5 HMM | 42.2% | −$936 | fail |
| D6 Hawkes | 37.8% | −$1,295 | fail |
| D7 GRU | 37.8% | −$1,144 | fail |
| D8 ensemble | 44.4% | −$960 | fail |

D3 per-month gated: Jan +150 · Feb −370 · Mar −464 · Apr +140 · **May +631 · Jun +142 ·
Jul −251** — lumpy; the direction accuracy is the stable part, the probe dollars are not.

## Honest reading

1. **What survived:** the direction call. 71.1% vs a 53.3% persistence baseline is a
   large, real gap, robust to the leak fix, from a zero-parameter rule.
2. **What did not:** dollar conversion inside the probes. Right calls save small dollars,
   rare wrong calls (missed momentum days) cost big ones. Any consumer that cares about
   *magnitude* must not use D3 raw.
3. **The consumer that fits:** REVERTER-style afternoon gating cares only about
   direction — "is dip-buying alive this afternoon?" That is exactly the 71% signal, and
   exactly what the live shadow now measures.
4. D3's edge is still concentrated May–Jul (FIT 48.5%): intraday regime persistence is
   itself a regime, currently ON. It can switch off; the shadow hit-rate ledger is the
   tripwire.
5. Selection risk across 8 detectors remains; forward shadow performance is the judge.

## Shipped (shadow only)

`internal/regime` — D3 as a log-only publisher: prediction ~11:31 ET, outcome scored
~16:05, journal `data/regime/<day>.jsonl`, report at `/api/regime` (trailing hit rate).
No desk consumes it. Promotion condition (operator decision later): ≥60% live hit rate
over ≥2 weeks → candidate gate for REVERTER afternoons + trail on/off.

## The meta-lesson (unchanged, now leak-proofed)

Zero-parameter empiricism beat logistic, LightGBM, GRU, HMM, and a Hawkes proxy — the
fourth such result this week. And the leak itself is the second lesson: the study
discipline (port forces re-read; pre-registered bars; repair before ship) caught a bug
that flattered our own headline by +$584 of phantom edge.

Artifacts: `regime_fetch.py`, `regime_panel.py` (v2 probes), `regime_repair.py`,
`regime_detectors.py`, `regime_days.pkl`, `regime_results.json` — session scratchpad.
