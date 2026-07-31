# AI quant desk — log digest (committed summary of the archived journals)

The raw journals are **not** in git: `backend/data/` is gitignored because it holds real
fill data. They live on the operator's machine at `backend/data/_archive/` (~59 MB,
verified copy-then-delete on 2026-07-31). This file is the committed record of what they
contained, so the numbers survive even if the machine doesn't.

## What was archived

| Path (now under `data/_archive/`) | Files | Size | Contents |
|---|---:|---:|---|
| `decisions/` | 24 | 9.8 MB | every agent decision, skip, order and outcome, 2026-06-29 → 07-31 |
| `signals/` | 21 | 2.6 MB | 4,989 published signals + counterfactual bracket outcomes |
| `backtests/` | 69 | 25.4 MB | signal-strategy backtest runs |
| `p21/`, `p22/` | 6 | 18.1 MB | PACKAGES program outputs |
| `models/` | 23 | 1.8 MB | clf gate LightGBM models + `clf_meta.json` parity rows |
| `strategist/` | 14 | 0.9 MB | daily pre-market posture/budget decisions |
| `reviews/` | 16 | 34 KB | daily LLM report cards, 06-29 → 07-24 |
| `evals/` | 1 | 1 KB | rolling strategy scoreboard |
| `sndk/` | 4 | 63 KB | SNDK desk state + last signal |
| `ml_dataset*.jsonl`, `ml_predictions*.jsonl`, `daily_universe.json` | 10 | ~21 MB | ML training sets, gate predictions, last live universe |

`btcache/` (1.9 GB of cached historical bars) was **deleted, not archived** — it is a
rebuildable cache.

## Lifetime activity (from `decisions/`, 24 trading days)

| Agent | Records |
|---|---:|
| Agent 3 (exit) | 6,417 |
| pipeline (dip detections etc.) | 1,307 |
| signal trader | 951 |
| Agent 2 (entry) | 926 |
| rise watcher | 800 |
| clf gate | 303 |
| signal judge | 157 |
| reviewer | 18 |
| strategist | 16 |
| allocator | 12 |

By event: 5,277 decisions · 3,783 journaled skips · 926 dips · 401 rise arms · 363 orders
· 80 outcomes · 77 errors.

## Lifetime P&L (77 graded outcomes)

| Source | Trades | P&L |
|---|---:|---:|
| rehydrated (positions adopted after restarts) | 22 | **−$42.10** |
| rise watcher | 42 | −$12.71 |
| signal desk | 9 | −$13.31 |
| dip (Agent 2's picks) | 4 | +$1.98 |
| **Total** | **77** | **−$66.14** · 24 wins (31%) |

SNDK, separately: **54 trades, −$146.70, 44% WR** (07-16 → 07-31); exits 22 target /
21 catastrophic stop / 6 stop loss / 5 time exit.

## Loose ends found at retirement

**Orphaned unprotected shares on the DIP account** — discovered 2026-07-31 while verifying
the account was clean:

| Symbol | Bought | Stop covered | Orphaned | Since |
|---|---:|---:|---:|---|
| COHR | 2 (07-29) | 1 | **1 share** | 2026-07-29 |
| CRWV | 9 (07-28) | 2 | **7 shares** | 2026-07-28 |

Cause: the quant Manager placed protective stops sized to **fewer shares than actually
filled**; when the stop filled, the remainder was left with no protection and no owner —
the same "stop sized to requested-not-filled qty" class as the RIDP 268-UNPROTECTED
incident. Both sat unprotected for 2–3 days. ≈ $770 of exposure.

**Action required:** `python ml/liquidate.py DIP` at the next open (cancels orders and
closes positions in one call). Until then those shares block SURGER from trading COHR or
CRWV, since SURGER only enters symbols the account holds zero of.

See [`DIP_RISE_ARCHIVE.md`](DIP_RISE_ARCHIVE.md) and [`SNDK_RETIREMENT.md`](SNDK_RETIREMENT.md).
