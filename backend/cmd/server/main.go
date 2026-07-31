// Command server runs the Live-Optimus backend: SIP market-data ingest, candle
// aggregation, a WebSocket fan-out hub, and the trading/account REST API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
	_ "time/tzdata" // bundle the tz database so America/New_York works on Windows

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"live-optimus/backend/internal/alpaca"
	"live-optimus/backend/internal/api"
	"live-optimus/backend/internal/breadcrumbs"
	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/config"
	"live-optimus/backend/internal/dipwatch"
	"live-optimus/backend/internal/execsym"
	"live-optimus/backend/internal/flow"
	"live-optimus/backend/internal/gemini"
	"live-optimus/backend/internal/hub"
	"live-optimus/backend/internal/quant"
	"live-optimus/backend/internal/rbt"
	"live-optimus/backend/internal/ridp"
	"live-optimus/backend/internal/moverwatch"
	"live-optimus/backend/internal/regime"
	"live-optimus/backend/internal/scanner"
	"live-optimus/backend/internal/universe"
	"live-optimus/backend/internal/surger"
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

	// Market-context symbols. These must ALWAYS be registered in the candle engine and
	// streamed like execution/watchlist symbols — candles.Engine.OnBar silently DROPS bars
	// for unregistered symbols, and RIDP's RIDER entry gate reads QQQ's session open/last
	// (ridp/rider.go: "QQQ not falling"). With no QQQ candles that gate reads 0 and RIDER
	// stops entering with no error and no log line. This list was previously gated on the
	// AI-quant account keys; when that desk was removed 2026-07-31 the gate was dropped so
	// the failure mode can never come back. Never make these conditional on a desk's keys.
	contextSymbols := []string{"SPY", "QQQ"}

	// All symbols that need full live (trades/quotes) treatment.
	liveSymbols := unionSymbols(unionSymbols(execMgr.All(), watchMgr.All()), contextSymbols)

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

	// Shadow Movers recorder (operator request 2026-07-22): re-derives the Risers
	// table server-side and journals green-signal names' prices every 15 min,
	// 09:45–16:00 ET → data/moverwatch/<day>.jsonl. Log-only; no orders, no UI change.
	var moverRec *moverwatch.Recorder
	if scn != nil {
		etzMw, merr := time.LoadLocation("America/New_York")
		if merr != nil {
			etzMw = time.UTC
		}
		moverRec = moverwatch.New(scn, etzMw, "data", func() map[string]bool {
			own := map[string]bool{}
			for _, s := range execMgr.All() {
				own[s] = true
			}
			for _, s := range watchMgr.All() {
				own[s] = true
			}
			return own
		})
		moverRec.Start(ctx)
	}

	// Order-flow tracker: estimates buyer/seller-initiated volume from trades + quotes.
	flowTracker := flow.New()

	// ---- Trading universe (QUANT_UNIVERSE.json) ----
	// The curated symbol set. The AI signal desk that originally owned this loader was
	// retired 2026-07-31; the universe itself is load-bearing for four live desks:
	// sigSymbols is the SIP bar-subscription set (barSymbols below) and RIDP's universe;
	// surgerSymbols (tradables only, never SPY/QQQ/SMH context tickers) feeds SURGER and
	// the regime detector. RBT loads its own baseline file separately further down.
	var sigSymbols []string
	var surgerSymbols []string
	if uni, err := universe.Load(cfg.QuantUniverseCandidates...); err != nil {
		log.Printf("universe: disabled — %v", err)
	} else {
		sigSymbols = uni.All()
		surgerSymbols = uni.Symbols()
		log.Printf("universe: %d tradable symbols (+%d context) loaded", len(uni.Symbols()), len(uni.Context()))
	}

	// SURGER v2 lab: three continuation detectors (C2 cusum / C1 purity / SPECTRAL),
	// validated over four backtest windows (SURGER_V2.md), trading LIVE paper on the
	// DIP+RISE account with srg*_ coid attribution. Additive bar consumer off the same
	// SIP stream (completed bars only — no forming-bar skew). Symbol exclusivity keeps
	// it from ever touching a dip+rise position; quant Rehydrate skips srg* coids.
	var srgMgr *surger.Manager
	if cfg.PaperDipKey != "" && cfg.PaperDipSecret != "" && len(surgerSymbols) > 0 && cfg.SurgerLive {
		srgBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperDipKey, cfg.PaperDipSecret)
		etzSrg, lerr := time.LoadLocation("America/New_York")
		if lerr != nil {
			etzSrg = time.UTC
		}
		srgMgr = surger.New(srgBroker, etzSrg, "data", true, surgerSymbols, cfg.SurgerNotional, cfg.SurgerSlots)
		srgMgr.Start(ctx)
	} else {
		log.Printf("surger: disabled (needs PAPER_DIP keys + signal universe + SURGER_LIVE)")
	}

	// Shadow regime detector (D3 morning-probe, REGIME_DETECTOR_STUDY.md): log-only
	// daily TREND/CHOP prediction at 11:31 ET, outcome scored 16:05. Needs no keys —
	// it reads candles and writes a journal; it gates nothing until it earns it.
	var regimeDet *regime.Detector
	if len(surgerSymbols) > 0 {
		etzRg, lerr := time.LoadLocation("America/New_York")
		if lerr != nil {
			etzRg = time.UTC
		}
		regimeDet = regime.New(engine, etzRg, "data", surgerSymbols)
		// Official 1-min bars via batched REST — the engine only carries the ~40
		// trade-subscribed names, not the 534-name universe the probes need (the
		// 2026-07-22 first live run died with "only 44 symbols with morning bars").
		regimeDet.SetBarsFn(func(symbols []string, start, end time.Time) map[string][]candles.Candle {
			hist, herr := client.GetMultiIntradayBars(symbols, start, end)
			if herr != nil {
				log.Printf("regime: bar fetch failed: %v", herr)
				return nil
			}
			out := make(map[string][]candles.Candle, len(hist))
			for sym, bars := range hist {
				cs := make([]candles.Candle, 0, len(bars))
				for _, b := range bars {
					cs = append(cs, candles.Candle{Time: b.Time.Unix(), Open: b.Open,
						High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
				}
				out[sym] = cs
			}
			return out
		})
		regimeDet.Start(ctx)
	}

	// Start the single SIP stream (reconnect on failure). On each (re)connect it
	// subscribes trades/quotes for the current execution symbols (base + added) and
	// bars for the union of execution + scan + signal universes, so added symbols
	// survive reconnects.
	go runStream(ctx, client, execMgr, watchMgr, scanSymbols, contextSymbols, sigSymbols, engine, scn, srgMgr, h, flowTracker)

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

	if srgMgr != nil {
		srv.Surger = func() interface{} { return srgMgr.Report() }
	}
	if regimeDet != nil {
		srv.Regime = func() interface{} { return regimeDet.Report() }
	}
	if moverRec != nil {
		srv.MoverWatch = func() interface{} { return moverRec.Report() }
	}
	// ---- RIDP: the two-strategy deterministic paper desk (RIDER + DIPPER) ----
	// The operator's two validated patterns, no LLM anywhere on the trade path, pure-code
	// budget allocation against its paper account's live buying power. Runs side by side
	// with (and never touches) the AI quant desk; order attribution via "ridp_" coids.
	// STRICT one-account-per-desk: RIDP runs only on its OWN keys (PAPER_RIDP_*) — no
	// fallback to a shared account (on a shared account the desks liquidate each other's
	// shares and starve each other's buying power; the 2026-07-13/14 incident).
	if cfg.PaperRidpKey != "" && cfg.PaperRidpSecret != "" {
		etzRidp, lerr := time.LoadLocation("America/New_York")
		if lerr != nil {
			etzRidp = time.UTC
		}
		ridpBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperRidpKey, cfg.PaperRidpSecret)
		ridpDaily := func(symbols []string, n int) (map[string][]ridp.DailyBar, error) {
			raw, err := client.GetMultiDailyBars(symbols, n)
			if err != nil {
				return nil, err
			}
			out := make(map[string][]ridp.DailyBar, len(raw))
			for sym, bars := range raw {
				db := make([]ridp.DailyBar, 0, len(bars))
				for _, b := range bars {
					db = append(db, ridp.DailyBar{Day: b.Time.In(etzRidp).Format("2006-01-02"),
						Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
				}
				out[sym] = db
			}
			return out, nil
		}
		ridpMgr := ridp.New(ridpBroker, engine, sigSymbols, etzRidp, "data", cfg.RidpLive, ridpDaily)
		ridpMgr.SetEnsureLive(func(sym string) { go srv.EnsureLive(sym) })
		// Time-of-day-aware RVOL for RIDER: build each symbol's intraday cumulative-volume
		// curve from ~12 days of 1-minute bars (U-shaped: heavy open/close, light midday) so
		// the "2x normal for this time of day" gate is honest instead of assuming volume
		// accrues linearly. Background-safe: RIDER uses a flat fallback until this lands.
		go func() {
			hist, err := client.GetMultiIntradayBars(sigSymbols, time.Now().AddDate(0, 0, -12), time.Now())
			if err != nil {
				log.Printf("ridp: volume-profile fetch error: %v", err)
				return
			}
			profs := make(map[string][]float64, len(hist))
			for sym, bars := range hist {
				vb := make([]ridp.VolBar, 0, len(bars))
				for _, b := range bars {
					vb = append(vb, ridp.VolBar{Time: b.Time.Unix(), Volume: b.Volume})
				}
				if p := ridp.BuildVolumeProfile(etzRidp, vb); p != nil {
					profs[sym] = p
				}
			}
			ridpMgr.SetVolumeProfiles(profs)
		}()
		ridpMgr.Start(ctx)
		// Shadow Guardian: log-only P&L overseer (desk-stop / ratchet / lock / cascade /
		// bench counterfactuals for the Friday decision). Cannot trade by construction.
		ridp.NewGuardian(ridpBroker, engine, etzRidp, "data").Start(ctx)
		srv.Ridp = func() interface{} { return ridpMgr.Report() }
	} else {
		log.Printf("ridp: disabled (no PAPER_RIDP keys — strict one account per desk)")
	}

	// RBT (Rubber Band Trading) mean-reversion paper desk
	if cfg.PaperRbtKey != "" && cfg.PaperRbtSecret != "" {
		etzRbt, lerr := time.LoadLocation("America/New_York")
		if lerr != nil {
			etzRbt = time.UTC
		}
		rbtBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperRbtKey, cfg.PaperRbtSecret)
		// 200 plan (2026-07-20): scan universe = curated liquid baseline (~160, the
		// pre-throughput-expansion QUANT_UNIVERSE snapshot — liquid + shortable, which the
		// full 534-name file is not) ∪ the legacy RBT 100. Priced via one REST snapshot at
		// scan time, so universe size adds nothing to the SIP stream.
		rbtUni := rbt.RbtUniverse
		if bu, err := universe.Load(os.Getenv("RBT_UNIVERSE_PATH"),
			"../QUANT_UNIVERSE.baseline-2026-07-16.json", "QUANT_UNIVERSE.baseline-2026-07-16.json"); err == nil {
			set := map[string]bool{}
			for _, s := range rbtUni {
				set[s] = true
			}
			for _, s := range bu.Symbols() {
				set[s] = true
			}
			rbtUni = make([]string, 0, len(set))
			for s := range set {
				rbtUni = append(rbtUni, s)
			}
			sort.Strings(rbtUni)
		} else {
			log.Printf("rbt: baseline universe file not found (%v) — legacy %d-name universe", err, len(rbtUni))
		}
		rbtMgr := rbt.New(rbtBroker, engine, etzRbt, "data", true, rbtUni)
		rbtMgr.SetEnsureLive(func(sym string) { go srv.EnsureLive(sym) })
		rbtMgr.SetDaySnapFn(func(syms []string) (map[string]rbt.DaySnap, error) {
			nowET := time.Now().In(etzRbt)
			open := time.Date(nowET.Year(), nowET.Month(), nowET.Day(), 9, 30, 0, 0, etzRbt)
			bars, err := client.GetMultiIntradayBars(syms, open, time.Now())
			if err != nil {
				return nil, err
			}
			out := make(map[string]rbt.DaySnap, len(bars))
			for sym, bl := range bars {
				if len(bl) == 0 {
					continue
				}
				s := rbt.DaySnap{High: -math.MaxFloat64, Low: math.MaxFloat64}
				for _, b := range bl {
					s.Close = b.Close
					if b.High > s.High {
						s.High = b.High
					}
					if b.Low < s.Low {
						s.Low = b.Low
					}
					s.Volume += b.Volume
				}
				out[sym] = s
			}
			return out, nil
		})
		rbtMgr.Start(ctx)
		srv.Rbt = func() interface{} { return rbtMgr.Report() }
		log.Printf("rbt: initialized and running on paper account")
	} else {
		log.Printf("rbt: disabled (no PAPER_RBT keys)")
	}


	// Breadcrumbs: the generalized volatility scalper (SNDK pipeline extended to the
	// validated 22-name volatile basket) with a hard budget tracker + a leak-proof book
	// reconciled against the broker every cycle. STRICT one-account-per-desk: its OWN keys
	// (PAPER_BREADCRUMBS_*) — empty keys = benched, no fallback.
	if cfg.PaperBreadcrumbsKey != "" && cfg.PaperBreadcrumbsSecret != "" {
		etzBC, lerr := time.LoadLocation("America/New_York")
		if lerr != nil {
			etzBC = time.UTC
		}
		bcBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperBreadcrumbsKey, cfg.PaperBreadcrumbsSecret)
		bcMgr := breadcrumbs.New(bcBroker, engine, etzBC, "data", cfg.BreadcrumbsLive,
			cfg.BreadcrumbsUniverse, cfg.BreadcrumbsBudget, cfg.BreadcrumbsNotional, cfg.BreadcrumbsMaxSlots,
			cfg.BreadcrumbsTPPct, cfg.BreadcrumbsSLPct, cfg.BreadcrumbsTrailPct, cfg.BreadcrumbsLock,
			cfg.BreadcrumbsLossCap)
		bcMgr.SetEnsureLive(func(sym string) { go srv.EnsureLive(sym) })
		bcMgr.SetCutUSD(cfg.BreadcrumbsCutUSD)
		bcMgr.SetRetrainDays(cfg.BreadcrumbsRetrainDays)
		bcMgr.Start(ctx)
		if cfg.BreadcrumbsRetrain {
			bcMgr.StartRetrain(ctx) // weekly rolling retrain + boot catch-up (hands-off)
		}
		srv.Breadcrumbs = func() interface{} { return bcMgr.Report() }
		log.Printf("breadcrumbs: initialized and running on paper account")
	} else {
		log.Printf("breadcrumbs: disabled (no PAPER_BREADCRUMBS keys — strict one account per desk)")
	}

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

func runStream(ctx context.Context, client *alpaca.Client, execMgr, watchMgr *execsym.Manager, scanSymbols, contextSymbols, sigSymbols []string, engine *candles.Engine, scn *scanner.Scanner, srgMgr *surger.Manager, h *hub.Hub, fl *flow.Tracker) {
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
			if srgMgr != nil {
				srgMgr.OnBar(symbol, t, o, hi, lo, c, v)
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
		tqSymbols := unionSymbols(unionSymbols(execMgr.All(), watchMgr.All()), contextSymbols)
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
