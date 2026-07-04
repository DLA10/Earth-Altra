# Earth-Altra — Live trading terminal (CLAUDE.md)

Single-user, real-money US-equity trading terminal built for **sub-second intraday
execution**. A Go backend ingests Alpaca's real-time SIP market data, aggregates candles
in memory, and fans them out to a React browser client over a WebSocket. The product name
shown in the UI is **Earth-Altra** (top nav) / **OPTIMUS** (the Execution page); the repo
folder is still `Live-Optimus`.

> The folder/app was renamed in the UI only — internal identifiers, package paths
> (`live-optimus/backend`), and `package.json` name still say `live-optimus`.

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

**Backend (Go 1.26)** — chosen for low-latency, high-throughput WebSocket fan-out with no
meaningful GC pauses. Credentials live only on the server.
- `github.com/alpacahq/alpaca-trade-api-go/v3` v3.8.1 — trading + market-data + streaming SDK
- `github.com/coder/websocket` v1.8.12 — browser WebSocket server
- `github.com/go-chi/chi/v5` + `go-chi/cors` — HTTP router/middleware
- `github.com/joho/godotenv` — `.env` loading
- `github.com/shopspring/decimal` — money math (SDK uses decimals; we convert to float for JSON)
- `_ "time/tzdata"` — bundles the tz DB so `America/New_York` works on Windows

**Frontend (React 18.3 + TypeScript 5.6 + Vite 5.4)**
- `lightweight-charts` ^4.2.3 — TradingView canvas charts (candles + volume + indicator overlays).
  **v4 has no native panes**, so the RSI sub-pane is a second chart synced on the logical range.
- Tabler icons via CDN webfont (`index.html`)
- No state library — plain React hooks; one resilient WebSocket per `useWebSocket()` consumer.

---

## 2. Architecture & data flow

```
Alpaca Trading REST ──┐
Alpaca SIP WebSocket ─┤      ┌──────────────── Go backend (:8080) ─────────────────┐
Alpaca Data REST  ────┘      │  alpaca.Client  → one SIP stream (trades/quotes/bars)│
                             │       │                                              │
                             │   candles.Engine (1/5/10m, in-memory, bad-tick guard)│
                             │   scanner.Scanner (DECEPTICON universe metrics)      │
                             │   flow.Tracker  (buy/sell pressure)                  │
                             │       │                                              │
                             │   hub.Hub  ── WebSocket fan-out ──► browsers         │
                             │   api.Server (chi) ── REST: orders/account/history…  │
                             └──────────────────────────────────────────────────────┘
                                            ▲                    │ /ws + /api/*
React + TypeScript (:5173, Vite) ───────────┘◄───────────────────┘
   Portal shell → Execution | Watchlist | DECEPTICON | History | Metrics
```

**Live price path (sub-second):** Alpaca trade tick → `alpaca` stream handler →
`candles.Engine.OnTrade` folds it into every timeframe's forming candle → `OnUpdate` →
`hub.BroadcastCandle` (throttled ~120ms) → only clients subscribed to that symbol → browser
`upsert()` into the chart. Quotes (last price) go to **all** clients via `BroadcastQuote`
(throttled ~150ms) and drive watchlists/headers.

**Single SIP connection.** Alpaca permits one market-data stream per account
(`alpaca/stream.go`). On each (re)connect it subscribes **trades+quotes** for
`tqSymbols = execution ∪ watchlist` and **bars** for `barSymbols = tq ∪ scan universe`.
Runtime-added symbols are subscribed live without a reconnect.

---

## 3. Repository layout

```
backend/
  cmd/server/main.go        wiring: config, stream loop, pollers, quant pipeline, HTTP server
  cmd/backtest/main.go      replay historical bars through the signal strategies (read-only)
  internal/
    alpaca/                 SDK wrapper + JSON DTOs (client, stream, types, news, screener)
    api/                    chi REST handlers + order validation (api.go), movers/stock news,
                            quant report (quant.go)
    candles/                in-memory OHLCV engine (1/5/10m), bad-tick guard
    config/                 env/.env loading (secrets stay server-side)
    dipwatch/               Telegram dip+bounce alert bot (read-only observer; feeds quant)
    execsym/                persisted symbol set: base + added − hidden
    flow/                   buy/sell order-flow estimator (quote rule)
    gemini/                 rate/budget-capped Gemini client ("why is it moving" summaries)
    hub/                    WebSocket fan-out, per-client (symbol, timeframe) subscription,
                            on-demand activation
    quant/                  AI quant pipeline: Agent 2 entry + Allocator + Broker/Manager +
                            Agent 3 exit + Agent 4 sentiment + daily review (paper account)
    risk/                   deterministic guardrails (loss cap, sizing, concurrency) — paper only
    signals/                multi-strategy intraday signal engine + backtester (paper/shadow only)
    scanner/                DECEPTICON per-ticker scan metrics
    watchlist/              parses EVENT_DRIVEN_WATCHLIST.md → departments/tickers
  data/                     runtime state (gitignored): symbol sets, daily_universe.json,
                            decisions/ (JSONL logs), reviews/
frontend/src/
  Portal.tsx                app shell + tab router + global SymbolSearch + OrderAlerts
  App.tsx                   Execution ("Optimus") page (default export ExecutionEngine)
  Watchlist.tsx             Watchlist page (stacked live charts + opening movers)
  Decepticon.tsx            DECEPTICON scanner page (+ MarketMovers with news dropdowns)
  Quant.tsx                 Paper · Claude page (quant pipeline report)
  Metrics.tsx               realized-P&L analytics
  TradeHistory.tsx          Alpaca fill log
  indicators.ts             Bollinger + RSI math + signal grading
  costBasis.ts              average-cost reconstruction + realized trades
  marketStatus.ts           client-side US market phase (pre/open/after/closed)
  order.ts                  localStorage symbol-order persistence + array move
  types.ts                  all shared TS types + ChartView/ChartRange helpers
  api/client.ts             typed fetch wrapper for every REST endpoint
  hooks/useWebSocket.ts     one resilient auto-reconnecting WS per consumer
  hooks/useHistoryBars.ts   fetch static range bars + mergeLastBar (live last bar)
  components/               Chart, OrderPanel, ChartOrderPopup (draw-order), ConfirmModal,
                            Header, Positions, Watchlist (left panel), LiveChart, MiniChart,
                            ChartModal, NewsPanel, MarketMovers, SymbolSearch, StrategyBadge,
                            OrderAlerts, RangeToggle, LazyMount, ErrorBoundary
EVENT_DRIVEN_WATCHLIST.md   DECEPTICON universe (markdown tables → ~27 depts, ~472 tickers)
QUANT_UNIVERSE.json         curated ~100-symbol signal-engine universe (liquid, no penny stocks)
Instruction.md              pre-market universe-selection playbook (writes daily_universe.json)
QUANT_VISION.md             design + roadmap for the AI agentic quant system
scripts/                    PowerShell launchers (check-keys, run-backend/frontend, launch)
START-Live-Optimus.bat      one-click Windows launcher
```

---

## 4. Backend packages

- **`config`** — loads `APCA_API_KEY_ID/SECRET`, `ALPACA_PAPER`, `ALPACA_DATA_FEED`
  (sip/iex/otc), `SYMBOLS`, `MAX_ORDER_NOTIONAL` (default 25000), `HTTP_ADDR`,
  `ALLOWED_ORIGINS`, `DECEPTICON_ENABLED`, plus the optional Gemini/Telegram/quant keys
  (full table in §9). Live vs paper toggles the Alpaca trading base URL. Secrets never
  reach the browser.

- **`alpaca`** — wraps the SDK behind float/JSON DTOs.
  - `client.go`: `VerifyKeys` (validates creds + probes SIP entitlement), `GetAccount`,
    `GetPositions`, `GetOpenOrders`, `GetAsset` (fractionable/tradable), `SearchAssets`
    (in-memory search over a cached ~10k tradable-equity list, 12h TTL), `PlaceOrder`
    (maps simple/bracket/oco/oto, stops, trailing, GTC, extended hours), `Readiness`
    (account gating + market clock), `CancelOrder`/`CancelAllOrders`, `GetFills`/`GetAllFills`
    (paginated activity log), `StreamTradeUpdates` (background order/fill events).
  - `stream.go`: `Backfill` (today's 1-min session), `RangeBars` (split-adjusted history for
    1W=hourly / 1M·6M·1Y=daily), `GetMultiDailyBars`/`GetMultiIntradayBars` (scanner seed),
    `StartStream`, `SubscribeTradeQuote`/`UnsubscribeTradeQuote` (runtime symbols).
  - `news.go`: Benzinga headlines + a keyword sentiment tag. `screener.go`: market-wide
    gainers/losers via the v1beta1 screener endpoint (called directly; SDK doesn't wrap it).

- **`candles`** — the live OHLCV engine. `series.apply()` folds a trade into the forming
  bar with a **bad-tick guard** (drops non-positive prices and wild jumps within a short
  window). Timeframes are 1/5/10 min; `Seed` loads REST backfill and rolls 1-min bars up;
  retention is 1500 bars/series (~a full extended session). `Tracks(sym)` lets callers skip
  re-backfilling an already-live symbol. `OnUpdate` callback drives `hub.BroadcastCandle`.

- **`hub`** — WebSocket fan-out. Each client has **one** active candle subscription — a
  (symbol, timeframe) pair (`subscribe`) — plus an optional scan subscription
  (`scan_subscribe`). `BroadcastCandle` → subscribers of that symbol+timeframe (throttled
  120ms per pair); `BroadcastQuote` → all (throttle 150ms); `BroadcastScan` → scan
  subscribers. `SnapshotFn` returns engine history on subscribe.
  **`EnsureLiveFn`** is called synchronously on subscribe so a client can subscribe to **any**
  symbol — the server backfills + starts streaming it on demand (see §7).

- **`scanner`** — per-ticker `State` over the DECEPTICON universe: price, % vs prior
  close, % vs open, opening-range moves (OR5/15/20), RVOL (vs typical-at-this-time),
  session VWAP, day high/low, bid/ask spread, catalyst. Seeded from daily (prior close +
  avg volume) and today's 1-min bars; updated by live bars/quotes. `SessionBars` feeds the
  DECEPTICON mini-charts; `OpeningAnalysis` ranks the watchlist by move from the open.

- **`flow`** — estimates buyer- vs seller-initiated volume (quote rule: trade ≥ ask = buy,
  ≤ bid = sell, else nearest side). Keeps a day-cumulative tally and a rolling 5-min window.

- **`execsym`** — thread-safe, disk-persisted symbol set: `base` (config) + `added`
  (runtime) − `hidden` (user-removed, incl. base). Powers both Execution and Watchlist
  symbol management; survives restarts via `data/*.json`.

- **`watchlist`** — parses `EVENT_DRIVEN_WATCHLIST.md` markdown tables into departments
  (with Tabler icons) and tickers/catalysts. Never hardcoded — parsed at load.

- **`api`** — chi handlers + **server-side order validation** (`validateOrder`,
  `checkSellable`). Holds `Server` deps and the on-demand `EnsureLive`/`activateSymbol`
  logic. Full endpoint list in §10.

- **`dipwatch`** — Telegram dip+bounce alert bot over the whole watchlist (oversold,
  below-VWAP pullback ≥ ~0.5×ATR confirmed by a green 5-min candle; 15-min cooldown).
  Read-only observer; its hook also feeds each confirmed dip to the quant pipeline.

- **`quant`** — the AI paper-trading team on a SEPARATE paper account (`PAPER_CLAUDE_*`):
  dip signal → Agent 2 (entry, forced tool call) → deterministic Allocator (shared budget,
  slot cap, conviction sizing, quality-ranked funding under contention) → Broker + Manager
  (market entry, trailing-stop floor, Agent 3 exit loop with ratchet-up-only stops) →
  Agent 4 sentiment (local Ollama, advisory) → daily Opus review to `data/reviews/`.
  Universe gated by `data/daily_universe.json` (written pre-market per `Instruction.md`);
  every decision logged as JSONL to `data/decisions/`. Model proposes, Go disposes.
  See `QUANT_VISION.md` for where this subsystem is headed.

- **`signals`** — the multi-strategy intraday signal engine (QUANT_VISION Phase 1): six
  deterministic detectors (ORB breakout, VWAP reclaim, momentum continuation, dip bounce,
  relative strength, first-hour reversal) over the curated QUANT_UNIVERSE.json (~100
  names), fed as an ADDITIVE bar consumer off the single SIP stream. Every signal + its
  counterfactual bracket outcome is journaled to `data/signals/*.jsonl` (the ML training
  set), annotated with the **time-of-day gate** verdict (`tod_stats.json`, decayed
  buckets — halflife 30 outcomes; `EntryAllowed`). The TOD gate is **shadow-only by
  default** since the 2026-07 re-validation (its edge didn't survive a regime change —
  RESEARCH_BACKLOG #3); `QUANT_TOD_GATE=true` re-enforces it. The same detectors power
  `cmd/backtest`. Execution: `quant.SignalTrader` bridges published signals to the PAPER
  broker — **ML entry gate** (`quant/clfgate.go`: per-strategy LightGBM classifiers
  trained nightly by `ml/train_live.py`, scored in-process via `leaves` with a load-time
  Python/Go parity check; rejects expected R < 0.03, fail-open without fresh models —
  the promoted RESEARCH_BACKLOG #15 mechanism) → LLM entry judge (`signaljudge.go`,
  red-flag veto + conviction sizing) → shared allocator → Manager (trailing-stop floor,
  Agent 3 exits, EOD flatten) — gated by `QUANT_SIGNALS_LIVE`, scoreboard demotion, and
  the daily loss cap.

- **`risk`** — deterministic guardrails shared by the backtester and (future) live-paper
  signal execution: daily loss cap, per-trade risk / notional sizing, concurrency cap,
  overnight cap. Pure rules; never wired to the real-money path.

- **`gemini`** — self-throttled (RPM + daily cap) Gemini client for the on-click
  "why is it moving" stock-news summaries. Disabled-safe; never on the order path.

`main.go` wires it all: load config → verify keys → build engine/hub/managers → backfill →
seed scanner (goroutine) → start the single SIP stream loop (auto-reconnect, re-backfill on
reconnect) → quant pipeline + dip watcher → account poller (3s, plus instant refresh on
trade-update events) → HTTP server.

---

## 5. Frontend pages & components

**`Portal`** — app shell. Tabs (each mounts only while selected, so DECEPTICON's scan
stream isn't running while you trade): **Execution · Watchlist · DECEPTICON · History ·
Metrics · Paper · Claude**, plus a global **SymbolSearch** (add any tradable US stock to
Execution/Watchlist) and portal-wide **OrderAlerts** fill animations.

**Paper · Claude page (`Quant.tsx`)** — read-only report of the quant pipeline: shared
budget, open positions, realized-only P&L, per-exit-reason attribution ("is Agent 3 adding
value vs a dumb stop?"), and the latest daily review.

**Execution page (`App.tsx` = `ExecutionEngine`)** — the core trading surface.
- Layout: left **Watchlist** panel · center **Chart + Positions + NewsPanel** · right **OrderPanel**.
- `Header`: LIVE/PAPER badge, market-phase badge (self-updating), feed badge (SIP warning),
  **Equity** (marked live to streaming prices), **Day P/L** (unrealized, live), **Buying power**,
  connection dot, and the **Cancel-all kill switch** (cancels open orders, not shares).
- Left panel (`components/Watchlist.tsx`): drag-to-reorder rows (persisted), price + %
  (blank when no live quote), `⋯` menu to move-to-watchlist / remove, company name.
- Live equity & Day P/L are computed client-side by marking each held position to its
  streaming price between 3s REST polls; cost basis is reconstructed from fills
  (`costBasis.ts`) to fix Alpaca's blended `avg_entry_price`.
- Chart toolbar: live signal badge + indicator toggle + **RangeToggle** (1m/5m/10m | 1W/1M/6M/1Y).

**Watchlist page (`Watchlist.tsx`)** — **Opening movers** ranking (top gainers/fallers at
+15/30/45/60 min from the 9:30 ET open; click a ticker to scroll to its chart; send to
Execution / add to watchlist) over a stack of full-size **`LiveChart`** charts (each opens
its own WebSocket). Page-level RangeToggle applies to all charts; drag the `⠿` grip to reorder.

**DECEPTICON page (`Decepticon.tsx`)** — event-driven sector scanner. Per department:
summary cards (sector move, breadth, high-RVOL, catalyst flags), top movers, catalyst radar,
and a heatmap of `MiniChart`s. Click any tile/card → **`ChartModal`** (a **live** WS chart
with indicators + add-to-Exec/Watchlist). A **MarketMovers** panel shows whole-market
screener gainers/losers; clicking a row opens the same live popup.

**History (`TradeHistory.tsx`)** — Alpaca fill log (authoritative; selectable day window).

**Metrics (`Metrics.tsx`)** — realized-P&L analytics bucketed by day/week/month from fills
(`costBasis.ts` `realizedTrades`: average-cost, merges partial fills per sell order, resets
on a flat position), with win rate, best/worst, and an equity-curve chart.

**Shared chart components:**
- `Chart.tsx` — candlesticks + volume sub-pane; optional **Bollinger band** overlay and a
  time-synced **RSI pane**; green "bought here" entry line; preserves user zoom on live
  updates, `scrollToRealTime` on intraday view change, `fitContent` for historical ranges.
- `OrderPanel.tsx` / `ConfirmModal.tsx` — see §8.
- `StrategyBadge`, `RangeToggle`, `LiveChart`, `MiniChart`, `ChartModal`, `NewsPanel`
  (buy/sell pressure meter + headlines), `Positions`, `SymbolSearch`, `LazyMount`
  (mounts children when scrolled into view), `ErrorBoundary`.

---

## 6. Indicators (Bollinger + RSI "Combo" strategy)

`indicators.ts` computes everything natively from the candle series shown:
- **Bollinger Bands**: SMA(20) ± 2 · population stdev (matches TradingView `ta.stdev`).
- **RSI**: Wilder's RSI(14) (matches `ta.rsi`).
- **`grade`/`evaluate`**: per-bar signal — **STRONG** (band AND RSI agree), **WEAK** (only
  one), **WAIT** (neither). BUY when price ≤ lower band or RSI ≤ 30; SELL when price ≥ upper
  band or RSI ≥ 70. Rendered as `StrategyBadge` with a plain-language reason.

Indicators are **display/decision aids only — they never place orders.** They apply to
whatever series is shown, so a 1-year daily view yields 20-day bands + 14-day RSI. Toggle
state persists in `localStorage` (`lo.indicators`).

---

## 7. Real-time + on-demand streaming model

**WebSocket protocol** (`/ws`, JSON `{type, data}`):
- Client → server: `{action:"subscribe", symbol, timeframe}`,
  `{action:"scan_subscribe"}`, `{action:"scan_unsubscribe"}`.
- Server → client: `snapshot` (candle history on subscribe), `candle` (live update),
  `quote` (last price, all clients), `account` / `positions` / `orders` (3s poll),
  `trade_update` (order/fill event), `scan` (DECEPTICON snapshot), `exec_symbols` /
  `watch_symbols` (symbol-set changes).

**On-demand activation (additive).** When a client subscribes to a symbol the engine isn't
tracking, `hub.EnsureLiveFn → api.EnsureLive → activateSymbol` backfills its session and
subscribes its trades/quotes on the SIP stream, then the normal candle path streams it
sub-second. This powers the DECEPTICON popup charting **any** symbol (incl. market movers).
It is **additive only** — symbols stay subscribed for the session (cleared on restart), so
there is no teardown that could disturb Execution symbols, and previewed symbols are pinned
in `inUse()`. For already-tracked symbols `EnsureLive` is a no-op (verified: zero added
latency on Execution).

**Per-component WebSocket.** `useWebSocket` opens a **fresh** connection per consumer, so a
popup or stacked `LiveChart` can subscribe independently without hijacking the Execution
chart's single-symbol subscription.

---

## 8. Order system & safety

**Order kinds (frontend `OrderPanel` + chart draw-order):**
- **Market** buy/sell — shares or dollars (notional; auto-disabled for non-fractionable
  symbols and in extended hours).
- **Conditional** — buy-limit (buy the dip, below market), sell-limit (take profit, above
  market), stop-loss (below market), trailing stop (follows price up by $ or %). Marketable
  prices are **blocked** with a direction-rule explanation.
- **OCO** — protect a held position with a take-profit (above) + stop-loss (below); whichever
  fills cancels the other. Whole shares only.
- **Bracket** — buy (market or resting limit) + auto take-profit + stop-loss in one order.
  For a LIMIT-entry bracket the TP/SL are validated against the **entry** price, not the
  current market price. Whole shares only.
- **Draw-order (`ChartOrderPopup`)** — click "✏ Draw order", click a price on the chart, and
  the popup offers the contextually-valid order types (buy-stop / take-profit above; buy-limit
  / stop-loss below). Routes through the same ConfirmModal + server validation as everything
  else.

**Safety guards (defense in depth):**
1. Frontend `OrderPanel` blocks fat-fingers (direction rules, oversell, fractional-stop, cap).
2. **Mandatory `ConfirmModal`** — LIVE-styled, with explicit **"this limit fills
   immediately"** and **"this stop triggers immediately"** warnings when a price is on the
   wrong side of the market.
3. Backend `validateOrder` re-checks everything server-side; `checkSellable` rejects selling
   more than held (no accidental shorting/overselling); `MAX_ORDER_NOTIONAL` caps order value.
4. `PlaceOrder` is a **REST** `POST /api/orders` (request→response) — orders are never sent
   over the market-data socket. The **kill switch** cancels all open orders (not positions).

---

## 9. Configuration (`backend/.env`)

| Key | Default | Meaning |
|-----|---------|---------|
| `APCA_API_KEY_ID` / `APCA_API_SECRET_KEY` | — | Alpaca credentials (server-only) |
| `ALPACA_PAPER` | `false` | `true` = paper trading endpoint |
| `ALPACA_DATA_FEED` | `sip` | `sip` (Algo Trader Plus) or `iex` (free) |
| `SYMBOLS` | `SNDK,SPCX,STX,NVDA,MRVL` | Base Execution symbols |
| `MAX_ORDER_NOTIONAL` | `25000` | Per-order USD cap (0 disables) |
| `HTTP_ADDR` | `:8080` | Backend listen address |
| `ALLOWED_ORIGINS` | `localhost:5173` | CORS + WS origin allowlist |
| `DECEPTICON_ENABLED` | `true` | Enable the scanner page/stream |
| `GEMINI_API_KEY` / `GEMINI_MODEL` / `GEMINI_RPM` / `GEMINI_DAILY_CAP` | — / `gemini-3.5-flash` / `8` / `200` | Optional movers-news summaries |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | — | Optional dip-watcher alerts |
| `PAPER_CLAUDE_KEY` / `PAPER_CLAUDE_SECRET` | — | Quant pipeline's SEPARATE paper account |
| `ANTHROPIC_API_KEY` | — | Quant agents (idle when empty) |
| `CLAUDE_SYMBOLS` | `SNDK,MU` | Always-streamed quant symbols (+ SPY/QQQ) |
| `QUANT_ENTRY_MODEL` / `QUANT_EXIT_MODEL` / `QUANT_REVIEW_MODEL` | haiku / haiku / opus | Agent models |
| `QUANT_TRAIL_PCT` | `1.5` | Deterministic trailing-stop floor % |
| `QUANT_LIVE` | `true` | `false` = shadow mode (log only, no paper orders) |
| `QUANT_OVERNIGHT_CAP` | `0` | Keep ≤1 profitable position up to this value overnight (0 = flatten all) |
| `QUANT_UNIVERSE_PATH` | `QUANT_UNIVERSE.json` | Signal-engine universe file override |
| `QUANT_SIGNALS_LIVE` | `true` | Route signal-engine entries to the paper broker (false = shadow only) |
| `QUANT_JUDGE_MODEL` | `claude-haiku-4-5` | Signal entry judge model |
| `QUANT_DAILY_LOSS_CAP` | `150` | Halt new signal entries once day P&L ≈ −cap |
| `QUANT_TOD_GATE` | `false` | Enforce the time-of-day gate (default shadow-only: journals verdicts, blocks nothing) |
| `QUANT_CLF_GATE` | `true` | ML entry gate: nightly LightGBM classifiers reject entries with expected R < 0.03 (fail-open without fresh models) |
| `QUANT_RETRAIN` | `true` | Auto-run `ml/train_live.py` weekdays ~17:05 ET (+ boot catch-up) to refresh the gate models |
| `OLLAMA_ENDPOINT` / `OLLAMA_MODEL` | `localhost:11434` / `gemma2:2b` | Agent 4 sentiment (local) |

Backfill always loads the full current session day per symbol (no bar-count knob).
Persistence (all gitignored under `backend/data/`): `execution_symbols.json`,
`watchlist_symbols.json` (added/hidden sets), `daily_universe.json`, `decisions/*.jsonl`,
`reviews/*.json`. Browser `localStorage`: `lo.execOrder` / `lo.watchOrder` (chart reorder),
`lo.indicators` (`on`/`off`), `lo.execAutoSort` (opening auto-sort marks).

---

## 10. REST API reference (all under `/api`, plus `/ws` and `/healthz`)

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/keycheck` | keys valid + SIP entitlement |
| GET | `/config` | symbols, mode, feed, fractionable flags, cap, decepticon_enabled (no secrets) |
| GET | `/account` · `/positions` · `/orders` | account snapshot / positions / open orders |
| POST | `/orders` | place an order (validated) |
| DELETE | `/orders` · `/orders/{id}` | cancel all / cancel one |
| GET/POST | `/execution/symbols` · DELETE `/execution/symbols/{symbol}` | Execution symbol set |
| GET/POST | `/watchlist/symbols` · DELETE `/watchlist/symbols/{symbol}` | Watchlist symbol set |
| GET | `/history?symbol&range` | static 1W/1M/6M/1Y bars (split-adjusted; any symbol) |
| GET | `/opening-analysis?scope` | movers ranking at +5/15/30/45/60 min (watchlist or execution) |
| GET | `/asset-names` · `/symbol-meta` | company name / name+sector (cached) |
| GET | `/assets` · `/assets/search?q&limit` | configured assets / global stock search |
| GET | `/movers?top` · `/movers-news?top` | screener gainers/losers / + news badges (DIP?/KNIFE) |
| GET | `/stock-news?symbol` | headlines + background AI "why is it moving" summary |
| GET | `/quotes` | per-symbol `{price, ref}` snapshot (seeds the left panel) |
| GET | `/rvol?symbol` | time-of-day-aware relative volume from the scanner |
| GET | `/activities?days&limit` · `/fills?days` | fill log / full-window fills (Metrics) |
| GET | `/news?symbols&limit` · `/pressure?symbol` | headlines+sentiment / buy-sell pressure |
| GET | `/readiness` | account trading-readiness gating + market clock |
| GET | `/quant` | quant pipeline report (Paper · Claude page) |
| GET | `/decepticon/watchlist` · `/decepticon/scan` · `/decepticon/bars?symbol` | scanner |

---

## 11. Build, run, verify

```powershell
# Backend (Go on PATH at C:\Program Files\Go\bin)
.\scripts\check-keys.ps1            # exit 0 = keys valid + SIP entitled; 2 = no SIP; 1 = bad keys
.\scripts\run-backend.ps1           # go run ./cmd/server  → http://localhost:8080
# Frontend
cd frontend; npm install            # first run
.\scripts\run-frontend.ps1          # vite dev → http://localhost:5173 (proxies /api + /ws)
# One-click (both, opens browser): START-Live-Optimus.bat
# Strategy backtest (read-only market data; bars cached in data/btcache/)
cd backend; go run ./cmd/backtest -days 21              # all strategies
cd backend; go run ./cmd/backtest -days 63 -sweep       # momentum tune (IS) + validate (OOS)
cd backend; go run ./cmd/backtest -days 63 -mlgate      # walk-forward ML-gate lift test
cd backend; go run ./cmd/backtest -days 63 -dataset data/ml_dataset.jsonl  # export ML rows
```

Checks before considering a change done:
- Backend: `go build ./...` (from `backend/`).
- Frontend: `npx tsc --noEmit` then `npm run build` (= `tsc -b && vite build`).
- Live smoke tests (Node 20 has a global `WebSocket`): subscribe to an Execution symbol and
  assert the snapshot symbol matches and no foreign symbol leaks; `curl /api/history` returns
  sane bar counts. The market data is real even when the market is closed (history backfills);
  live ticks only flow during trading hours.

---

## 12. Conventions & gotchas

- **Don't break the Execution streaming/order path.** It's real money. New features should be
  additive and isolated; re-verify Execution after backend stream/hub/api changes.
- **Times are unix seconds** in candle DTOs; the ET session helpers (`sessionStartET`,
  `sessionDayStartET`) and `marketStatus.ts` handle the trading calendar — but **holidays are
  not modeled** (e.g. on a holiday there are no live quotes; the sidebar price is blank and
  charts show backfilled history).
- **lightweight-charts is v4** — no native panes; the RSI pane is a synced second chart. Daily
  ranges call `fitContent`; intraday calls `scrollToRealTime` and preserves user zoom.
- **Throttles** (`BroadcastCandle` 120ms, `BroadcastQuote` 150ms) are deliberate flood
  control and are still sub-second; change them only with care.
- **Money math**: the SDK uses `decimal`; the `alpaca` package converts to `float64` for JSON
  at the boundary. Keep order-size logic (qty vs notional, fractional rules) intact.
- **DECEPTICON universe** comes from `EVENT_DRIVEN_WATCHLIST.md` (parsed, not hardcoded);
  editing that file changes the scanner's departments/tickers.
- This file is documentation **and** agent guidance; keep it accurate when features change.
```
