// Command server runs the Live-Optimus backend: SIP market-data ingest, candle
// aggregation, a WebSocket fan-out hub, and the trading/account REST API.
package main

import (
	"os/exec"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // bundle the tz database so America/New_York works on Windows

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"live-optimus/backend/internal/alpaca"
	"live-optimus/backend/internal/api"
	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/config"
	"live-optimus/backend/internal/dipwatch"
	"live-optimus/backend/internal/evals"
	"live-optimus/backend/internal/execsym"
	"live-optimus/backend/internal/flow"
	"live-optimus/backend/internal/gemini"
	"live-optimus/backend/internal/hub"
	"live-optimus/backend/internal/quant"
	"live-optimus/backend/internal/risk"
	"live-optimus/backend/internal/scanner"
	"live-optimus/backend/internal/signals"
	"live-optimus/backend/internal/watchlist"
)

func main() {
	keycheckOnly := flag.Bool("keycheck", false, "validate API keys + SIP entitlement, then exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	client := alpaca.New(cfg)

	if *keycheckOnly {
		runKeycheck(client)
		return
	}

	log.Printf("Live-Optimus backend starting | mode=%s feed=%s symbols=%v cap=$%.0f",
		cfg.Mode(), cfg.DataFeed, cfg.Symbols, cfg.MaxOrderNotional)

	// Verify keys at startup and warn loudly if SIP is not entitled.
	kc := client.VerifyKeys()
	if !kc.KeysValid {
		log.Fatalf("keycheck: %s", kc.Detail)
	}
	if !kc.SIPEntitled {
		log.Printf("WARNING: %s", kc.Detail)
	} else {
		log.Printf("keycheck OK: %s", kc.Detail)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Execution symbols: base (config) + any persisted user-added symbols.
	execMgr := execsym.New(cfg.Symbols, "data/execution_symbols.json")

	// Watchlist symbols (full-size live-chart page). Seeded with a default set on the
	// very first run; thereafter the persisted set wins.
	watchPath := "data/watchlist_symbols.json"
	watchSeed := !fileExists(watchPath)
	watchMgr := execsym.New(nil, watchPath)
	if watchSeed {
		for _, sym := range []string{
			"AAPL", "DELL", "ARM", "QCOM", "DDOG", "INTC", "ORCL", "MDB",
			"SNOW", "NET", "NVDA", "GOOGL", "IBM", "APP", "AMZN",
		} {
			watchMgr.Add(sym)
		}
		log.Printf("watchlist seeded with %d default symbols", len(watchMgr.All()))
	}

	// Quant-pipeline symbols that must stream like execution/watchlist symbols so the agents
	// always see fresh candles (incl. across SIP reconnects). SPY/QQQ provide the
	// market-context backdrop the entry/exit agents read on every decision.
	var paperSymbols []string
	if cfg.PaperClaudeKey != "" && cfg.PaperClaudeSecret != "" {
		paperSymbols = unionSymbols(cfg.ClaudeSymbols, []string{"SPY", "QQQ"})
	}

	// All symbols that need full live (trades/quotes) treatment.
	liveSymbols := unionSymbols(unionSymbols(execMgr.All(), watchMgr.All()), paperSymbols)

	// Candle engine + hub. Keep enough 1-minute candles to hold a full extended
	// session (premarket + regular + after-hours ≈ 960 min) with headroom, so the
	// chart shows the whole day, not just the last few hours.
	const candleRetention = 1500
	engine := candles.NewEngine(liveSymbols, candleRetention)
	h := hub.New()
	h.SnapshotFn = func(symbol string, tf int) interface{} {
		return engine.Snapshot(symbol, tf)
	}
	engine.OnUpdate = func(u candles.Update) {
		h.BroadcastCandle(u.Symbol, u.Timeframe, u)
	}

	// Resolve fractionable flags for all live symbols.
	fractionable := map[string]bool{}
	for _, sym := range liveSymbols {
		if a, err := client.GetAsset(sym); err == nil {
			fractionable[sym] = a.Fractionable
		} else {
			log.Printf("asset lookup failed for %s: %v", sym, err)
			fractionable[sym] = false
		}
	}

	// Backfill history for each live symbol.
	for _, sym := range liveSymbols {
		bars, err := client.Backfill(sym)
		if err != nil {
			log.Printf("backfill %s failed: %v", sym, err)
			continue
		}
		engine.Seed(sym, bars)
		log.Printf("backfilled %s: %d 1m bars", sym, len(bars))
	}

	// ---- DECEPTICON scanner ----
	var (
		wl  *watchlist.Watchlist
		scn *scanner.Scanner
	)
	var scanSymbols []string
	if cfg.DecepticonEnabled {
		loaded, err := watchlist.Load(cfg.WatchlistCandidates...)
		if err != nil {
			log.Printf("DECEPTICON: watchlist load failed (%v); scanner disabled", err)
		} else {
			wl = loaded
			// First-seen catalyst per symbol for the store; the per-department view
			// uses the department-specific catalyst from the watchlist API.
			catalysts := map[string]string{}
			for _, d := range wl.Departments {
				for _, t := range d.Tickers {
					if _, ok := catalysts[t.Symbol]; !ok {
						catalysts[t.Symbol] = t.Catalyst
					}
				}
			}
			scn = scanner.New(catalysts)
			scanSymbols = wl.Symbols
			log.Printf("DECEPTICON: %d departments, %d unique tickers", len(wl.Departments), len(wl.Symbols))
			go seedScanner(client, scn, wl.Symbols)
			go runScanBroadcaster(ctx, scn, h)
		}
	}

	// Order-flow tracker: estimates buyer/seller-initiated volume from trades + quotes.
	flowTracker := flow.New()

	// ---- Quant signal engine (QUANT_VISION Phase 1) ----
	// Multi-strategy detectors over the curated universe (QUANT_UNIVERSE.json), fed by
	// the SAME single SIP stream as an ADDITIVE bar consumer — nothing on the Execution
	// page's path changes. SHADOW-ONLY: it detects + logs signals and their counterfactual
	// bracket outcomes (data/signals/); it places no orders until backtesting validates
	// the strategy set and execution is explicitly enabled.
	var sigEngine *signals.Engine
	var sigSymbols []string
	if uni, err := signals.LoadUniverse(cfg.QuantUniverseCandidates...); err != nil {
		log.Printf("signals: disabled — %v", err)
	} else {
		sigEngine = signals.NewEngine(uni, "data")
		sigSymbols = uni.All()
		go seedSignalEngine(client, sigEngine, sigSymbols)
		// Live-only microstructure columns (spread, order flow) for the ML training set —
		// historical bars can't reconstruct these, so they only get recorded live.
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
			// P2.1: sector lead-lag (RESEARCH_BACKLOG #9) — reconstructable from bars, but
			// computed here too so live journal rows carry it without waiting on a backfill.
			for k, v := range sigEngine.SectorLeadLag(sym) {
				out[k] = v
			}
			return out
		})
		log.Printf("signals: SHADOW scanning %d symbols (+%d context) across %d strategies → data/signals/",
			len(uni.Symbols()), len(uni.Context()), len(signals.DefaultStrategies()))
	}

	// Start the single SIP stream (reconnect on failure). On each (re)connect it
	// subscribes trades/quotes for the current execution symbols (base + added) and
	// bars for the union of execution + scan + signal universes, so added symbols
	// survive reconnects.
	go runStream(ctx, client, execMgr, watchMgr, scanSymbols, paperSymbols, sigSymbols, engine, scn, sigEngine, h, flowTracker)

	// Periodically push account + positions + open orders to clients. A trade update
	// (fill/cancel/etc.) signals refreshCh to refresh immediately instead of waiting
	// for the next tick.
	refreshCh := make(chan struct{}, 1)
	go runAccountPoller(ctx, client, h, refreshCh)

	// Real-time account order/fill events: broadcast to clients and trigger an
	// immediate account/positions refresh (auto-reconnects in the background).
	client.StreamTradeUpdates(ctx, func(tu alpaca.TradeUpdate) {
		log.Printf("trade update: %s %s %s qty=%s price=%.2f", tu.Event, tu.Side, tu.Symbol, tu.Qty, tu.Price)
		h.BroadcastTyped("trade_update", tu)
		select {
		case refreshCh <- struct{}{}:
		default:
		}
	})

	// Dip Watcher Telegram bot: read-only observer that alerts on dips+bounces for the ENTIRE
	// watchlist (evaluated live each scan, so runtime-added symbols are covered). Same dip
	// rules as before — only the symbol set was broadened. Never touches the order/stream path.
	dw := dipwatch.New(cfg.TelegramBotToken, cfg.TelegramChatID, watchMgr.All, engine, client)

	var quantEngine *quant.Engine        // exposed to the API after srv is built
	var evalsFn func() interface{}       // eval scoreboard accessor for /api/evals
	// AI quant pipeline — Agent 2 (entry decision) in SHADOW mode. On each confirmed dip, if the
	// symbol is in today's curated universe (backend/data/daily_universe.json), Opus decides
	// buy/no_buy; the decision is logged (backend/data/decisions/) and its label is appended to
	// the Telegram alert (one detector, labeled output). No orders are placed until the exit
	// manager (Agent 3) is wired. Idle/zero-cost until ANTHROPIC_API_KEY + a universe file exist.
	{
		etz, err := time.LoadLocation("America/New_York")
		if err != nil {
			etz = time.UTC
		}
		qUniverse := quant.NewUniverse("data")
		qAlloc := quant.NewAllocator()
		qAlloc.Configure(qUniverse.Allocation())
		qLog := quant.NewDecisionLog("data", etz)
		qAnth := quant.NewAnthropic(cfg.AnthropicAPIKey)
		qAgent2 := quant.NewAgent2(qAnth, cfg.QuantEntryModel) // default Haiku 4.5
		qEngine := quant.NewEngine(qUniverse, qAlloc, qLog, qAgent2, engine, scn, etz)
		qEngine.SetDataDir("data")
		quantEngine = qEngine

		// Agent 4 (sentiment) on the local LLM (Ollama). Advisory only; its cached score enriches
		// Agent 2's snapshot. News comes from the same Alpaca/Benzinga feed the dip bot uses.
		newsFn := func(sym string, limit int) []string {
			items, err := client.GetNews([]string{sym}, limit)
			if err != nil {
				return nil
			}
			out := make([]string, 0, len(items))
			for _, n := range items {
				out = append(out, n.Headline)
			}
			return out
		}
		qAgent4 := quant.NewAgent4(cfg.OllamaEndpoint, cfg.OllamaModel, newsFn, etz)
		qEngine.SetAgent4(qAgent4)
		if qAgent2.Enabled() {
			go qAgent4.Run(ctx, qUniverse.Symbols) // no point scoring sentiment if Agent 2 is idle
		}

		// Pick up a freshly-written daily_universe.json (pre-market session) without a restart.
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if qUniverse.Reload() == nil {
						qAlloc.Configure(qUniverse.Allocation())
					}
				}
			}
		}()

		dw.SetHook(func(s dipwatch.DipSignal) string {
			return qEngine.OnDip(quant.DipInput{
				Symbol: s.Symbol, Time: s.Time, Price: s.Price, DayHigh: s.DayHigh, DipLow: s.DipLow,
				ATR: s.ATR, RSI: s.RSI, VWAP: s.VWAP, DayOpen: s.DayOpen, RVOL: s.RVOL,
				BounceVolume: s.BounceVolume, NegativeNews: s.NegativeNews,
			})
		})

		// Execution: Agent 3 (Haiku exit) + paper broker + position manager (deterministic
		// trailing-stop floor at fill, then Agent 3 manages the exit). Live order placement is
		// gated by QUANT_LIVE (default true) AND a configured key/broker; otherwise shadow.
		qAgent3 := quant.NewAgent3(qAnth, cfg.QuantExitModel)
		qBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperClaudeKey, cfg.PaperClaudeSecret)
		qMgr := quant.NewManager(qEngine, qBroker, qAgent3, cfg.QuantTrailPct, cfg.QuantOvernightCap)
		qEngine.SetExecution(qBroker, qMgr)
		qEngine.SetContext(ctx)

		// Restart resilience: re-adopt any position that survived a process restart —
		// re-attach (or freshly place) its protective stop, re-fund the allocator so the
		// shared budget can't be oversubscribed, and resume its Agent-3 manage loop.
		// Runs BEFORE any entry source (dip hook / signal trader) can act.
		if n := qMgr.Rehydrate(ctx); n > 0 {
			log.Printf("quant: rehydrated %d open position(s) from the paper account", n)
		}

		// Daily post-market review (Opus): reads the decision log + reconstructed trades, writes a
		// structured report to backend/data/reviews/ for the next pre-market session to learn from.
		qReviewer := quant.NewReviewer(qAnth, cfg.QuantReviewModel, qLog,
			func() quant.QuantState {
				if !qBroker.Enabled() {
					return quant.QuantState{}
				}
				st, _ := qBroker.Reconstruct(qEngine.LastClose)
				return st
			},
			"data", etz)
		go qReviewer.RunDaily(ctx)

		if cfg.QuantLive && qAgent2.Enabled() && qBroker.Enabled() {
			qEngine.SetLive(true)
			log.Printf("quant: LIVE (paper) | entry=%s exit=%s trail=%.1f%% | universe=%d symbols",
				cfg.QuantEntryModel, cfg.QuantExitModel, cfg.QuantTrailPct, len(qUniverse.Symbols()))
		} else if qAgent2.Enabled() {
			log.Printf("quant: SHADOW mode (set QUANT_LIVE=true + paper keys to place orders) | universe=%d", len(qUniverse.Symbols()))
		} else {
			log.Printf("quant: idle (set ANTHROPIC_API_KEY); dip hook installed")
		}

		// ---- Signal-engine paper execution (the validated Tier-1 champion config) ----
		// Bridge: signal → learned time-of-day gate → LLM entry judge (red-flag veto +
		// conviction sizing) → shared allocator → Manager (entry, trailing-stop floor,
		// Agent 3 exits, EOD flatten) → Claude PAPER account. Shares the allocator with
		// the dip pipeline so the two can never oversubscribe the $8k budget. The
		// dipwatch Telegram flow is untouched.
		// ---- Eval scoreboard (QUANT_VISION §5): rolling strategy expectancy, CUSUM ----
		// watchdog, judge calibration; recomputed every 10 min, persisted, served at
		// /api/evals, and enforced as strategy demotion in the signal trader.
		var sbMu sync.RWMutex
		var scoreboard *evals.Scoreboard
		refreshEvals := func() {
			if sb, err := evals.Compute("data", 20, etz); err == nil {
				sb.Save("data")
				sbMu.Lock()
				scoreboard = sb
				sbMu.Unlock()
			}
		}
		refreshEvals()
		go func() {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					refreshEvals()
				}
			}
		}()
		evalsFn = func() interface{} {
			sbMu.RLock()
			defer sbMu.RUnlock()
			if scoreboard == nil {
				return map[string]interface{}{"enabled": false}
			}
			return scoreboard
		}

		// The promoted ML entry gate (RESEARCH_BACKLOG #15): per-strategy LightGBM
		// classifiers trained nightly by ml/train_live.py, scored in-process via leaves
		// with a load-time parity check. Fail-open everywhere: no model → no gating.
		var clfGate *quant.ClfGate
		if sigEngine != nil && cfg.QuantClfGate {
			clfGate = quant.NewClfGate("data/models")
		}

		if sigEngine != nil && qBroker.Enabled() {
			judge := quant.NewSignalJudge(qAnth, cfg.QuantJudgeModel)
			limits := risk.Defaults()
			limits.DailyLossCapUSD = cfg.QuantDailyLossCap
			trader := quant.NewSignalTrader(ctx, qEngine, qMgr, sigEngine, judge, limits, cfg.QuantSignalsLive, cfg.QuantTODGate)
			trader.Clf = clfGate
			trader.Demoted = func(strategy string) bool {
				sbMu.RLock()
				defer sbMu.RUnlock()
				return scoreboard != nil && scoreboard.IsDemoted(strategy)
			}
			sigEngine.SetOnSignal(trader.OnSignal)
			if cfg.QuantSignalsLive {
				todMode := "enforcing"
				if !cfg.QuantTODGate {
					todMode = "shadow-only"
				}
				log.Printf("signal-trader: LIVE (paper) | judge=%s (enabled=%v) | daily loss cap $%.0f | TOD gate %s | scoreboard demotion active",
					cfg.QuantJudgeModel, judge.Enabled(), cfg.QuantDailyLossCap, todMode)
			} else {
				log.Printf("signal-trader: disabled (QUANT_SIGNALS_LIVE=false) — shadow journaling only")
			}
		} else if sigEngine != nil {
			log.Printf("signal-trader: no paper broker keys — shadow journaling only")
		}

		// ---- Strategist agent: pre-market posture + allocation → daily_universe.json ----
		if cfg.QuantStrategist {
			marketFn := func() (quant.MarketState, error) {
				bars, err := client.GetMultiDailyBars([]string{"SPY", "QQQ"}, 30)
				if err != nil {
					return quant.MarketState{}, err
				}
				return marketStateFrom(bars), nil
			}
			strategist := quant.NewStrategist(qAnth, cfg.QuantStrategistModel, "data", etz, qLog, marketFn, func() {
				if qUniverse.Reload() == nil {
					qAlloc.Configure(qUniverse.Allocation())
				}
			})
			go strategist.RunDaily(ctx)
			log.Printf("strategist: armed (weekdays 08:50-09:25 ET, model=%s, llm=%v)", cfg.QuantStrategistModel, qAnth.Enabled())
		}

		// Nightly retrain: grow the clf training set with today's resolved journal
		// outcomes and refresh the models after the close (boot catch-up when stale).
		if sigEngine != nil && cfg.QuantRetrain {
			go runNightlyRetrain(ctx, clfGate)
		}
	}

	// Research loop: auto-run at 13:30 ET (open + 4h) on weekdays; the Python side
	// analyzes the session so far, asks Opus for (rarely) proposals, and reports to
	// Telegram. Proposals are NEVER auto-applied — the operator changes knobs manually.
	if cfg.ResearchLoop {
		go runResearchLoop(ctx)
	}

	if dw.Enabled() {
		dw.Start(ctx)
		log.Printf("dip watcher: ENABLED for the full watchlist (%d symbols, 5-min bounce confirm)", len(watchMgr.All()))
	} else {
		log.Printf("dip watcher: disabled (set TELEGRAM_BOT_TOKEN + TELEGRAM_CHAT_ID)")
	}

	// News enrichment for the DECEPTICON movers panel (optional / disabled-safe).
	geminiClient := gemini.New(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.GeminiRPM, cfg.GeminiDailyCap)
	log.Printf("news enrichment | gemini=%v (model=%s, rpm=%d, cap/day=%d)",
		geminiClient.Enabled(), cfg.GeminiModel, cfg.GeminiRPM, cfg.GeminiDailyCap)

	// HTTP server.
	srv := &api.Server{
		Client:       client,
		Cfg:          cfg,
		Engine:       engine,
		Hub:          h,
		ExecMgr:      execMgr,
		WatchMgr:     watchMgr,
		Fractionable: fractionable,
		Watchlist:    wl,
		Scanner:      scn,
		Flow:         flowTracker,
		Gemini:       geminiClient,
	}
	// Let clients subscribe to ANY symbol over WS (e.g. a DECEPTICON market mover): the
	// hub calls this to backfill + start streaming it on demand if it isn't already live.
	h.EnsureLiveFn = srv.EnsureLive

	srv.Quant = quantEngine
	srv.Evals = evalsFn

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type"},
		AllowCredentials: false,
	}))
	srv.Routes(r)
	r.Get("/ws", wsHandler(ctx, h, cfg.AllowedOrigins))

	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: r}
	go func() {
		log.Printf("HTTP listening on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func runKeycheck(client *alpaca.Client) {
	kc := client.VerifyKeys()
	out, _ := json.MarshalIndent(kc, "", "  ")
	fmt.Println(string(out))
	if !kc.KeysValid {
		fmt.Println("\nRESULT: keys are INVALID.")
		os.Exit(1)
	}
	if !kc.SIPEntitled {
		fmt.Println("\nRESULT: keys valid, but SIP / Algo Trader Plus is NOT entitled.")
		os.Exit(2)
	}
	fmt.Println("\nRESULT: keys valid AND SIP / Algo Trader Plus is ACTIVE.")
}

func runStream(ctx context.Context, client *alpaca.Client, execMgr, watchMgr *execsym.Manager, scanSymbols, paperSymbols, sigSymbols []string, engine *candles.Engine, scn *scanner.Scanner, sigEngine *signals.Engine, h *hub.Hub, fl *flow.Tracker) {
	handlers := alpaca.StreamHandlers{
		OnTrade: func(symbol string, t time.Time, price, size float64) {
			engine.OnTrade(symbol, t, price, size)
			fl.OnTrade(symbol, price, size, t)
			h.BroadcastQuote(hub.Quote{Symbol: symbol, Price: price, Time: t.Unix()})
		},
		OnBar: func(symbol string, t time.Time, o, hi, lo, c, v, vwap float64) {
			// Each consumer no-ops for symbols it doesn't track — all additive.
			engine.OnBar(symbol, t, o, hi, lo, c, v)
			if scn != nil {
				scn.OnBar(symbol, t, o, hi, lo, c, v, vwap)
			}
			if sigEngine != nil {
				sigEngine.OnBar(symbol, t, o, hi, lo, c, v)
			}
		},
		OnQuote: func(symbol string, bid, ask float64, t time.Time) {
			if bid > 0 && ask > 0 {
				h.BroadcastQuote(hub.Quote{Symbol: symbol, Price: (bid + ask) / 2, Time: t.Unix()})
			}
			fl.OnQuote(symbol, bid, ask)
			if scn != nil {
				scn.OnQuote(symbol, bid, ask)
			}
		},
	}
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		// Recompute each (re)connect so runtime-added symbols are re-subscribed. Paper-engine
		// symbols are always included so those engines never lose their candle feed; the
		// signal universe rides the bar channel only (no trades/quotes needed).
		tqSymbols := unionSymbols(unionSymbols(execMgr.All(), watchMgr.All()), paperSymbols)
		barSymbols := unionSymbols(unionSymbols(tqSymbols, scanSymbols), sigSymbols)
		// On every reconnect (but not the first connect — main already backfilled),
		// re-pull the session so any minutes missed while the stream was down (e.g.
		// the laptop slept) are filled in. Seed is idempotent and authoritative.
		if !first {
			backfillLive(client, engine, tqSymbols)
		}
		first = false
		err := client.StartStream(ctx, tqSymbols, barSymbols, handlers)
		if ctx.Err() != nil {
			return
		}
		log.Printf("stream ended: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// backfillLive re-pulls the full session for the given live symbols and re-seeds the
// engine, healing any gap that opened while the stream was disconnected.
func backfillLive(client *alpaca.Client, engine *candles.Engine, symbols []string) {
	for _, sym := range symbols {
		bars, err := client.Backfill(sym)
		if err != nil {
			log.Printf("re-backfill %s failed: %v", sym, err)
			continue
		}
		engine.Seed(sym, bars)
	}
	if len(symbols) > 0 {
		log.Printf("re-backfilled %d symbols after stream reconnect", len(symbols))
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// unionSymbols returns the de-duplicated union of two symbol lists.
func unionSymbols(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// seedScanner backfills prior-close/avg-volume (daily) and today's session bars
// (1-minute) for the whole scan universe, in batched REST calls.
func seedScanner(client *alpaca.Client, scn *scanner.Scanner, symbols []string) {
	// Daily bars → prior close + average daily volume.
	daily, err := client.GetMultiDailyBars(symbols, 25)
	if err != nil {
		log.Printf("DECEPTICON: daily seed error: %v", err)
	}
	today := sessionStartET(time.Now())
	for sym, bars := range daily {
		if len(bars) == 0 {
			continue
		}
		// Drop today's (partial) daily bar if present.
		closed := bars
		if last := bars[len(bars)-1]; !last.Time.Before(today) {
			closed = bars[:len(bars)-1]
		}
		if len(closed) == 0 {
			continue
		}
		prevClose := closed[len(closed)-1].Close
		var sum float64
		n := 0
		for i := len(closed) - 1; i >= 0 && n < 20; i-- {
			sum += closed[i].Volume
			n++
		}
		avgVol := 0.0
		if n > 0 {
			avgVol = sum / float64(n)
		}
		scn.SeedDaily(sym, prevClose, avgVol)
	}

	// Today's 1-minute session bars → immediate intraday metrics + chart data.
	intra, err := client.GetMultiIntradayBars(symbols, today, time.Now())
	if err != nil {
		log.Printf("DECEPTICON: intraday seed error: %v", err)
	}
	for sym, bars := range intra {
		sb := make([]scanner.Bar, 0, len(bars))
		for _, b := range bars {
			sb = append(sb, scanner.Bar{
				Time:   b.Time.Unix(),
				Open:   b.Open,
				High:   b.High,
				Low:    b.Low,
				Close:  b.Close,
				Volume: b.Volume,
				VWAP:   b.VWAP,
			})
		}
		scn.SeedIntraday(sym, sb)
	}

	// Intraday volume profile → time-of-day-aware RVOL (replaces the old flat assumption that
	// over-stated morning volume). Pull ~12 days of 1-minute bars and build each symbol's
	// cumulative-volume curve. Background-safe: RVOL falls back to a flat estimate until a
	// profile lands. This is a one-time heavier fetch, run after the fast seed above.
	if hist, err := client.GetMultiIntradayBars(symbols, time.Now().AddDate(0, 0, -12), time.Now()); err != nil {
		log.Printf("DECEPTICON: volume-profile fetch error: %v", err)
	} else {
		built := 0
		for sym, bars := range hist {
			sb := make([]scanner.Bar, 0, len(bars))
			for _, b := range bars {
				sb = append(sb, scanner.Bar{Time: b.Time.Unix(), Volume: b.Volume})
			}
			if prof := scn.BuildVolumeProfile(sb); len(prof) > 0 {
				scn.SetVolumeProfile(sym, prof)
				built++
			}
		}
		log.Printf("DECEPTICON: built intraday volume profiles for %d symbols", built)
	}
	log.Printf("DECEPTICON: seeded %d daily, %d intraday", len(daily), len(intra))
}

// seedSignalEngine backfills the signal engine's context: daily ATR(14) + 20-day average
// volume per universe symbol, plus today's 1-minute session bars so RVOL and VWAP are
// correct from the first live bar. Read-only REST; runs once in the background.
func seedSignalEngine(client *alpaca.Client, se *signals.Engine, symbols []string) {
	daily, err := client.GetMultiDailyBars(symbols, 25)
	if err != nil {
		log.Printf("signals: daily seed error: %v", err)
	}
	for sym, bars := range daily {
		if len(bars) < 5 {
			continue
		}
		var highs, lows, closes, vols []float64
		for _, b := range bars {
			highs = append(highs, b.High)
			lows = append(lows, b.Low)
			closes = append(closes, b.Close)
			vols = append(vols, b.Volume)
		}
		// ATR(14) + avg volume(20) over the trailing window (same math as dipwatch).
		var trs []float64
		for i := 1; i < len(closes); i++ {
			tr := highs[i] - lows[i]
			if x := closes[i-1] - lows[i]; x > tr {
				tr = x
			}
			if x := highs[i] - closes[i-1]; x > tr {
				tr = x
			}
			trs = append(trs, tr)
		}
		atr := avgLastF(trs, 14)
		av := avgLastF(vols, 20)
		se.SeedDaily(sym, atr, av)
	}

	today := sessionStartET(time.Now())
	intra, err := client.GetMultiIntradayBars(symbols, today, time.Now())
	if err != nil {
		log.Printf("signals: intraday seed error: %v", err)
	}
	for sym, bars := range intra {
		sb := make([]signals.Bar, 0, len(bars))
		for _, b := range bars {
			sb = append(sb, signals.Bar{Time: b.Time.Unix(), Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
		}
		se.SeedBars(sym, sb)
	}
	log.Printf("signals: seeded %d daily contexts, %d intraday sessions", len(daily), len(intra))
}

// runResearchLoop fires ml/research_loop.py once per weekday at 13:30 ET (open + 4h),
// non-interactively with Telegram delivery. Best-effort: a failure logs and retries the
// next day; it can never affect trading.
func runResearchLoop(ctx context.Context) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastDay := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().In(loc)
			day := now.Format("2006-01-02")
			weekday := now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
			if !weekday || day == lastDay || now.Hour() != 13 || now.Minute() < 30 || now.Minute() > 40 {
				continue
			}
			lastDay = day
			go func() {
				cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				cmd := exec.CommandContext(cctx, "../ml/.venv/Scripts/python.exe", "../ml/research_loop.py", "--notify")
				cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
				out, err := cmd.CombinedOutput()
				tail := string(out)
				if len(tail) > 600 {
					tail = tail[len(tail)-600:]
				}
				if err != nil {
					log.Printf("research-loop: failed: %v | %s", err, tail)
					return
				}
				log.Printf("research-loop: done | %s", tail)
			}()
		}
	}
}

// runNightlyRetrain fires ml/train_live.py once per weekday in the 17:05–17:20 ET
// window (after the close and the 16:10 reviewer, so today's resolved journal outcomes
// are included), then hot-reloads the clf gate's models. Boot catch-up: if the gate has
// no fresh models when the process starts (machine was off overnight), it retrains
// immediately instead of trading a stale week. Best-effort: a failure logs, the gate
// fails open, and the next window retries. Paper-side only.
func runNightlyRetrain(ctx context.Context, gate *quant.ClfGate) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	run := func(reason string) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		log.Printf("clf-retrain: starting (%s)", reason)
		cmd := exec.CommandContext(cctx, "../ml/.venv/Scripts/python.exe", "../ml/train_live.py",
			"--data", "data/ml_dataset_12mo.jsonl", "--journal", "data/signals", "--outdir", "data/models")
		cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
		out, err := cmd.CombinedOutput()
		tail := string(out)
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		if err != nil {
			log.Printf("clf-retrain: failed: %v | %s", err, tail)
			return
		}
		log.Printf("clf-retrain: done | %s", tail)
		if gate != nil {
			gate.Reload()
		}
	}

	if now := time.Now().In(loc); now.Weekday() >= time.Monday && now.Weekday() <= time.Friday &&
		gate != nil && !gate.Ready() {
		run("boot catch-up: no fresh models")
	}

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastDay := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().In(loc)
			day := now.Format("2006-01-02")
			weekday := now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
			if !weekday || day == lastDay || now.Hour() != 17 || now.Minute() < 5 || now.Minute() > 20 {
				continue
			}
			lastDay = day
			go run("nightly window")
		}
	}
}

// marketStateFrom derives the Strategist's deterministic pre-market picture from
// SPY/QQQ daily bars (trend, 20d-MA state, ATR% vol proxy).
func marketStateFrom(bars map[string][]alpaca.HistBar) quant.MarketState {
	ms := quant.MarketState{}
	stats := func(bs []alpaca.HistBar) (pct5 float64, above20 bool, atrPct, prevDay float64) {
		n := len(bs)
		if n < 21 {
			return
		}
		last := bs[n-1].Close
		if p := bs[n-6].Close; p > 0 {
			pct5 = (last - p) / p * 100
		}
		var sma float64
		for _, b := range bs[n-20:] {
			sma += b.Close
		}
		above20 = last > sma/20
		var atr float64
		for i := n - 14; i < n; i++ {
			tr := bs[i].High - bs[i].Low
			if x := bs[i].High - bs[i-1].Close; x > tr {
				tr = x
			}
			if x := bs[i-1].Close - bs[i].Low; x > tr {
				tr = x
			}
			atr += tr
		}
		if last > 0 {
			atrPct = atr / 14 / last * 100
		}
		if p := bs[n-2].Close; p > 0 {
			prevDay = (last - p) / p * 100
		}
		return
	}
	if bs := bars["SPY"]; len(bs) > 0 {
		ms.SpyPct5d, ms.SpyAbove20d, _, _ = stats(bs)
	}
	if bs := bars["QQQ"]; len(bs) > 0 {
		ms.QqqPct5d, ms.QqqAbove20d, ms.QqqATRPct, ms.PrevDayQqq = stats(bs)
	}
	return ms
}

// avgLastF averages the last n values of a series.
func avgLastF(vals []float64, n int) float64 {
	if len(vals) == 0 {
		return 0
	}
	if len(vals) > n {
		vals = vals[len(vals)-n:]
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// runScanBroadcaster pushes a throttled scan snapshot (~1/sec) to scan subscribers.
func runScanBroadcaster(ctx context.Context, scn *scanner.Scanner, h *hub.Hub) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if h.ScanSubscriberCount() > 0 {
				h.BroadcastScan(scn.Snapshot())
			}
		}
	}
}

// sessionStartET returns the start (09:30 ET) of the most recent US trading session.
func sessionStartET(now time.Time) time.Time {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	n := now.In(et)
	start := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, et)
	if n.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	for start.Weekday() == time.Saturday || start.Weekday() == time.Sunday {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

func runAccountPoller(ctx context.Context, client *alpaca.Client, h *hub.Hub, refresh <-chan struct{}) {
	// Reconciliation cadence. Equity/P&L glide live on the client between polls;
	// this just keeps the authoritative numbers (cash, buying power) fresh.
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	push := func() {
		if a, err := client.GetAccount(); err == nil {
			h.BroadcastTyped("account", a)
		}
		if p, err := client.GetPositions(); err == nil {
			h.BroadcastTyped("positions", p)
		}
		if o, err := client.GetOpenOrders(); err == nil {
			h.BroadcastTyped("orders", o)
		}
	}
	push()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			push()
		case <-refresh:
			push()
		}
	}
}

func wsHandler(ctx context.Context, h *hub.Hub, origins []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: originPatterns(origins),
		})
		if err != nil {
			return
		}
		h.Serve(ctx, conn)
	}
}

// originPatterns converts allowed origin URLs to host patterns for websocket.Accept.
func originPatterns(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		host := o
		if i := indexAfter(o, "://"); i >= 0 {
			host = o[i:]
		}
		out = append(out, host)
	}
	return out
}

func indexAfter(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i + len(sub)
		}
	}
	return -1
}
