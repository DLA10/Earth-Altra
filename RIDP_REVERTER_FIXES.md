# RIDP reverter Fixes — end-of-day decision docket

**Created:** 2026-07-17 (during the live session). **Decide:** after the US market close
(≥16:00 ET / ~21:00 UK). Invoke by saying **"RIDP reverter Fixes"**.

Everything below is **paper-only** (RIDP + Dip+Rise desks). The real-money Execution page
is never touched. Discipline: **one change per live test, judge by data, every dial has an
env rollback** (see `THROUGHPUT_MODE.md`).

Status legend: 🔵 built, needs go-live confirm · 🟡 designed, awaiting decision · ⚪ investigate first

---

## Item 1 — REVERTER knife filters  🟡 OBSERVING ONE MORE WEEK (full spec: `REVERTER_FILTERS.md`)

**The three filters (final form after tuning), entry-side only, all env-dialed:**
- **Green-confirm** — only buy if the current 1-min bar is green (don't grab a falling
  knife). Single strongest filter; makes the old "falling-anvil" idea redundant.
- **Dock (−0.2σ)** — skip if the symbol's own 15-min mean is itself sinking fast. Per-symbol,
  not a market switch — keeps trading calm names on a red-QQQ day.
- **Breaker (3 stops / 10-min window / 15-min pause)** — 3 REVERTER stop-outs in 10 min →
  pause new entries 15 min. Tuning showed trip-early + rest-long beats the original 10-min
  pause and beats raising the stop count.

**14-day backtest (2026-06-29…07-17, faithful replay of reverter.go), frictionless:**
| Config | Net | Trades | Win% | PF |
|---|---|---|---|---|
| NONE | −$2,433 | 11,584 | 56% | 0.92 |
| dock+green | −$259 | 1,183 | 57% | 0.91 |
| **dock+green+breaker(15m)** | **−$100** | 1,018 | 58% | 0.96 |

**Two hard findings:** (1) the filters cut the loss ~96% — real and worth having; (2) even
filtered + frictionless, REVERTER stays slightly NEGATIVE (PF < 1) and is savagely
cost-sensitive (−$100 → −$656 at 2bp). Fidelity check: on the two live days the frictionless
backtest was $750–1,230/day BETTER than reality — the gap is fill slippage, so absolute P&L
is optimistic; only the *relative* ranking is trustworthy. Verdict: filters = damage control,
not an edge. REVERTER has not shown a profitable day yet.

**Current stance (operator, 2026-07-18):** NOT implementing yet — observing one more week of
live REVERTER, then revisit. If still negative, bench via `RIDP_REVERTER_TOP_N=0` and move
attention to RIDER / signal desk. Full detail, examples, dials, and scripts in
**`REVERTER_FILTERS.md`**.

---

## Item 2 — RIDER opening-wave upgrade  🔵 SHIPPED, confirm with full-day data

**Problem (2026-07-17):** RIDER missed the 09:45–10:00 ET sector wave
(NBIS/FCEL/MU/NFLX/DELL/SNDK/ARM all +3–5%). Causes: started at 10:00 (missed 09:45), only
3 slots (journal full of "no free rider slot" skips), first-scanned-wins seating (TFC took
a seat by scan order and became the only loser while MU/SNDK/ARM were skipped).

**Solution (SHIPPED — commits 7b2d1b9, 2cf8f1f):**
- Seats: **uncapped** on paper (budget-only). `RIDP_RIDER_SLOTS`
- Start **09:45 ET** with an early-strict ramp: original +1%/2× gates until 10:00, then
  throughput +0.7%/1.5×. `RIDP_RIDER_START_MIN=15`
- **Ranked** entries: candidates sorted by gain×rvol, strongest funded first
- **Noise re-entry**: re-board a shaken-out symbol (max 3/day) — see Item 6 for the bar
- Fixed two silent blockers: pre-open VWAP reference window + 35-bar minimum at 09:45
- Verified by replay: would have caught 6/6 morning risers, all profitable (~+$216)

**Confirm at close:** the running desk had the OLD binary until restart; check the first
post-restart session behaves (09:45 entries, ranked, uncapped, re-boards journaled).

**Note — ranking is essential at real scale:** on paper the budget funds many seats so
ranking just orders them; at a real $500 the budget funds ~1 seat, so ranking IS the
strategy (best candidate only). Confirmed in place.

---

## Item 3 — SURGER (new strategy)  🟡 DECIDE (shadow-first)

**Problem:** RIDER measures *altitude* (% above the day's OPEN). A stock that opens flat/red
then rockets is invisible to it. 2026-07-17 universe sweep: 265 momentum bursts
(≥2%/30min, above VWAP) after 10:00; RIDER's lens covers 220, but **45 are structurally
invisible** — IREN +3.5%, RIOT +3.1%, HOOD +2.7%, ARRY +2.8% (at −1.7% from open), SNPS
bounce at −6.3% from open. Nobody catches these: dip+rise's front door needs an oversold
washout; RIDER needs altitude.

**Solution (proposed):** SURGER = a *velocity* lens — enter on ≥2% rise over the last 30
min + above VWAP + volume surge, **regardless of position vs the open**. Trail-style exits
like RIDER. **SHADOW-ONLY first**: journal every signal + counterfactual for ~a week (zero
cost), then decide a paper slot. Caution: below-open bursts include more dead-cat bounces
(the SNPS one was a bounce inside a −6% day), so shadow proof before real paper.
Sweep script: `scratchpad/burst_sweep.py`.

**Decision needed:** build SURGER in shadow now / defer.

---

## Item 4 — RIDER trail ladder  🟡 DECIDE (data says: leave it)

**Problem/worry (operator):** is 3.5% trailing too wide? Fear that a stock rises then gives
back 3.5% to breakeven/loss. Real gap identified: a move that peaks +1% to +2.9% never hits
the +3% tighten trigger, so it can round-trip to ~−0.7%.

**Data (2026-07-17, six morning momentum entries):** trail widths **2.0% / 2.5% / 3.5% /
ladder all produced IDENTICAL exits** (+$215.29, 6/6) — every winner blew past +3% so fast
the 2% tighten took over immediately; the initial width never got to decide. **1.5%
destroyed a winner** (shook NFLX to −$17). Also: the stocks wiggled 2.6–5.3% mid-run and
kept going (MU shrugged off a $32 dip → +7.3%), so tight trails would have shaken out the
big movers. The real money left on the table was the *tight 2% tighten* exiting at
10:04–11:13 while tops printed 12:00–13:26 — which the **re-board rule (Item 6) already
recovers**.

**Recommendation:** **keep 3.5%→2% unchanged.** The width isn't the risk and isn't the
bottleneck. Quick confirm and close. (Optional future: a middle rung 2.5% @ +2% for the
dead-zone movers, but data doesn't demand it.)

---

## Item 5 — RIDER red-QQQ compensating bar  🟡 DECIDE

**Problem:** QQQ < −0.15% from open = RIDER **fully benched**, even for exceptional single
leaders. But the signals 12-month study says "strong stock / weak market" (rel_strength) is
the **most profitable cell of all** (+0.227 mean R). A stock making highs on huge volume
*while* the index bleeds is the most information-rich strength there is — RIDER throws it
away. Same bluntness we rejected for REVERTER's QQQ switch.

**Solution (proposed):** tiered gate instead of on/off —
- QQQ −0.15% … −1%: hunt, but **strict gates only** (+1.5% from open, 2.5× vol)
- QQQ < −1%: benched (genuine slide = knife weather)
Same grammar as the 09:45 early-strict ramp: worse conditions, louder proof required.

**Decision needed:** implement tiered gate / keep the current bench.

---

## Item 6 — RIDER re-board bar: peak vs exit price  🟡 DECIDE (recommend change)

**Problem (operator caught the inconsistency):** the re-board rule blocks re-entry until
price > the previous run's PEAK — but a FRESH entrant (never traded the symbol) faces no
such bar. Same chart, two different decisions based on our private history. 2026-07-17 MU:
at 11:30 ET MU passed all gates at $858, but re-board waited for $881.98 (11:58) while a
fresh RIDER would've bought at 11:30. The market doesn't know we traded MU this morning.

**Options:**
- (a) keep peak bar (current) — over-conservative, cost ~$18/share on MU
- (b) drop the bar entirely — clean symmetry, but allows sell-at-$862/rebuy-at-$858 churn
- (c) **RECOMMENDED: bar = prior EXIT price** ($862.40 → re-board ~11:35 @ ~$863). The one
  honest asymmetry: *never re-buy below your own sale.* Self-scaling (shallow shakeout =
  low bar, deep collapse = cautious bar). One-line change in `rider.go`: record exit price
  instead of peak into `riderExitPeak` at `finalize`.

**Decision needed:** peak (a) / none (b) / **exit-price (c)**.

---

## Investigation ⚪ — signal engine silent on MU

The signal engine published **190 signals** on 2026-07-17 (58 vwap_reclaim, 87 dip_bounce…)
but **ZERO on MU** — one of the day's biggest movers, with a textbook 11:06 VWAP pullback +
resume. Check before deciding coverage gaps: is it legitimate (MU's shape never fit a
detector window — never oversold, only grazed VWAP) or a **seeding/coverage gap after the
534-universe expansion**? If the engine isn't seeing half the universe, that's bigger than
any single item here.

---

## Cross-cutting notes

- **Dip+Rise grace period is confirmed WORKING** (2026-07-17): 0 AI exits under 10 min
  (vs 8 of 9 yesterday); noise floor vetoed ~43 premature AI exits (SNOW, AMZN panic loops
  all blocked → those trades survived/won). No action needed; keep watching the sample grow.
- **Coverage grid after these items:** washout bounces → dip+rise ✓ · fresh breakouts →
  RIDER ✓ · second legs → re-board (Item 6) · velocity-from-anywhere → SURGER (Item 3) ·
  leaders resting at VWAP → SURGER + re-board together.
- All replay/analysis scripts live in the session scratchpad:
  `filter_replay2.py`, `breaker_timeline.py`, `burst_sweep.py`, `trail_compare.py`,
  `rider_replay.py`, `mu_1130.py`.
