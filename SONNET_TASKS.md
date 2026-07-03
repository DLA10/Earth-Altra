# SONNET_TASKS.md — implementation queue for the executor model

> **Who this is for:** a Claude Sonnet session executing well-scoped tasks. The
> architecture and all judgment calls are already made — implement exactly what's
> specified, verify, commit. If a task seems to require a design decision not written
> here, STOP and leave a `// TODO(review)` comment instead of deciding.
>
> **Read first:** `CLAUDE.md` (system map), `QUANT_VISION.md` (architecture + eval
> rules), `RESEARCH_BACKLOG.md` (idea queue).

## Hard guardrails (non-negotiable, apply to every task)

1. **Never touch the live-money path**: `frontend/src/App.tsx` order flow,
   `OrderPanel/ConfirmModal/ChartOrderPopup`, `backend/internal/api` order validation
   (`validateOrder`, `checkSellable`, `placeOrder`), or anything that changes how the
   human's live orders work.
2. **Never touch the dip watcher** (`backend/internal/dipwatch/`) — the operator's
   Telegram alerts must keep working byte-for-byte.
3. All new work is **paper-side only** (signals / quant / evals / ml). The AI must never
   gain a path to the live keys.
4. **Do not change pre-registered constants** (`condMinSamples`, `cusumSlack`,
   `cusumThreshold`, cooldowns, bracket multipliers, gate margins, demotion rules). If an
   experiment needs a different value, add a FLAG, keep the default, and label the run
   exploratory in your report.
5. Verification bar for every commit, from `backend/`:
   `go build ./... && go vet ./... && go test ./...` — and if frontend touched, from
   `frontend/`: `npx tsc --noEmit && npm run build`. All green or don't commit.
6. Commit messages: imperative summary + why + evidence; end with
   `Co-Authored-By: Claude <noreply@anthropic.com>`. Push to `origin main`.
7. Never commit anything under `backend/data/`, any `.env`, or any key material.
8. Style: match surrounding code (this repo deliberately uses `interface{}`, classic
   loops, etc. — do NOT mass-apply modernize lints).
9. Windows environment: Go at `"C:\Program Files\Go\bin\go"`; Python at
   `ml/.venv/Scripts/python.exe`; run Go commands from `backend/`, Python from repo root
   with `PYTHONIOENCODING=utf-8`.

---

## Task 1 — Quant page: eval scoreboard UI  (frontend, ~1h)

`GET /api/evals` already serves (shape in `backend/internal/evals/evals.go`:
`Scoreboard{generated_at, window_days, strategies[], judge{}, demoted_set[]}`).

- Add TS types to `frontend/src/types.ts` mirroring `Scoreboard`/`StrategyRow`/`JudgeCalib`
  (note: `strategies` and `demoted_set` may be `null` — guard like `Quant.tsx` does).
- Add `api.evals()` to `frontend/src/api/client.ts`.
- In `frontend/src/Quant.tsx`, add a panel **"Strategy scoreboard (rolling 20d)"** below
  the agents row: table `strategy | signals | outcomes | mean R | traded | status`, where
  status shows `DEMOTED (reason)` in red (`neg` class) or `active` in green (`pos`).
  Below it a one-line judge card: decisions, approved/vetoed, veto value R, Brier —
  render "collecting data…" while `judge.joined < 10`.
- Poll with the page's existing 5s interval (piggyback the same effect or add one).
- Verify: tsc + build; then `curl localhost:8080/api/evals` shape matches your types.

## Task 2 — Microstructure features into the live shadow journal  (Go, ~1h)  [Backlog #8]

Goal: every published signal's `features` gains live-only microstructure columns so the
future ML gate can train on them. Historical replay cannot have these — that's the point.

- In `backend/internal/signals/engine.go`, add a nil-safe hook:
  `ExtraFeatures func(sym string) map[string]float64` (exported field, set via a
  `SetExtraFeatures` method with the same mutex pattern as `SetOnSignal`).
- In `detect()`, after a strategy returns a signal and BEFORE `publish`, merge the hook's
  map into `sig.Features` (prefix keys exactly as given; do not overwrite existing keys).
- In `backend/cmd/server/main.go`, wire it where the signal engine is built (needs `scn`
  and `flowTracker`, both in scope):
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
  Both reads are in-memory (no I/O) — safe on the bar path.
- Unit test: engine test with `ExtraFeatures` returning a fixed map → published signal's
  Features contains the keys.
- NOTE: scanner covers only DECEPTICON-universe symbols; missing spread is fine (omit
  the key). Do NOT call any network API in the hook.

## Task 3 — Reviewer digest upgrade  (Go, ~30m)

`backend/internal/quant/review.go` `buildInput` currently digests only `agent2_entry`.
Add, from the same day's records: count of `signal_judge` decisions (approved/vetoed +
mean confidence), count + notes of `signal_trader` `order` events, top-5 `signal_trader`
skip reasons, and `strategist` posture if present. Keep the digest compact (it's token
cost). Test: extend none (no test infra for reviewer) — just build/vet.

## Task 4 — 12-month dataset via chunked fetch  (Go + runs, ~2h)  [Backlog #7]

The single 357-day fetch stalls on Alpaca rate limits. Fix `loadBars` in
`backend/cmd/backtest/main.go`:

- Add flag `-chunkdays int` (default 0 = old behavior). When >0: split the calendar
  window into consecutive chunks of that many days; fetch daily bars once (unchanged) and
  minute bars per chunk via `client.GetMultiIntradayBars(symbols, chunkStart, chunkEnd)`;
  cache EACH chunk as its own gob (`data/btcache/<start>_<end>_<n>syms.gob`, existing
  naming) and merge maps (append per symbol, then sort each symbol's bars by Time).
  Log progress per chunk.
- Then run, from `backend/` (each may take minutes; use 45-day chunks):
  ```
  go run ./cmd/backtest -days 252 -chunkdays 45 -dataset data/ml_dataset_12mo.jsonl        # baseline all-6 + dataset
  go run ./cmd/backtest -days 252 -chunkdays 45 -tod                                        # champion at 12 months
  go run ./cmd/backtest -days 252 -chunkdays 45 -tod -router                                # defensive variant
  cd .. && PYTHONIOENCODING=utf-8 ml/.venv/Scripts/python.exe ml/train_gate.py --data backend/data/ml_dataset_12mo.jsonl --outdir backend/data --variants clf,rank --halflife 0 --holdout 2026-04-01
  cd backend && go run ./cmd/backtest -days 252 -chunkdays 45 -mlpred data/ml_predictions_rank.jsonl -mltopq 0.70
  ```
- Report in your summary: the totals/holdout tables exactly as printed + whether the
  rank-gate selectivity (accepted vs rejected cf avg R) is positive at 12 months. DO NOT
  change any strategy or promote anything — findings go to the operator.

## Task 5 — Ops hardening odds & ends  (Go, ~1h)

a. `signals.Engine`: the `cool`/`dayCnt` maps grow forever (keys include the day). Add a
   daily sweep: when the store rolls to a new session day (Store.OnBar day change), clear
   `dayCnt` entries not from today and `cool` entries older than 24h. Keep it simple and
   locked correctly.
b. `evals.Compute` runs fine with missing dirs — add a tiny unit test (tempdir with two
   fabricated journal files + one decisions file; assert demotion rule and judge join).
c. `data/evals/proposals_*.json` + `approved_changes.jsonl`: add a `GET /api/proposals`
   endpoint returning the latest pending proposals file (nil-safe `{"pending": []}`), so
   the UI can show them later. No frontend work yet.

## Task 6 — README + docs sync  (~20m)

- README: add a short "Agent governance" paragraph (scoreboard, demotion, Strategist,
  LangGraph human-gated research loop) and the two new run commands (`research_loop.py`,
  `/api/evals`).
- CLAUDE.md → regenerate AGENTS.md mirror afterwards:
  `sed 's/(CLAUDE.md)/(AGENTS.md)/' CLAUDE.md > AGENTS.md` (from repo root).

## Explicitly OUT of scope for you

- Anything in RESEARCH_BACKLOG Tier 2 not listed above (lead-lag graph, conformal
  ensembles, pairs lab, temporal CNN) — these need design judgment.
- Changing promotion bars, risk limits, strategy parameters, or the universe.
- Auto-applying research-loop proposals.
- Frontend redesigns beyond Task 1's panel.

## Definition of done (report back with)

1. Per task: what changed, verification output (build/vet/test/tsc lines), and any
   `TODO(review)` you left.
2. Task 4's result tables verbatim.
3. `git log --oneline` of your commits, pushed to origin main.
