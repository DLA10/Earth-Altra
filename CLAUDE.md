# Earth-Altra — Live trading terminal (CLAUDE.md)

Single-user, real-money US-equity trading terminal built for **sub-second intraday
execution**. A Go backend ingests Alpaca's real-time SIP market data, aggregates candles
in memory, and fans them out to a React browser client over a WebSocket. UI name:
**Earth-Altra** (top nav) / **OPTIMUS** (Execution page); repo folder is `Live-Optimus`
(internal identifiers and package paths still say `live-optimus`).

---

## 0. Who uses this & the safety bar (read first)

The user is a **relatively novice trader placing LIVE, real-money orders** through a paid
Alpaca **Algo Trader Plus (SIP)** account. They value speed but above all **safety** (no
accidental trades, no overselling, no surprise fills) and **plain-language clarity** over
jargon.

A real incident shaped this: a *buy* limit set far above market filled instantly (a
marketable limit fills at the current price). So for any order/trade feature:

- Add a **hard guard + a loud, jargon-free warning** at the confirm step.
- Explain the **direction rule** inline: a **buy limit waits only if set BELOW market**; a
  **sell limit only if ABOVE**; OCO **take-profit ABOVE** / **stop-loss BELOW** the current
  price; a **stop-loss waits only if BELOW** market.
- Prefer **blocking egregious fat-fingers** over trusting the user to notice.
- **Never auto-trade.** Every order passes through a mandatory confirm modal.
- When touching the Execution ("Optimus") page, treat its streaming + order path as
  load-bearing: verify changes don't add latency or alter order wiring.

---

## 1. Tech stack

**Backend (Go 1.26)** — low-latency WebSocket fan-out, no meaningful GC pauses. Credentials
live only on the server.
- `github.com/alpacahq/alpaca-trade-api-go/v3` — trading + market-data + streaming SDK
- `github.com/coder/websocket` — browser WebSocket server
- `github.com/go-chi/chi/v5` + `go-chi/cors` — HTTP router/middleware
- `github.com/joho/godotenv` — `.env` loading
- `github.com/shopspring/decimal` — money math (converted to float64 at the JSON boundary)
- `_ "time/tzdata"` — bundles the tz DB so `America/New_York` works on Windows

**Frontend (React 18.3 + TypeScript 5.6 + Vite 5.4)**
- `lightweight-charts` ^4.2.3 — TradingView canvas charts. **v4 has no native panes**, so
  the RSI sub-pane is a second chart synced on the logical range.
- Tabler icons via CDN webfont; no state library — plain React hooks; one resilient
  WebSocket per `useWebSocket()` consumer.

---

## 2. Architecture & data flow

```
Alpaca Trading REST ──┐
Alpaca SIP WebSocket ─┤      ┌──────────────── Go backend (:8080) ─────────────────┐
Alpaca Data REST  ────┘      │  alpaca.Client  → one SIP stream (trades/quotes/bars)│
                             │   candles.Engine (1/5/10m, in-memory, bad-tick guard)│
                             │   scanner.Scanner (DECEPTICON universe metrics)      │
                             │   flow.Tracker  (buy/sell pressure)                  │
                             │   hub.Hub  ── WebSocket fan-out ──► browsers         │
                             │   api.Server (chi) ── REST: orders/account/history…  │
                             └──────────────────────────────────────────────────────┘
                                            ▲                    │ /ws + /api/*
React + TypeScript (:5173, Vite) ───────────┘◄───────────────────┘
   Portal shell → Execution | Watchlist | DECEPTICON | History | Metrics | Paper·Claude | RIDP
```

**Live price path (sub-second):** Alpaca trade tick → `candles.Engine.OnTrade` folds it
into every timeframe's forming candle → `OnUpdate` → `hub.BroadcastCandle` (throttled
~120ms) → subscribed clients → browser `upsert()`. Quotes go to **all** clients via
`BroadcastQuote` (~150ms) and drive watchlists/headers.

**Single SIP connection.** One market-data stream per account (`alpaca/stream.go`). On each
(re)connect it subscribes **trades+quotes** for `tqSymbols = execution ∪ watchlist` (+
runtime-activated symbols) and **bars** for `barSymbols = tq ∪ scan universe`. Runtime
symbols subscribe live without a reconnect.

---

## 3. Repository layout

```
backend/
  cmd/server/main.go        wiring: config, stream loop, pollers, all desks, HTTP server
  cmd/backtest/main.go      replay historical bars through the signal strategies (read-only)
  internal/
    alpaca/                 SDK wrapper + JSON DTOs (client, stream, types, news, screener)
    api/                    chi REST handlers + server-side order validation
    candles/                in-memory OHLCV engine (1/5/10m), bad-tick guard
    config/                 env/.env loading (secrets stay server-side)
    dipwatch/               Telegram dip+bounce alert bot (read-only; feeds quant)
    execsym/                persisted symbol set: base + added − hidden
    flow/                   buy/sell order-flow estimator (quote rule)
    gemini/                 rate/budget-capped Gemini client ("why is it moving" summaries)
    hub/                    WebSocket fan-out, per-client (symbol,timeframe) subscription
    quant/                  AI quant pipelines: signal desk + dip/rise desk (see §13)
    ridp/                   RIDP deterministic paper desk: RIDER + DIPPER + REVERTER, no
                            LLM on the trade path, own journal under data/ridp/. REVERTER
                            (−1.5σ dip below 15-min mean, exit at mean, z=−4 floor, flat
                            15:55) is thin-edge & cost-sensitive; 3 entry knife filters are
                            DESIGNED+BACKTESTED, not yet implemented — observation week in
                            progress, evidence + decision rule in REVERTER_FILTERS.md /
                            RIDP_REVERTER_FIXES.md
    rbt/                    RBT pairs/mean-reversion paper desk (see §14)
    sndk/                   SNDK 1-min micro-scalper paper desk (see §14)
    surger/                 SURGER v2: 3 continuation detectors on the dip+rise account (§14)
    breadcrumbs/            generalized volatility-scalper paper desk (see §14)
    risk/                   deterministic guardrails (loss cap, sizing, concurrency) — paper only
    signals/                multi-strategy intraday signal engine + backtester (paper/shadow)
    scanner/                DECEPTICON per-ticker scan metrics
    watchlist/              parses EVENT_DRIVEN_WATCHLIST.md → departments/tickers
  data/                     runtime state (gitignored): symbol sets, daily_universe.json,
                            decisions/, signals/, reviews/, ridp/, rbt/, sndk/, breadcrumbs/
frontend/src/
  Portal.tsx                app shell + tab router + global SymbolSearch + OrderAlerts
  App.tsx                   Execution ("Optimus") page (ExecutionEngine)
  Watchlist.tsx / Decepticon.tsx / Quant.tsx / Ridp.tsx / Metrics.tsx / TradeHistory.tsx
  indicators.ts             Bollinger + RSI math + signal grading
  costBasis.ts              average-cost reconstruction + realized trades
  marketStatus.ts           client-side US market phase
  order.ts / types.ts / api/client.ts / hooks/ / components/   (Chart, OrderPanel,
                            ChartOrderPopup, ConfirmModal, Header, Positions, Watchlist,
                            LiveChart, MiniChart, ChartModal, NewsPanel, MarketMovers,
                            SymbolSearch, StrategyBadge, OrderAlerts, RangeToggle,
                            LazyMount, ErrorBoundary)
EVENT_DRIVEN_WATCHLIST.md   DECEPTICON universe (39 depts, ~683 tickers incl. full S&P 500)
QUANT_UNIVERSE.json         signal-engine universe (534 names since the 2026-07-16
                            throughput expansion; curated ~160 liquid set preserved in
                            QUANT_UNIVERSE.baseline-2026-07-16.json)
Instruction.md              pre-market universe-selection playbook
QUANT_VISION.md             design + roadmap for the AI agentic quant system
THROUGHPUT_MODE.md          all loosened dials 2026-07-16 + rollback env overrides
scripts/                    PowerShell launchers · START-Live-Optimus.bat  one-click launcher
```

---

## 4. Backend packages (details that matter)

- **`config`** — loads Alpaca keys, `ALPACA_PAPER`, `ALPACA_DATA_FEED`, `SYMBOLS`,
  `MAX_ORDER_NOTIONAL`, CORS, desk keys and flags (full table §9). Live vs paper toggles
  the trading base URL. Secrets never reach the browser.

- **`alpaca`** — SDK wrapper behind float/JSON DTOs. `client.go`: `VerifyKeys` (creds +
  SIP probe), account/positions/orders, `GetAsset`, `SearchAssets` (cached ~10k list),
  `PlaceOrder` (simple/bracket/oco/oto, stops, trailing, GTC, extended hours),
  `Readiness`, cancels, `GetFills`/`GetAllFills`, `StreamTradeUpdates`. `stream.go`:
  `Backfill` (today's 1-min session), `RangeBars` (1W hourly / 1M·6M·1Y daily),
  `GetMultiDailyBars`/`GetMultiIntradayBars` (scanner seed + RBT day-snapshot),
  `StartStream`, runtime `SubscribeTradeQuote`. `news.go` Benzinga headlines;
  `screener.go` market movers.

- **`candles`** — live OHLCV engine. `series.apply()` folds trades into the forming bar
  with a bad-tick guard (drops non-positive prices and wild jumps). Timeframes 1/5/10 min;
  `Seed` from REST backfill; retention 1500 bars/series; `Tracks(sym)`; `OnUpdate` drives
  the hub. `Snapshot` INCLUDES the still-forming bar — scorers that need completed bars
  must cut it (breadcrumbs does).

- **`hub`** — WebSocket fan-out. One active candle subscription per client (symbol,
  timeframe) + optional scan subscription. `SnapshotFn` returns history on subscribe.
  **`EnsureLiveFn`** is called synchronously on subscribe so a client can subscribe to
  **any** symbol — the server backfills + streams it on demand (§7).

- **`scanner`** — per-ticker `State` over the DECEPTICON universe: price, % vs prior
  close/open, opening-range moves, time-of-day RVOL, session VWAP, day high/low, spread,
  catalyst. `SessionBars` feeds mini-charts; `OpeningAnalysis` ranks movers from the open.

- **`api`** — chi handlers + **server-side order validation** (`validateOrder`,
  `checkSellable`), on-demand `EnsureLive`/`activateSymbol`. Endpoints in §10.

- **`quant` + `signals`** — the AI quant team: TWO desks on two paper accounts (§13.9),
  sharing Agent 3 exits, Agent 4 sentiment (optional), Strategist, scoreboard, Reviewer.
  Signal desk: six deterministic detectors over QUANT_UNIVERSE → `SignalTrader` gauntlet
  (§13.2) → shared `Manager` (market entry, trailing-stop floor, Agent 3 exit loop, 15:55
  flatten, `Rehydrate` on restart; positions carry an `EntryContext` so exits know intent
  and P&L attributes per pipeline). Dip/rise desk: Telegram dips → Agent 2 buy/no-buy;
  declined dips arm the deterministic rise watcher (`risewatch.go`, a REGIME tool — live
  only under cautious/corrective posture via `QUANT_RISE_LIVE`). Governance: pre-market
  Strategist → `daily_universe.json`, post-close Reviewer, eval-scoreboard demotion (with
  a 5-outcome probation fast-path), nightly clf retrain, daily research loop
  (human-gated). Every decision → JSONL in `data/decisions/`. Model proposes, Go disposes.

- **`risk`** — deterministic guardrails shared by backtester and paper desks: daily loss
  cap, per-trade sizing, concurrency, overnight cap. Never wired to the real-money path.

- **`dipwatch`** — Telegram dip+bounce alerts over the whole watchlist (oversold,
  below-VWAP pullback ≥ ~0.5×ATR + green 5-min confirm; 15-min cooldown). Read-only
  observer; do NOT disturb — its hook feeds the quant dip pipeline.

`main.go` wires everything: config → verify keys → engine/hub/managers → backfill → seed
scanner + signal engine → SIP stream loop (auto-reconnect, re-backfill) → quant block
(only arms when `PAPER_CLAUDE_*` set; each governance piece independently flag-gated) →
evals refresh → research loop → dip watcher → RIDP/RBT/SNDK/Breadcrumbs desks (each only
with its OWN keys) → account poller (2–3s) → HTTP server.

---

## 5. Frontend pages

**`Portal`** — tabs mount only while selected (DECEPTICON's scan stream isn't running
while you trade): **Execution · Watchlist · DECEPTICON · History · Metrics · Paper ·
Claude · RIDP**, plus global SymbolSearch and portal-wide OrderAlerts fill animations.

**Execution (`App.tsx`)** — the core trading surface. Left Watchlist panel
(drag-to-reorder, persisted) · center Chart + Positions + NewsPanel · right OrderPanel.
`Header`: LIVE/PAPER badge, market-phase badge, feed badge, **Equity** and **Day P/L**
marked live to streaming prices between 3s REST polls (cost basis reconstructed from
fills in `costBasis.ts` to fix Alpaca's blended `avg_entry_price`), buying power,
connection dot, **Cancel-all kill switch** (cancels open orders, not shares). Chart
toolbar: signal badge, indicator toggle, RangeToggle (1m/5m/10m | 1W/1M/6M/1Y).

**Watchlist page** — Opening-movers ranking (+15/30/45/60 min from the open) over stacked
full-size `LiveChart`s (each opens its own WebSocket); drag `⠿` to reorder.

**DECEPTICON** — event-driven sector scanner: per-department summary cards, top movers,
catalyst radar, `MiniChart` heatmap. Click any tile → `ChartModal` (live WS chart, any
symbol incl. market movers). MarketMovers panel shows whole-market gainers/losers.

**Paper · Claude (`Quant.tsx`)** — read-only quant report (polls `/api/quant` +
`/api/evals` 5s): P&L cards, allocator budget vs real equity, team P&L by pipeline, agent
roster (actual configured models from the backend), strategy scoreboard, dip scorecard,
Agent-3 exit attribution, open/closed trades, daily review.

**RIDP (`Ridp.tsx`)** — Rider/Dipper/Reverter desk report; open-position P&L marked live
to the WS quote stream between 3s polls.

**History** — Alpaca fill log (authoritative). **Metrics** — realized-P&L analytics from
fills (`realizedTrades`: average-cost, merges partial fills, resets on flat).

**`Chart.tsx`** — candles + volume; optional Bollinger overlay + time-synced RSI pane;
green "bought here" line; preserves user zoom on live updates; `scrollToRealTime`
intraday, `fitContent` for historical ranges.

---

## 6. Indicators (Bollinger + RSI "Combo")

`indicators.ts`, computed natively from the series shown: Bollinger = SMA(20) ± 2·pop
stdev; RSI = Wilder 14 (both match TradingView). `grade`: **STRONG** (band AND RSI agree),
**WEAK** (one), **WAIT** (neither); BUY at ≤ lower band or RSI ≤ 30, SELL at ≥ upper band
or RSI ≥ 70. **Display/decision aids only — they never place orders.** Toggle persists in
`localStorage` (`lo.indicators`).

---

## 7. Real-time + on-demand streaming model

**WebSocket protocol** (`/ws`, JSON `{type, data}`): client → `{action:"subscribe",
symbol, timeframe}`, `scan_subscribe`/`scan_unsubscribe`. Server → `snapshot`, `candle`,
`quote`, `account`/`positions`/`orders` (3s poll), `trade_update`, `scan`,
`exec_symbols`/`watch_symbols`.

**On-demand activation (additive).** Subscribing to an untracked symbol triggers
`hub.EnsureLiveFn → api.EnsureLive → activateSymbol`: backfill + live SIP subscribe, then
the normal sub-second candle path. Additive only — symbols stay subscribed for the session
(no teardown that could disturb Execution); already-tracked symbols are a no-op.

**Per-component WebSocket.** `useWebSocket` opens a fresh connection per consumer, so
popups and stacked charts subscribe independently of the Execution chart.

---

## 8. Order system & safety

**Order kinds (OrderPanel + chart draw-order):**
- **Market** buy/sell — shares or dollars (notional auto-disabled for non-fractionable
  symbols and extended hours).
- **Conditional** — buy-limit (below market), sell-limit (above), stop-loss (below),
  trailing stop ($ or %). Marketable prices are **blocked** with a direction-rule
  explanation.
- **OCO** — take-profit (above) + stop-loss (below) on a held position; whole shares only.
- **Bracket** — entry (market or resting limit) + TP + SL in one; for a LIMIT-entry
  bracket the TP/SL validate against the **entry** price. Whole shares only.
- **Draw-order (`ChartOrderPopup`)** — click a price on the chart; the popup offers only
  the contextually-valid order types. Same ConfirmModal + server validation as everything.

**Safety guards (defense in depth):**
1. Frontend OrderPanel blocks fat-fingers (direction rules, oversell, fractional-stop, cap).
2. **Mandatory `ConfirmModal`** — explicit "this fills immediately"/"this triggers
   immediately" warnings when a price is on the wrong side of the market.
3. Backend `validateOrder` re-checks everything; `checkSellable` rejects selling more than
   held; `MAX_ORDER_NOTIONAL` caps order value.
4. Orders go over **REST** `POST /api/orders` — never the market-data socket. The kill
   switch cancels all open orders (not positions).

---

## 9. Configuration (`backend/.env`)

| Key | Default | Meaning |
|-----|---------|---------|
| `APCA_API_KEY_ID` / `APCA_API_SECRET_KEY` | — | Alpaca credentials (server-only) |
| `ALPACA_PAPER` | `false` | `true` = paper trading endpoint |
| `ALPACA_DATA_FEED` | `sip` | `sip` (Algo Trader Plus) or `iex` |
| `SYMBOLS` | `SNDK,SPCX,STX,NVDA,MRVL` | Base Execution symbols |
| `MAX_ORDER_NOTIONAL` | `25000` | Per-order USD cap (0 disables) |
| `HTTP_ADDR` / `ALLOWED_ORIGINS` | `:8080` / localhost:5173 | listen addr / CORS allowlist |
| `DECEPTICON_ENABLED` | `true` | Scanner page/stream |
| `GEMINI_API_KEY` / `GEMINI_MODEL` / `GEMINI_RPM` / `GEMINI_DAILY_CAP` | — / flash / 8 / 200 | Movers-news summaries |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | — | Dip-watcher alerts |
| `PAPER_CLAUDE_KEY/SECRET` | — | SIGNAL desk paper account |
| `PAPER_DIP_KEY/SECRET` | — | DIP+RISE desk paper account (empty = family shadow) |
| `PAPER_RIDP_KEY/SECRET` | — | RIDP desk account (empty = OFF; one account per desk) |
| `PAPER_RBT_KEY/SECRET` | — | RBT desk account (empty = OFF) |
| `PAPER_SNDK_KEY/SECRET` | — | SNDK scalper account (empty = benched) |
| `PAPER_BREADCRUMBS_KEY/SECRET` | — | Breadcrumbs desk account (empty = OFF) |
| `ANTHROPIC_API_KEY` | — | Quant agents (idle when empty) |
| `CLAUDE_SYMBOLS` | `SNDK,MU` | Always-streamed quant symbols (+ SPY/QQQ) |
| `QUANT_ENTRY_MODEL` / `QUANT_EXIT_MODEL` / `QUANT_REVIEW_MODEL` | haiku/haiku/opus | Agent models |
| `QUANT_TRAIL_PCT` | `1.5` | Deterministic trailing-stop floor % |
| `QUANT_EXIT_GRACE_MIN` | `10` | Agent 3 not consulted for a position's first N min |
| `QUANT_BREAKEVEN_R` / `QUANT_CHK_HALF_R` / `QUANT_CHK_FULL_R` / `QUANT_EXIT_NOISE_R` | `0.5/0.75/0.5/0.25` | Mechanical exit rails in R units (0 disables each) |
| `QUANT_LIVE` | `true` | `false` = DIP+RISE desk shadow only (does NOT bench the signal desk or RIDP — each has its own flag) |
| `QUANT_OVERNIGHT_CAP` | `0` | Keep ≤1 profitable position overnight up to this (0 = flatten all) |
| `QUANT_UNIVERSE_PATH` | `QUANT_UNIVERSE.json` | Signal-engine universe override |
| `QUANT_SIGNALS_LIVE` | `true` | Signal-engine entries to paper broker (false = shadow) |
| `QUANT_JUDGE_MODEL` | `claude-haiku-4-5` | Signal entry judge |
| `QUANT_DAILY_LOSS_CAP` | `150` | Halt new signal entries at −cap |
| `QUANT_TOD_GATE` | `false` | Time-of-day gate (default shadow-only) |
| `QUANT_RISE_LIVE` | `false` | Rise watcher places paper orders (currently true in .env — corrective-regime slot) |
| `QUANT_ALIGN_GATE` | `true` | Trend-alignment gate (throughput mode blocks only proven-negative cells; `QUANT_ALIGN_STRICT=true` = original playbook; see THROUGHPUT_MODE.md) |
| `RIDP_LIVE` | `true` | RIDP desk places paper orders (false = shadow) |
| `QUANT_CLF_GATE` | `true` | ML entry gate (fail-open without fresh models) |
| `QUANT_CLF_MARGIN` | `0.0` | clf expected-R margin (pre-registered original 0.03) |
| `QUANT_RETRAIN` | `true` | Nightly clf retrain ~17:05 ET + boot catch-up |
| `QUANT_STRATEGIST` / `QUANT_STRATEGIST_MODEL` | `true` / opus | Pre-market posture/budget agent |
| `RESEARCH_LOOP` | `true` | Daily 13:30 ET research proposals → Telegram (never auto-applied) |
| `OLLAMA_ENDPOINT` / `OLLAMA_MODEL` | localhost:11434 / gemma2:2b | Agent 4 sentiment |
| `QUANT_SENTIMENT` | `true` | `false` = never wire Agent 4 |
| `BC_LIVE` | `true` | Breadcrumbs places paper orders (false = shadow) |
| `BC_UNIVERSE` | 22-name volatile basket | Breadcrumbs basket |
| `BC_BUDGET` / `BC_NOTIONAL` / `BC_MAX_SLOTS` | `200000/2000/0` | Budget / slice / slots (0 = one per symbol). .env currently runs notional 5000 |
| `BC_TP_PCT` / `BC_SL_PCT` / `BC_TRAIL_PCT` / `BC_LOCK` | `.0057/.0071/.002/true` | Exit dials (must match model labels) |
| `BC_RETRAIN` | `true` | Monthly rolling retrain + boot catch-up |
| `BC_DAILY_LOSS_CAP` | `500` | Halt NEW breadcrumbs entries at −cap (0 = disabled; .env currently 0 by operator choice — uncapped data collection) |
| `SURGER_LIVE` / `SURGER_NOTIONAL` / `SURGER_SLOTS` | `true/5000/5` | SURGER lab on the DIP account (slice USD / slots per variant) |
| `RBT_Z_ENTRY` | `2.0` | RBT entry stretch σ (original 2.5) |
| `RBT_MAX_CLUSTER` | `12` | RBT family size cap |
| `RBT_COINT_P` / `RBT_MIN_FAMILY` | `0.10` / `2` | RBT family admission (originals 0.05 / 3) |
| `RBT_UNIVERSE_PATH` | baseline JSON | RBT curated-universe file override |

Backfill always loads the full current session day per symbol. Persistence under
gitignored `backend/data/`: `execution_symbols.json`, `watchlist_symbols.json`,
`daily_universe.json`, `decisions/*.jsonl`, `signals/`, `reviews/`, plus per-desk state
dirs. Browser `localStorage`: `lo.execOrder`/`lo.watchOrder`, `lo.indicators`,
`lo.execAutoSort`.

---

## 10. REST API reference (all under `/api`, plus `/ws` and `/healthz`)

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/keycheck` · `/config` · `/readiness` | keys+SIP / public config / trading readiness |
| GET | `/account` · `/positions` · `/orders` | account snapshot / positions / open orders |
| POST | `/orders` · DELETE `/orders`, `/orders/{id}` | place (validated) / cancel all / one |
| GET/POST/DELETE | `/execution/symbols[/{symbol}]` · `/watchlist/symbols[/{symbol}]` | symbol sets |
| GET | `/history?symbol&range` | 1W/1M/6M/1Y bars (split-adjusted, any symbol) |
| GET | `/opening-analysis?scope` | movers ranking at +5/15/30/45/60 min |
| GET | `/asset-names` · `/symbol-meta` · `/assets` · `/assets/search?q` | names/meta/search |
| GET | `/movers?top` · `/movers-news?top` · `/stock-news?symbol` | screener / news badges / headlines+AI summary |
| GET | `/quotes` · `/rvol?symbol` · `/news?symbols` · `/pressure?symbol` | quotes / RVOL / news / buy-sell pressure |
| GET | `/activities?days&limit` · `/fills?days` | fill log / full-window fills |
| GET | `/quant` · `/evals` · `/proposals` | quant report / scoreboard / research proposals |
| GET | `/ridp` · `/rbt` · `/sndk` · `/breadcrumbs` · `/surger` | per-desk reports |
| GET | `/decepticon/watchlist` · `/decepticon/scan` · `/decepticon/bars?symbol` | scanner |

---

## 11. Build, run, verify

```powershell
.\scripts\check-keys.ps1            # 0 = keys valid + SIP; 2 = no SIP; 1 = bad keys
.\scripts\run-backend.ps1           # go run ./cmd/server  → :8080
cd frontend; npm install; ..\scripts\run-frontend.ps1   # vite dev → :5173
# One-click: START-Live-Optimus.bat
# Strategy backtest (read-only; bars cached in data/btcache/, safe to delete):
cd backend; go run ./cmd/backtest -days 21   # (-sweep, -mlgate, -dataset variants exist)
```

Checks before considering a change done — backend (from `backend/`):
`"C:\Program Files\Go\bin\go" build ./... && go vet ./... && go test ./...`; frontend:
`npx tsc --noEmit && npm run build`. Live smoke: subscribe to an Execution symbol, assert
the snapshot symbol matches, no foreign symbol leaks; `curl /api/history` returns sane
counts. History works when the market is closed; live ticks only during trading hours.

---

## 12. Conventions & gotchas

- **Don't break the Execution streaming/order path.** Real money. New features are
  additive and isolated; re-verify Execution after backend stream/hub/api changes.
- **Times are unix seconds** in candle DTOs; ET session helpers + `marketStatus.ts` handle
  the calendar — **holidays are not modeled** (no live quotes on one; blank sidebar prices
  and backfilled charts are expected, not a bug).
- **lightweight-charts is v4** — no native panes; RSI is a synced second chart.
- **Throttles** (candle 120ms, quote 150ms) are deliberate flood control.
- **Money math**: SDK decimals → float64 at the JSON boundary; keep qty-vs-notional and
  fractional rules intact.
- **DECEPTICON universe** comes from `EVENT_DRIVEN_WATCHLIST.md` (parsed, not hardcoded).
- **Order-lifecycle discipline (hard-won, applies to every desk):** confirm a cancel
  before replacing an order (Alpaca cancels are async — the old order still holds the
  shares, so an instant replacement 403s "insufficient qty available"); settle entries to
  a TERMINAL state and book only what actually filled; a resting sell-stop must sit BELOW
  market (if price is already at/below the stop level, flatten instead of re-placing — it
  can only 422); cancel open orders before flattening untracked shares; never sell book
  qty when the account holds zero (403 "not allowed to short" loop).
- This file is documentation **and** agent guidance; keep it accurate when features change.

---

## 13. Live AI quant team — operations & debugging playbook

> **Standing golden rule:** the AI quant desk is **paper only**. Never touch the Execution
> page or the live order path while debugging it. Restart the backend after code changes —
> a long-running binary predates them.

### 13.1 Daily clock (all times **America/New_York**)

| Time (ET) | What fires | Where |
|---|---|---|
| boot | clf gate load+parity · equity sync · `Rehydrate` · Strategist catch-up | `main.go` quant block |
| 08:50–09:25 | **Strategist** writes `daily_universe.json` (posture+budget) | `strategist.go` |
| 09:30–15:30 | signals → gauntlet → paper entries (nothing fresh after 15:30) | `signaltrader.go` |
| every 10 min | **eval scoreboard** recompute → auto-demote/reinstate | `evals.Compute` |
| every 60 s | allocator budget re-synced to real account equity | `qBroker.Account` |
| 13:30–13:40 | **research loop** → Telegram (proposals only) | `research_loop.py` |
| 15:50–16:00 | **RBT** once-daily scan window | `rbt.go` |
| 15:55 | quant Manager **flattens** (one overnight winner ≤ cap may ride) | `manager.go` |
| ≥16:10 | **Reviewer** writes `data/reviews/<day>.json` | `review.go` |
| 17:05–17:20 | **nightly retrains**: clf gate (`train_live.py`) + RBT (`rbt_train.py`) | retrain goroutines |

### 13.2 The signal-trader gauntlet (exact order — every skip is journaled with a reason)

`OnSignal → handle`: **(0)** trend-alignment playbook (`QUANT_ALIGN_GATE`) → **(1)** TOD
gate (only if `QUANT_TOD_GATE=true`; default shadow) → **(2)** scoreboard demotion
(computed over allowed-cell outcomes only; probation fast-path: a benched strategy whose
last 5 counterfactuals are net positive is reinstated immediately) → **(3)** clf gate
(`Clf.Score ≥ QUANT_CLF_MARGIN`) → **(4)** session guard (no entries after 15:30) →
**(5)** posture `stand_down` → **(6)** daily loss cap → **(7)** allocator `CanFund`
(before the LLM call) → **(8)** LLM judge (veto or conviction) → **(9)** cautious posture
requires conviction ≥ 0.60 → `Size/Fund` → `Manager.OpenPosition`. The dip pipeline skips
1–3 and 8 (Agent 2 instead) but shares 4–7 and the Manager.

### 13.3 Fail-open / fail-closed (so "not trading" isn't misdiagnosed)

- **clf gate**: missing / stale (>7d) / parity-failed models → **fails OPEN**. Check the
  startup log + `models/clf_meta.json`.
- **LLM judge**: no `ANTHROPIC_API_KEY` → judge idle, entries proceed at conviction 0.6.
  A judge error mid-call → that one trade skipped (fail-closed).
- **Strategist**: LLM failure → rules fallback (QQQ 20-day-MA posture).
- **Allocator**: equity sync failure → configured budget only. In effect =
  `min(configured, account equity)`.

### 13.4 Where to look (gitignored `backend/data/`)

- `decisions/<day>.jsonl` — every decision/order/skip/outcome; skip notes say WHY a signal
  died; close outcomes carry `{source,pnl,win,conf,held_min}`.
- `signals/<day>.jsonl` — every published signal + counterfactual outcome (`r_multiple`).
- `models/clf_meta.json` — gate models + parity rows. `evals/scoreboard.json` — rolling
  scoreboard. `daily_universe.json` — today's live config. `reviews/<day>.json` — report
  card. `ridp/<day>.jsonl` + `ridp/trades.jsonl` — RIDP journal. `rbt/` — models, history
  CSVs, `signals_today.json` (mtime tells you whether the 15:50 scan ran).
  `breadcrumbs/state.json` — trades with per-trade `prob`, `signal_px`, `entry_slip_bps`,
  `high_px`/`low_px` attribution.

### 13.5 Common failures → diagnosis

1. **"Barely trading."** Usually normal (slots + gauntlet). Check `decisions` skip
   reasons; rule out holiday, `stand_down`, loss cap, after 15:30, clf rejections.
2. **"clf gate not filtering."** Models missing/stale/parity-failed → fail-open by
   design. Retrain: `PYTHONIOENCODING=utf-8 ml/.venv/Scripts/python.exe ml/train_live.py`.
3. **Stale morning config.** `daily_universe.json` date must equal today; boot catch-up
   fires only 08:00–15:00 ET; delete a stale file to force defaults.
4. **Budget looks wrong.** `min(configured, account equity)`; check `/api/quant` `alloc`.
5. **Agents idle.** `ANTHROPIC_API_KEY` empty → fallbacks (§13.3); Agent 4 needs Ollama.
6. **Orphaned positions after restart.** `Rehydrate` re-adopts + re-stops; it SKIPS
   positions whose newest filled buy carries a sibling desk's coid prefix
   (`ridp_`/`rbt_`/`sndk_`/`srg*`) — on a shared account those are not ours (2026-07-13/14
   incident).
7. **Desks interfering.** Every desk runs ONLY on its OWN paper account; empty keys =
   OFF. Never point two desks at one account — they liquidate each other's shares.
8. **RBT zero-trade day.** Distinguish "scan produced 0 signals" (legitimate — check
   `signals_today.json` content) from "scan never ran" (`live_prices.json` mtime ≠ today
   15:50 ET — the backend was down during the once-daily window).

### 13.6 Kill switches (`.env`, then restart)

`QUANT_SIGNALS_LIVE=false` · `QUANT_CLF_GATE=false` · `QUANT_ALIGN_GATE=false` ·
`QUANT_RETRAIN=false` · `QUANT_TOD_GATE=true` · `QUANT_STRATEGIST=false` ·
`RESEARCH_LOOP=false` · `QUANT_LIVE=false` (dip shadow) · `RIDP_LIVE=false` ·
`BC_LIVE=false` · desk keys emptied = desk OFF.

### 13.7 Verify from the shell

`curl localhost:8080/api/quant` · `/api/evals` · `/api/ridp` · `/api/rbt` · `/api/sndk` ·
`/api/breadcrumbs` · `/api/proposals`.

### 13.8 ⚠ Timezone gotcha (has burned a session)

**The operator's local wall clock runs AHEAD of New York (+5h).** Before concluding "the
market is closed" / "X didn't run", convert to ET and check §13.1 — never reason from the
local clock. Holidays are also not modeled (no live ticks on one — expected).

### 13.9 Two-desk reminder

The quant team is **two desks on two paper accounts**, each with its own allocator
(equity-capped), Manager (stops/Agent 3/EOD flatten), Rehydrate, and $150/day loss cap:
**dip+rise desk** (`PAPER_DIP_*`: Agent 2 dips + rise watcher, gated by `QUANT_LIVE` /
`QUANT_RISE_LIVE`) and **signal desk** (`PAPER_CLAUDE_*`: 6-strategy engine → clf gate →
judge, gated by `QUANT_SIGNALS_LIVE`). Shared and stateless across both: Agent 3, Agent 4,
Strategist, scoreboard, Reviewer. Attribute P&L per pipeline via source tags; per desk via
the report's `desks` array (broker-level truth).

---

## 14. Independent paper scalper desks (current state, 2026-07-20)

All paper-only, one Alpaca paper account each, zero contact with the live path.

- **Breadcrumbs** (`internal/breadcrumbs`): 22-name volatile basket, pooled LightGBM
  scalper — 9 scale-free features → prob≥0.65 + Close>EMA100 + ≤2σ-VWAP gates → 0.2%
  trail locked at the +0.57% target, −0.71% hard stop, EOD flat; monthly rolling retrain.
  Hardened 2026-07-20 after a −$1,216 first live day: **completed-bar scoring** (the
  forming bar is cut before scoring — scoring the seconds-old stub fired phantom
  entries), confirmed-cancel stop ratchets, underwater-stop → flatten, terminal-state
  fill settlement, **5-min re-entry cooldown** (the post-stop bounce fades ~minute 5),
  **bench after 2 losing stop-outs/day**, daily loss cap (env; currently 0 = off for
  data collection), per-trade attribution (`prob`, `signal_px`, `entry_slip_bps`,
  MFE/MAE watermarks). Walk-forward Jul 6–20: the dials turn −$2,409 into +$758
  @2bp/side, but the raw edge is regime-compressed — treat as a measurement desk, not an
  earner. A 5-min time exit was tested the same way and REJECTED.
- **RBT** (`internal/rbt`): daily-bar pairs/spread mean reversion. Universe = **199
  liquid names** (legacy 100 ∪ curated baseline; single source `ml/rbt_universe.py`,
  mirrored in `main.go`; deliberately NOT the 534-name throughput file — the desk shorts,
  and 500+ names turn the cointegration screen into noise). Family admission p<0.10 +
  pairs allowed (the old 0.05/min-3 left only 24 tradable names ≈ 2 candidates/day — its
  zero-trade history was starvation, not breakage). Scans ONCE daily 15:50–16:00 ET;
  prices the universe via one REST snapshot (`SetDaySnapFn`) so universe size adds
  nothing to the SIP stream; streams only HELD positions. Entry `|z_spread| ≥ 2.0`, LGBM
  prob ranks the top-5 slots; 1.5×ATR stop; nightly retrain 17:05 (45-min timeout).
- **SNDK** (`internal/sndk`): single-name 1-min scalper (±$8 exits, 5-min time exit,
  qty 2). Hardened 2026-07-20 against phantom exits: exits sell the FULL account qty,
  confirm `PositionQty`==0 before clearing the book, and a per-cycle **orphan sweep**
  flattens untracked shares (canceling resting orders first). The 4-day ~32-share ghost
  pile was cleaned by the sweep on 2026-07-20; equity==cash again.
- **SURGER** (`internal/surger`): 3 intraday continuation detectors (C2 cusum / C1
  purity / SPECTRAL) over the 534-name quant universe, deployed 2026-07-21. The ONE
  deliberate shared-account exception: runs on the DIP+RISE paper account with strict
  `srg1_/srg2_/srg3_` coid attribution (quant Rehydrate skips `srg*`; dip P&L keys off
  `QuantDip__`); enters only symbols the account holds zero of. Completed-bar signals,
  RTH-only feature windows, entries 10:00–15:30 ET (warm-up means nothing fires before
  ~11:30), per-variant trails (C2 1.5→0.5% · C1 2.5→1.0% · SPECTRAL 3.5→2.0% — exit
  study in SURGER_V2.md), EOD flat 15:55, 3 separate books + journal in `data/surger/`.
- **RIDP** (`internal/ridp`): see §3 — REVERTER observation week in progress (unfiltered
  live −$2,209 over 3 sessions; the 3 designed filters replay to −$300; decision after
  the week per REVERTER_FILTERS.md). Known open ops issues, deliberately parked with that
  decision: protective stops sized to requested-not-filled qty (268 UNPROTECTED events on
  07-20) and ghost flattens that don't cancel resting orders first (64 failures 07-20).
