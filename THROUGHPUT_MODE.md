# THROUGHPUT MODE — 2026-07-16

**Why:** in the two weeks before this date the AI quant team, RIDER, DIPPER, and RBT
placed **0–2 trades combined**. Every silent day was "nothing met criteria." On paper
money the scarce resource is DATA, not dollars — a desk that never trades can never be
validated, improved, or trusted with real money. This change loosens **entry** gates
across all desks to generate a measurable trade flow for the final paper month.

**What did NOT change:** every exit rule, trailing stop, hard stop, EOD flatten,
$150/day loss caps, allocator budgets, position slices, and the real-money Execution
page. Wider front door; same fire exits.

**Full rollback:** `git revert` the throughput-mode commit, or apply the per-dial env
overrides below (each restores one dial without a code change), then restart the backend.
The pre-expansion universe is snapshotted at `QUANT_UNIVERSE.baseline-2026-07-16.json`.

---

## 1. QUANT_UNIVERSE.json (signal desk + RIDER/DIPPER/REVERTER universe)

| | Original | Throughput mode |
|---|---|---|
| Symbols | 160 curated (17 sectors) | **534** = original 160 − PSTG/CFLT (delisted, verified untradable on Alpaca) + full S&P 500 (`sp500_*` sectors) + SKHY, SPCX |

**Rollback:** `Copy-Item QUANT_UNIVERSE.baseline-2026-07-16.json QUANT_UNIVERSE.json` and restart.
(If rolling back, note PSTG/CFLT in the baseline are dead tickers — harmless, they just fail backfill.)

## 2. RIDP — RIDER (backend/internal/ridp/ridp.go, env-overridable)

| Dial | Original | Now | Env override (set to original to roll back) |
|---|---|---|---|
| Gain from open | ≥ 1.0% | ≥ 0.7% after 10:00 ET; **the original ≥1.0% applies 09:45–10:00** (early-strict ramp) | `RIDP_RIDER_GAIN_MIN=0.01` |
| Time-of-day RVOL | ≥ 2.0× | ≥ 1.5× after 10:00 ET; **original 2.0× applies 09:45–10:00** | `RIDP_RIDER_RVOL_MIN=2.0` |
| QQQ gate | strictly green (≥ open) | ≥ −0.15% from open | `RIDP_RIDER_QQQ_MIN=0` |
| Entry window start | 10:30 ET (min 60) | **09:45 ET (min 15)** — the 07-17 sector wave ran 09:45–10:00 and RIDER missed it | `RIDP_RIDER_START_MIN=60` |
| Slots | 2 | **uncapped** (budget-only; operator 07-17: no seat limit on paper — the wave had 10 qualifiers for 3 seats) | `RIDP_RIDER_SLOTS=2` |
| Seat allocation | first-scanned wins | **ranked**: candidates sorted by gain×rvol, strongest funded first (07-17: TFC took a seat by scan order, ended the only loser) | code (no dial) |
| Re-entry | one entry per symbol per day | up to **3 entries/day**, re-board only ABOVE the previous run's peak (a new high proves the shakeout was noise) | `RIDP_RIDER_MAX_ENTRIES=1` |

Unchanged: $1,500 slice, 3.5%→2% trail, tighten at +3%, last entry 14:30, flat 15:55.

## 3. RIDP — DIPPER (ridp.go, env-overridable)

| Dial | Original | Now | Env override |
|---|---|---|---|
| Setup: consecutive red closes | ≥ 3 | ≥ 2 | `RIDP_DIPPER_RED_DAYS=3` |
| Setup: 5-session drop | ≤ −6% | ≤ −4% | `RIDP_DIPPER_DROP_5D=-0.06` |
| Turn trigger | close > prior day's high (only) | that, OR close ≥ prior close +1.5% on above-average volume | `RIDP_DIPPER_TURN_PCT=0` (disables the alternate trigger) |

Unchanged: $50 risk sizing, 2×ATR hard stop, 2.5×ATR trail, 3 slots, 40-session max hold.

## 4. RIDP — REVERTER (reverter.go / ridp.go)

| Dial | Original | Now | Env override |
|---|---|---|---|
| Candidate pool | top ⅓ of universe by ATR% (~53 of 160; would be ~178 of 534) | top **55** names by ATR% (fixed count) | `RIDP_REVERTER_TOP_N=55` (raise/lower deliberately) |

Entry/exit dials (−1.5σ in, mean out, −4σ floor, $1,500 slice) unchanged. The fixed
count exists so the universe expansion could not silently 3× REVERTER's concurrency
right after the 2026-07-16 scale incident.

## 5. RBT (rbt.go / ml/rbt_live_signals.py / ml/rbt_train.py)

| Dial | Original | Now | Env override |
|---|---|---|---|
| Entry stretch | ±2.5σ | ±2.0σ | `RBT_Z_ENTRY=2.5` (read by BOTH scorer and trainer) |
| Model probability floor | ≥ 0.65 | ≥ 0.60 | `RBT_PROB_MIN=0.65` |
| Trainer label threshold | hardcoded ±1.5σ (≠ live 2.5σ — the model was scored on a regime it never traded) | same `RBT_Z_ENTRY` as live (2.0) | intentional fix; no rollback recommended |
| Cluster size | unbounded (53-name mega-blob) | ≤ 12 per family (oversized components re-split with a stricter correlation bar) | `RBT_MAX_CLUSTER=1000` ≈ unbounded |

⚠ The trainer changes take effect at the next `ml/rbt_train.py` run — until then the
live scorer uses the old clusters/model with the new 2.0σ bar.

## 6. Signal desk (quant/clfgate.go, signals/alignment.go, .env)

| Dial | Original | Now | Rollback |
|---|---|---|---|
| ML gate margin | reject EV < 0.03 (pre-registered) | reject EV < 0.00 (any positive expectancy passes) | `QUANT_CLF_MARGIN=0.03` |
| Trend-alignment playbook | each strategy ONLY its best cell(s) | block only proven-negative: the −228R toxic cell (mkt-up/sym-down, except orb_breakout) + fh_reversal (retired) | `QUANT_ALIGN_STRICT=true` restores the original table verbatim |
| Rise watcher | shadow (`QUANT_RISE_LIVE=false`) | LIVE on the dip+rise paper account | `QUANT_RISE_LIVE=false` in backend/.env |
| Agent 3 exit grace period | none — LLM consulted from the first tick (2026-07-16 audit: 7 of 9 rise exits LLM-cut within 4 min for being pennies below entry) | Agent 3 not consulted for a position's first **10 minutes**; exchange stop / trailing floor / target / max hold / EOD flatten run from second zero | `QUANT_EXIT_GRACE_MIN=0` |
| Breakeven ratchet (rail D) | none | at **+0.5R** the stop moves to entry, mechanically — a winner can no longer become a loser | `QUANT_BREAKEVEN_R=0` disables; value = R multiple that arms it |
| Grace checkpoints (rail E) | none | one-shot mechanical inspections at grace/2 (exit if down ≥ **0.75R** AND below VWAP) and grace end (≥ **0.5R** AND below VWAP) — both conditions required so a wiggle can't trip it | `QUANT_CHK_HALF_R=0` / `QUANT_CHK_FULL_R=0` disable each |
| Agent 3 noise floor | exit_now always honored (fired at 0.06% red today) | post-grace, an exit_now on a LOSING position is honored only when down ≥ **0.25R**; profit-taking exits always pass; vetoes are journaled | `QUANT_EXIT_NOISE_R=0` |

All four rails measure in units of each trade's OWN planned risk R (entry − original
stop; trailing distance when no fixed stop), so they scale with position sizing
automatically. Exchange-side stops are never removed by any rail — the ratchet only
ever REPLACES a stop with a higher one via the confirm-cancel path.

Unchanged: LLM judge veto, $150/day loss cap, 3 slots, no entries after 15:30,
cautious-posture conviction bar, scoreboard demotion (with probation fast-path).

---

## Review checklist (run weekly; decision date = 4 weeks from 2026-07-16)

Per desk: trades/week, win rate, expectancy (mean R or $/trade), and the scoreboard's
per-strategy read. Tighten (via the env overrides above) whatever the data says is
bleeding; keep what pays. The point of this month is to buy that data.
