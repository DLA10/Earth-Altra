# SNDK desk — retirement note (2026-07-16 → 2026-07-31)

**Status: REMOVED 2026-07-31.** Code recoverable at `1c1b710~1:backend/internal/sndk/`.
Journals archived under `backend/data/_archive/sndk/`.

**Read these first — they are NOT archived, they are live lineage:**
- [`SNDK_VOLATILITY_PIPELINE_STUDY.md`](SNDK_VOLATILITY_PIPELINE_STUDY.md) — the research
  that produced the 9-feature volatility pipeline, and the *why* behind Breadcrumbs' 22-name
  volatile basket.
- [`SNDK_GENERALIZED_IMPLEMENTATION.md`](SNDK_GENERALIZED_IMPLEMENTATION.md) — the spec that
  became `internal/breadcrumbs`. **The SNDK pipeline did not die; it was generalized.**

---

## The strategy, in one page

A single-name 1-minute micro-scalper on **SNDK**, one position at a time, qty **2 shares**.

**Entry** (scan once per new minute, 09:31–15:50 ET, skipping the 11:30–13:30 lunch):
snapshot ≥100 1-min bars → shell out to `ml/sndk_live_signals.py` → LightGBM
(`sndk_lgbm_model.bin`) scores 9 scale-free features — `Z_Score`, `RSI_5`, `RSI_14`,
`ROC_3`, `ROC_10`, `ATR_Ratio`, `MACD_Hist`, `Z_BB`, `Vol_Ratio` — and fires when:

- `prob ≥ 0.65`, **and**
- `Close > EMA_100` (trend filter), **and**
- `|Close − intraday VWAP| ≤ 2.0σ` (the "ultimate armor" — never chase an extended print).

**Exits**, checked every 5s in priority order: **target** `entry + $8` · **stop**
`entry − $8` · **time exit** at 5 minutes held · **EOD** liquidation 15:59 ET · plus an
exchange-side catastrophic `StopSell` resting at the −$8 level from the moment of entry.
All prices come from the 1-minute candle close, so exits are effectively bar-granular.

Everything was **hardcoded** — there were never any `SNDK_*` env vars, only the account
keys. The env-var design in `SNDK_GENERALIZED_IMPLEMENTATION.md` §160-173 was built into
Breadcrumbs as `BC_*` instead.

## The hardening (2026-07-20) — the part worth remembering

SNDK is where the desk-safety patterns were forged, after an incident stranded ~32
untracked shares over 4 days. All of these were later copied into Breadcrumbs and SURGER:

1. **Full-qty exits** — sell `PositionQty()` (account truth), never the book quantity.
   Selling more than held → 403 short-loop; selling less → stranded remainder.
2. **Confirmed cancel before selling** — poll the stop order up to 5s; if it *filled*
   mid-race, record `catastrophic_stop` instead; if unconfirmed, **defer** rather than
   risk a double sell.
3. **Phantom-exit guard** — after the exit fill, re-read `PositionQty`; if anything
   remains, keep the book open, re-protect the remainder, retry next tick.
4. **Re-protect on failed exit** — the stop is already canceled at that point, so a
   failed sell immediately re-places one.
5. **Orphan sweep** — while flat, if the account still holds shares, cancel open orders
   *first* (an orphaned stop holds the shares and 403s the sell), then flatten.
6. **Terminal-state settlement** — never book on a first partial.

## Final ledger

**54 trades, −$146.70, 44% win rate** (2026-07-16 → 07-31). Exits: 22 target, 21
catastrophic stop, 6 stop loss, 5 time exit. Flat at retirement, no open orders.

## Why retired

Not because it was broken — it was clean and well-behaved by the end. It was retired
because it is **a single stock, a fixed ±$8 bracket and 2 shares**: too small to matter,
too narrow to research, and fully superseded by Breadcrumbs, which runs the same pipeline
across 22 names with proper sizing, budget tracking, and a live A/B experiment. HARVEST
(`HARVEST_STUDY.md`) also closed the small-bracket-scalper family: no deployable edge
found in 16 variants.

## Removed with it

`backend/internal/sndk/`, `frontend/src/Sndk.tsx`, `/api/sndk`, `PAPER_SNDK_*` config,
`ml/sndk_live_signals.py`, `ml/train_sndk_production_model.py`, `ml/rolling_retrain.py`
(the weekly SNDK retrainer), and the `Optimus_Rolling_Retrain` Windows scheduled task
(unregistered 2026-07-31 — it would otherwise have fired Saturday 02:00 against a deleted
script). `ml/liquidate.py` keeps its other desks; only the `SNDK` entry was dropped.
