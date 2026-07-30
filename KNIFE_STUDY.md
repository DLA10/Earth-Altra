# KNIFE study — can an independent detector spot falling knives in open positions?

**Round 1:** 2026-07-28 (14 detectors) · **Round 2 (same day):** 8 parallel pipeline
families + a corrected evaluator.

> ## ⚠ CORRECTION TO ROUND 1
> Round 1 reported "K6 (below falling VWAP) selects kills +3.4σ over random." **That
> result was an artifact and is retracted.** The round-1 control (rate-matched random
> kills at a *uniform random minute*) fires later on average than any real rule, and the
> synthetic position population is deeply net-losing (−$71k FIT / −$26k TEST, ≈ −$6 per
> position). So *killing earlier* beats the control mechanically, regardless of skill.
> Proof: "kill EVERY position at bar 2" — a rule with zero intelligence — scores **+20.6σ
> (FIT) / +15.1σ (TEST)** against that control.
> **Round 2 adds a TIMING-MATCHED control:** same kill rate *and* the identical multiset
> of kill delays, but assigned to randomly chosen positions. It isolates "did you pick the
> right positions" from "did you kill early." Validation: the degenerate all-fire rule
> scores **+0.1σ** against it (correct — it has no selection). Under this control:
> **K6 = −2.8σ on TEST.** Every conclusion below uses the timing-matched control.

**Verdict: no robust knife detector exists in this data.** Forecasting is dead (round 1,
unchanged). Position *triage* survives only as a narrow, rare-event effect — mid-morning
underwater positions — whose entire measured value came from one violent session.

---

## 1. Design (pre-registered)

762,424 RTH 1-min bars, 47 stocks + SPY/QQQ, 2026-06-01 → 07-28 (Alpaca SIP).
16,852 synthetic long positions: **F1** momentum (r30 ≥ +1.5% above VWAP, n=1,848) and
**F2** dip-buy (z15 ≤ −1.5, n=15,004); entry next-bar open, life ≤ 60 min, EOD-capped.
Baselines: F1 2% peak-trail; F2 mean-revert / z ≤ −4 floor / timeout.
Eval minutes = drawdown ≥ 0.5×ATR30 → **518k labeled minutes**.
Two labels: **barrier** (down 0.75 ATR before up 0.5 ATR within 30 min; base 41%) and
**triage** (killing now beats the baseline exit by ≥0.25 ATR; base **18.2%**).
Split: FIT < 07-11 (tune) / TEST ≥ 07-13 (one frozen pass). Slice $5,000.

## 2. Round 1 — fourteen single detectors (unchanged, all negative)

K1 speed · K2 red-run · K3 new-low cascade · K4 volume avalanche · K5 bounce-failure ·
K6 below-falling-VWAP · K7 idiosyncratic drop · K8 efficient fall · K9 z-escape ·
K10 downside CUSUM → **every one scored barrier precision 0.40–0.41 = the base rate.**
K11 logistic **AUC 0.500** · K12 LightGBM **0.666 FIT → 0.513 TEST** (memorization
collapse) · K13 GRU **0.503** · K14 ensemble (moot).
**Finding: minute-level knife forecasting is impossible with these features. Do not retry.**

## 3. Round 2 — eight pipeline families (FIT-tuned, one frozen TEST pass)

Each family tuned only on FIT through one shared evaluator; 5 of 8 completed
(specialist / triage-ML / seq-DL hit a usage limit — genuinely untested, see §6).

| family | approach | outcome |
|---|---|---|
| INTERSECT | all 165 AND-pairs/triples of K1–K10 | **clean negative** — max barrier precision 0.408 (< base), max triage 0.182 (= base). Conjunction buys zero information and *delays* firing to local lows |
| VOTE | learned weights over the 10 detector states | weights **invert** the detectors (K5/K6/K10 negative = "when they fire, HOLD"); best variants are kill-by-default → +1.1σ timing-matched (mostly artifact) |
| HAZARD | actuarial P(kill-wins) table over depth × duration × VWAP-state × hour × family | highest triage precision of all (0.48–0.54) with honest day-grouped OOF — **yet −0.1σ / −2.6σ on TEST**: it fires late (median 26 min in), so precision without profit |
| TIME-OF-DAY | clock as the gate | **the only real survivor** (below) |
| GATES (breadcrumbs-style) | context → drowning → trigger → persistence | classic K6 chains all failed; the one positive cell is the *opposite* — **drowning ONSET** (shallow ≤0.8 ATR, fast 2-of-3 below falling VWAP) in the first 45 min |

### Frozen TEST results (4,990 positions), timing-matched control

| rule | kill% | med. delay | $ | lift vs timing-matched | σ | triage P |
|---|---:|---:|---:|---:|---:|---:|
| ALL_FIRE@bar2 (degenerate, validates the control) | 100% | 2 | +15,922 | **+110** | +0.1 | 0.19 |
| K6 — round-1 headline | 73% | 4 | +5,132 | **−2,646** | **−2.8** | 0.17 |
| **TOD: underwater during 10:00–10:30 ET** | 10% | 2 | +5,490 | **+4,242** | **+7.0** | 0.42 |
| **BCG-C: drowning onset, first 45 min** | 3% | 3 | +2,887 | **+2,547** | **+9.4** | 0.38 |
| TOD 10:00–10:40 | 14% | 2 | +4,578 | +2,893 | +3.6 | 0.39 |
| VOTE kill-by-default | 86% | 3 | +11,501 | +1,786 | +1.1 | 0.37 |
| HAZ-1 / HAZ-2 / HAZ-3 | 26–39% | 26 | −463…+1,973 | −1,985…−103 | −2.6…−0.1 | 0.48–0.54 |
| INTERSECT K1+K2+K7 | 66% | 11 | +703 | −3,800 | −4.0 | 0.18 |

Both survivors agree on the same statement, from independent starting points, and both
reproduce on FIT (TOD +10.1σ, BCG-C +3.8σ) and in both TEST halves:
**an underwater position in the 10:00–10:30 ET window should be killed immediately.**

## 4. The killer caveat

**90% of the TOD rule's TEST dollars came from a single session (2026-07-27); only 5 of
12 TEST days were positive.** The σ values assume independent positions and therefore
overstate confidence — the effective sample is closer to *one event* than 4,990 trades.
Not a market-drift artifact (SPY's 10:00–10:30 bucket is flat: mean +0.012%, down 42% of
days), so it is genuinely about *positions*, not the tape. But this is the statistical
signature of a **tail-risk tool**: rare, clustered, violent — which is exactly what a
knife is. It cannot be validated further on two months of data; it needs live shadow
observation across many sessions.

## 5. Convergence with the live desks (the one durable takeaway)

This lab, from a completely independent direction (synthetic positions, 47 symbols,
2 months, no desk journals), rediscovered what the REVERTER ledger has been saying all
week: **10:00–11:00 ET is when knives happen** (7 consecutive confirming sessions;
2026-07-27 alone: −$1,166 in that hour, 17 straight stop-outs). Two unrelated methods,
one answer. That raises confidence in the *finding* even though no deployable detector
survived.

## 6. Status & what would settle it

- **Not deployable.** No wiring proposed. The honest options: (a) a log-only shadow that
  alerts when a dip position is underwater in 10:00–10:30 and let live sessions judge it;
  (b) treat it as *entry-time* evidence instead — a 10:00–11:00 entry blackout, already on
  the REVERTER weekend table, needs no detector at all and is the cheaper way to act on
  the same fact.
- **Untested (usage limit):** the specialist (per-family/volatility), triage-ML
  (LightGBM on the triage label), and seq-DL families. TRIAGE-ML is the one genuinely
  promising gap — the hazard table shows the triage label *is* learnable (precision
  0.48–0.54); its failure was timing, not discrimination. A model that predicts
  kill-wins **and** fires early is the remaining unexplored corner.
- **Method note for future studies:** always include a degenerate benchmark and a
  timing-matched control. A rate-matched random control is not enough when the population
  has drift — it silently rewards earliness. That mistake produced round 1's retracted
  headline.

*Lab: scratchpad/knife_lab/ (fetch, harness, ml_finals, controls, verify, final_test,
pipes/ per family). 22 detector/pipeline variants, 16,852 positions, 518k labeled
minutes, 8 parallel experiment agents.*
