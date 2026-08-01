# The Dip+Rise desk: archive (2026-06-29 → 2026-07-31)

**Status: REMOVED 2026-07-31.** This document exists because the desk was never written
up anywhere else, `QUANT_EXPLAINED.md` covers the *signal* desk in depth and mentions
dip/rise only in passing. Code is recoverable from git history at `1c1b710~1`
(`backend/internal/quant/{engine,agent2,risewatch,manager,allocator}.go`); raw journals
are archived under `backend/data/_archive/` (gitignored, real fill data).

---

## 1. What it was

A two-stage paper desk on its own Alpaca account (`PAPER_DIP_*`), built on the idea that
**a stock that drops hard and then stops dropping is a tradable bounce**. Stage one asked
an LLM whether a dip was worth buying; stage two was a purely deterministic watcher that
picked up the dips the LLM *declined*, if the market later proved them right.

```
dip watcher (Go, deterministic)            ── the detector, still alive today ──
  oversold + below VWAP by >= ~0.5 ATR + a green 5-min confirm, 15-min cooldown
        │
        ├─► Telegram alert to the operator            (kept: dipwatch survives)
        │
        └─► Agent 2 (Claude Haiku): buy / no_buy      (REMOVED)
                │
       buy ─────┴───── no_buy
        │                │
   allocator             └─► rise watcher (Go, deterministic)  (REMOVED)
   funds a slice              arms the declined dip; enters ONLY if price later
        │                     reclaims the dip high with volume — "the market
        │                     disagreed with Agent 2 and proved it"
        └──────────┬──────────┘
                   ▼
            shared Manager  (REMOVED)
      market entry → trailing-stop floor → mechanical exit rails
      (breakeven ratchet, grace checkpoints, rail-F math) → 15:55 flatten
```

## 2. The pieces, precisely

| Component | File (pre-removal) | What it did |
|---|---|---|
| Dip watcher | `internal/dipwatch/dipwatch.go` | **STILL LIVE.** Detects the dip, sends Telegram. Its `SetHook` callback, the only line that fed the AI, was deleted; alerts fire *before* the hook, so nothing about detection or messaging changed. |
| Agent 2 (entry) | `quant/agent2.go` | One Claude call per confirmed dip: buy or no_buy, with conviction. Model went Sonnet → Haiku on 2026-07-31 for cost. The last LLM left in the whole system when it was removed. |
| Rise watcher | `quant/risewatch.go` | Deterministic. Armed every dip Agent 2 declined and entered only on a confirmed reclaim. Gated by `QUANT_RISE_LIVE` (default false, it spent most of its life in shadow, journaling would-be entries). |
| Allocator | `quant/allocator.go` | Budget = `min(configured, live account equity)`, re-synced every 60s. Per-position slice, concurrency cap. Pure code. |
| Manager | `quant/manager.go` | Shared with the signal desk. Entry, trailing-stop floor, breakeven ratchet (rail D), grace checkpoints (rail E), **rail F** (the deterministic stack that replaced Agent 3 on 2026-07-25), 15:55 flatten, `Rehydrate` on restart. |
| Agent 3 (exit) | `quant/agent3.go` | Removed from the trade path 2026-07-25, deleted 2026-07-31. |

## 3. What the journals say (lifetime, 24 trading days)

From `data/decisions/*.jsonl`, 5,277 decisions, 3,783 journaled skips, 77 graded outcomes:

| Source | Trades | P&L |
|---|---:|---:|
| rehydrated (positions adopted after restarts) | 22 | **−$42.10** |
| rise watcher | 42 | −$12.71 |
| signal desk | 9 | −$13.31 |
| **dip (Agent 2's own picks)** | 4 | **+$1.98** |
| **Total** | **77** | **−$66.14** (24 wins, 31%) |

Agent-call volume: **926 dips** detected and judged by Agent 2, 800 rise-watch records,
6,417 Agent-3 exit consultations, 157 signal-judge calls, 16 strategist runs, 18 reviews.
Alongside it the signal engine journaled **4,989 signals** over 20 days and the reviewer
wrote **16 daily report cards**.

## 4. Why it was retired

1. **It never made money.** −$66 lifetime across every source; the biggest single bucket
   of loss was *rehydrated* positions, i.e. the plumbing around restarts, not the thesis.
2. **Agent 2's edge was unproven at best.** A pre-registered exam on 2026-07-25 (329
   graded alerts, rail-F simulation, FIT/TEST split) found: take-all −$97 on TEST, the
   best knife-style gate −$14, Agent 2's own picks **+$14**, on only 5 graded picks. It
   kept its seat on that thin evidence, then the desk was retired six days later.
3. **The exits that worked were the deterministic ones.** The operator's observation, and
   the reason Agent 3 was replaced by rail-F math rather than tuned.
4. **Cost and complexity.** It was the last consumer of `ANTHROPIC_API_KEY`. With it gone,
   the trading system makes **zero LLM calls**.

## 5. What survived it

- **The dip watcher and its Telegram alerts**: unchanged, hook removed.
- **Rail-F exit math**: the ideas (not-green-by-30m, stale-90m, two-stage tighten,
  $-lock) proved out here and are documented in `CLAUDE.md`; the RIDP and Breadcrumbs
  desks use the same *patterns*.
- **The order-lifecycle discipline** it taught the hard way: settle to a TERMINAL state,
  confirm cancels before replacing, never book a fictional exit, reconcile account vs
  book. Every surviving desk inherits those rules.
- **Its paper account.** SURGER now runs alone on `PAPER_DIP_*` (key names kept
  deliberately, no migration risk to a live book).

## 6. If you ever want it back

`git show 1c1b710~1:backend/internal/quant/engine.go` (and siblings). You would need:
the `quant` package's agent files, `internal/evals`, the `ANTHROPIC_API_KEY` config
plumbing, the `/api/diprise` handler, and `DipRise.tsx`. Before doing any of that, read
§4, and re-run the Agent 2 exam with a bigger journal first.
