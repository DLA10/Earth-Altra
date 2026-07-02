# Earth-Altra / OPTIMUS — AI Agentic Quant Trading System

A production-grade, real-money **intraday trading terminal** (Go + React, sub-second SIP
streaming) that grew an autonomous **AI quant desk** on top: a multi-strategy signal
engine, a deterministic risk officer, an event-driven backtester with walk-forward ML
gating, and an LLM agent team (entry / exit / sentiment / daily review) — all governed by
an **evaluation framework whose job is to kill bad ideas before they touch money**.

> **Honesty first:** the human trades real money through the terminal; **every automated
> and AI-driven component trades a paper account only.** This repo's most important
> results are the ideas its own eval harness *rejected* — receipts below. Nothing here is
> financial advice.

---

## Why this project is interesting

Most trading-bot repos show a backtest that goes up and to the right. This one shows an
**engineering system designed to find out the truth**:

| Eval receipt | What happened |
|---|---|
| 🔪 Curve-fit killed | The best in-sample momentum config (+$596 IS) **died out-of-sample** (−$83). Rejected before deployment by the walk-forward split. |
| 🔪 Linear ML gate killed | A ridge expected-R gate **anti-selected in all 5 strategies** (rejected signals out-performed accepted ones). Caught by a counterfactual selectivity metric, not P&L. |
| 🔪 Own validation overturned | A "+$19/day validated" strategy pair was re-tested on 6 months instead of 3 — the profit was **regime-carried** (Q1 gave it all back). Promotion revoked. |
| ✅ LightGBM promoted (partially) | With 9,014 training signals, a walk-forward LightGBM classifier **passed the signal-ranking bar** (+0.038R accepted vs +0.000R rejected, held-out month positive) — but failed the portfolio-dollar bar, so it still doesn't gate orders. |

Every decision by every layer — strategy detector, ML model, LLM agent — is logged as
structured JSONL with its **counterfactual outcome** (what the trade would have done,
taken or not), so promotion and demotion are earned with evidence, never vibes.

---

## Architecture — three planes + a terminal

```
┌────────────────────────── GO — DATA & EXECUTION PLANE ───────────────────────────────┐
│ single Alpaca SIP WebSocket → in-memory candle engine (1/5/10m, bad-tick guard)      │
│    ├─► WebSocket hub → React terminal (sub-second charts, ~120ms fan-out throttle)   │
│    ├─► DECEPTICON scanner (~470 tickers: RVOL, VWAP, opening-range, volume profiles) │
│    ├─► signal engine  (5 deterministic strategies × 96-symbol curated universe)      │
│    │      └─ every signal journaled: features + bracket plan + counterfactual result │
│    └─► risk officer   (daily loss cap · per-trade risk · concurrency · no overnight) │
│                                                                                      │
│ order paths: HUMAN → validated REST → live account (mandatory confirm modal)         │
│              AI    → paper broker ONLY (market entry + server-side trailing stop)    │
└──────────────────────────────────────────────────────────────────────────────────────┘
┌────────────────────────── PYTHON — RESEARCH & ML PLANE ──────────────────────────────┐
│ backtester exports labeled datasets (9k+ signals) → LightGBM trained walk-forward    │
│ (daily retrain, zero lookahead) → predictions replayed through the Go simulator      │
└──────────────────────────────────────────────────────────────────────────────────────┘
┌────────────────────────── LLM — AGENT PLANE (judgment, never math) ──────────────────┐
│ Entry agent (buy/no-buy on dips) · Exit agent (hold/tighten/exit verbs over a hard   │
│ stop floor) · Sentiment agent (local Ollama) · Daily reviewer (Opus) → structured    │
│ reports + evidence-cited change proposals, human-approved                            │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

**Division of labor (the core design rule):** numbers → models · judgment → LLMs ·
money → deterministic Go. LLM agents are invoked via forced tool calls (structured JSON
out, prompt-cached system playbooks), can only choose *direction* or *veto*, and every
rail (sizing, budget, stops, halts) is enforced in code they cannot override.

## The trading terminal (human, real money)

- **Sub-second live charts** for any US equity — one SIP socket fans out to N browser
  clients; symbols activate on demand (subscribe → backfill → stream in one round trip).
- **Order safety as a product feature**: every order passes a fat-finger validator
  (direction rules — e.g. a buy-limit above market fills instantly and is blocked), a
  mandatory confirm modal with plain-language warnings, server-side re-validation,
  oversell/reserved-shares checks, and a notional cap. Kill switch cancels all orders.
- Market / limit / stop / trailing / OCO / bracket orders, draw-a-price-on-the-chart
  order entry, live positions marked to streaming prices, realized-P&L analytics
  reconstructed from fills (average-cost, partial-fill merging).
- **DECEPTICON scanner**: ~470-ticker event-driven sector scanner (RVOL with learned
  intraday volume curves, opening-range moves, VWAP, catalyst tags) + whole-market
  movers with news badges and rate-budgeted LLM "why is it moving" summaries.

## The quant desk (AI, paper money)

- **5 deterministic long-only intraday strategies** (opening-range breakout, VWAP
  reclaim, momentum continuation, dip-bounce, relative strength) over a curated
  96-symbol universe (semis/memory, datacenter, software, space, quantum — no penny
  stocks). ATR-scaled brackets, signal cooldowns, session-time filters.
- **Event-driven backtester** replays years of SIP 1-minute bars through the *same*
  detector code the live engine runs: slippage, whole-share sizing, loss-cap halts,
  in-sample/out-of-sample sweeps, regime filters, and one-command **ML dataset export**
  (every signal + features + counterfactual outcome).
- **Walk-forward ML gate** (LightGBM in Python, ridge baseline in Go): daily retrain on
  prior days only; judged by counterfactual selectivity (accepted-R vs rejected-R), not
  cherry-picked P&L.
- **Risk officer**: $150/day loss cap → halt, $40 max risk/trade, 3 concurrent
  positions, 15:55 ET flatten (optional single-winner overnight cap), kill switch.
- **Shadow mode**: the live engine journals every signal and its counterfactual result
  during market hours without placing orders — growing the training set daily and
  cross-checking the backtester against reality for free.

## Tech stack

| Layer | Choices |
|---|---|
| Backend | Go 1.26 · chi · coder/websocket · Alpaca SDK v3 (SIP real-time + trading) |
| Frontend | React 18 + TypeScript + Vite · TradingView Lightweight Charts |
| ML | Python 3.11 · LightGBM · walk-forward training/eval · JSONL datasets |
| LLM agents | Anthropic Messages API (forced tool calls, prompt caching) · local Ollama |
| Infra | single-binary backend, disk-persisted state, no external DB/queue needed |

## Run it

```powershell
Copy-Item .env.example backend\.env          # add your Alpaca keys (never committed)
.\scripts\check-keys.ps1                     # verify keys + SIP entitlement
.\scripts\run-backend.ps1                    # Go backend :8080
.\scripts\run-frontend.ps1                   # React UI :5173
```

Backtesting & research (read-only market data; bars cached locally):

```powershell
cd backend
go run ./cmd/backtest -days 63                                   # full strategy suite
go run ./cmd/backtest -days 63 -sweep                            # tune IS, validate OOS
go run ./cmd/backtest -days 126 -dataset data/ml.jsonl           # export ML dataset
ml/.venv/Scripts/python.exe ml/train_gate.py                     # walk-forward LightGBM
go run ./cmd/backtest -days 126 -mlpred data/ml_predictions_clf.jsonl  # replay gated
```

## Roadmap & research

- [`QUANT_VISION.md`](QUANT_VISION.md) — the full architecture, phase gates, and the
  running log of what the eval framework promoted or killed (with numbers).
- [`RESEARCH_BACKLOG.md`](RESEARCH_BACKLOG.md) — the prioritized idea queue (cross-
  sectional ranking, regime mixture-of-experts, lead-lag graph features, execution
  alpha, conformal abstention ensembles, …).

## Safety model

Live credentials exist only in a server-side `.env`; the browser never sees them. The AI
planes hold **paper keys only** — there is no code path from any model or agent to the
live account. Runtime account data (`backend/data/`) is gitignored.

---

*Personal project. Markets are hard; the point is the engineering.*
