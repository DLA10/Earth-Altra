# SONNET_REPORT.md — Phase 1 execution report

Executed `SONNET_TASKS.md` Phase 1, tasks 1–6, top to bottom. All 6 tasks complete, all
committed and pushed to `origin main` individually. No `TODO(review)` left — every task
was implementable exactly as specified. Phase 2 was **not** started (permission-gated).

---

## Task 1 — Quant page: eval scoreboard UI (frontend)

**Files changed:**
- `frontend/src/types.ts` — added `StrategyRow`, `JudgeCalib`, `Scoreboard` (mirrors
  `backend/internal/evals/evals.go`'s JSON tags; `strategies`/`demoted_set` typed as
  nullable arrays).
- `frontend/src/api/client.ts` — added `api.evals() => req<Scoreboard>("/api/evals")`.
- `frontend/src/Quant.tsx` — new panel **"Strategy scoreboard (rolling 20d)"** below the
  agents row: table `strategy | signals | outcomes | mean R | traded | status`
  (`DEMOTED (reason)` styled `neg`, else `active` styled `pos`), plus a judge calibration
  line ("collecting data…" while `judge.joined < 10`). Reuses the page's existing 5s
  `setInterval` polling pattern in a second `useEffect`.

**Verification:**
- `npx tsc --noEmit` — clean.
- `npm run build` — clean (`vite build` succeeded, 414.74 kB bundle).
- Shape verified **statically** against `evals.go`/`api.go` source (exact JSON field
  names, the `{"enabled": false}` sentinel before the first computation) rather than by
  starting the live backend — deliberately avoided spinning up the full server
  unattended, since it opens the real SIP stream and can trigger real dip-watcher
  Telegram alerts as a side effect. This is a conscious deviation from the task's "verify
  against a running backend" instruction; flagging it rather than silently skipping.

**Commit:** `42ad244` Add eval scoreboard panel to Quant page

---

## Task 2 — Microstructure features into the live shadow journal (Go)

**Files changed:**
- `backend/internal/signals/engine.go` — added `ExtraFeatures func(sym string)
  map[string]float64` field + `SetExtraFeatures` setter (same mutex pattern as
  `SetOnSignal`). Wired into `detect()`: after a strategy returns a signal and before
  `publish`, the hook's map is merged into `sig.Features` without overwriting existing
  keys; nil-safe.
- `backend/cmd/server/main.go` — wired exactly as specified: `spread_bps` from the
  scanner (`scn.Get(sym)`, omitted if price/spread ≤ 0), `flow_delta_5m` and
  `flow_buy_frac` from `flowTracker.Snapshot(sym)`. Both reads are in-memory, no I/O.
- `backend/internal/signals/strategies_test.go` — added
  `TestEngineExtraFeaturesMergedIntoPublishedSignal`: drives an ORB setup through the
  live `OnBar` path with `SetExtraFeatures` returning a fixed map, asserts the published
  signal's `Features` contains those exact keys/values.

**Verification:**
```
go build ./...   → clean
go vet ./...     → clean
go test ./...    → ok (internal/signals 0.882s, all packages pass)
```

**Commit:** `566c177` Record live-only microstructure features in the signal journal

---

## Task 3 — Reviewer digest upgrade (Go)

**Files changed:**
- `backend/internal/quant/review.go` `buildInput` — now also digests:
  - `signal_judge`: decision count, approved/vetoed count, mean confidence.
  - `signal_trader`: order count + notes.
  - `signal_trader_top_skip_reasons`: top-5 skip reasons by count (grouped by the
    underlying reason after the strategy prefix, not the raw per-strategy note text, so
    reasons cluster across strategies; ties broken alphabetically for determinism).
  - `strategist_posture`: included only when a `strategist` decision record is present
    that day (key omitted otherwise, per "if present").
  - Kept the existing `agent2_entry` digest unchanged.

**Verification:**
```
go build ./...   → clean
go vet ./...     → clean
go test ./...    → ok (internal/quant 4.254s)
```

**Commit:** `a9aa0a7` Widen the daily reviewer's digest to the full agent stack

---

## Task 4 — 12-month dataset via chunked fetch + confirmation runs (Go + runs)

**Files changed:**
- `backend/cmd/backtest/main.go` — added `-chunkdays int` flag (default 0 = unchanged
  single-fetch behavior). When `>0`, `loadBarsChunked` splits the minute-bar window into
  consecutive `chunkdays`-day pieces, fetches each via `GetMultiIntradayBars`, caches each
  chunk independently under the existing `<start>_<end>_<n>syms.gob` naming (chunk bounds
  make names unique — verified no collision with the whole-window cache), merges bars per
  symbol, and sorts each symbol's slice by `Time`. Daily bars are fetched once,
  unaffected. Smoke-tested first with `-days 10 -chunkdays 5`: 4 chunks fetched + cached,
  753,393 bars across 97 symbols; a second run confirmed all 4 chunks loaded from cache.

**Confirmation runs executed** (from `backend/`, 45-day chunks, `-days 252`):

### Step 1 — dataset export (`-dataset data/ml_dataset_12mo.jsonl`)
8 chunks fetched (2025-07-11 → 2026-07-03), 12,919,966 minute bars across 99 symbols,
~4–5.5 min/chunk (rate-limit-bound, exactly the problem `-chunkdays` fixes).

```
════════ BACKTEST — 246 days, 2075 trades ════════
Total P&L: $-717.62   ·   Avg/day: $-2.92   ·   Max drawdown: $2499.65
Skipped by risk caps: 14783   ·   Unfundable (<1 share): 0

strategy          signals  trades     hit%  totalP&L    avgP&L      avgR  avgMin   eod  time
dip_bounce           6193     624    45.4%   -859.47     -1.38     -0.05      93   119     0
fh_reversal          1050      49    57.1%    124.36      2.54      0.10     144    11     0
momentum_cont        2114     195    44.6%   -398.81     -2.05     -0.05     166   111     0
orb_breakout          897     296    44.6%    550.11      1.86      0.02     137    64     0
rel_strength         1123      98    45.9%     -2.11     -0.02      0.04     113    16     0
vwap_reclaim         6134     813    46.0%   -131.69     -0.16     -0.03      91    92     0
```

### Step 2 — TOD gate (`-tod`)
```
════════ BACKTEST — 246 days, 1827 trades ════════
Total P&L: $-960.64   ·   Avg/day: $-3.91   ·   Max drawdown: $2405.99
Skipped by risk caps: 7075   ·   Unfundable (<1 share): 0
Time-of-day filter: 8335 entries blocked

strategy          signals  trades     hit%  totalP&L    avgP&L      avgR  avgMin   eod  time
dip_bounce           6193     302    45.4%   -521.35     -1.73     -0.06      88    69     0
fh_reversal          1050      40    42.5%   -183.22     -4.58     -0.14     141    10     0
momentum_cont        2114     198    44.4%   -172.58     -0.87     -0.04     167   106     0
orb_breakout          897     281    41.6%   -163.66     -0.58     -0.05     147    70     0
rel_strength         1123      92    41.3%   -239.63     -2.60     -0.09     125    24     0
vwap_reclaim         6134     914    47.5%    319.79      0.35     -0.00      94   119     0
```

### Step 3 — TOD + router (`-tod -router`)
```
════════ BACKTEST — 246 days, 1583 trades ════════
Total P&L: $-1426.40   ·   Avg/day: $-5.80   ·   Max drawdown: $2255.49
Skipped by risk caps: 4730   ·   Unfundable (<1 share): 0
Time-of-day filter: 8335 entries blocked
Regime router: 2674 entries blocked

strategy          signals  trades     hit%  totalP&L    avgP&L      avgR  avgMin   eod  time
dip_bounce           6193     170    44.7%   -247.01     -1.45     -0.05      98    43     0
fh_reversal          1050       9    44.4%    -30.00     -3.33     -0.10     144     2     0
momentum_cont        2114     188    41.5%   -474.67     -2.52     -0.07     174    96     0
orb_breakout          897     238    39.5%   -330.88     -1.39     -0.08     147    59     0
rel_strength         1123      33    54.5%    183.08      5.55      0.23     114     6     0
vwap_reclaim         6134     945    45.4%   -526.92     -0.56     -0.04      94   116     0
```

### Step 4 — `ml/train_gate.py --variants clf,rank --halflife 0 --holdout 2026-04-01`
```
dataset: 17511 rows, 2025-07-14 → 2026-07-02, halflife=0.0, strategies: dip_bounce, fh_reversal, momentum_cont, orb_breakout, rel_strength, vwap_reclaim

════════ LightGBM clf — walk-forward, 16543 scored signals ════════

──── FULL WINDOW ────
strategy          scored   acc     accR   rej     rejR   spread
dip_bounce          6027  2697   -0.033  3330   -0.036   +0.003
fh_reversal          900   400   -0.051   500   -0.080   +0.029
momentum_cont       1960  1198   +0.016   762   +0.036   -0.021
orb_breakout         744   422   +0.061   322   +0.058   +0.004
rel_strength         968   407   +0.028   561   -0.010   +0.038
vwap_reclaim        5944  3242   +0.040  2702   +0.027   +0.013
TOTAL              16543  8366   +0.009  8177   -0.006   +0.015
quintiles (pred low→high, mean actual R): -0.013  -0.007  -0.012  +0.025  +0.017

──── HOLDOUT ≥ 2026-04-01 (OOS by construction) ────
strategy          scored   acc     accR   rej     rejR   spread
dip_bounce          1577   698   +0.057   879   -0.005   +0.062
fh_reversal          256   110   -0.058   146   -0.030   -0.027
momentum_cont        606   416   +0.005   190   +0.117   -0.112
orb_breakout         237   123   +0.112   114   +0.111   +0.001
rel_strength         289   129   +0.181   160   +0.097   +0.084
vwap_reclaim        1680   971   +0.115   709   +0.110   +0.005
TOTAL               4645  2447   +0.075  2198   +0.054   +0.021
quintiles (pred low→high, mean actual R): +0.086  +0.016  +0.042  +0.081  +0.103

════════ LightGBM rank — walk-forward, 16543 scored signals ════════

──── FULL WINDOW ────
strategy          scored   acc     accR   rej     rejR   spread
dip_bounce          6027  3014   -0.041  3013   -0.028   -0.013
fh_reversal          900   450   -0.081   450   -0.054   -0.027
momentum_cont       1960   980   +0.043   980   +0.005   +0.037
orb_breakout         744   372   +0.037   372   +0.082   -0.045
rel_strength         968   484   -0.036   484   +0.049   -0.085
vwap_reclaim        5944  2972   +0.025  2972   +0.043   -0.018
TOTAL              16543  8272   -0.006  8271   +0.010   -0.015
quintiles (pred low→high, mean actual R): +0.013  +0.017  +0.008  +0.011  -0.039

──── HOLDOUT ≥ 2026-04-01 (OOS by construction) ────
strategy          scored   acc     accR   rej     rejR   spread
dip_bounce          1577   789   +0.015   788   +0.030   -0.016
fh_reversal          256   128   -0.002   128   -0.082   +0.080
momentum_cont        606   303   +0.066   303   +0.014   +0.052
orb_breakout         237   119   +0.021   118   +0.203   -0.182
rel_strength         289   145   +0.042   144   +0.228   -0.186
vwap_reclaim        1680   840   +0.063   840   +0.162   -0.099
TOTAL               4645  2324   +0.040  2321   +0.091   -0.051
quintiles (pred low→high, mean actual R): +0.073  +0.116  +0.061  +0.042  +0.036
predictions written: backend/data/ml_predictions_clf.jsonl
predictions written: backend/data/ml_predictions_rank.jsonl

Promotion bar: accepted-R must exceed rejected-R (positive spread), overall AND
in the holdout, and the Go replay must confirm in dollars, before any gate
touches order flow.
```

### Step 5 — dollar replay (`-mlpred data/ml_predictions_rank.jsonl -mltopq 0.70`)
```
════════ BACKTEST — 246 days, 1567 trades ════════
Total P&L: $-430.23   ·   Avg/day: $-1.75   ·   Max drawdown: $1876.95
Skipped by risk caps: 4402   ·   Unfundable (<1 share): 0
ML gate: accepted 4584 (cf avg R -0.020)  ·  rejected 11279 (cf avg R +0.009)  ·  warmup 1648
  dip_bounce       accepted 1663 (R -0.070) · rejected 4231 (R -0.018)
  fh_reversal      accepted  208 (R -0.125) · rejected  587 (R -0.066)
  momentum_cont    accepted  561 (R +0.042) · rejected 1295 (R +0.005)
  orb_breakout     accepted  192 (R +0.017) · rejected  451 (R +0.063)
  rel_strength     accepted  304 (R -0.034) · rejected  547 (R -0.005)
  vwap_reclaim     accepted 1656 (R +0.022) · rejected 4168 (R +0.043)

strategy          signals  trades     hit%  totalP&L    avgP&L      avgR  avgMin   eod  time
dip_bounce           6193     354    46.6%   -298.83     -0.84     -0.04      81    40     0
fh_reversal          1050      48    47.9%    -85.92     -1.79     -0.07     155    13     0
momentum_cont        2114     209    49.8%    320.93      1.54      0.05     198   100     0
orb_breakout          897     264    42.0%    252.17      0.96     -0.02     138    57     0
rel_strength         1123     104    51.0%    204.91      1.97      0.12     135    24     0
vwap_reclaim         6134     588    41.5%   -823.49     -1.40     -0.10      87    69     0
```

**Findings for the operator (reported only — nothing changed):**
- The 12-month window is materially worse than the previously-validated 6-month window
  for the base strategy suite (**−$717.62** total vs the 6-month baseline).
- **TOD gate and TOD+router both made 12-month results worse**, not better
  (−$960.64 and −$1426.40 respectively) — the opposite of their effect on the shorter
  windows this mechanism was promoted on. Consistent with "own validation overturned" —
  a longer window changes the regime mix the gate was fit against.
- **ML rank gate at `-mltopq 0.70` REJECTS the promotion bar on 12 months**: accepted-R
  (−0.020) is *below* rejected-R (+0.009) — a negative spread, i.e. the gate would be
  anti-selecting if it touched order flow. It does not meet "accepted-R must exceed
  rejected-R… before any gate touches order flow" and correctly still gates nothing live.
- Per Task 4's explicit instruction: **no strategy, constant, or promotion was changed**
  based on these results. They are handed to the operator for review.

**Verification:** each step's console output captured verbatim above; no code changes
beyond the `-chunkdays` flag itself, which passed the existing bar (`go build`, `go vet`,
`go test`, plus the smoke test described above).

**Commit:** `dfa9539` Add chunked minute-bar fetch to the backtester (-chunkdays)

*(`data/ml_dataset_12mo.jsonl`, `data/ml_predictions_{clf,rank}.jsonl`,
`data/btcache/*.gob`, `data/backtests/*.json` are all under gitignored `backend/data/` —
not committed, per guardrail 7.)*

---

## Task 5 — Ops hardening (Go)

**Files changed:**
- **5a** `backend/internal/signals/store.go` — added `Store.Day() string`.
  `backend/internal/signals/engine.go` — added `sweptDay` field + `sweepOnNewDay()`,
  called from `OnBar` on every bar. On a session-day rollover it sweeps `cool` entries
  older than 24h and `dayCnt` entries not tagged with today's date, under the existing
  `e.mu` lock. Bounds both maps' growth across a long-running process.
- **5b** `backend/internal/evals/evals_test.go` (new) —
  `TestComputeDemotionAndJudgeJoin`: two fabricated `signals/*.jsonl` files (30 negative
  outcomes + 5 signals for `test_strat`, 5 positive outcomes for `healthy_strat` below the
  demotion floor, 1 outcome `joinme` for `judge_strat`) + one `decisions/*.jsonl` file (2
  `signal_trader` orders, 1 `signal_judge` decision referencing `signal_id: "joinme"`).
  Asserts: per-strategy signal/outcome/traded counts, `test_strat` demoted with reason
  `"negative rolling expectancy"` (crosses `demoteMinOutcomes=30` with negative mean R),
  `healthy_strat` NOT demoted (below the floor), and the judge join
  (`Joined=1`, `ApprovedMeanR=0.5` matching the `joinme` outcome).
- **5c** `backend/internal/api/api.go` — `GET /api/proposals` reads the newest
  `data/evals/proposals_*.json` (lexical filename max = latest, since filenames are
  `proposals_<YYYY-MM-DD>.json`) and returns its raw content, or `{"pending": []}` when
  the directory is missing/empty. Nil-safe like `/api/evals`. No frontend work (per spec).

**Verification:**
```
go build ./...   → clean
go vet ./...     → clean
go test ./...    → ok, including new internal/evals test (0.883s) and existing
                    internal/signals suite (0.893s) unaffected by the sweep change
```

**Commit:** `69a37c4` Ops hardening: bound live maps, cover the demotion rule, add /api/proposals

---

## Task 6 — README + docs sync

**Files changed:**
- `README.md` — new **"Agent governance"** section: scoreboard + demotion rule, the
  Strategist (posture/budget with boot catch-up), the human-gated LangGraph research
  loop, and a command block (`go run ./cmd/server`, `curl /api/evals`,
  `curl /api/proposals`, `python ml/research_loop.py`).
- `CLAUDE.md` was **not** edited this session, so the `AGENTS.md` mirror was checked via
  `diff <(sed 's/(CLAUDE.md)/(AGENTS.md)/' CLAUDE.md) AGENTS.md` — already in sync
  (exit 0, no diff). No regeneration needed.

**Commit:** `1bf5ddc` Document agent governance in the README

---

## TODO(review) left

None. Every task was fully specified and implementable as written; no design decisions
were deferred.

## Deviation flagged for review

Task 1's spec said "verify shape against a running backend: `curl localhost:8080/api/evals`."
I verified the response shape **statically** against the `evals.go`/`api.go` source instead
of starting the live server, to avoid an unattended process opening the real Alpaca SIP
stream and potentially firing real Telegram dip-watch alerts as a side effect during a
task that didn't otherwise need the server running. The JSON field names match exactly
(confirmed by reading `Scoreboard`/`StrategyRow`/`JudgeCalib`'s struct tags), and
`tsc --noEmit` + `npm run build` both pass. Flagging this so the operator can spot-check
the live page once, rather than trusting an unverified assumption silently.

---

## Final verification (this report)

Backend, from `backend/`:
```
go build ./...   → clean
go vet ./...     → clean
go test ./...    → ok  (internal/api, internal/evals, internal/quant, internal/scanner,
                         internal/signals — all pass; other packages have no test files)
```

Frontend, from `frontend/`:
```
npx tsc --noEmit → clean
npm run build    → clean (vite build succeeded)
```

## `git log --oneline` (this session's commits, pushed to `origin main`)

```
1bf5ddc Document agent governance in the README
69a37c4 Ops hardening: bound live maps, cover the demotion rule, add /api/proposals
dfa9539 Add chunked minute-bar fetch to the backtester (-chunkdays)
a9aa0a7 Widen the daily reviewer's digest to the full agent stack
566c177 Record live-only microstructure features in the signal journal
42ad244 Add eval scoreboard panel to Quant page
```

All six commits are on `origin main` (confirmed via `git push` output after each commit).

---

**Phase 1 complete. Stopping here per instructions — Phase 2 requires the operator's
explicit permission after reviewing this report.**
