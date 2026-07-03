# SONNET_TASKS.md — implementation queue for the executor model

> **Who this is for:** a Claude Sonnet session executing well-scoped tasks. The
> architecture and all judgment calls are already made — implement exactly what's
> specified, verify, commit, report. If a task seems to require a design decision not
> written here, STOP and leave a `// TODO(review)` comment instead of deciding.
>
> **Read first:** `CLAUDE.md` (system map), `QUANT_VISION.md` (architecture + eval
> rules), `RESEARCH_BACKLOG.md` (idea queue + status).
>
> **Big picture so you don't redo finished work:** the trading system is COMPLETE and
> live on the paper account — signal engine (6 strategies), learned time-of-day gate,
> eval scoreboard backend (computation + `/api/evals` + demotion enforcement), LLM entry
> judge, allocator, manager with Agent-3 exits and restart rehydration, Strategist
> pre-market agent (with boot catch-up), daily Reviewer, LangGraph research loop
> (`ml/research_loop.py`). **Phase 1 below is visibility, data collection, hardening,
> and a bigger-data confirmation — it does NOT change trading logic.** Phase 2 is
> research and is PERMISSION-GATED: do not start it.

## Hard guardrails (non-negotiable, apply to every task)

1. **Never touch the live-money path**: `frontend/src/App.tsx` order flow,
   `OrderPanel/ConfirmModal/ChartOrderPopup`, `backend/internal/api` order validation
   (`validateOrder`, `checkSellable`, `placeOrder`), or anything that changes how the
   human's live orders work.
2. **Never touch the dip watcher** (`backend/internal/dipwatch/`) — the operator's
   Telegram alerts must keep working byte-for-byte.
3. All work is **paper-side only** (signals / quant / evals / ml). The AI must never
   gain a path to the live keys.
4. **Do not change pre-registered constants** (`condMinSamples`, `cusumSlack`,
   `cusumThreshold`, `cusumDecayN`, cooldowns, bracket multipliers, gate margins,
   demotion rules). Recent code you must NOT "fix": the Strategist's boot catch-up +
   `freshFor`, the CUSUM alarm decay, `signal_id` inside judge snapshots — all intended.
5. Verification bar for every commit, from `backend/`:
   `"C:\Program Files\Go\bin\go" build ./... && go vet ./... && go test ./...` — and if
   frontend touched, from `frontend/`: `npx tsc --noEmit && npm run build`. All green or
   don't commit.
6. Commit messages: imperative summary + why + evidence; end with
   `Co-Authored-By: Claude <noreply@anthropic.com>`. Push to `origin main`.
7. Never commit anything under `backend/data/`, any `.env`, or any key material.
8. Style: match surrounding code (this repo deliberately uses `interface{}`, classic
   loops — do NOT mass-apply modernize lints).
9. Windows environment: run Go from `backend/`; Python is `ml/.venv/Scripts/python.exe`
   run from the repo root with `PYTHONIOENCODING=utf-8`.

---

# PHASE 1 — do these now, in order

## Task 1 — Quant page: eval scoreboard UI  (frontend only)

The backend is DONE (`backend/internal/evals/evals.go`, served at `GET /api/evals`).
This task is only the browser display.

- Add TS types to `frontend/src/types.ts` mirroring `Scoreboard`/`StrategyRow`/`JudgeCalib`
  (JSON field names are in evals.go; `strategies` and `demoted_set` may be `null` —
  guard like `Quant.tsx` guards its nullables).
- Add `api.evals()` to `frontend/src/api/client.ts`.
- In `frontend/src/Quant.tsx`, add a panel **"Strategy scoreboard (rolling 20d)"** below
  the agents row: table `strategy | signals | outcomes | mean R | traded | status`;
  status = `DEMOTED (reason)` styled `neg`, else `active` styled `pos`. Below it one
  judge line: decisions, approved/vetoed, veto value R, Brier — show "collecting data…"
  while `judge.joined < 10`. Reuse the page's existing 5s polling pattern.
- Verify shape against a running backend: `curl localhost:8080/api/evals`.

## Task 2 — Microstructure features into the live shadow journal  (Go)

Goal: every published signal's `features` gains live-only columns for future ML.
Historical bars cannot reconstruct these — that's why we record them live.

- In `backend/internal/signals/engine.go`: add field
  `ExtraFeatures func(sym string) map[string]float64` + a `SetExtraFeatures` setter
  using the same mutex pattern as `SetOnSignal`; in `detect()`, after a strategy returns
  a signal and BEFORE `publish`, merge the hook's map into `sig.Features` (do not
  overwrite existing keys; hook may be nil).
- Wire in `backend/cmd/server/main.go` where `sigEngine` is created (`scn` and
  `flowTracker` are in scope):
  ```go
  sigEngine.SetExtraFeatures(func(sym string) map[string]float64 {
      out := map[string]float64{}
      if scn != nil {
          if st, ok := scn.Get(sym); ok && st.Price > 0 && st.Spread > 0 {
              out["spread_bps"] = st.Spread / st.Price * 10000
          }
      }
      p := flowTracker.Snapshot(sym)
      out["flow_delta_5m"] = p.RollBuyVol - p.RollSellVol
      if tot := p.RollBuyVol + p.RollSellVol; tot > 0 {
          out["flow_buy_frac"] = p.RollBuyVol / tot
      }
      return out
  })
  ```
  Both reads are in-memory — no I/O, no network in the hook. Missing spread → omit key.
- Unit test in `strategies_test.go`: engine with `SetExtraFeatures` returning a fixed
  map → the published signal's Features contains those keys.

## Task 3 — Reviewer digest upgrade  (Go)

`backend/internal/quant/review.go` `buildInput` digests only `agent2_entry`. Add from
the same day's records: `signal_judge` decision count + approved/vetoed + mean
confidence; `signal_trader` order count + notes; top-5 `signal_trader` skip reasons;
`strategist` posture if present. Keep it compact (token cost). Build/vet is enough.

## Task 4 — 12-month dataset via chunked fetch + confirmation runs  (Go + runs)

Purpose: everything was validated on 6 months; this re-confirms at 12 months and grows
the ML dataset. The old single 357-day fetch stalls on Alpaca rate limits — fix by
chunking.

- In `backend/cmd/backtest/main.go` `loadBars`: add flag `-chunkdays int` (default 0 =
  current behavior). When >0: split the minute-bar window into consecutive chunks of
  that many calendar days, fetch each via `client.GetMultiIntradayBars(symbols, cs, ce)`,
  cache EACH chunk as its own gob (existing `<start>_<end>_<n>syms.gob` naming — chunk
  bounds make names unique), merge per symbol, sort each symbol's bars by Time. Daily
  bars: fetch once (unchanged). Log progress per chunk.
- Then run from `backend/` (minutes each; 45-day chunks):
  ```
  go run ./cmd/backtest -days 252 -chunkdays 45 -dataset data/ml_dataset_12mo.jsonl
  go run ./cmd/backtest -days 252 -chunkdays 45 -tod
  go run ./cmd/backtest -days 252 -chunkdays 45 -tod -router
  cd .. && PYTHONIOENCODING=utf-8 ml/.venv/Scripts/python.exe ml/train_gate.py --data backend/data/ml_dataset_12mo.jsonl --outdir backend/data --variants clf,rank --halflife 0 --holdout 2026-04-01
  cd backend && go run ./cmd/backtest -days 252 -chunkdays 45 -mlpred data/ml_predictions_rank.jsonl -mltopq 0.70
  ```
- Record ALL printed result tables verbatim in your report. DO NOT change any strategy,
  constant, or promotion based on the results — findings go to the operator.

## Task 5 — Ops hardening  (Go)

a. `signals.Engine`: `cool`/`dayCnt` maps grow forever. When the store rolls to a new
   session day, sweep `dayCnt` keys not from today and `cool` entries older than 24h
   (correct locking; keep it simple).
b. Unit test for `evals.Compute`: tempdir with two fabricated signal-journal files + one
   decisions file → assert per-strategy counts, the negative-expectancy demotion rule,
   and the judge join via `signal_id`.
c. `GET /api/proposals`: return the newest `data/evals/proposals_*.json` content, or
   `{"pending": []}` when none. Nil-safe like `/api/evals`. No frontend work.

## Task 6 — README + docs sync

- README: short "Agent governance" paragraph (scoreboard + demotion, Strategist,
  LangGraph human-gated research loop) + the `research_loop.py` and `/api/evals`
  commands.
- If you edited CLAUDE.md, regenerate the mirror from repo root:
  `sed 's/(CLAUDE.md)/(AGENTS.md)/' CLAUDE.md > AGENTS.md`.

## Phase-1 completion report — REQUIRED before anything else

Write **`SONNET_REPORT.md`** at the repo root: per task — what changed (files), the
verification output lines (build/vet/test/tsc), any `TODO(review)` left; Task 4's result
tables verbatim; `git log --oneline` of your commits (pushed). **Then STOP.** Phase 2
requires the operator's explicit permission after reviewing your report.

---

# PHASE 2 — research (DO NOT START without operator permission)

Rules: every experiment runs through the existing harness (walk-forward, counterfactual
selectivity accepted-vs-rejected R, dollar replay); exploratory settings go behind flags
with defaults unchanged; nothing is promoted to production — results are reported for
the operator + verifier to judge.

- **P2.1 Sector lead-lag features** (backlog #9, feature stage): in the backtest event
  loop and via the live `ExtraFeatures` hook, compute per-signal `sector_ret_15m`
  (mean 15-min return of same-sector universe peers) and `peer_gap_15m` (that mean minus
  the symbol's own 15-min return). Regenerate the 12-month dataset, retrain clf+rank,
  report selectivity with vs without the new features (ablation).
- **P2.2 Ensemble agreement filter** (backlog #10, simplified): using the walk-forward
  predictions of reg+clf+rank on identical data, accept a signal only when clf EV ≥ 0.03
  AND rank score ≥ its causal per-strategy 0.70 quantile AND reg ≥ 0. Report selectivity
  + `-mlpred`-style dollar replay vs each single model. Label clearly as an agreement
  filter (not formal conformal prediction).
- **P2.3 Execution model v2** (backlog #5 continuation): passive limit for 3 minutes,
  then chase-to-market only if unfilled AND price is within 0.1×ATR of the entry;
  compare net expectancy vs pure-market and pure-passive on 12 months.
(P2.4 scheduling and P2.5 auto-apply were REMOVED by the operator: the research loop now
auto-runs from the backend at 13:30 ET weekdays with Telegram delivery — do not add
schedulers or any auto-apply mechanism; proposals are always applied manually by the
operator.)

After Phase 2 reporting, the verifier (Fable session) audits everything before any
result is promoted.
