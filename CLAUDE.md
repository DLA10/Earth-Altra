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
   Portal shell → Execution | Movers | Watchlist | DECEPTICON | History | Metrics |
                   RIDP | RBT | SURGER | Breadcrumbs
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
  internal/
    alpaca/                 SDK wrapper + JSON DTOs (client, stream, types, news, screener)
    api/                    chi REST handlers + server-side order validation
    candles/                in-memory OHLCV engine (1/5/10m), bad-tick guard
    config/                 env/.env loading (secrets stay server-side)
    dipwatch/               Telegram dip+bounce alert bot (read-only, log+alert only)
    execsym/                persisted symbol set: base + added − hidden
    flow/                   buy/sell order-flow estimator (quote rule)
    gemini/                 rate/budget-capped Gemini client ("why is it moving" summaries)
    hub/                    WebSocket fan-out, per-client (symbol,timeframe) subscription
    moverwatch/             SHADOW recorder: Movers "Risers" table + green-signal price
                            series at 15-min marks 09:45–16:00 ET (log-only, no orders)
    quant/                  ⚠ NAME IS LEGACY — now ONLY the shared Alpaca paper-broker
                            client (broker.go) used by ridp/rbt/surger/breadcrumbs (§13)
    ridp/                   RIDP deterministic paper desk: RIDER + DIPPER + REVERTER, no
                            LLM on the trade path, own journal under data/ridp/. REVERTER
                            (−1.5σ dip below 15-min mean, exit at mean, z=−4 floor, flat
                            15:55) is thin-edge & cost-sensitive; 3 entry knife filters are
                            DESIGNED+BACKTESTED, not yet implemented — SECOND observation
                            week (2026-07-27→31) running unchanged by operator decision;
                            finale (curfew grid × cascade halt × filters) decides at the
                            2026-08-01/02 weekend. Evidence in REVERTER_FILTERS.md /
                            RIDP_REVERTER_FIXES.md
    rbt/                    RBT pairs/mean-reversion paper desk (see §14)
    surger/                 SURGER v2: 3 continuation detectors, own page + account (§14)
    breadcrumbs/            generalized volatility-scalper paper desk (see §14)
    universe/               loads QUANT_UNIVERSE.json (RIDP/SURGER/REGIME/RBT + SIP bars)
    scanner/                DECEPTICON per-ticker scan metrics
    watchlist/              parses EVENT_DRIVEN_WATCHLIST.md → departments/tickers
  data/                     runtime state (gitignored): symbol sets, ridp/, rbt/,
                            breadcrumbs/, surger/, regime/, moverwatch/, _archive/
frontend/src/
  Portal.tsx                app shell + tab router + global SymbolSearch + OrderAlerts
  App.tsx                   Execution ("Optimus") page (ExecutionEngine)
  Watchlist.tsx / Decepticon.tsx / Surger.tsx / Ridp.tsx / Metrics.tsx / TradeHistory.tsx
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
Instruction.md              pre-market universe-selection playbook      [local only]
QUANT_VISION.md             design + roadmap for the removed AI quant system [local only]
THROUGHPUT_MODE.md          all loosened dials 2026-07-16 + rollback overrides [local only]
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

- **`quant`** — ⚠ legacy name: since 2026-07-31 this package holds ONLY `broker.go`,
  the shared Alpaca paper-broker REST client (orders, account, positions, open orders)
  used by RIDP (+Guardian), RBT, SURGER and Breadcrumbs. Renaming it to `paperbroker`
  is a safe mechanical follow-up; deleting it breaks four live desks.

- **`universe`** — loads `QUANT_UNIVERSE.json` (also legacy-named). Supplies the SIP
  bar-subscription set, RIDP's universe, SURGER's tradables and the regime detector's
  symbol list. `universe_test.go` guards against a malformed/BOM'd file booting empty.

- **`risk`** — deterministic guardrails shared by backtester and paper desks: daily loss
  cap, per-trade sizing, concurrency, overnight cap. Never wired to the real-money path.

- **`dipwatch`** — Telegram dip+bounce alerts over the whole watchlist (oversold,
  below-VWAP pullback ≥ ~0.5×ATR + green 5-min confirm; 15-min cooldown). Read-only
  observer; do NOT disturb — its hook feeds the quant dip pipeline.

`main.go` wires everything: config → verify keys → engine/hub/managers → backfill → seed
scanner + signal engine → SIP stream loop (auto-reconnect, re-backfill) → quant block
→ dip watcher (Telegram alerts) → RIDP/RBT/Breadcrumbs/SURGER desks (each only with its
OWN keys) → account poller (2–3s) → HTTP server.

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

**SURGER (`Surger.tsx`)** — the 3-detector continuation lab (C2 cusum / C1 purity /
SPECTRAL): per-variant P&L + win rate cards, open positions with stop/peak/entry-slip,
and the last 25 closed trades with exit reasons. Polls `/api/surger` every 3s. Promoted
from a panel inside the retired Dip+Rise page on 2026-07-31.

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
| `PAPER_RIDP_KEY/SECRET` | — | RIDP desk account (empty = OFF; one account per desk) |
| `PAPER_DIP_KEY/SECRET` | — | **SURGER's account** (legacy name — the dip desk it belonged to was removed 2026-07-31) |
| `PAPER_RBT_KEY/SECRET` | — | RBT desk account (empty = OFF) |
| `PAPER_BREADCRUMBS_KEY/SECRET` | — | Breadcrumbs desk account (empty = OFF) |
| `QUANT_UNIVERSE_PATH` | `QUANT_UNIVERSE.json` | Universe file override (legacy name; feeds RIDP/SURGER/REGIME/RBT + SIP bars) |
| `RIDP_LIVE` | `true` | RIDP desk places paper orders (false = shadow) |
| `BC_LIVE` | `true` | Breadcrumbs places paper orders (false = shadow) |
| `BC_UNIVERSE` | 22-name volatile basket | Breadcrumbs basket |
| `BC_BUDGET` / `BC_NOTIONAL` / `BC_MAX_SLOTS` | `200000/2000/0` | Budget / slice / slots (0 = one per symbol). .env currently runs notional 5000 |
| `BC_TP_PCT` / `BC_SL_PCT` / `BC_TRAIL_PCT` / `BC_LOCK` | `.0057/.0071/.002/true` | Exit dials (must match model labels) |
| `BC_RETRAIN` / `BC_RETRAIN_DAYS` | `true` / `7` | Rolling retrain + boot catch-up (weekly since 2026-07-25; was monthly) |
| `BC_CUT_USD` | `0` (env: 25) | Flat $-cut, ARMED live 2026-07-25 with a built-in control arm: every cut trade continues as a ghost under the desk's own rules (cutshadow_<day>.jsonl; cut_stats in /api/breadcrumbs) — review 2026-08-21 compares cut vs uncut per trade |
| `BC_DAILY_LOSS_CAP` | `500` | Halt NEW breadcrumbs entries at −cap (0 = disabled; .env currently 0 by operator choice — uncapped data collection) |
| `SURGER_LIVE` / `SURGER_NOTIONAL` / `SURGER_SLOTS` | `true/5000/5` | SURGER lab on the DIP account (slice USD / slots per variant) |
| `RBT_Z_ENTRY` | `2.0` | RBT entry stretch σ (original 2.5) |
| `RBT_MAX_SLOTS` | `10` | RBT concurrent positions AND position sizer (was a hardcoded 5) |
| `RBT_PROB_MIN` | `0.60` | RBT model probability floor (original 0.65) |
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
| GET | `/ridp` · `/rbt` · `/breadcrumbs` · `/surger` · `/regime` · `/moverwatch` | per-desk / shadow reports |
| GET | `/decepticon/watchlist` · `/decepticon/scan` · `/decepticon/bars?symbol` | scanner |

---

## 11. Build, run, verify

```powershell
.\scripts\check-keys.ps1            # 0 = keys valid + SIP; 2 = no SIP; 1 = bad keys
.\scripts\run-backend.ps1           # go run ./cmd/server  → :8080
cd frontend; npm install; ..\scripts\run-frontend.ps1   # vite dev → :5173
# One-click: START-Live-Optimus.bat
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

## 13. Paper-desk operations & debugging playbook

> **Standing golden rule:** every desk below is **paper only**. Never touch the Execution
> page or the live order path while debugging one. Restart the backend after code changes —
> a long-running binary predates them.

**The AI quant team was REMOVED on 2026-07-31** (signal desk, dip+rise desk, all LLM
agents, ML gate, strategist, reviewer, research loop, evals scoreboard) along with the
SNDK desk. The system now makes **zero LLM calls**. History and rationale:
[DIP_RISE_ARCHIVE.md](DIP_RISE_ARCHIVE.md) · [SNDK_RETIREMENT.md](SNDK_RETIREMENT.md) ·
[AI_QUANT_LOG_DIGEST.md](AI_QUANT_LOG_DIGEST.md). Code is recoverable from git at
`1c1b710~1`. `QUANT_EXPLAINED.md`, `QUANT_VISION.md` and `STRATEGY_ENGINE.md` describe the
removed system and are kept as historical documents only. NOTE: since 2026-08-01 those
three, plus `AGENTS.md`, `Instruction.md`, `RESEARCH_BACKLOG.md` and `THROUGHPUT_MODE.md`,
are **local-only** (gitignored, still on the operator's disk). A fresh clone will not have
them, so do not link to them from repo-facing docs.

Two names survive the removal and are **load-bearing — do not "clean them up"**:
- **`internal/quant`** now contains ONLY `broker.go`, the shared Alpaca paper-broker
  client that RIDP (+Guardian), RBT, SURGER and Breadcrumbs all run on.
- **`QUANT_UNIVERSE.json`** + `cfg.QuantUniverseCandidates` feed RIDP, SURGER, the regime
  detector, RBT's baseline and the SIP bar subscription (`internal/universe`).

### 13.1 Daily clock (all times **America/New_York**)

| Time (ET) | What fires | Where |
|---|---|---|
| boot | per-desk rehydrate/reconcile; Breadcrumbs retrain catch-up if stale | `main.go` desk blocks |
| 09:30–15:30 | RIDP (reverter/rider/dipper), Breadcrumbs, SURGER entries | per desk |
| 09:45–16:00 | moverwatch journals the Risers table every 15 min | `moverwatch.go` |
| 11:31 | regime detector publishes its afternoon call | `regime.go` |
| 15:50–16:00 | **RBT** once-daily scan window | `rbt.go` |
| 15:55 | SURGER + RIDP EOD flatten | per desk |
| 15:59 | Breadcrumbs EOD flatten | `breadcrumbs.go` |
| 16:05 | regime detector scores the day | `regime.go` |
| 17:05 | Breadcrumbs weekly rolling retrain (when stale) · RBT nightly retrain | retrain goroutines |

### 13.2 Fail-open / fail-closed (so "not trading" isn't misdiagnosed)

- **Breadcrumbs ML model** missing/stale → retrains on boot; until then it scores with the
  old model. Check `model_trained` in `/api/breadcrumbs`.
- **RBT** scans once daily: distinguish "scan produced 0 signals" (legitimate — read
  `data/rbt/signals_today.json`) from "scan never ran" (its mtime isn't today ~15:50 ET).
- **SURGER** fires ~0.4×/day by design; flat days are normal and cost nothing.

### 13.3 Where to look (gitignored `backend/data/`)

`ridp/<day>.jsonl` + `ridp/trades.jsonl` + `ridp/guardian_<day>.jsonl` (shadow
counterfactuals) · `rbt/` (models, history CSVs, `signals_today.json`) ·
`breadcrumbs/state.json` (per-trade `prob`, `signal_px`, `entry_slip_bps`, MFE/MAE) +
`cutshadow_<day>.jsonl` (the $25-cut control arm) · `surger/` (3 books + journal with
`exit_slip_bps`) · `regime/<day>.jsonl` · `moverwatch/<day>.jsonl` ·
`_archive/` (the retired AI desks' journals).

### 13.4 Common failures → diagnosis

1. **A desk is quiet.** Usually normal (slots, gates, windows). Check its journal for
   skip reasons; rule out a holiday and the desk's own entry window.
2. **Orphaned positions after restart.** Each desk rehydrates independently: RIDP
   `rehydrate()` + ghost reconcile, RBT `adoptUntracked`, Breadcrumbs boot reconcile,
   SURGER in-flight settle. Read the boot log for each.
3. **Desks interfering.** Every desk runs on its OWN paper account; empty keys = OFF.
   The ONE exception is SURGER on `PAPER_DIP_*` (key names kept after the dip desk was
   retired) — it now has that account to itself.
4. **Cross-desk adoption guard.** The old `foreignDeskPrefixes` list died with the quant
   Manager. It is no longer needed (each surviving desk owns its account outright), but
   SURGER — sole occupant of `PAPER_DIP_*` — protects itself by entering only symbols the
   account holds **zero** of. ⚠ SURGER has **no tests**; add some before changing it.

### 13.5 Kill switches (`.env`, then restart)

`RIDP_LIVE=false` · `BC_LIVE=false` · `SURGER_LIVE=false` · any desk's keys emptied =
desk OFF.

### 13.6 Verify from the shell

`curl localhost:8080/api/ridp` · `/api/rbt` · `/api/breadcrumbs` · `/api/surger` ·
`/api/regime` · `/api/moverwatch` · `/api/readiness`.

### 13.7 ⚠ Timezone gotcha (has burned a session)

**The operator's local wall clock runs AHEAD of New York (+5h).** Before concluding "the
market is closed" / "X didn't run", convert to ET and check §13.1 — never reason from the
local clock. Holidays are also not modeled (no live ticks on one — expected).

---

## 14. Independent paper desks (current state, 2026-07-31)

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
  earner. A 5-min time exit was tested the same way and REJECTED. A 16-idea small-bracket
  reversion program (HARVEST study 2026-07-21) found NO deployable edge in this family —
  62% WR on July dev collapsed to 46–50% on Jun/May walk-backs; see HARVEST_STUDY.md
  before proposing new small-profit scalpers.
- **RBT** (`internal/rbt`): daily-bar pairs/spread mean reversion. Universe = **199
  liquid names** (legacy 100 ∪ curated baseline; single source `ml/rbt_universe.py`,
  mirrored in `main.go`; deliberately NOT the 534-name throughput file — the desk shorts,
  and 500+ names turn the cointegration screen into noise). Family admission p<0.10 +
  pairs allowed (the old 0.05/min-3 left only 24 tradable names ≈ 2 candidates/day — its
  zero-trade history was starvation, not breakage). Scans ONCE daily 15:50–16:00 ET;
  prices the universe via one REST snapshot (`SetDaySnapFn`) so universe size adds
  nothing to the SIP stream; streams only HELD positions. Entry `|z_spread| ≥ 2.0`, LGBM
  prob ranks candidates and the free slots are filled best-first; 1.5×ATR stop; nightly
  retrain 17:05 (45-min timeout). **Slots 5 → 10 on 2026-08-02** (`RBT_MAX_SLOTS`): with a
  5-session hold, 5 slots freed ~1 seat/day, so the desk only ever took its TOP-RANKED
  signal out of a median 10 candidates. A 5-year replay of its own pipeline (8,837
  signals, quarterly walk-forward) found ranks 1–7 all profitable and rank 8 sharply
  negative — the seats were the binding constraint, not the picking. `maxSlots` is also
  the position sizer (`equity/maxSlots`), so this halves position size rather than adding
  exposure; above ~30 slots `math.Floor(budget/price)` starts silently dropping expensive
  names. Positions and trades now record `rank`/`of_n` so the deeper picks can be graded.
- **SURGER** (`internal/surger`): 3 intraday continuation detectors (C2 cusum / C1
  purity / SPECTRAL) over the 534-name universe, deployed 2026-07-21. Has its own page
  since 2026-07-31 and now owns the `PAPER_DIP_*` account outright (the dip desk it used
  to share it with was removed); still uses strict
  `srg1_/srg2_/srg3_` coid attribution (quant Rehydrate skips `srg*`; dip P&L keys off
  `QuantDip__`); enters only symbols the account holds zero of. Completed-bar signals,
  RTH-only feature windows, entries 10:00–15:30 ET (main detectors fire from ~11:30;
  a validated short-window "early mode" covers ~10:05–11:29 under C2's book, journal
  tag `early:`), per-variant trails (C2 1.5→0.5% · C1 2.5→1.0% · SPECTRAL 3.5→2.0% —
  exit study in SURGER_V2.md), EOD flat 15:55, 3 books + journal in `data/surger/`.
  Exit-slip journaling (planned stop level vs actual fill, bps; eod excluded) live since
  2026-07-27 feeding the ~08-18 promote review — first datum RSG trail_stop −3.2bps.
- **RIDP** (`internal/ridp`): shadow **Guardian** since 2026-07-21 — log-only P&L
  overseer (desk-stop/ratchet/lock/cascade/bench counterfactuals →
  `data/ridp/guardian_<day>.jsonl`, cannot trade by construction) feeding the Friday
  filter decision. See §3 — REVERTER second observation week in progress (week-1
  unfiltered −$5,024/6 sessions; only green bucket 09:30–10:00 lifetime +$265; the 3
  designed filters replay to −$300; finale decides 2026-08-01/02 per REVERTER_FILTERS.md).
  The two weekend-parked ops issues were FIXED 2026-07-25 (66e4ce4): entries settle to a
  TERMINAL state before protection is placed (plus a reprotect loop for stopless
  positions), and ghost flattens cancel the symbol's resting orders first. First session
  on the fixes (07-27): 18/18 reverter entries protected first-try, zero UNPROTECTED,
  zero failed ghost flattens.
