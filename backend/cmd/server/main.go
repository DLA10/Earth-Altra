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
	"os/exec"
	"os/signal"
	"sort"
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
	"live-optimus/backend/internal/breadcrumbs"
	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/config"
	"live-optimus/backend/internal/dipwatch"
	"live-optimus/backend/internal/evals"
	"live-optimus/backend/internal/execsym"
	"live-optimus/backend/internal/flow"
	"live-optimus/backend/internal/gemini"
	"live-optimus/backend/internal/hub"
	"live-optimus/backend/internal/quant"
	"live-optimus/backend/internal/rbt"
	"live-optimus/backend/internal/ridp"
	"live-optimus/backend/internal/risk"
	"live-optimus/backend/internal/scanner"
	"live-optimus/backend/internal/signals"
	"live-optimus/backend/internal/sndk"
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

	// Quant-pipeline symbols that must stream like execution/watchlist symbols so the agents
	// always see fresh candles (incl. across SIP reconnects). SPY/QQQ provide the
	// market-context backdrop the entry/exit agents read on every decision.
	var paperSymbols []string
	if (cfg.PaperClaudeKey != "" && cfg.PaperClaudeSecret != "") ||
		(cfg.PaperDipKey != "" && cfg.PaperDipSecret != "") {
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
	var surgerSymbols []string // tradables ONLY — never SPY/QQQ/SMH context symbols
	if uni, err := signals.LoadUniverse(cfg.QuantUniverseCandidates...); err != nil {
		log.Printf("signals: disabled — %v", err)
	} else {
		sigEngine = signals.NewEngine(uni, "data")
		sigSymbols = uni.All()
		surgerSymbols = uni.Symbols()
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

	// Start the single SIP stream (reconnect on failure). On each (re)connect it
	// subscribes trades/quotes for the current execution symbols (base + added) and
	// bars for the union of execution + scan + signal universes, so added symbols
	// survive reconnects.
	go runStream(ctx, client, execMgr, watchMgr, scanSymbols, paperSymbols, sigSymbols, engine, scn, sigEngine, srgMgr, h, flowTracker)

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

	var quantEngine *quant.Engine      // exposed to the API after srv is built
	var quantManagers []*quant.Manager // both desk managers — wired to EnsureLive after srv is built
	var evalsFn func() interface{}     // eval scoreboard accessor for /api/evals
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
		// TWO desks, TWO capital pots: qAlloc funds the DIP+RISE desk (Agent 2 dips + the
		// rise watcher, on the PAPER_DIP account); sigAlloc funds the SIGNAL desk (the
		// 6-strategy engine, on the PAPER_CLAUDE account). Same configured budget each;
		// each is equity-capped to its OWN account below.
		qAlloc := quant.NewAllocator()
		qAlloc.Configure(qUniverse.Allocation())
		sigAlloc := quant.NewAllocator()
		sigAlloc.Configure(qUniverse.Allocation())
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
		// Agent 4 is advisory only and nil-safe everywhere (engine.sentimentScore / snapshot
		// both guard on agent4 == nil), so when QUANT_SENTIMENT=false we simply never wire it:
		// the dip/rise desk runs identically to how it does with Ollama offline, minus the
		// per-symbol failed-request log spam. Live execution, RIDP, SNDK, RBT and Breadcrumbs
		// never touch this path.
		if cfg.QuantSentiment {
			qAgent4 := quant.NewAgent4(cfg.OllamaEndpoint, cfg.OllamaModel, newsFn, etz)
			qEngine.SetAgent4(qAgent4)
			if qAgent2.Enabled() {
				go qAgent4.Run(ctx, qUniverse.Symbols) // no point scoring sentiment if Agent 2 is idle
			}
		} else {
			log.Printf("quant: Agent 4 sentiment disabled (QUANT_SENTIMENT=false)")
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
						sigAlloc.Configure(qUniverse.Allocation())
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

		// Execution: Agent 3 (exit) + per-desk paper brokers + per-desk position managers
		// (deterministic trailing-stop floor at fill, then Agent 3 manages the exit).
		// dipBroker/qMgr = the DIP+RISE desk (PAPER_DIP account); sigBroker/sigMgr = the
		// SIGNAL desk (PAPER_CLAUDE account). Agent 3 itself is stateless and shared.
		qAgent3 := quant.NewAgent3(qAnth, cfg.QuantExitModel)
		dipBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperDipKey, cfg.PaperDipSecret)
		sigBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperClaudeKey, cfg.PaperClaudeSecret)
		qMgr := quant.NewManager(qEngine, qAlloc, dipBroker, qAgent3, cfg.QuantTrailPct, cfg.QuantOvernightCap)
		sigMgr := quant.NewManager(qEngine, sigAlloc, sigBroker, qAgent3, cfg.QuantTrailPct, cfg.QuantOvernightCap)
		quantManagers = append(quantManagers, qMgr, sigMgr)
		qEngine.SetExecution(dipBroker, qMgr)
		qEngine.SetSignalExecution(sigBroker, sigAlloc, cfg.QuantSignalsLive && sigBroker.Enabled())
		qEngine.SetContext(ctx)

		// Keep each desk's allocator budget capped at ITS account's REAL equity, so a desk
		// never tries to deploy more cash than its account actually holds (e.g. after a
		// drawdown). Sync once now, before any entry, then every 60s.
		syncDesk := func(name string, b *quant.Broker, a *quant.Allocator) {
			if !b.Enabled() {
				return
			}
			syncEquity := func() {
				if ai, err := b.Account(); err == nil && ai.Equity > 0 {
					a.SetEquityCeiling(ai.Equity)
				}
			}
			syncEquity()
			log.Printf("quant: %s desk allocator synced to its paper account equity", name)
			go func() {
				t := time.NewTicker(60 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						syncEquity()
					}
				}
			}()
		}
		syncDesk("dip+rise", dipBroker, qAlloc)
		syncDesk("signal", sigBroker, sigAlloc)

		// Restart resilience, per desk: re-adopt any position that survived a process
		// restart — re-attach (or freshly place) its protective stop, re-fund that desk's
		// allocator, and resume its Agent-3 manage loop. Runs BEFORE any entry source
		// (dip hook / signal trader) can act.
		if n := qMgr.Rehydrate(ctx); n > 0 {
			log.Printf("quant: rehydrated %d open position(s) on the dip+rise account", n)
		}
		if n := sigMgr.Rehydrate(ctx); n > 0 {
			log.Printf("quant: rehydrated %d open position(s) on the signal account", n)
		}

		// Daily post-market review (Opus): reads the decision log + the MERGED trades of
		// both desk accounts, writes a structured report to backend/data/reviews/.
		qReviewer := quant.NewReviewer(qAnth, cfg.QuantReviewModel, qLog,
			qEngine.MergedState, "data", etz)
		go qReviewer.RunDaily(ctx)

		if cfg.QuantLive && qAgent2.Enabled() && dipBroker.Enabled() {
			qEngine.SetLive(true)
			log.Printf("quant: dip+rise desk LIVE (paper) | entry=%s exit=%s trail=%.1f%% | universe=%d symbols",
				cfg.QuantEntryModel, cfg.QuantExitModel, cfg.QuantTrailPct, len(qUniverse.Symbols()))
		} else if cfg.QuantLive && qAgent2.Enabled() {
			log.Printf("quant: dip+rise desk SHADOW — QUANT_LIVE=true but no PAPER_DIP keys (set PAPER_DIP_KEY/SECRET to arm it)")
		} else if qAgent2.Enabled() {
			log.Printf("quant: dip+rise desk SHADOW (set QUANT_LIVE=true + PAPER_DIP keys to place orders) | universe=%d", len(qUniverse.Symbols()))
		} else {
			log.Printf("quant: idle (set ANTHROPIC_API_KEY); dip hook installed")
		}
		// QUANT_LIVE only benches the DIP+RISE desk. Say so loudly when the signal desk is
		// still live, so "I benched the quant desk" can't silently mean "only half of it"
		// (the 2026-07 incident: QUANT_SIGNALS_LIVE was absent and defaulted to true).
		if !cfg.QuantLive && cfg.QuantSignalsLive && sigBroker.Enabled() {
			log.Printf("quant: ⚠ NOTE — dip+rise desk is SHADOW (QUANT_LIVE=false) but the SIGNAL desk is still LIVE; set QUANT_SIGNALS_LIVE=false to bench the whole team")
		}

		// Dip+rise desk daily loss cap (same $ cap as the signal desk, tracked separately):
		// approximate realized P&L from every close on THIS desk; once −cap is hit, new dip
		// and rise entries are skipped until tomorrow.
		dipLimits := risk.Defaults()
		dipLimits.DailyLossCapUSD = cfg.QuantDailyLossCap
		dipDay := risk.NewDay(dipLimits, etz)
		qMgr.OnClosed = func(sym string, pnl float64) {
			dipDay.OnRealized(pnl, time.Now())
			if realized, halted := dipDay.Realized(time.Now()); halted {
				log.Printf("[dip-desk] DAILY LOSS CAP HIT (day P&L ≈ $%.2f) — no more dip/rise entries today", realized)
			}
		}
		qEngine.SetDayRisk(dipDay)

		// ---- Signal-engine paper execution (the validated Tier-1 champion config) ----
		// Bridge: signal → learned time-of-day gate → LLM entry judge (red-flag veto +
		// conviction sizing) → the SIGNAL desk's own allocator → its own Manager (entry,
		// trailing-stop floor, Agent 3 exits, EOD flatten) → the PAPER_CLAUDE account.
		// Fully separate from the dip+rise desk's account/budget/loss cap. The dipwatch
		// Telegram flow is untouched.
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

		if sigEngine != nil && sigBroker.Enabled() {
			judge := quant.NewSignalJudge(qAnth, cfg.QuantJudgeModel)
			limits := risk.Defaults()
			limits.DailyLossCapUSD = cfg.QuantDailyLossCap
			trader := quant.NewSignalTrader(ctx, qEngine, sigAlloc, sigMgr, sigEngine, judge, limits, cfg.QuantSignalsLive, cfg.QuantTODGate, cfg.QuantAlignGate)
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
				alignMode := "enforcing"
				if !cfg.QuantAlignGate {
					alignMode = "off"
				}
				log.Printf("signal-trader: LIVE (paper) | judge=%s (enabled=%v) | daily loss cap $%.0f | TOD gate %s | alignment playbook %s | scoreboard demotion active",
					cfg.QuantJudgeModel, judge.Enabled(), cfg.QuantDailyLossCap, todMode, alignMode)
			} else {
				log.Printf("signal-trader: disabled (QUANT_SIGNALS_LIVE=false) — shadow journaling only")
			}
		} else if sigEngine != nil {
			log.Printf("signal-trader: no paper broker keys — shadow journaling only")
		}

		// ---- Rising watcher: monetize confirmed post-dip bounces Agent 2 passed on ----
		// Every in-universe dip Agent 2 declines is armed for 10 minutes; a green 1-min
		// close +0.10% above the dip price — with the dip low intact and no volume fade —
		// confirms the rise and enters with deterministic, time-boxed exits (1.5% trail,
		// +2R target, 40-min max hold). Rule validated on a month-long replay of the
		// dipwatch recipe (391 reconstructed dips). No LLM on this path. Part of the
		// DIP+RISE desk: shares its allocator, loss cap, Manager, and PAPER_DIP account;
		// gated by QUANT_RISE_LIVE (false = shadow: journals + Telegram, no orders).
		riseLive := cfg.QuantRiseLive && dipBroker.Enabled()
		riseWatch := quant.NewRiseWatch(qEngine, qMgr, dipDay, etz, riseLive, dw.Notify)
		qEngine.SetRiseWatch(riseWatch)
		riseWatch.Start(ctx)
		switch {
		case riseLive:
			log.Printf("rise-watch: LIVE (paper) | confirm = green 1-min close +0.10%% within 10m, dip low holds, no volume fade | trail 1.5%% | target +2R | max hold 40m")
		case cfg.QuantRiseLive:
			log.Printf("rise-watch: SHADOW — QUANT_RISE_LIVE=true but no PAPER_DIP keys (set PAPER_DIP_KEY/SECRET to arm the dip+rise desk)")
		default:
			log.Printf("rise-watch: SHADOW (set QUANT_RISE_LIVE=true to place paper orders) — arms declined dips, journals every trigger")
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
					// The Strategist's posture/budget applies to BOTH desks.
					qAlloc.Configure(qUniverse.Allocation())
					sigAlloc.Configure(qUniverse.Allocation())
				}
			}, sigSymbols)
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
	if srgMgr != nil {
		srv.Surger = func() interface{} { return srgMgr.Report() }
	}
	srv.Evals = evalsFn

	// Sub-second position P&L on the quant pages: subscribe each desk position's
	// trades/quotes when it opens (and any already-rehydrated survivors right now).
	// Signal-universe symbols otherwise get 1-minute bars only, which made open-position
	// prices look frozen on the frontend while the backend was fine.
	for _, m := range quantManagers {
		m.SetEnsureLive(func(sym string) { go srv.EnsureLive(sym) })
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
		if bu, err := signals.LoadUniverse(os.Getenv("RBT_UNIVERSE_PATH"),
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

	// SNDK 1-Minute Micro-Scalper paper desk. STRICT one-account-per-desk: runs only on
	// its OWN keys (PAPER_SNDK_*) — no fallback to the RBT account. Empty keys = benched.
	if cfg.PaperSndkKey != "" && cfg.PaperSndkSecret != "" {
		etzSndk, lerr := time.LoadLocation("America/New_York")
		if lerr != nil {
			etzSndk = time.UTC
		}
		sndkBroker := quant.NewBroker("https://paper-api.alpaca.markets/v2", cfg.PaperSndkKey, cfg.PaperSndkSecret)
		sndkMgr := sndk.New(sndkBroker, engine, etzSndk, "data", true)
		sndkMgr.SetEnsureLive(func(sym string) { go srv.EnsureLive(sym) })
		sndkMgr.Start(ctx)
		srv.Sndk = func() interface{} { return sndkMgr.Report() }
		log.Printf("sndk: initialized and running on paper account")
	} else {
		log.Printf("sndk: disabled (no PAPER_SNDK keys — strict one account per desk)")
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
		bcMgr.Start(ctx)
		if cfg.BreadcrumbsRetrain {
			bcMgr.StartRetrain(ctx) // monthly rolling retrain + boot catch-up (hands-off)
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

func runStream(ctx context.Context, client *alpaca.Client, execMgr, watchMgr *execsym.Manager, scanSymbols, paperSymbols, sigSymbols []string, engine *candles.Engine, scn *scanner.Scanner, sigEngine *signals.Engine, srgMgr *surger.Manager, h *hub.Hub, fl *flow.Tracker) {
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

		// Rolling trend for the alignment playbook: yesterday's close vs the SMA of the
		// prior 20 daily closes. Drop today's (possibly partial) daily bar first so a
		// mid-session boot doesn't peek at today.
		et := sessionStartET(time.Now()).Location()
		todayET := time.Now().In(et).Format("2006-01-02")
		tcl := closes
		if last := bars[len(bars)-1]; last.Time.In(et).Format("2006-01-02") == todayET {
			tcl = closes[:len(closes)-1]
		}
		if len(tcl) >= 21 {
			var ma float64
			for _, c := range tcl[len(tcl)-21 : len(tcl)-1] {
				ma += c
			}
			ma /= 20
			se.SeedTrend(sym, tcl[len(tcl)-1] > ma)
		}
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
