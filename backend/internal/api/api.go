// Package api exposes the REST surface: orders (with safety validation), account,
// positions, open orders, asset metadata, and the key/subscription check.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"live-optimus/backend/internal/alpaca"
	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/config"
	"live-optimus/backend/internal/execsym"
	"live-optimus/backend/internal/flow"
	"live-optimus/backend/internal/gemini"
	"live-optimus/backend/internal/hub"
	"live-optimus/backend/internal/quant"
	"live-optimus/backend/internal/scanner"
	"live-optimus/backend/internal/watchlist"
)

// Server holds dependencies for the HTTP handlers.
type Server struct {
	Client   *alpaca.Client
	Cfg      *config.Config
	Engine   *candles.Engine
	Hub      *hub.Hub
	ExecMgr  *execsym.Manager
	WatchMgr *execsym.Manager

	// Fractionable caches per-symbol fractionable flags (mutable at runtime as symbols
	// are added); guarded by fracMu.
	fracMu       sync.RWMutex
	Fractionable map[string]bool

	// nameCache memoizes symbol -> company name (from the asset lookup).
	nameMu    sync.RWMutex
	nameCache map[string]string

	// sectorMap memoizes symbol -> sector (DECEPTICON department), built once.
	sectorOnce sync.Once
	sectorMap  map[string]string

	// DECEPTICON scanner (nil if disabled).
	Watchlist *watchlist.Watchlist
	Scanner   *scanner.Scanner

	// Order-flow tracker (buyer/seller-initiated volume).
	Flow *flow.Tracker

	// Quant is the dip-driven AI pipeline (Agent 2/3 + allocator) on the Claude paper account.
	// Read-only here; nil-safe.
	Quant *quant.Engine

	// Gemini writes the on-click "why is it moving" summary for the movers news dropdown.
	// Nil-safe / disabled-safe; never on the order path.
	Gemini *gemini.Client

	// Evals returns the current eval scoreboard (nil-safe; set by main).
	Evals func() interface{}

	// Ridp returns the RIDP two-strategy paper desk report (nil-safe; set by main).
	Ridp func() interface{}

	// Rbt returns the RBT paper desk report (nil-safe; set by main).
	Rbt func() interface{}

	// Sndk returns the SNDK paper desk report (nil-safe; set by main).
	Sndk func() interface{}

	// Breadcrumbs returns the Breadcrumbs generalized-scalper desk report (nil-safe; set by main).
	Breadcrumbs func() interface{}

	// Surger returns the SURGER v2 three-detector lab report (nil-safe; set by main).
	Surger func() interface{}

	// Regime returns the shadow regime-detector report (nil-safe; set by main).
	Regime func() interface{}

	// movers-news badge cache (Alpaca-only, cheap): short TTL keeps the board snappy.
	mnMu   sync.Mutex
	mnResp *MoversNews
	mnKey  string
	mnAt   time.Time

	// per-symbol headlines cache for the on-click dropdown (Alpaca + optional AV). Short
	// TTL; the heavy Gemini summary is computed separately so the dropdown never blocks.
	snMu    sync.Mutex
	snCache map[string]snEntry

	// background-computed Gemini summaries: the handler returns "pending" immediately and
	// a single-flight goroutine fills this cache; the UI polls until it lands. Guarded by sumMu.
	sumMu       sync.Mutex
	sumCache    map[string]sumEntry
	sumInflight map[string]bool

	// activated tracks symbols brought live ON DEMAND (e.g. a previewed DECEPTICON
	// mover) that aren't in the execution/watchlist sets. Additive only: once live they
	// stay subscribed for the session, and inUse() reports them so a later exec/watch
	// removal can't unsubscribe a symbol someone is still previewing. Guarded by previewMu.
	previewMu sync.Mutex
	activated map[string]bool
}

// EnsureLive makes any symbol live on demand: if the engine isn't already tracking it,
// backfill its session and subscribe its trades/quotes so its chart streams in real
// time. Idempotent and additive — already-live symbols return instantly and nothing is
// ever unsubscribed here. Called from the WS subscribe path.
func (s *Server) EnsureLive(sym string) {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" || s.Engine == nil || s.Engine.Tracks(sym) {
		return
	}
	s.previewMu.Lock()
	if s.activated == nil {
		s.activated = map[string]bool{}
	}
	if s.activated[sym] { // already activated (or in flight) by an earlier preview
		s.previewMu.Unlock()
		return
	}
	s.activated[sym] = true
	s.previewMu.Unlock()

	if _, err := s.activateSymbol(sym); err != nil {
		// Roll back the flag so a later preview can retry a transient failure.
		s.previewMu.Lock()
		delete(s.activated, sym)
		s.previewMu.Unlock()
		log.Printf("preview activate %s failed: %v", sym, err)
		return
	}
	log.Printf("preview: %s is now streaming live", sym)
}

// isActivated reports whether a symbol was brought live on demand (and so must stay
// subscribed even if it's removed from the execution/watchlist sets).
func (s *Server) isActivated(sym string) bool {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	return s.activated[sym]
}

func (s *Server) fractionable(sym string) (bool, bool) {
	s.fracMu.RLock()
	defer s.fracMu.RUnlock()
	v, ok := s.Fractionable[sym]
	return v, ok
}

func (s *Server) setFractionable(sym string, v bool) {
	s.fracMu.Lock()
	s.Fractionable[sym] = v
	s.fracMu.Unlock()
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Routes registers all REST routes on the given router.
func (s *Server) Routes(r chi.Router) {
	r.Get("/healthz", s.health)
	r.Get("/api/keycheck", s.keycheck)
	r.Get("/api/account", s.account)
	r.Get("/api/positions", s.positions)
	r.Get("/api/assets", s.assets)
	r.Get("/api/config", s.publicConfig)
	r.Get("/api/orders", s.listOrders)
	r.Post("/api/orders", s.placeOrder)
	r.Delete("/api/orders", s.cancelAll)
	r.Delete("/api/orders/{id}", s.cancelOne)

	// Execution symbol management (add a stock from DECEPTICON, ready to trade).
	r.Get("/api/execution/symbols", s.listExecSymbols)
	r.Post("/api/execution/symbols", s.addExecSymbol)
	r.Delete("/api/execution/symbols/{symbol}", s.removeExecSymbol)

	// Watchlist (full-size live charts page).
	r.Get("/api/watchlist/symbols", s.listWatchSymbols)
	r.Post("/api/watchlist/symbols", s.addWatchSymbol)
	r.Delete("/api/watchlist/symbols/{symbol}", s.removeWatchSymbol)

	// Historical bars for the 1W/1M/6M/1Y chart ranges (static REST, any symbol).
	r.Get("/api/history", s.history)

	// Opening-movers ranking (15/30/45/60 min from the open).
	r.Get("/api/opening-analysis", s.openingAnalysis)

	// Company names for symbols (cached) — e.g. for chart headings.
	r.Get("/api/asset-names", s.assetNames)

	// Company name + sector for symbols (sector from DECEPTICON departments).
	r.Get("/api/symbol-meta", s.symbolMeta)

	// Search all tradable US stocks by ticker or company name (add-any-stock).
	r.Get("/api/assets/search", s.searchAssets)

	// Market-wide top gainers / losers (Alpaca screener).
	r.Get("/api/movers", s.movers)
	r.Get("/api/movers-news", s.moversNews)
	r.Get("/api/stock-news", s.stockNews)

	// Trade history (fills) — pulled from Alpaca's authoritative activity log.
	r.Get("/api/activities", s.activities)

	// All fills in a window (paginated past the 100 cap) — drives the Metrics page.
	r.Get("/api/fills", s.fills)

	// News headlines (with a sentiment tag) and order-flow (buy/sell pressure).
	r.Get("/api/news", s.news)
	r.Get("/api/pressure", s.pressure)
	r.Get("/api/rvol", s.rvol)

	// Latest price per tracked symbol — seeds the watchlist instantly on load.
	r.Get("/api/quotes", s.quotesSnapshot)

	// Trading readiness (account gating flags + market clock).
	r.Get("/api/readiness", s.readiness)

	// DECEPTICON scanner.
	r.Get("/api/decepticon/watchlist", s.decepticonWatchlist)
	r.Get("/api/decepticon/scan", s.decepticonScan)
	r.Get("/api/decepticon/bars", s.decepticonBars)

	// Quant pipeline (the AI paper-trading team): full report for the Paper·Claude page.
	r.Get("/api/quant", s.quantReport)
	r.Get("/api/diprise", s.dipRiseReport)
	r.Get("/api/ridp", s.ridpReport)
	r.Get("/api/rbt", s.rbtReport)
	r.Get("/api/sndk", s.sndkReport)
	r.Get("/api/breadcrumbs", s.breadcrumbsReport)
	r.Get("/api/surger", s.surgerReport)
	r.Get("/api/regime", s.regimeReport)

	// Eval scoreboard (per-strategy expectancy, demotions, judge calibration).
	r.Get("/api/evals", func(w http.ResponseWriter, r *http.Request) {
		if s.Evals == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
			return
		}
		writeJSON(w, http.StatusOK, s.Evals())
	})

	// Research-loop proposals (ml/research_loop.py): the newest pending batch, or
	// {"pending": []} when none exist yet. Read-only — proposals are always applied by
	// the operator manually, never from here.
	r.Get("/api/proposals", s.latestProposals)
}

func (s *Server) latestProposals(w http.ResponseWriter, r *http.Request) {
	empty := map[string]interface{}{"pending": []interface{}{}}
	dir := filepath.Join("data", "evals")
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	latest := ""
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "proposals_") && strings.HasSuffix(n, ".json") && n > latest {
			latest = n
		}
	}
	if latest == "" {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, latest))
	if err != nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) symbols() []string {
	if s.ExecMgr != nil {
		return s.ExecMgr.All()
	}
	return s.Cfg.Symbols
}

func (s *Server) fractionableCopy() map[string]bool {
	s.fracMu.RLock()
	defer s.fracMu.RUnlock()
	out := make(map[string]bool, len(s.Fractionable))
	for k, v := range s.Fractionable {
		out[k] = v
	}
	return out
}

func (s *Server) listExecSymbols(w http.ResponseWriter, r *http.Request) {
	added := []string{}
	if s.ExecMgr != nil {
		added = s.ExecMgr.Added()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"base":  s.Cfg.Symbols,
		"added": added,
		"all":   s.symbols(),
	})
}

// readSymbolBody parses {"symbol": "..."} and uppercases it.
func readSymbolBody(r *http.Request) (string, error) {
	var body struct {
		Symbol string `json:"symbol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("invalid JSON body")
	}
	sym := strings.ToUpper(strings.TrimSpace(body.Symbol))
	if sym == "" {
		return "", fmt.Errorf("symbol required")
	}
	return sym, nil
}

// activateSymbol makes a symbol live (engine series + backfill + trades/quotes stream
// + fractionable). Shared by execution and watchlist adds. Idempotent at the stream
// level. Returns the asset for validation.
func (s *Server) activateSymbol(sym string) (*alpaca.Asset, error) {
	asset, err := s.Client.GetAsset(sym)
	if err != nil {
		return nil, fmt.Errorf("unknown symbol: %w", err)
	}
	if !asset.Tradable {
		return nil, fmt.Errorf("%s is not tradable on this account", sym)
	}
	s.setFractionable(sym, asset.Fractionable)
	if s.Engine != nil {
		s.Engine.AddSymbol(sym)
		if bars, err := s.Client.Backfill(sym); err == nil {
			s.Engine.Seed(sym, bars)
		}
	}
	_ = s.Client.SubscribeTradeQuote(sym)
	return asset, nil
}

// inUse reports whether a symbol is still needed for live trades/quotes by either set
// (or because it was brought live on demand for a preview — those stay subscribed).
func (s *Server) inUse(sym string) bool {
	if s.isActivated(sym) {
		return true
	}
	if s.ExecMgr != nil {
		for _, x := range s.ExecMgr.All() {
			if x == sym {
				return true
			}
		}
	}
	if s.WatchMgr != nil {
		for _, x := range s.WatchMgr.All() {
			if x == sym {
				return true
			}
		}
	}
	return false
}

func (s *Server) addExecSymbol(w http.ResponseWriter, r *http.Request) {
	if s.ExecMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "execution symbol management unavailable")
		return
	}
	sym, err := readSymbolBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.activateSymbol(sym); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	added := s.ExecMgr.Add(sym)
	// Adding to Execution also adds it to the Watchlist — Execution stocks are always
	// watched. (Watchlist-only adds stay watchlist-only; see addWatchSymbol.)
	if s.WatchMgr != nil && s.WatchMgr.Add(sym) && s.Hub != nil {
		s.Hub.BroadcastTyped("watch_symbols", s.WatchMgr.All())
	}
	if s.Hub != nil {
		s.Hub.BroadcastTyped("exec_symbols", s.symbols())
	}
	writeJSON(w, statusFor(added), map[string]interface{}{"symbol": sym, "added": added, "all": s.symbols()})
}

func (s *Server) removeExecSymbol(w http.ResponseWriter, r *http.Request) {
	if s.ExecMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "execution symbol management unavailable")
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "symbol")))
	// ?both=1 also removes it from the Watchlist (one-shot remove from both lists).
	removeBoth := r.URL.Query().Get("both") == "1"
	if !s.ExecMgr.Remove(sym) {
		writeErr(w, http.StatusNotFound, "symbol not in the execution list")
		return
	}
	if removeBoth && s.WatchMgr != nil && s.WatchMgr.Remove(sym) && s.Hub != nil {
		s.Hub.BroadcastTyped("watch_symbols", s.WatchMgr.All())
	}
	if !s.inUse(sym) {
		_ = s.Client.UnsubscribeTradeQuote(sym)
	}
	if s.Hub != nil {
		s.Hub.BroadcastTyped("exec_symbols", s.symbols())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"symbol": sym, "removed": true, "both": removeBoth, "all": s.symbols()})
}

func (s *Server) listWatchSymbols(w http.ResponseWriter, r *http.Request) {
	syms := []string{}
	if s.WatchMgr != nil {
		syms = s.WatchMgr.All()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"symbols": syms})
}

func (s *Server) addWatchSymbol(w http.ResponseWriter, r *http.Request) {
	if s.WatchMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "watchlist unavailable")
		return
	}
	sym, err := readSymbolBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.activateSymbol(sym); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	added := s.WatchMgr.Add(sym)
	if s.Hub != nil {
		s.Hub.BroadcastTyped("watch_symbols", s.WatchMgr.All())
	}
	writeJSON(w, statusFor(added), map[string]interface{}{"symbol": sym, "added": added, "symbols": s.WatchMgr.All()})
}

func (s *Server) removeWatchSymbol(w http.ResponseWriter, r *http.Request) {
	if s.WatchMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "watchlist unavailable")
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "symbol")))
	if !s.WatchMgr.Remove(sym) {
		writeErr(w, http.StatusNotFound, "symbol not in watchlist")
		return
	}
	if !s.inUse(sym) {
		_ = s.Client.UnsubscribeTradeQuote(sym)
	}
	if s.Hub != nil {
		s.Hub.BroadcastTyped("watch_symbols", s.WatchMgr.All())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"symbol": sym, "removed": true, "symbols": s.WatchMgr.All()})
}

// openingAnalysis ranks ONLY the watchlist symbols by their % move from today's
// regular 09:30 ET open, at the +15/+30/+45/+60 marks. It is computed from the candle
// engine (which holds every watchlist symbol's full session, including any added
// mid-day), and is session-aware: blank before the open, filling in as each mark
// elapses, and blank again after the after-hours close (20:00 ET) until the next open.
// assetName returns the company name for a symbol, memoized (falls back to the symbol).
func (s *Server) assetName(sym string) string {
	s.nameMu.RLock()
	n, ok := s.nameCache[sym]
	s.nameMu.RUnlock()
	if ok {
		return n
	}
	name := sym
	if a, err := s.Client.GetAsset(sym); err == nil && a.Name != "" {
		name = a.Name
	}
	s.nameMu.Lock()
	if s.nameCache == nil {
		s.nameCache = map[string]string{}
	}
	s.nameCache[sym] = name
	s.nameMu.Unlock()
	return name
}

// movers returns market-wide top gainers and losers from Alpaca's screener.
func (s *Server) movers(w http.ResponseWriter, r *http.Request) {
	top := 50
	if v := r.URL.Query().Get("top"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			top = n
		}
	}
	m, err := s.Client.GetMarketMovers(top)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// quotesSnapshot returns, per tracked symbol, the latest known price (the live 1-minute
// candle close from the in-memory engine) and the session reference price (the first
// retained 1-minute close — the same reference a chart snapshot uses). The watchlist
// fetches this on load so every row shows a price AND a % change immediately, instead of
// the % staying blank until the symbol's chart has been opened once. Read-only; engine
// data only — never touches the order/stream path.
func (s *Server) quotesSnapshot(w http.ResponseWriter, r *http.Request) {
	type quoteSnap struct {
		Price float64 `json:"price"`
		Ref   float64 `json:"ref"`
	}
	out := map[string]quoteSnap{}
	if s.Engine != nil {
		syms := s.symbols()
		if s.WatchMgr != nil {
			syms = append(syms, s.WatchMgr.All()...)
		}
		seen := map[string]bool{}
		for _, sym := range syms {
			if seen[sym] {
				continue
			}
			seen[sym] = true
			if snap := s.Engine.Snapshot(sym, 1); len(snap) > 0 {
				if px := snap[len(snap)-1].Close; px > 0 {
					out[sym] = quoteSnap{Price: px, Ref: snap[0].Close}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// searchAssets returns tradable US stocks matching the query by ticker or company name.
func (s *Server) searchAssets(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	res, err := s.Client.SearchAssets(r.URL.Query().Get("q"), limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// sectorFor returns the DECEPTICON department (sector) for a symbol, or "" if unknown.
func (s *Server) sectorFor(sym string) string {
	s.sectorOnce.Do(func() {
		s.sectorMap = map[string]string{}
		if s.Watchlist != nil {
			for _, d := range s.Watchlist.Departments {
				for _, t := range d.Tickers {
					if _, ok := s.sectorMap[t.Symbol]; !ok {
						s.sectorMap[t.Symbol] = d.Name
					}
				}
			}
		}
	})
	return s.sectorMap[sym]
}

// symbolMeta returns {symbol: {name, sector}} for the requested symbols.
func (s *Server) symbolMeta(w http.ResponseWriter, r *http.Request) {
	type meta struct {
		Name   string `json:"name"`
		Sector string `json:"sector"`
	}
	out := map[string]meta{}
	for _, raw := range strings.Split(r.URL.Query().Get("symbols"), ",") {
		if sym := strings.ToUpper(strings.TrimSpace(raw)); sym != "" {
			out[sym] = meta{Name: s.assetName(sym), Sector: s.sectorFor(sym)}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// assetNames returns a {symbol: company name} map for the requested symbols.
func (s *Server) assetNames(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{}
	for _, raw := range strings.Split(r.URL.Query().Get("symbols"), ",") {
		sym := strings.ToUpper(strings.TrimSpace(raw))
		if sym != "" {
			out[sym] = s.assetName(sym)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) openingAnalysis(w http.ResponseWriter, r *http.Request) {
	// scope=execution ranks the EXECUTION symbols (for the execution left-panel auto-sort);
	// the default ranks the WATCHLIST symbols (existing Watchlist-page behavior, unchanged).
	syms := []string{}
	if r.URL.Query().Get("scope") == "execution" {
		syms = s.symbols()
	} else if s.WatchMgr != nil {
		syms = s.WatchMgr.All()
	}
	if s.Engine == nil {
		writeJSON(w, http.StatusOK, []scanner.IntervalRank{})
		return
	}
	writeJSON(w, http.StatusOK, sessionAnalysis(s.Engine, syms, time.Now()))
}

// sessionAnalysis builds the opening-movers ranking from engine candles for the given
// symbols, relative to today's 09:30 ET regular open.
func sessionAnalysis(engine *candles.Engine, symbols []string, now time.Time) []scanner.IntervalRank {
	marks := []int{5, 15, 30, 45, 60}
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	n := now.In(et)
	open := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, et) // regular open
	end := time.Date(n.Year(), n.Month(), n.Day(), 20, 0, 0, 0, et)  // after-hours close
	weekend := n.Weekday() == time.Saturday || n.Weekday() == time.Sunday
	// Active only on a weekday, from the regular open through the after-hours close.
	active := !weekend && !n.Before(open) && !n.After(end)
	openUnix := open.Unix()

	out := make([]scanner.IntervalRank, 0, len(marks))
	for _, m := range marks {
		ir := scanner.IntervalRank{Minutes: m}
		if active && !n.Before(open.Add(time.Duration(m)*time.Minute)) {
			ir.Elapsed = true
			target := openUnix + int64(m*60)
			movers := make([]scanner.Mover, 0, len(symbols))
			for _, sym := range symbols {
				bars := engine.Snapshot(sym, 1)
				var openPx, px float64
				haveOpen := false
				for _, b := range bars {
					if b.Time < openUnix { // ignore pre-market — open is the 09:30 bar
						continue
					}
					if !haveOpen {
						openPx = b.Open
						haveOpen = true
					}
					if b.Time <= target {
						px = b.Close
					}
				}
				if !haveOpen || openPx <= 0 || px <= 0 {
					continue
				}
				movers = append(movers, scanner.Mover{Symbol: sym, Open: openPx, Price: px, Pct: (px - openPx) / openPx * 100})
			}
			rising := append([]scanner.Mover{}, movers...)
			falling := append([]scanner.Mover{}, movers...)
			sort.Slice(rising, func(i, j int) bool { return rising[i].Pct > rising[j].Pct })
			sort.Slice(falling, func(i, j int) bool { return falling[i].Pct < falling[j].Pct })
			ir.Rising = signedMovers(rising, true)
			ir.Falling = signedMovers(falling, false)
		}
		out = append(out, ir)
	}
	return out
}

// signedMovers keeps only genuinely up (or down) names, preserving sort order.
func signedMovers(sorted []scanner.Mover, rising bool) []scanner.Mover {
	out := make([]scanner.Mover, 0, len(sorted))
	for _, mv := range sorted {
		if rising && mv.Pct <= 0 {
			break
		}
		if !rising && mv.Pct >= 0 {
			break
		}
		out = append(out, mv)
	}
	return out
}

func statusFor(added bool) int {
	if added {
		return http.StatusCreated
	}
	return http.StatusOK
}

func (s *Server) activities(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	var after time.Time
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			after = time.Now().AddDate(0, 0, -n)
		}
	}
	fills, err := s.Client.GetFills(after, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, fills)
}

// fills returns every fill in the last N days (default 120, capped 400), paginated
// past Alpaca's 100-per-page limit. The Metrics page computes realized P&L from these.
func (s *Server) fills(w http.ResponseWriter, r *http.Request) {
	days := 120
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 400 {
			days = n
		}
	}
	all, err := s.Client.GetAllFills(time.Now().AddDate(0, 0, -days))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, all)
}

// news returns recent headlines (with a sentiment tag) for the given symbols.
func (s *Server) news(w http.ResponseWriter, r *http.Request) {
	var syms []string
	for _, raw := range strings.Split(r.URL.Query().Get("symbols"), ",") {
		if sym := strings.ToUpper(strings.TrimSpace(raw)); sym != "" {
			syms = append(syms, sym)
		}
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	items, err := s.Client.GetNews(syms, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// pressure returns the buyer/seller-initiated volume estimate for a symbol.
func (s *Server) pressure(w http.ResponseWriter, r *http.Request) {
	sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if sym == "" {
		writeErr(w, http.StatusBadRequest, "symbol required")
		return
	}
	if s.Flow == nil {
		writeJSON(w, http.StatusOK, flow.Pressure{Symbol: sym})
		return
	}
	writeJSON(w, http.StatusOK, s.Flow.Snapshot(sym))
}

// rvol returns the relative-volume reading for a symbol from the DECEPTICON scanner (which
// maintains a time-of-day-aware RVOL via each symbol's learned intraday volume curve). RVOL
// compares today's cumulative volume to what's normal for this stock at this time of day:
// ~1.0 = normal, >1.5 = unusually active, <0.7 = quiet. `available` is false when the symbol
// isn't in the scanner universe (or DECEPTICON is disabled) — the UI shows "n/a" then.
// Read-only; never touches the order/stream path.
func (s *Server) rvol(w http.ResponseWriter, r *http.Request) {
	sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if sym == "" {
		writeErr(w, http.StatusBadRequest, "symbol required")
		return
	}
	out := map[string]interface{}{"symbol": sym, "available": false, "rvol": 0.0}
	if s.Scanner != nil {
		if st, ok := s.Scanner.Get(sym); ok && st.RVOL > 0 {
			out["available"] = true
			out["rvol"] = st.RVOL
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) decepticonWatchlist(w http.ResponseWriter, r *http.Request) {
	if s.Watchlist == nil {
		writeErr(w, http.StatusServiceUnavailable, "DECEPTICON disabled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"departments":  s.Watchlist.Departments,
		"symbol_count": len(s.Watchlist.Symbols),
		"feed":         s.Cfg.DataFeed,
		"sip_degraded": strings.ToLower(s.Cfg.DataFeed) != "sip",
	})
}

func (s *Server) decepticonScan(w http.ResponseWriter, r *http.Request) {
	if s.Scanner == nil {
		writeErr(w, http.StatusServiceUnavailable, "DECEPTICON disabled")
		return
	}
	writeJSON(w, http.StatusOK, s.Scanner.Snapshot())
}

func (s *Server) decepticonBars(w http.ResponseWriter, r *http.Request) {
	if s.Scanner == nil {
		writeErr(w, http.StatusServiceUnavailable, "DECEPTICON disabled")
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if sym == "" {
		writeErr(w, http.StatusBadRequest, "symbol required")
		return
	}
	bars, vwap := s.Scanner.SessionBars(sym)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"symbol": sym,
		"bars":   bars,
		"vwap":   vwap,
	})
}

var historyRanges = map[string]bool{"1W": true, "1M": true, "6M": true, "1Y": true}

// history serves static historical bars for the 1W/1M/6M/1Y chart ranges. It's a pure
// REST passthrough to Alpaca (no engine, no live subscription, no tradable check), so it
// works for ANY symbol and never touches the live streaming path that the Execution page
// depends on. Bars come back in the same {time,open,high,low,close,volume} shape (unix
// seconds) the chart already consumes.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if sym == "" {
		writeErr(w, http.StatusBadRequest, "symbol required")
		return
	}
	rng := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("range")))
	if !historyRanges[rng] {
		writeErr(w, http.StatusBadRequest, "range must be 1W, 1M, 6M, or 1Y")
		return
	}
	bars, err := s.Client.RangeBars(sym, rng)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]candles.Candle, 0, len(bars))
	for _, b := range bars {
		out = append(out, candles.Candle{
			Time:   b.Time.Unix(),
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: b.Volume,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"symbol": sym, "range": rng, "bars": out})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": s.Cfg.Mode()})
}

// publicConfig returns non-secret config the UI needs (symbols, mode, fractionable
// flags, notional cap). Never includes credentials.
func (s *Server) publicConfig(w http.ResponseWriter, r *http.Request) {
	base := s.Cfg.Symbols
	var added []string
	if s.ExecMgr != nil {
		added = s.ExecMgr.Added()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"symbols":            s.symbols(),
		"base_symbols":       base,
		"added_symbols":      added,
		"mode":               s.Cfg.Mode(),
		"feed":               s.Cfg.DataFeed,
		"timeframes":         []int{1, 5, 10},
		"max_order_notional": s.Cfg.MaxOrderNotional,
		"fractionable":       s.fractionableCopy(),
		"decepticon_enabled": s.Scanner != nil,
		"sip_degraded":       strings.ToLower(s.Cfg.DataFeed) != "sip",
	})
}

func (s *Server) keycheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Client.VerifyKeys())
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	a, err := s.Client.GetAccount()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) positions(w http.ResponseWriter, r *http.Request) {
	p, err := s.Client.GetPositions()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	out := make([]*alpaca.Asset, 0, len(s.Cfg.Symbols))
	for _, sym := range s.Cfg.Symbols {
		a, err := s.Client.GetAsset(sym)
		if err != nil {
			out = append(out, &alpaca.Asset{Symbol: sym, Tradable: false, Name: "unavailable: " + err.Error()})
			continue
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	o, err := s.Client.GetOpenOrders()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var req alpaca.OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Symbol = strings.ToUpper(strings.TrimSpace(req.Symbol))
	req.Side = strings.ToLower(strings.TrimSpace(req.Side))
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))

	if err := s.validateOrder(req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Safety: you can only sell what you actually hold (no accidental shorting or
	// overselling). Applies to plain sells and OCO exits.
	if req.Side == "sell" {
		if err := s.checkSellable(req); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}

	o, err := s.Client.PlaceOrder(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "alpaca rejected order: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

// lastPrice returns the latest known price for a symbol from the in-memory candle engine
// (close of the most recent 1-minute candle), or 0 if unknown. Used for server-side order
// size + direction sanity checks (defense in depth alongside the frontend guards).
func (s *Server) lastPrice(sym string) float64 {
	if s.Engine == nil {
		return 0
	}
	if snap := s.Engine.Snapshot(sym, 1); len(snap) > 0 {
		return snap[len(snap)-1].Close
	}
	return 0
}

// checkSellable rejects sells larger than the held position (or with no position),
// so the user can't oversell or accidentally open a short.
func (s *Server) checkSellable(req alpaca.OrderRequest) error {
	positions, err := s.Client.GetPositions()
	if err != nil {
		return fmt.Errorf("could not verify your holdings: %v", err)
	}
	var held *alpaca.Position
	for i := range positions {
		if positions[i].Symbol == req.Symbol {
			held = &positions[i]
			break
		}
	}
	if held == nil || held.Qty <= 0 {
		return fmt.Errorf("you don't hold any %s to sell", req.Symbol)
	}
	// Shares already promised to a resting sell order can't be sold again — two sells that
	// are each <= the position would oversell if both filled. Alpaca "held" legs (the
	// inactive side of an OCO/bracket) share their shares with the active leg, so counting
	// them too would double-count one protection. Best-effort: a transient order-list
	// failure falls back to the plain position check (Alpaca still hard-rejects oversells).
	reserved := 0.0
	if orders, oerr := s.Client.GetOpenOrders(); oerr == nil {
		for _, o := range orders {
			if o.Symbol != req.Symbol || o.Side != "sell" || o.Status == "held" {
				continue
			}
			q, _ := strconv.ParseFloat(o.Qty, 64)
			fq, _ := strconv.ParseFloat(o.FilledQty, 64)
			if rem := q - fq; rem > 0 {
				reserved += rem
			}
		}
	}
	avail := held.Qty - reserved
	if avail <= 1e-9 {
		return fmt.Errorf("all %g of your %s shares are already reserved by open sell orders — cancel one first", held.Qty, req.Symbol)
	}
	if req.Qty > 0 && req.Qty > avail+1e-9 {
		if reserved > 0 {
			return fmt.Errorf("you hold %g %s but %g are already reserved by open sell orders — only %g can still be sold", held.Qty, req.Symbol, reserved, avail)
		}
		return fmt.Errorf("you only hold %g %s — can't sell %g", held.Qty, req.Symbol, req.Qty)
	}
	if req.Notional > 0 {
		// If we can't determine the position's current value, don't let a dollar-amount sell
		// through unchecked — require a share-quantity sell instead.
		if held.MarketValue <= 0 {
			return fmt.Errorf("can't verify the value of your %s position right now — sell by share quantity instead", req.Symbol)
		}
		availValue := held.MarketValue * (avail / held.Qty)
		if req.Notional > availValue+0.01 {
			if reserved > 0 {
				return fmt.Errorf("only $%.2f of your %s is not already reserved by open sell orders — can't sell $%.2f", availValue, req.Symbol, req.Notional)
			}
			return fmt.Errorf("you only hold $%.2f of %s — can't sell $%.2f", held.MarketValue, req.Symbol, req.Notional)
		}
	}
	return nil
}

var (
	validTypes   = map[string]bool{"market": true, "limit": true, "stop": true, "stop_limit": true, "trailing_stop": true}
	validTIF     = map[string]bool{"day": true, "gtc": true, "ioc": true, "fok": true, "opg": true, "cls": true}
	validClasses = map[string]bool{"": true, "simple": true, "bracket": true, "oco": true, "oto": true}
)

// validateOrder enforces our safety rules and the relevant Alpaca constraints BEFORE
// hitting Alpaca (clearer errors than the raw API rejection).
func (s *Server) validateOrder(req alpaca.OrderRequest) error {
	if req.Symbol == "" {
		return fmt.Errorf("symbol required")
	}
	if req.Side != "buy" && req.Side != "sell" {
		return fmt.Errorf("side must be buy or sell")
	}
	if !validTypes[req.Type] {
		return fmt.Errorf("type must be market, limit, stop, stop_limit, or trailing_stop")
	}
	tif := req.TimeInForce
	if tif == "" {
		tif = "day"
	}
	if !validTIF[tif] {
		return fmt.Errorf("invalid time_in_force")
	}
	class := req.OrderClass
	if !validClasses[class] {
		return fmt.Errorf("order_class must be simple, bracket, oco, or oto")
	}
	multiLeg := class == "bracket" || class == "oco" || class == "oto"

	hasQty := req.Qty > 0
	hasNotional := req.Notional > 0
	if hasQty == hasNotional {
		return fmt.Errorf("provide exactly one of qty or notional (and it must be > 0)")
	}

	// Per-type price requirements. OCO carries its prices in the take-profit/stop-loss
	// legs (validated below), not a top-level limit price, so skip this check for OCO.
	if (req.Type == "limit" || req.Type == "stop_limit") && req.LimitPrice <= 0 && class != "oco" {
		return fmt.Errorf("limit_price required for %s orders", req.Type)
	}
	if (req.Type == "stop" || req.Type == "stop_limit") && req.StopPrice <= 0 {
		return fmt.Errorf("stop_price required for %s orders", req.Type)
	}
	if req.Type == "trailing_stop" && req.TrailPrice <= 0 && req.TrailPercent <= 0 {
		return fmt.Errorf("trailing_stop requires trail_price or trail_percent")
	}

	// Notional (dollar) orders: only simple market/limit, TIF day, regular hours.
	if hasNotional {
		if multiLeg {
			return fmt.Errorf("dollar-amount orders aren't allowed for bracket/oco/oto — use share quantity")
		}
		if req.Type != "market" && req.Type != "limit" {
			return fmt.Errorf("dollar-amount orders are only allowed for market or limit orders")
		}
		if tif != "day" {
			return fmt.Errorf("dollar-amount orders require time_in_force = day")
		}
		if req.ExtendedHours {
			return fmt.Errorf("dollar-amount orders aren't allowed in extended hours — use share quantity")
		}
		if frac, ok := s.fractionable(req.Symbol); ok && !frac {
			return fmt.Errorf("%s does not support dollar-amount (notional) orders; order by share quantity instead", req.Symbol)
		}
	}

	// Extended-hours: Alpaca only accepts limit orders with TIF day.
	if req.ExtendedHours {
		if req.Type != "limit" {
			return fmt.Errorf("extended-hours (pre/post-market) orders must be limit orders")
		}
		if tif != "day" {
			return fmt.Errorf("extended-hours orders require time_in_force = day")
		}
	}

	// Bracket/OCO/OTO leg requirements.
	switch class {
	case "bracket":
		if req.TakeProfitLimit <= 0 || req.StopLossStop <= 0 {
			return fmt.Errorf("bracket orders require both a take-profit limit and a stop-loss stop price")
		}
	case "oco":
		if req.TakeProfitLimit <= 0 || req.StopLossStop <= 0 {
			return fmt.Errorf("OCO orders require both a take-profit limit and a stop-loss stop price")
		}
	case "oto":
		if (req.TakeProfitLimit > 0) == (req.StopLossStop > 0) {
			return fmt.Errorf("OTO orders require exactly one of take-profit or stop-loss")
		}
	}

	// Server-side direction guard (defense in depth — the frontend blocks these too). Reject a
	// price that's clearly on the wrong side of the market and would fill/trigger immediately
	// (the "limit far from market fills instantly" incident). A generous 3% band avoids false
	// rejects on near-market / marketable-by-design orders while still catching egregious
	// fat-fingers. Skipped when the live price is unknown.
	if lp := s.lastPrice(req.Symbol); lp > 0 {
		const band = 0.03
		// OCO / bracket protective legs: take-profit must be ABOVE, stop-loss BELOW the
		// reference price. For an OCO (protecting an existing position) and a market-entry
		// bracket that reference is the current market price; for a LIMIT-entry bracket the
		// legs bound the ENTRY price (e.g. stock at $100, buy at $95, TP $98 is valid).
		if class == "oco" || class == "bracket" {
			ref, refName := lp, "current price"
			if class == "bracket" && req.Type == "limit" && req.LimitPrice > 0 {
				ref, refName = req.LimitPrice, "entry price"
			}
			if req.TakeProfitLimit > 0 && req.TakeProfitLimit <= ref {
				return fmt.Errorf("take-profit $%.2f must be ABOVE the %s $%.2f", req.TakeProfitLimit, refName, ref)
			}
			if req.StopLossStop > 0 && req.StopLossStop >= ref {
				return fmt.Errorf("stop-loss $%.2f must be BELOW the %s $%.2f", req.StopLossStop, refName, ref)
			}
		}
		// Top-level entry/exit price (a plain conditional order, or a bracket's LIMIT entry).
		// OCO carries no top-level limit price (LimitPrice == 0), so this skips it.
		if req.Type == "limit" && req.LimitPrice > 0 {
			if req.Side == "buy" && req.LimitPrice > lp*(1+band) {
				return fmt.Errorf("buy-limit $%.2f is well above the current $%.2f — it would fill immediately (likely a mistake)", req.LimitPrice, lp)
			}
			if req.Side == "sell" && req.LimitPrice < lp*(1-band) {
				return fmt.Errorf("sell-limit $%.2f is well below the current $%.2f — it would fill immediately (likely a mistake)", req.LimitPrice, lp)
			}
		} else if req.Type == "stop" && req.StopPrice > 0 {
			if req.Side == "sell" && req.StopPrice > lp*(1+band) {
				return fmt.Errorf("sell-stop $%.2f is well above the current $%.2f — it would trigger immediately (likely a mistake)", req.StopPrice, lp)
			}
			if req.Side == "buy" && req.StopPrice < lp*(1-band) {
				return fmt.Errorf("buy-stop $%.2f is well below the current $%.2f — it would trigger immediately (likely a mistake)", req.StopPrice, lp)
			}
		}
	}

	// Notional safety cap — applies to BUYS only (new exposure / fat-finger entries). SELLS are
	// bounded by checkSellable (you can't sell more than you hold), so a protective stop/limit/
	// trailing sell of a large position is never wrongly blocked here. Estimate the order value
	// from a known price, falling back to the live last price so a market/trailing order sized
	// by SHARE QUANTITY is still capped (previously those slipped through with est=0).
	if s.Cfg.MaxOrderNotional > 0 && req.Side == "buy" {
		est := req.Notional
		if est == 0 {
			px := req.LimitPrice
			if px == 0 {
				px = req.StopPrice
			}
			if px == 0 {
				px = s.lastPrice(req.Symbol)
			}
			est = req.Qty * px
		}
		if est > s.Cfg.MaxOrderNotional {
			return fmt.Errorf("order value $%.2f exceeds MAX_ORDER_NOTIONAL cap $%.2f", est, s.Cfg.MaxOrderNotional)
		}
	}
	return nil
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	rd, err := s.Client.Readiness()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rd)
}

func (s *Server) cancelOne(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Client.CancelOrder(id); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled", "id": id})
}

func (s *Server) cancelAll(w http.ResponseWriter, r *http.Request) {
	if err := s.Client.CancelAllOrders(); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "all_canceled"})
}
