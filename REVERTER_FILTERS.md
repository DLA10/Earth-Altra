# REVERTER knife filters: specification & evidence

**Status (2026-07-18): DESIGNED + BACKTESTED, NOT YET IMPLEMENTED.** Observing one more
week of live REVERTER before deciding whether to build them. Decision tracked in
`RIDP_REVERTER_FIXES.md` (Item 1). Paper-only; the real-money Execution path is untouched.

## Why these exist

REVERTER buys a stock stretched **−1.5σ below its 15-minute rolling mean** (a "rubber
band") and exits back at the mean. It works in calm/range-bound tape and **bleeds in market
slides**: it keeps buying dips into a falling market whose mean is *also* falling, so the
dips never bounce and the −4σ stops fire in clusters. Its win/loss shape is asymmetric
(avg loss ≈ 1.6× avg win *and* low win-rate on knife days), so on a slide it loses fast.
All three filters are **entry-side gates only**, exits, stops, sizing are untouched, and
each is **env-dialed** (see rollback values). Every skip should journal its counterfactual.

---

## Filter 1: Green-confirm ("don't grab a falling knife")

**Plain:** only buy if the *current* 1-minute bar is green (closed at or above its open).
Don't buy while the price is still ticking down this minute; wait for the first up-bar.

**Math:** at entry candidate, require `bar.close >= bar.open`. Else skip (symbol stays
eligible; can trigger on a later green bar).

**Example:** NBIS hits −1.5σ at $170. If the live minute-bar opened $170.40 and is now
$169.90 (red) → WAIT. If it opened $169.80 and is now $170.10 (green, buyers stepping in)
→ allowed. *Single most powerful filter, cut ~78% of one bad day's losses alone, and it
makes the old "falling-anvil" idea redundant.*

**Dial:** on/off (env, e.g. `RIDP_REV_GREEN=0` to disable).

---

## Filter 2: Dock ("don't bet on a sinking target")

**Plain:** REVERTER bets the price returns to its 15-minute average. If that average is
*itself* collapsing, there's no stable target to revert to. Skip such symbols. **Per-symbol,
NOT a market switch**, it keeps trading calm stocks even on a red-QQQ day (operator
rejected a blanket QQQ kill-switch for exactly this reason).

**Math:** let `mean_now` = mean of the last 15 closes, `mean_ago` = mean of the 15 closes
ending 5 bars back, `sigma` = population std of the last 15 closes. Skip if
`(mean_now - mean_ago) < thr * sigma`, with **`thr` default −0.2σ** (a fast-sinking dock).

**Example:** NBIS is −1.5σ, green bar passes, but its 15-min mean fell $172.50→$170.80 in
5 minutes (steep) → SKIP. KO on the same red day has a flat mean → allowed.

**Dial:** `RIDP_REV_DOCK_THR` (default −0.2; −0.3 is more permissive; 0 or large negative
to disable). Backtest: −0.2 slightly better than −0.3 on the sample, but both help.

---

## Filter 3: Circuit breaker ("when you keep getting cut, stop reaching")

**Plain:** ignore any single stock, if the *desk* got stopped out repeatedly just now,
the weather has turned; pause all NEW REVERTER entries for a while. Existing positions and
their exits keep running. Uses our own realized stop-outs as the knife alarm (hard evidence,
not a prediction). Never trips during a winning streak (winners don't produce stop-outs).

**Math:** if **3** REVERTER stop-outs occur within a rolling **10-minute** window → block
new entries for **15 minutes**. Re-trips only on 3 *fresh* stop-outs after a pause ends.

**Tuning result (both live days):** trip-early + rest-long wins. Raising to 5 stops = worse
(trips too late). Shorter 5-min pause = mixed. Best = **3 stops / 10-min window / 15-min
pause**. On a knife day it can pause ~68% of the session, which was *correct* (REVERTER
should barely trade then); on the good morning hour it paused only ~3 min (proportional).

**Dials:** `RIDP_REV_BRK_STOPS` (3), `RIDP_REV_BRK_WINDOW_MIN` (10), `RIDP_REV_BRK_PAUSE_MIN`
(15). Set stops to a huge number to disable.

**Stacking order on one entry:** breaker (desk) → dock (symbol) → green-confirm (this bar).
A trade must clear all three: calm desk, steady dock, buyers showing up now.

---

## 14-day backtest evidence (2026-06-29 … 2026-07-17, SIP 1-min, faithful replay of reverter.go)

Frictionless (see the fidelity caveat below, absolute numbers are optimistic):

| Config | Net P&L | Trades | Win% | Profit factor |
|---|---|---|---|---|
| NONE (no filter) | −$2,433 | 11,584 | 56.4% | 0.92 |
| dock + green (no breaker) | −$259 | 1,183 | 56.6% | 0.91 |
| dock + green + breaker(10m) | −$145 | 1,019 | 57.1% | 0.94 |
| **dock + green + breaker(15m)** | **−$100** | 1,018 | 57.6% | 0.96 |
| dgb(15m), dock −0.3σ | −$61 | 1,234 | 58.1% | 0.98 |

Cost sensitivity (round-trip bps), REVERTER is savagely cost-sensitive:
- dgb(15m): 0bp −$100 · 1bp −$378 · 2bp −$656 · 5bp −$1,489
- NONE: 0bp −$2,433 · 1bp −$5,586 · 2bp −$8,739 · 5bp −$18,199

## Fidelity check (the critical caveat)

Backtest vs the two days we actually traded live:

| Day | Backtest (frictionless) | Live actual | Gap |
|---|---|---|---|
| 2026-07-16 | +$404 | −$344 | −$748 |
| 2026-07-17 | −$24 | −$1,258 | −$1,234 |

The $750–1,230/day gap is **real fill slippage** (plus one-time ops disasters on those two
days: the ghost meltdown 07-16, the dark-hour offline stops 07-17). Therefore:
- **Absolute backtest P&L is NOT a live predictor**: it is optimistic by a large margin.
- **Relative ranking IS trustworthy**: same fill model both sides; the ordering
  (dgb15 > dgb10 > dg > none) held across 14 days, both dock thresholds, and the cost sweep.

Logic was verified line-by-line against `backend/internal/ridp/reverter.go` (population-std
z-score, z≤−1.5 entry, `int(1500/last)` sizing, `round(last−2.5·std,2)` stop, exit ladder
stop→mean→z≤−4→15:55, one-per-symbol, 90s cooldown, per-day breaker). No look-ahead.

## Honest verdict

1. **The filters work at what they do**, cut the frictionless loss ~96% (−$2,433 → −$100),
   consistently, by trading ~91% less and dodging trend-down days.
2. **But REVERTER has no demonstrable edge**, even frictionless + filtered it stays slightly
   negative (profit factor < 1.0) over 14 days, and clearly negative after realistic costs.
   The filters reduce losses; they cannot manufacture an edge that isn't in the data.

**Practical stance:** if REVERTER runs live, these filters are a strict, reversible
improvement (losing $100 beats losing $2,433), but do not count on REVERTER as an earner.
Its honest role is a cheap live experiment that has, so far, returned a negative answer. If
the filtered live journal is still negative after the observation week, bench it
(`RIDP_REVERTER_TOP_N=0`) and redeploy attention to the momentum side (RIDER) / signal desk.

## PROPOSED CHANGE (2026-07-18): limit-sell the profit exit (NOT yet implemented)

**Idea (operator):** REVERTER's profit exit is a sell *into rising price* (price returning
up to the mean), the ideal case for a resting LIMIT order that CAPTURES the spread instead
of paying it. So: **place a resting limit sell at the mean (entry + 1.5σ) at entry time;
keep the emergency stop as a market/exchange stop; keep the entry at market.** ("Limit for
selling only, market for buying", limiting the entry would cause adverse selection.)

**8-week validation (all downloaded data, ~48 days, corrected proper limit-fill model,
profit fills only when the bar high reaches the target; stop-fills-first = pessimistic):**

| Config | Frictionless | Trades | Market @1/2bp | Limit-sell @1/2bp |
|---|--:|--:|--|--|
| No filters | −$536 | 36,154 | −10.4k / −20.4k | −7.4k / −14.2k |
| Green+Dock | +$205 | 4,038 | −904 / −2,013 | −563 / −1,331 |
| **All three** | **+$759** | 3,535 | −212 / −1,182 | **+93 / −573** |

Regime (all-three, 2bp): May(good) LIMIT +$166 vs MKT −$109; Jun(neutral) +$28 vs −$143;
Jul(bad) −$767 vs −$930. Limit-sell **flips good/neutral regimes positive at 2bp**, softens
(doesn't cure) the bad regime.

**Verdict:** limit-sell is a genuine, consistent improvement, it roughly halves the cost
drag and pushes break-even from just under 1bp to ~1.2bp. It is NOT a silver bullet:
filtered+limit REVERTER is still only positive below ~1.2bp and still loses in a genuinely
bad regime. This model is the PESSIMISTIC bound (stop-fills-first); reality is between it and
the optimistic run, so the true benefit is likely a bit better. The deciding unknown remains
the REAL limit fill quality / slippage, measurable only live.

**Levers tested, both empty:** (A) target a hair below the mean, no measurable backtest
help (its real benefit is fill-rate, which a bar backtest can't capture); (B) dock threshold
the current −0.2σ is already optimal (beats −0.1 and −0.3/−0.4). No free tuning gains.

**To implement (after live observation):** in `reverter.go`, change the profit exit from
"detect z≥0 → market sell" to "place a resting limit sell at entry+1.5σ at entry time";
leave the exchange stop as-is. Env-flag it (e.g. `RIDP_REVERTER_LIMIT_EXIT`). Then run live
and measure actual fill rate + slippage, the last real unknown.

## Scripts (session scratchpad, re-runnable)
`bt_fetch.py` (download SIP), `bt_replay.py` (14-day filter replay + cost + fidelity),
`filter_replay2.py` / `filter_table3.py` (single-day rich tables), `breaker_sweep.py`
(knob sweep on both live days), `filter_tune.py` (threshold/green-confirm tuning).
