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

## 5b. Round 3 — the three unfinished families + the reality checks (2026-07-28)

Everything left on the list was run: the triage-ML family, specialist, seq-DL, plus an
entry-time model, a leave-one-day-out audit, and — decisively — a replay on the desk's
**own logged reverter trades**.

| test | result |
|---|---|
| **T1 triage-ML, one-shot at age 2/3/5/8** | **AUC 0.72 → 0.81, and it HOLDS out-of-sample** (CV 0.805 → TEST 0.810). The triage label is genuinely learnable — unlike the barrier label (0.50). **But the dollars don't follow: −1.9σ … +1.8σ.** It knows *whether* killing wins, not *how much*. |
| **T2 money-weighted regressor** (predict $ saved, the obvious fix) | **−4.8σ / +0.6σ / −1.6σ / −1.8σ.** Optimizing dollars directly does not rescue it. |
| **T3 specialist (per family, per volatility)** | noise: F2 +2.6σ at one threshold, −7.1σ at the next; **F1 negative at every threshold** (momentum kills hurt — third independent confirmation) |
| **T4 seq-DL GRU on $ target** | corr +0.11, no usable threshold |
| **T7 leave-one-day-out on the TOD survivor** | **drop 2026-07-27 and it collapses from +$4,242 to −$186 (−0.3σ).** Every other day-drop leaves it strong. The "survivor" was one session. |
| **T6 REAL trades — 4,370 logged reverter trades, kills bounded by each trade's true exit** | K6 fires 1,131×, raw **+$1,853** — but vs the timing-matched control **+$255 (+0.2σ)**, and **82% of it is 5 trades**, with +$1,376 landing on 2026-07-21 — the session containing the known ANET $0-exit booking bug. A data artifact, not an edge. |
| **T5 ENTRY-TIME model** (decide at entry, not mid-position) | **the only positive result.** Skipping the worst-predicted 20% of entries saves **$12,696** on TEST vs **$5,273** for skipping a random 20% — ≈2.4× better than chance (30%: $16.1k; 50%: $21.7k). Skipped entries average −$12.72 vs −$3.42 for kept. |

**Round-3 verdict: the Knife Police is dead in every form.** Ten independent avenues,
including a model with real predictive power (AUC 0.81), and not one produces dollars
that survive a timing-matched control on either synthetic or real trades.

**Why the synthetic study looked better than reality:** real reverter trades have a
**median hold of 5 bars**; the synthetic baseline exit held up to 60. The lab was
measuring "beat a bad baseline," and the desk's actual exits are far better than that
strawman. Sobering companion number from the real replay: *exiting every reverter trade
at bar 2* would have turned −$7,454 into −$3,086 — again the earliness artifact, but on
real money it says something worth its own test: **reverter may simply hold too long.**
(Oracle ceiling — killing at the perfect bar in every trade — is +$11,113.)

## 6. Status & what would settle it

- **NOT DEPLOYABLE — program closed.** No knife-detection shadow should be built. All
  families are now tested (round 3 completed the three that hit a usage limit).
- **Where the surviving value actually is: the ENTRY side.** T5's entry-time model beat
  random entry-skipping by ~2.4×, and the same 10:00–11:00 fact keeps reappearing. That
  points at the REVERTER weekend table's existing items (entry blackout + knife filters),
  which need no detector, no new service, and no live kill authority.
- **New candidate raised by the real-trade replay (not part of the original brief):**
  reverter's hold time. Median 5 bars; a mechanical 2-bar exit would have cut the sample's
  loss almost in half. Worth its own pre-registered replay at the weekend — with the same
  caution that in a net-losing sample, *any* earlier exit flatters itself.
- **Method note for future studies:** always include a degenerate benchmark and a
  timing-matched control. A rate-matched random control is not enough when the population
  has drift — it silently rewards earliness. That mistake produced round 1's retracted
  headline.

*Lab: scratchpad/knife_lab/ (fetch, harness, ml_finals, controls, verify, final_test,
pipes/ per family). 22 detector/pipeline variants, 16,852 positions, 518k labeled
minutes, 8 parallel experiment agents.*
