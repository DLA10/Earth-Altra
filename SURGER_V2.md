# SURGER v2 — continuation detectors (validated + deployed 2026-07-21)

**Status: LIVE (paper)** — three detector variants trade the **dip+rise paper account**
(`PAPER_DIP_*`) with strict `srg1_/srg2_/srg3_` client-order-id attribution.
Backend: `internal/surger` (detect.go = math, surger.go = order lifecycle).
UI: the SURGER section on the Dip+Rise page. Journal: `backend/data/surger/<day>.jsonl`
+ `state.json`. Env: `SURGER_LIVE` (true), `SURGER_NOTIONAL` (5000), `SURGER_SLOTS`
(5 per variant).

## Why v1 was scrapped

SURGER v1 ("+2% in 30 min, above VWAP, volume surge, regardless of open") was replayed
over 11 sessions × 534 names before building: **every dial setting lost money gross** —
including the trend day it was designed around (Jul 17: −$514). Buying *speed* buys
exhaustion; the redesign question became "will this move CONTINUE?".

## The three deployed detectors (exact dials)

Shared quality core ("composite"): `efficiency(30m) ≥ 0.55` ∧ `up-volume share(30m) ≥
0.60` ∧ `vol-normalized drift r30/σ60 ≥ 2.0` ∧ `variance-ratio(5,1|120m) ≥ 1.1` ∧
`volume surge ≥ 1.5×` ∧ `r30 > 0` ∧ `close > session VWAP`.

1. **C2 "cusum"** (`srg1_`, the primary) — composite ∧ a CUSUM drift-break
   (k=0.75, h=8, σ60-normalized) fired within the last 15 minutes. *Clean shape AND it
   just started.*
2. **C1 "purity"** (`srg2_`) — composite ∧ Fourier trend-purity ≥ 2.0 (mean power of
   FFT bins 1–8 vs 32–64 over the last 128 one-minute returns).
3. **SPECTRAL** (`srg3_`) — purity ≥ 3.0 ∧ r30 ≥ 0.4% ∧ up-share ≥ 0.55 ∧ above VWAP
   (no composite — the standalone corroborator).

**Execution (all three):** completed bars only (stream bars arrive after minute close) ·
entries 10:00–15:30 ET · market buy on signal, exchange trailing stop (confirmed-cancel
ratchets) · EOD flat 15:55 · one position/symbol across ALL variants + the whole account
(exclusivity), 30-min re-entry cooldown, 5 slots/variant, $5k slices.

**Per-variant exits (since 2026-07-21, see Exit study below):** C2 trails 1.5% below
peak → 0.5% once peak ≥ +1.5% · C1 trails 2.5% → 1.0% once ≥ +2.5% · SPECTRAL trails
3.5% → 2.0% once ≥ +3.5% (the original RIDER-validated exit).

**Early mode (operator request 2026-07-21, validated same day):** the main windows
(VR-120/purity-128) mean nothing fires before ~11:30 ET. A short-window variant covers
bars 35–119 (≈10:05–11:29): 30-bar composite ∧ sd30-CUSUM break ≤15 bars ∧ VR(5,1|30)
≥ 1.1. Study over the same 97 sessions (E0/E1 configs FAILED — the loose morning
versions lose): E2 passed the pre-registered bar — 38 trades, 63.2% WR, @2bp green in
3/4 windows incl. both true OOS months (+$37 Jun, +$104 May, +$122 Mar–Apr; −$4 on 2
Jul trades). Rare by design (~0.4 fires/day) with a thin recent sample — books under C2
with an `early:` journal tag so it stays separately attributable.

## Validation record (four windows, 97 sessions, $1.5k harness slices)

| Window | Sessions | C2 trades | WR | Gross | @5bp/side |
|---|--:|--:|--:|--:|--:|
| Jul 6–20 (tuning) | 11 | 32 | 71.9% | +$125 | +$79 |
| Jun 10–Jul 3 (rule-OOS) | 16 | 83 | 68.7% | +$848 | +$731 |
| May 1–Jun 9 (unseen) | 27 | 59 (slot-5) | 59.3% | +$264 | +$179 |
| Mar 2–Apr 30 (final, pre-registered) | 43 | 83 (slot-5) | 53.0% | +$227 | +$108 |

Honest expectations: long-run WR ≈ 53–55%, worst backtested day −$122, profits skew
toward storm days; flat weeks are normal and cost nothing. The slot-5 cap *improved*
results. (The validation above ran on the original 3.5%→2.0% exit for all variants.)

## Exit study (2026-07-21) — per-variant trails adopted

Same 97 sessions, 5-slot sim, identical entries; only the exit varied. A = 3.5%→2.0%
@+3.5% (original), B = 1.5%→0.5% @+1.5% (operator proposal), C = 2.5%→1.0% @+2.5%.
Totals @2bp/side: **C2** A $562 / **B $740** / C $569 (B won 3/4 windows incl. the
pre-registered Mar–Apr check, $337 vs $179) · **C1** A $439 / B $513 / **C $561** (C
never worst in any window) · **SPECTRAL** **A $626** / B $421 / C $578 (its slow
grinders need room). Decision: each variant runs the exit its data earned (B/C/A).
Caveats on file: tight trails lost for ALL variants in the choppy May window (regime
sensitivity), and the sim fills stops at the stop price — real fills on a 0.5% trail
run slightly worse, so live C2 results should be read against the B row with that
discount in mind. Full tables: scratchpad `exit_compare_results.json`.

## Shared-account safety (this desk does NOT own its account)

- Dip+rise P&L is reconstructed from `QuantDip__` coids → SURGER is invisible to it.
- Quant `Rehydrate` skips `srg*` coids (foreignDeskPrefixes) → a restart can never adopt
  SURGER's positions (the 2026-07-13 incident class).
- SURGER enters a symbol only when the ACCOUNT holds zero shares of it → it can never
  touch a dip+rise position and no opposite-side resting order can wash-trade it.
- Exits sell exactly SURGER's own quantity (never account-wide).
- No-ghost ledger: every order journaled to state BEFORE placement, settled to a
  terminal fill state after; in-flight orders are settled at boot; partial exits keep
  the remainder tracked and re-protected.
- `account_day_pnl` on the desks panel is shared by definition — use the per-variant
  cards for SURGER truth.

## Runner-ups worth testing later (positive in the study, not deployed)

| Candidate | Evidence | Why parked / what would promote it |
|---|---|---|
| **Meta-label p85** (LGBM take/skip on relaxed-composite candidates, 13 feats incl. market ctx) | May: +$491 gross / +$281 @5bp, 18/27 green — best single unseen window | Red @5bp on July; needs nightly retraining + a forward (not reverse-time) test. Promote if a live-shadow month is cost-positive. |
| **C4** = composite ∧ meta p60 | May: +$409 / +$142 @5bp | Same caveats as meta; simpler threshold. |
| **C10** = strict composite ∧ purity≥2 | Green Jul (+$63 @5bp +$16) & May-window sibling C1 strong | Red on Mar–Apr (−$27 gross); dial sensitivity between C1/C10 unresolved. |
| **C6** = composite ∧ breadth≥0.55 | Jul: +$68/+$11 @5bp | Red on May; breadth threshold likely regime-specific. |
| **C3** = union(composite, cusum-q, spectral) | Best gross in 2 windows (+$151 Jul, +$437 Mar–Apr) | Dies at 5bp (thin per-trade edge); revisit if measured live fills ≤ 2bp/side. |
| **CUSUM fix2 standalone** (h=10 + eff + flow) | Jul: +$30, 64% WR | Marginal at costs; already inside C2. |
| **AE novelty + quality gates** | −$60 over 11d (from −$1,922 raw) | The only DL shape that nearly worked; needs a bigger training set + forward test. |

Definitively rejected (2–3 mechanism fixes each, never green): flow-share standalone,
squeeze-release, VWAP-persistence, cross-sectional leaders, raw t-stat, LGBM continuation
classifier on loose candidates, per-day ranker, 1D-CNN, tiny transformer, raw AE novelty.
Full evidence: 100+ result rows in the session results store (surger_results.json) and
the conversation post-mortems of 2026-07-20/21.

## Bench / promote rules (pre-registered)

- **Bench a variant** if its realized P&L over any rolling 20 live sessions < −$300, or
  its WR over ≥30 trades < 45%.
- **Promote/size-up** only after ≥4 weeks live with measured round-trip fills ≤ 5bp-
  equivalent and positive realized P&L.
- Compare the three variants ONLY on their own `srg*_` books — never on the shared
  account day P&L.
