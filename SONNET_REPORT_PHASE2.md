# SONNET_REPORT_PHASE2.md — Phase 2 research report

Executed `SONNET_TASKS.md` Phase 2 (P2.1, P2.2, P2.3) on the operator's explicit
go-ahead after review of `SONNET_REPORT.md`. Every experiment ran through the existing
walk-forward/counterfactual harness; all new mechanisms are behind flags defaulting to
their old behavior (verified — see each section); **nothing was promoted**. This report
is for the operator and the verifier (a separate Fable session) to judge.

---

## P2.1 — Sector lead-lag features (RESEARCH_BACKLOG #9)

**Thesis tested:** does a "peer catch-up gap" feature help the ML gate separate good
signals from bad ones?

**Implementation** (`backend/internal/signals/sectorlag.go`, new file):
- `return15m(bars []Bar) (float64, bool)` — a symbol's % return over the trailing 15
  minutes of a chronological bar slice (binary-search cutoff lookup).
- `sectorLeadLagFeatures(uni, sym, getBars)` — `sector_ret_15m` (mean 15-min return of
  sym's OTHER same-sector universe peers) and `peer_gap_15m` (that mean minus sym's own
  15-min return). Nil if sym has no sector, its own return isn't computable yet (<15 min
  into the session), or no peer has 15 minutes of history either.
- Wired into **both** paths as the task specified:
  - **Backtest**: `BTConfig.SectorLeadLag` (default `false`) + `cmd/backtest -sectorlag`
    flag; merged into `sig.Features` in the event loop right after `strat.Detect`, before
    the cooldown gate (mirrors Task 2's live `ExtraFeatures` ordering).
  - **Live**: `Store.BarsCopy` (new) + `Engine.SectorLeadLag` (new), composed into the
    existing `ExtraFeatures` hook in `main.go` alongside the microstructure columns from
    Task 2.
- Unit tests (`sectorlag_test.go`): `return15m`'s lookback math against hand-computed
  values, and `sectorLeadLagFeatures` against synthetic sector peers (values checked
  against direct `return15m` calls, not hand-derived arithmetic — avoids a subtle
  compounding-return bug I caught myself making on the first draft) plus the no-sector /
  insufficient-history nil paths.

**Off-by-default check:** ran the identical 12-month backtest with `-sectorlag` on;
trade-level output (signals/trades/hit%/P&L per strategy, total P&L $-717.62) is
**byte-identical** to the Task 4 baseline — confirms the flag only adds feature columns,
changes no trading decision.

**Ablation — retrained clf+rank on the 12-month dataset with vs without the new
features** (`backend/data/ml_dataset_12mo.jsonl` vs `backend/data/p21/ml_dataset_12mo_sectorlag.jsonl`,
identical `--holdout 2026-04-01`):

| Model | Metric | Without (Task 4) | With sector lead-lag | Δ |
|---|---|---|---|---|
| clf | FULL WINDOW spread | +0.015 | **+0.028** | better |
| clf | HOLDOUT spread | +0.021 | +0.007 | **worse** |
| rank | FULL WINDOW spread | -0.015 | -0.025 | worse |
| rank | HOLDOUT spread | -0.051 | -0.025 | better (still negative) |

**Verdict:** mixed, not a clean win. clf's full-window selectivity improves but its
holdout (the metric that actually matters — true out-of-sample) gets *worse* with the
new features. rank is negative both ways regardless. **Does not clear the "clearly
helps" bar** — no promotion. Worth revisiting once the dataset is bigger (the backlog's
own framing: "reach for the deep version only once ablation shows alpha"); today it
doesn't unambiguously.

---

## P2.2 — Ensemble agreement filter (RESEARCH_BACKLOG #10, simplified)

**Thesis tested:** does requiring reg+clf+rank to all agree produce a "fewer but
better" filter?

**Implementation** (`backend/internal/signals/backtest.go`):
- `BTConfig.PredictionsReg/Clf/Rank` (three parallel prediction maps) +
  `EnsembleAgreement` (default `false`) + `EnsembleClfMargin` (default 0.03) +
  `EnsembleRankQuantile` (default 0.70).
- Rule: accept only when `clf >= EnsembleClfMargin` AND `rank >= its causal
  per-strategy EnsembleRankQuantile` (prior-days-only, reusing the existing
  `buildTopqThresholds` mechanism) AND `reg >= 0`. Any leg missing a score, or the rank
  threshold not yet established, is warmup pass-through — identical semantics to the
  existing single-model gates, so a cold-start day doesn't silently trade ungated
  forever.
- New `cmd/backtest` flags: `-ensemble`, `-predreg/-predclf/-predrank`,
  `-ensembleclfmargin`, `-ensemblerankq`.
- **Correctness check:** every one of the dataset's 17,511 rows was accounted for
  exactly once across accepted (3083) + rejected (12780) + warmup (1648) = 17511 — no
  row double-counted or dropped by the new gating branch.

**Setup:** trained reg+clf+rank on the **identical** 12-month baseline dataset
(`ml_dataset_12mo.jsonl`, no sector-lag) so all four configs below are on the same data.
clf/rank numbers reproduce Task 4's Step 4 exactly (consistency check passed).

**Selectivity + dollar replay, ensemble vs each single model** (all on the 12-month
window, cached bars):

| Config | Trades | Total P&L | Avg/day | Accepted R | Rejected R | Spread | Passes promotion bar? |
|---|---|---|---|---|---|---|---|
| No gate (baseline) | 2075 | -$717.62 | -$2.92 | — | — | — | n/a |
| rank-only (topq 0.70) | 1567 | -$430.23 | -$1.75 | -0.020 | +0.009 | -0.029 | No |
| **clf-only (margin 0.03)** | 1862 | **+$328.57** | **+$1.34** | +0.009 | -0.006 | **+0.015** | Overall spread positive (Task 4 also showed holdout +0.021 positive) — **best single-model result seen this session** |
| reg-only (margin 0.03) | 1867 | -$604.19 | -$2.47 | +0.008 | -0.003 | +0.011 | Spread positive but $ replay negative — fails the "AND the Go replay confirms in dollars" leg |
| **Ensemble (clf AND rank AND reg)** | 1435 | +$170.70 | +$0.70 | -0.011 | +0.003 | **-0.014** | **No** — negative spread despite positive $ |

**Verdict — the ensemble does NOT pass the promotion bar** (`accepted-R must exceed
rejected-R, overall AND in the holdout, AND the Go replay must confirm in dollars`): its
accepted-vs-rejected spread is negative (-0.014), i.e. the ensemble is *anti-selecting*
even though the resulting P&L happens to be positive. This is exactly the kind of
plausible-looking-but-wrong result the eval framework exists to catch (same pattern as
this repo's "curve-fit killed" and "own validation overturned" receipts) — the positive
dollar figure is explainable by the ensemble simply cutting trade volume hard (1435 vs
2075 baseline), not by picking better trades. **Flagging it explicitly so it isn't
mistaken for a win.**

**Side-finding worth the operator's attention:** the plain **clf gate alone** (margin
0.03, no ensemble) is the standout result of this entire round — positive spread on
both full-window (+0.015) and holdout (+0.021, from Task 4) *and* now dollar-confirmed
positive (+$328.57 total, +$1.34/day) — which is the complete promotion bar. This
wasn't tested end-to-end in Task 4 (only `rank` got a dollar replay there). It is **not
promoted** here (out of this task's scope and Phase 2's "nothing gets promoted"
instruction) but it's the most promising lead from this session and worth the
operator/verifier's attention as a follow-up candidate.

---

## P2.3 — Execution model v2: passive-then-chase (RESEARCH_BACKLOG #5 continuation)

**Thesis tested:** does chasing to market on near-misses recover some of the
missed-winner penalty of pure passive execution?

**Implementation** (`backend/internal/signals/backtest.go`):
- `BTConfig.ChaseExecution` (default `false`): rests a passive limit at the signal price
  for `chaseWindowMin` (3, new pre-registered constant — the existing `passiveWindowMin`
  = 5 used by #5 was left untouched per the guardrail against changing pre-registered
  constants) minutes. Same passive-fill semantics as #5. At expiry, instead of always
  missing: chases to market (fills at the current bar's close + slippage) only if price
  is within `chaseATRMult` (0.1, new pre-registered constant) `* ATR` of the original
  entry; otherwise it's a miss, identical to pure passive.
- New `fillPending` local closure factors the shared "re-check slot/loss-cap/throttle,
  open the position, immediate-stop check" logic used by both the passive-fill and the
  chase-fill branches (the existing #5 code was left as its own untouched, working
  block — no refactor of code outside this task's scope).
- New `cmd/backtest -chase` flag; new `BTResult` fields
  (`ChaseAttempts/ChasePassiveFills/ChaseMarketFills/ChaseMisses`) + a `Report()` line.
- **Correctness check:** rested (16625) = passively filled (2013) + chased to market
  (45) + missed (214) + rejected by risk caps at fill time (14353, the same
  `SkippedRisk` bucket the ungated baseline and #5 both hit heavily) — every resting
  order accounted for exactly once. Confirmed the *same* accounting identity holds for
  the untouched #5 mechanism (16611 = 2054 + 509 + 14048), so this is a pre-existing
  convention, not something new or broken.

**Net expectancy comparison, 12-month window** (all three configs, identical
strategies/risk limits/dates):

| Execution model | Trades | Total P&L | Avg/day | Rested | Filled passively | Chased | Missed |
|---|---|---|---|---|---|---|---|
| Pure market (baseline) | 2075 | -$717.62 | -$2.92 | — | — | — | — |
| Pure passive (#5, 5-min window) | 2054 | **-$637.76** | **-$2.60** | 16611 | 2054 (12%) | n/a | 509 |
| Chase (P2.3: 3-min then chase ≤0.1×ATR) | 2058 | -$802.15 | -$3.27 | 16625 | 2013 | 45 | 214 |

**Verdict:** chase execution is the **worst** of the three on 12 months of data — worse
than even the pure-market baseline, and clearly worse than pure passive. The shorter
3-minute resting window very slightly reduces clean passive fills vs the existing 5-minute
window (2013 vs 2054), and the 45 trades it rescues by chasing to market apparently
skew toward lower-quality setups (near-misses that didn't fill because price ran away
from the entry — chasing into that motion is adverse selection, exactly the risk this
backlog item's own thesis warned about). **No promotion; the operator's existing #5
passive-entry mechanism remains the better of the two execution ideas tested here.**

---

## Summary for the operator

| Idea | Passes its bar? | Recommendation |
|---|---|---|
| P2.1 sector lead-lag | No (mixed: clf full-window better, clf holdout worse, rank still negative) | Don't promote; revisit at a bigger dataset |
| P2.2 ensemble filter | No (negative counterfactual spread despite positive $) | Don't promote |
| P2.2 side-finding: plain clf gate alone | **Yes, on paper** (full+holdout spread positive, dollar-confirmed) | Not promoted (out of scope here) — flagging as the most promising follow-up lead from this session |
| P2.3 chase execution | No (worst of 3 configs tested) | Don't promote; keep existing #5 passive entry |

No strategy, constant, promotion, or default was changed based on any of these results —
every new mechanism defaults to off/unchanged behavior, verified per-section above.

---

## Verification

Backend, from `backend/`:
```
go build ./...   → clean
go vet ./...     → clean
go test ./...    → ok (internal/api, internal/evals, internal/quant, internal/scanner,
                        internal/signals — all pass, including 3 new test files)
```
No frontend changes this round.

## `git log --oneline` (this session's Phase-2 commits, pushed to `origin main`)

```
c2a0396 P2.3: add chase-execution model (research-only, off by default)
d53f220 P2.2: add ensemble agreement filter (research-only, off by default)
61394d6 P2.1: add sector lead-lag features (research-only, off by default)
```

All three are on `origin main` (confirmed via `git push` output after each commit).

---

**Phase 2 complete. Per the rules, the verifier (Fable session) should audit this before
any result is promoted.**
